package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Personal transactions: one trades row seen from the caller's side (role = the side whose party is the caller's,
// counterparty = the other party, peer = the same trade id when the other side is a user). The settlement flow is
// trade_engine.go; money is finance.go.

const txSelect = `
	SELECT t.id::text, t.code, t.title, (t.buyer_party_id = $1), cp.name, cp.display_kind, cp.verified, cp.user_id::text,
	       t.status, t.quantity::float8, t.unit, t.unit_price_idr, t.total_idr, t.created_at, t.updated_at, t.due_at,
	       t.auction_id::text, t.terms, t.maker_fee_rate::float8,
	       (SELECT accepted_at FROM trade_acceptances WHERE trade_id = t.id AND side = 'buyer'),
	       (SELECT accepted_at FROM trade_acceptances WHERE trade_id = t.id AND side = 'supplier'),
	       i.number, i.issued_at, i.due_at, coalesce(i.status, 'unpaid'), i.paid_at,
	       q.outcome, q.accepted_quantity::float8, q.note, q.at,
	       coalesce((SELECT cp.id::text FROM collective_pools cp WHERE cp.settlement_id = sl.settlement_id), sl.settlement_id::text), t.group_label, t.group_share::float8, t.delivery_address
	FROM trades t
	JOIN parties cp ON cp.id = CASE WHEN t.buyer_party_id = $1 THEN t.supplier_party_id ELSE t.buyer_party_id END
	LEFT JOIN invoices i ON i.trade_id = t.id
	LEFT JOIN qc_results q ON q.trade_id = t.id
	LEFT JOIN settlement_lines sl ON sl.trade_id = t.id
	WHERE (t.buyer_party_id = $1 OR t.supplier_party_id = $1)`

// scanTx reads one txSelect row into the summary fields of a TransactionDetail.
func scanTx(row pgx.Row) (api.TransactionDetail, error) {
	var d api.TransactionDetail
	var isBuyer bool
	var cpKind, terms, payStatus string
	var cpUser, auctionID, invNumber, qcOutcome, qcNote, groupID, groupLabel *string
	var buyerAt, supplierAt, invIssued, invDue, paidAt, qcAt *time.Time
	var qcQty, groupShare *float64
	var rate float64
	err := row.Scan(&d.Id, &d.Code, &d.Title, &isBuyer, &d.Counterparty.Name, &cpKind, &d.Counterparty.Verified, &cpUser,
		&d.Status, &d.Quantity.Value, &d.Quantity.Unit, &d.UnitPriceIdr, &d.TotalIdr, &d.CreatedAt, &d.UpdatedAt, &d.DueAt,
		&auctionID, &terms, &rate, &buyerAt, &supplierAt, &invNumber, &invIssued, &invDue, &payStatus, &paidAt,
		&qcOutcome, &qcQty, &qcNote, &qcAt, &groupID, &groupLabel, &groupShare, &d.Delivery.Address)
	if err != nil {
		return d, err
	}
	d.Role = api.TransactionDetailRoleSupplier
	if isBuyer {
		d.Role = api.TransactionDetailRoleBuyer
	}
	d.Counterparty.Kind = api.PartyRefKind(cpKind)
	if cpUser != nil {
		d.Peer = &struct {
			TxId   string `json:"txId"`
			UserId string `json:"userId"`
		}{TxId: d.Id, UserId: *cpUser}
	}
	d.AuctionId = auctionID
	t := api.PaymentTerms(terms)
	d.Terms, d.MakerFeeRate = &t, &rate
	d.Agreement = &struct {
		BuyerAcceptedAt    *time.Time `json:"buyerAcceptedAt,omitempty"`
		SupplierAcceptedAt *time.Time `json:"supplierAcceptedAt,omitempty"`
	}{buyerAt, supplierAt}
	if invNumber != nil {
		d.Invoice = &struct {
			DueAt    time.Time `json:"dueAt"`
			IssuedAt time.Time `json:"issuedAt"`
			Number   string    `json:"number"`
		}{*invDue, *invIssued, *invNumber}
	}
	d.Payment.Status, d.Payment.PaidAt = api.TransactionDetailPaymentStatus(payStatus), paidAt
	if qcOutcome != nil {
		d.Qc = &struct {
			AcceptedQty float64                        `json:"acceptedQty"`
			At          time.Time                      `json:"at"`
			Note        *string                        `json:"note,omitempty"`
			Outcome     api.TransactionDetailQcOutcome `json:"outcome"`
		}{*qcQty, *qcAt, qcNote, api.TransactionDetailQcOutcome(*qcOutcome)}
	}
	if groupLabel != nil {
		d.Group = &struct {
			Id    string  `json:"id"`
			Label string  `json:"label"`
			Share float64 `json:"share"`
		}{Id: deref(groupID), Label: *groupLabel, Share: *groupShare}
	}
	return d, nil
}

// fileURL presigns object keys for the read models; nil gives no URLs (storage off, or a viewer who may not open the
// trade's files).
type fileURL func(key string) *string

const fileURLTTL = time.Hour

// fileURLs signs for viewers who may open a trade's files: its two sides and admins on its dispute case.
func (s *Server) fileURLs(ctx context.Context) fileURL {
	if s.Storage == nil {
		return nil
	}
	return func(key string) *string {
		url, err := s.Storage.PresignGet(ctx, key, fileURLTTL)
		if err != nil { // ponytail: presigning is a local HMAC and does not fail in practice; the file just shows unlinked
			return nil
		}
		return &url
	}
}

func (f fileURL) of(key *string) *string {
	if f == nil || key == nil || *key == "" {
		return nil
	}
	return f(*key)
}

// loadTransaction is the full TransactionDetail of trade `id` as seen by `party` (not_found when not a party), with
// uploaded files signed by `files` (nil: no URLs).
func loadTransaction(ctx context.Context, q dbtx, party, id string, files fileURL) (api.TransactionDetail, error) {
	if party == "" || !isUUID(id) {
		return api.TransactionDetail{}, errTradeNotFound
	}
	d, err := scanTx(q.QueryRow(ctx, txSelect+` AND t.id = $2`, party, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return d, errTradeNotFound
	}
	if err != nil {
		return d, err
	}

	// Timeline: the happy path of the terms with when/why each step was reached (latest event per status); off-path
	// statuses (cancelled, disputed) go right after the last step reached before them.
	type ev struct {
		status string
		at     time.Time
		note   *string
	}
	var evs []ev
	rows, err := q.Query(ctx, `SELECT status, at, note FROM trade_events WHERE trade_id = $1 ORDER BY at, id`, id)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var e ev
		if err := rows.Scan(&e.status, &e.at, &e.note); err != nil {
			return d, err
		}
		evs = append(evs, e)
	}
	if err := rows.Err(); err != nil {
		return d, err
	}
	type step = struct {
		At     *time.Time            `json:"at,omitempty"`
		Note   *string               `json:"note,omitempty"`
		Status api.TransactionStatus `json:"status"`
	}
	path := timelineFor(string(*d.Terms))
	last := map[string]ev{}
	for _, e := range evs {
		last[e.status] = e
	}
	d.Timeline = []step{}
	for _, st := range path {
		x := step{Status: api.TransactionStatus(st)}
		if e, ok := last[st]; ok {
			x.At, x.Note = &e.at, e.note
		}
		d.Timeline = append(d.Timeline, x)
	}
	for _, e := range evs {
		if slices.Contains(path, e.status) {
			continue
		}
		at := 0
		for i, x := range d.Timeline {
			if x.At != nil && !x.At.After(e.at) {
				at = i + 1
			}
		}
		d.Timeline = slices.Insert(d.Timeline, at, step{Status: api.TransactionStatus(e.status), At: &e.at, Note: e.note})
	}

	type document = struct {
		At   time.Time                          `json:"at"`
		Id   string                             `json:"id"`
		Kind api.TransactionDetailDocumentsKind `json:"kind"`
		Name string                             `json:"name"`
		Url  *string                            `json:"url,omitempty"`
	}
	d.Documents = []document{}
	rows, err = q.Query(ctx, `SELECT id::text, kind, name, object_key, created_at FROM trade_documents WHERE trade_id = $1 AND kind <> 'other' ORDER BY created_at, id`, id)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var x document
		var key *string
		if err := rows.Scan(&x.Id, &x.Kind, &x.Name, &key, &x.At); err != nil {
			return d, err
		}
		x.Url = files.of(key)
		d.Documents = append(d.Documents, x)
	}
	if err := rows.Err(); err != nil {
		return d, err
	}

	shipments := []api.Shipment{}
	rows, err = q.Query(ctx, `
		SELECT s.id::text, s.quantity::float8, s.drop_point, s.carrier, s.scheduled_at, s.status, s.delivered_at, doc.name, doc.object_key
		FROM shipments s LEFT JOIN trade_documents doc ON doc.id = s.proof_document_id
		WHERE s.trade_id = $1 ORDER BY s.created_at, s.id`, id)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var x api.Shipment
		var key *string
		if err := rows.Scan(&x.Id, &x.Quantity, &x.DropPoint, &x.Carrier, &x.ScheduledAt, &x.Status, &x.DeliveredAt, &x.Proof, &key); err != nil {
			return d, err
		}
		x.ProofUrl = files.of(key)
		shipments = append(shipments, x)
		d.Delivery.Eta = &x.ScheduledAt
		if x.Proof != nil {
			d.Delivery.Proof = x.Proof
		}
	}
	if err := rows.Err(); err != nil {
		return d, err
	}
	d.Shipments = &shipments

	d.Reviews = &struct {
		Buyer    *api.Review `json:"buyer,omitempty"`
		Supplier *api.Review `json:"supplier,omitempty"`
	}{}
	rows, err = q.Query(ctx, `
		SELECT r.side, r.rating::float8, r.quality::float8, r.timeliness::float8, r.communication::float8, r.text, r.created_at,
		       coalesce(u.name, p.name)
		FROM reviews r JOIN trades t ON t.id = r.trade_id
		JOIN parties p ON p.id = CASE r.side WHEN 'buyer' THEN t.buyer_party_id ELSE t.supplier_party_id END
		LEFT JOIN users u ON u.id = r.reviewed_by
		WHERE r.trade_id = $1`, id)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var side string
		var r api.Review
		if err := rows.Scan(&side, &r.Rating, &r.Quality, &r.Timeliness, &r.Communication, &r.Text, &r.At, &r.By); err != nil {
			return d, err
		}
		if side == "buyer" {
			d.Reviews.Buyer = &r
		} else {
			d.Reviews.Supplier = &r
		}
	}
	if err := rows.Err(); err != nil {
		return d, err
	}

	// The latest dispute (open, or the decided one) with the parties' evidence.
	var disputeID, dStatus, reason string
	var openedAt time.Time
	err = q.QueryRow(ctx, `SELECT id::text, status, reason, opened_at FROM disputes WHERE trade_id = $1 ORDER BY opened_at DESC LIMIT 1`, id).
		Scan(&disputeID, &dStatus, &reason, &openedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	type evidence = struct {
		At   time.Time                              `json:"at"`
		By   api.TransactionDetailDisputeEvidenceBy `json:"by"`
		File *string                                `json:"file,omitempty"`
		Id   string                                 `json:"id"`
		Name string                                 `json:"name"`
		Text string                                 `json:"text"`
		Url  *string                                `json:"url,omitempty"`
	}
	list := []evidence{}
	rows, err = q.Query(ctx, `
		SELECT id::text, side, author_name, text, file_name, file_key, created_at FROM dispute_evidence
		WHERE dispute_id = $1 AND side IN ('buyer','supplier') ORDER BY created_at, id`, disputeID)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var x evidence
		var key *string
		if err := rows.Scan(&x.Id, &x.By, &x.Name, &x.Text, &x.File, &key, &x.At); err != nil {
			return d, err
		}
		x.Url = files.of(key)
		list = append(list, x)
	}
	if err := rows.Err(); err != nil {
		return d, err
	}
	d.Dispute = &struct {
		Evidence *[]evidence       `json:"evidence,omitempty"`
		OpenedAt time.Time         `json:"openedAt"`
		Reason   string            `json:"reason"`
		Status   api.DisputeStatus `json:"status"`
	}{&list, openedAt, reason, api.DisputeStatus(dStatus)}
	return d, nil
}

// txSummary is the list shape: TransactionDetail without the detail-only fields (shadowed by nil fields).
type txSummary struct {
	api.TransactionDetail
	Timeline  *struct{} `json:"timeline,omitempty"`
	Documents *struct{} `json:"documents,omitempty"`
	Payment   *struct{} `json:"payment,omitempty"`
	Delivery  *struct{} `json:"delivery,omitempty"`
	Dispute   *struct{} `json:"dispute,omitempty"`
	Shipments *struct{} `json:"shipments,omitempty"`
	Reviews   *struct{} `json:"reviews,omitempty"`
}

// txList: the summary carries F6 fields (agreement, invoice, qc, makerFeeRate, group) that the Transaction schema
// does not declare, so it is written by hand.
type txList []txSummary

func (l txList) VisitListMyTransactionsResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(l)
}

func (s *Server) ListMyTransactions(ctx context.Context, req api.ListMyTransactionsRequestObject) (api.ListMyTransactionsResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Reader()
	party, err := myPartyID(ctx, q, sess.UserID)
	if err != nil || party == "" {
		return txList{}, err
	}
	sql, args := txSelect, []any{party}
	if r := req.Params.Role; r != nil {
		args = append(args, string(*r))
		sql += fmt.Sprintf(` AND CASE WHEN t.buyer_party_id = $1 THEN 'buyer' ELSE 'supplier' END = $%d`, len(args))
	}
	if st := req.Params.Status; st != nil && strings.TrimSpace(*st) != "" {
		args = append(args, strings.Split(*st, ","))
		sql += fmt.Sprintf(` AND t.status = ANY($%d)`, len(args))
	}
	// ponytail: not paginated (contract); newest 500.
	rows, err := q.Query(ctx, sql+` ORDER BY t.created_at DESC LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := txList{}
	for rows.Next() {
		d, err := scanTx(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, txSummary{TransactionDetail: d})
	}
	return out, rows.Err()
}

func (s *Server) GetMyTransaction(ctx context.Context, req api.GetMyTransactionRequestObject) (api.GetMyTransactionResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Reader()
	party, err := myPartyID(ctx, q, sess.UserID)
	if err != nil {
		return nil, err
	}
	d, err := loadTransaction(ctx, q, party, req.Id, s.fileURLs(ctx))
	if err != nil {
		return nil, err
	}
	return api.GetMyTransaction200JSONResponse(d), nil
}

// ApplyMyTransactionAction: the caller acts for the side their own party holds on the trade.
func (s *Server) ApplyMyTransactionAction(ctx context.Context, req api.ApplyMyTransactionActionRequestObject) (api.ApplyMyTransactionActionResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	var d api.TransactionDetail
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		party, err := myPartyID(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		side, err := tradeSide(ctx, tx, req.Id, party)
		if err != nil {
			return err
		}
		if req.Body.Action == "pay" {
			return errPayViaGateway
		}
		file, err := s.tradeFile(ctx, tx, sess.UserID, *req.Body)
		if err != nil {
			return err
		}
		if err := applyTradeAction(ctx, tx, req.Id, tradeActor{Side: side, UserID: &sess.UserID, Name: sess.Name, File: file}, *req.Body); err != nil {
			return err
		}
		d, err = loadTransaction(ctx, tx, party, req.Id, s.fileURLs(ctx))
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.ApplyMyTransactionAction200JSONResponse(d), nil
}

// ── Direct market order ──────────────────────────────────────────

// CreateDirectMarketOrder buys from a posted supply listing of a direct_market market at its price (escrow, 0.5% maker
// fee). The listing row is locked, so concurrent orders never oversell it.
func (s *Server) CreateDirectMarketOrder(ctx context.Context, req api.CreateDirectMarketOrderRequestObject) (api.CreateDirectMarketOrderResponseObject, error) {
	sess, err := requireActive(ctx)
	if err != nil {
		return nil, err
	}
	var tradeID string
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var marketID, marketCode, region string
		err := tx.QueryRow(ctx, `SELECT id::text, code, region FROM markets WHERE id = $1 AND mechanism = 'direct_market' AND status = 'active'`,
			nilIfNotUUID(req.Id)).Scan(&marketID, &marketCode, &region)
		if errors.Is(err, pgx.ErrNoRows) {
			return &Error{Status: http.StatusConflict, Code: "not_direct", Message: "Market ini tidak menerima order langsung"}
		} else if err != nil {
			return err
		}
		var l struct {
			ID, Code, Item, Category, Unit, Owner, OwnerName, Status string
			OwnerUser                                                *string
			Qty                                                      float64
			Price                                                    int64
		}
		err = tx.QueryRow(ctx, `
			SELECT l.id::text, l.code, l.item, l.category_id, l.unit, l.owner_party_id::text, p.name, p.user_id::text, l.status, l.quantity::float8, l.price_idr
			FROM listings l JOIN parties p ON p.id = l.owner_party_id
			WHERE l.id = $1 AND l.market_id = $2 AND l.kind = 'supply' AND l.status IN ('available','in_market')
			FOR UPDATE OF l`, nilIfNotUUID(req.Body.ListingId), marketID).
			Scan(&l.ID, &l.Code, &l.Item, &l.Category, &l.Unit, &l.Owner, &l.OwnerName, &l.OwnerUser, &l.Status, &l.Qty, &l.Price)
		if errors.Is(err, pgx.ErrNoRows) {
			return &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Penawaran tidak ditemukan"}
		} else if err != nil {
			return err
		}
		if l.OwnerUser != nil && *l.OwnerUser == sess.UserID {
			return &Error{Status: http.StatusConflict, Code: "own_listing", Message: "Ini penawaranmu sendiri"}
		}
		qty := req.Body.Quantity
		if !(qty > 0 && qty <= l.Qty+1e-9) {
			msg := fmt.Sprintf("1–%s %s", qtyLabel(l.Qty), l.Unit)
			return &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: "Kuantitas tidak valid", Fields: map[string]string{"quantity": msg}}
		}
		value := round(qty * float64(l.Price))
		if err := s.commitGuard(ctx, tx, sess.UserID, value); err != nil {
			return err
		}
		buyer, err := userParty(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		var address string
		_ = tx.QueryRow(ctx, `SELECT coalesce(nullif(btrim(location), ''), '') FROM users WHERE id = $1`, sess.UserID).Scan(&address)
		title := fmt.Sprintf("%s · %s %s (%s)", l.Item, qtyLabel(qty), l.Unit, marketCode)
		t, err := createTrade(ctx, tx, newTrade{Title: title,
			BuyerParty: buyer, SupplierParty: l.Owner, Quantity: qty, Unit: l.Unit, UnitPriceIdr: l.Price, Terms: "escrow", MakerFeeRate: 0.005,
			MarketID: &marketID, SourceListingID: &l.ID, DeliveryAddress: nonEmpty(address, "Alamat pengiriman dari profil"), Via: "direct",
			Category: l.Category, Region: region, ActorUserID: &sess.UserID})
		if err != nil {
			return err
		}
		tradeID = t.ID
		status := l.Status
		if l.Qty-qty <= 1e-9 {
			status = "sold"
		}
		if _, err := tx.Exec(ctx, `UPDATE listings SET quantity = greatest(quantity - $2::numeric, 0), status = $3 WHERE id = $1`, l.ID, qty, status); err != nil {
			return err
		}
		if err := listingEvent(ctx, tx, l.ID, status, fmt.Sprintf("Order langsung %s %s · %s", qtyLabel(qty), l.Unit, rupiah(value))); err != nil {
			return err
		}
		if l.OwnerUser != nil {
			return notify(ctx, tx, *l.OwnerUser, notification{Type: "transaction_update", Title: "Agreement baru: " + title,
				Body: sess.Name + " menunggu persetujuanmu.", Href: "/app/transactions/" + t.ID})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return api.CreateDirectMarketOrder201JSONResponse{TransactionId: tradeID}, nil
}

// nilIfNotUUID turns a malformed id into NULL so `id = $1` simply matches nothing.
func nilIfNotUUID(id string) *string {
	if !isUUID(id) {
		return nil
	}
	return &id
}
