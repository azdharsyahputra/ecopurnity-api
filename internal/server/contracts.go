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

// Standing supply contracts (PRD F6): the same order re-placed every period; each run is an ordinary trade
// (createTrade + contract_orders). Rules: trade_rules.go. Due orders are placed by ContractTick (trade_clock.go).

// wib: contract runs start at 08:00 Jakarta time.
var wib = time.FixedZone("WIB", 7*3600)

type contractRow struct {
	View          api.ContractView
	ID, Code      string
	Party         map[string]string  // side -> party id
	User          map[string]*string // side -> user id
	Status, Every string
	ProposedBy    string
}

const contractSelect = `
	SELECT c.id::text, c.code, c.item, c.buyer_party_id::text, bp.name, bp.display_kind, bp.verified, bp.user_id::text,
	       c.supplier_party_id::text, sp.name, sp.display_kind, sp.verified, sp.user_id::text,
	       c.quantity::float8, c.unit, c.unit_price_idr, c.terms, c.every, c.runs, c.next_at, c.status, c.proposed_by_side,
	       c.source_trade_id::text, c.created_at,
	       (SELECT coalesce(json_agg(json_build_object('at', o.placed_at, 'id', o.trade_id) ORDER BY o.run), '[]')
	          FROM contract_orders o WHERE o.contract_id = c.id)
	FROM supply_contracts c JOIN parties bp ON bp.id = c.buyer_party_id JOIN parties sp ON sp.id = c.supplier_party_id`

func scanContract(row pgx.Row) (contractRow, error) {
	c := contractRow{Party: map[string]string{}, User: map[string]*string{}}
	v := &c.View
	var bp, sp, bk, sk, terms, every, status, proposed string
	var bu, su *string
	var orders []byte
	err := row.Scan(&v.Id, &v.Code, &v.Item, &bp, &v.Buyer.Name, &bk, &v.Buyer.Verified, &bu, &sp, &v.Supplier.Name, &sk, &v.Supplier.Verified, &su,
		&v.Quantity.Value, &v.Quantity.Unit, &v.UnitPriceIdr, &terms, &every, &v.Runs, &v.NextAt, &status, &proposed, &v.SourceTxId, &v.CreatedAt, &orders)
	if err != nil {
		return c, err
	}
	c.ID, c.Code, c.Status, c.Every, c.ProposedBy = v.Id, v.Code, status, every, proposed
	c.Party["buyer"], c.Party["supplier"], c.User["buyer"], c.User["supplier"] = bp, sp, bu, su
	v.Buyer.Kind, v.Buyer.UserId, v.Supplier.Kind, v.Supplier.UserId = api.TradePartyKind(bk), bu, api.TradePartyKind(sk), su
	v.Terms, v.Every, v.Status, v.ProposedBy = api.PaymentTerms(terms), api.ContractEvery(every), api.ContractStatus(status), api.Role(proposed)
	var os []struct {
		At time.Time `json:"at"`
		ID string    `json:"id"`
	}
	if err := json.Unmarshal(orders, &os); err != nil {
		return c, err
	}
	v.Orders = []struct {
		At           time.Time `json:"at"`
		BuyerTxId    *string   `json:"buyerTxId,omitempty"`
		SupplierTxId *string   `json:"supplierTxId,omitempty"`
	}{}
	for _, o := range os {
		// One trade row serves both sides, so both ids are the same trade.
		v.Orders = append(v.Orders, struct {
			At           time.Time `json:"at"`
			BuyerTxId    *string   `json:"buyerTxId,omitempty"`
			SupplierTxId *string   `json:"supplierTxId,omitempty"`
		}{o.At, &o.ID, &o.ID})
	}
	return c, nil
}

// as returns the view for `side` with the actions open to it.
func (c contractRow) as(side string) api.ContractView {
	v := c.View
	v.Side = api.Role(side)
	v.Actions = []api.ContractAction{}
	for _, a := range contractActions(c.Status, c.ProposedBy, side) {
		v.Actions = append(v.Actions, api.ContractAction(a))
	}
	return v
}

func (c contractRow) sideOf(party string) string {
	if c.Party["buyer"] == party {
		return "buyer"
	}
	return "supplier"
}

var errContractNotFound = &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Kontrak tidak ditemukan"}

// myContract loads one of the party's contracts (locked for update when asked).
func myContract(ctx context.Context, q dbtx, party, id string, forUpdate bool) (contractRow, error) {
	if party == "" || !isUUID(id) {
		return contractRow{}, errContractNotFound
	}
	sql := contractSelect + ` WHERE c.id = $1 AND $2 IN (c.buyer_party_id, c.supplier_party_id)`
	if forUpdate {
		sql += ` FOR UPDATE OF c`
	}
	c, err := scanContract(q.QueryRow(ctx, sql, id, party))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, errContractNotFound
	}
	return c, err
}

func (s *Server) ListMyContracts(ctx context.Context, _ api.ListMyContractsRequestObject) (api.ListMyContractsResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Reader()
	party, err := myPartyID(ctx, q, sess.UserID)
	out := api.ListMyContracts200JSONResponse{}
	if err != nil || party == "" {
		return out, err
	}
	rows, err := q.Query(ctx, contractSelect+` WHERE $1 IN (c.buyer_party_id, c.supplier_party_id) ORDER BY c.created_at DESC`, party)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanContract(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c.as(c.sideOf(party)))
	}
	return out, rows.Err()
}

func (s *Server) GetContract(ctx context.Context, req api.GetContractRequestObject) (api.GetContractResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Reader()
	party, err := myPartyID(ctx, q, sess.UserID)
	if err != nil {
		return nil, err
	}
	c, err := myContract(ctx, q, party, req.Id, false)
	if err != nil {
		return nil, err
	}
	return api.GetContract200JSONResponse(c.as(c.sideOf(party))), nil
}

func (s *Server) CreateContract(ctx context.Context, req api.CreateContractRequestObject) (api.CreateContractResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	var view api.ContractView
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		party, err := myPartyID(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		if party == "" || !isUUID(b.FromTx) {
			return errTradeNotFound
		}
		var side, status, title, unit, terms, other string
		var qty float64
		var price int64
		var otherUser *string
		err = tx.QueryRow(ctx, `
			SELECT CASE WHEN t.buyer_party_id = $2 THEN 'buyer' ELSE 'supplier' END, t.status, t.title, t.quantity::float8, t.unit, t.unit_price_idr, t.terms,
			       op.id::text, op.user_id::text
			FROM trades t JOIN parties op ON op.id = CASE WHEN t.buyer_party_id = $2 THEN t.supplier_party_id ELSE t.buyer_party_id END
			WHERE t.id = $1 AND $2 IN (t.buyer_party_id, t.supplier_party_id)`, b.FromTx, party).
			Scan(&side, &status, &title, &qty, &unit, &price, &terms, &other, &otherUser)
		if errors.Is(err, pgx.ErrNoRows) {
			return errTradeNotFound
		} else if err != nil {
			return err
		}
		if status != "completed" {
			return &Error{Status: http.StatusConflict, Code: "not_completed", Message: "Kontrak rutin dibuat dari transaksi yang sudah selesai"}
		}
		start := b.StartAt.Time
		if start.Format(time.DateOnly) < time.Now().UTC().Format(time.DateOnly) {
			return fieldErr("startAt", "Mulai hari ini atau nanti")
		}
		every := string(b.Every)
		if _, ok := contractEvery[every]; !ok {
			return fieldErr("every", "Pilih frekuensi")
		}
		if b.Runs < 2 || b.Runs > 52 {
			return fieldErr("runs", "Antara 2 dan 52 order")
		}
		item, _, _ := strings.Cut(title, " · ")
		buyer, supplier := party, other
		if side == "supplier" {
			buyer, supplier = other, party
		}
		nextAt := time.Date(start.Year(), start.Month(), start.Day(), 8, 0, 0, 0, wib)
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO supply_contracts (buyer_party_id, supplier_party_id, item, quantity, unit, unit_price_idr, terms, every, runs, next_at,
			                              proposed_by_side, source_trade_id, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING id::text`,
			buyer, supplier, item, qty, unit, price, terms, every, b.Runs, nextAt, side, b.FromTx, sess.UserID).Scan(&id); err != nil {
			return err
		}
		c, err := myContract(ctx, tx, party, id, false)
		if err != nil {
			return err
		}
		if err := writeAudit(ctx, tx, audit{ActorUserID: &sess.UserID, ActorLabel: sess.Name, Action: "Usulkan kontrak rutin " + c.Code,
			EntityType: "transaction", EntityID: b.FromTx, EntityLabel: title,
			Changes: []change{{Field: "Kontrak", After: fmt.Sprintf("%s × %d, %s/%s", contractEvery[every], b.Runs, rupiah(price), unit)}}}); err != nil {
			return err
		}
		if otherUser != nil {
			if err := notify(ctx, tx, *otherUser, notification{Type: "transaction_update", Title: sess.Name + " mengusulkan kontrak rutin",
				Body: fmt.Sprintf("%s · %s, %d order", item, contractEvery[every], b.Runs), Href: "/app/contracts/" + id}); err != nil {
				return err
			}
		}
		view = c.as(side)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return api.CreateContract201JSONResponse(view), nil
}

var contractVerb = map[string]string{"accept": "disetujui", "decline": "ditolak", "pause": "dijeda", "resume": "dilanjutkan", "end": "diakhiri"}

func (s *Server) ApplyContractAction(ctx context.Context, req api.ApplyContractActionRequestObject) (api.ApplyContractActionResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	action := string(req.Body.Action)
	var view api.ContractView
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		party, err := myPartyID(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		c, err := myContract(ctx, tx, party, req.Id, true)
		if err != nil {
			return err
		}
		side := c.sideOf(party)
		to := ""
		if slices.Contains(contractActions(c.Status, c.ProposedBy, side), action) {
			to = contractTransition(c.Status, action)
		}
		if to == "" {
			return &Error{Status: http.StatusConflict, Code: "invalid_transition", Message: "Aksi ini tidak tersedia sekarang"}
		}
		before := c.Status
		if _, err := tx.Exec(ctx, `
			UPDATE supply_contracts SET status = $2, next_at = CASE WHEN $3 AND next_at < now() THEN now() ELSE next_at END WHERE id = $1`,
			c.ID, to, action == "resume"); err != nil {
			return err
		}
		if action == "run_now" {
			if err := placeContractOrder(ctx, tx, c.ID, &sess.UserID); err != nil {
				return err
			}
		}
		if c, err = myContract(ctx, tx, party, c.ID, false); err != nil {
			return err
		}
		if err := writeAudit(ctx, tx, audit{ActorUserID: &sess.UserID, ActorLabel: sess.Name, Action: fmt.Sprintf("Kontrak %s: %s", c.Code, action),
			EntityType: "transaction", EntityID: c.ID, EntityLabel: c.View.Item, Changes: []change{{Field: "status", Before: &before, After: c.Status}}}); err != nil {
			return err
		}
		if peer := c.User[otherSide(side)]; peer != nil && action != "run_now" {
			if err := notify(ctx, tx, *peer, notification{Type: "transaction_update", Title: fmt.Sprintf("Kontrak %s: %s", c.Code, contractVerb[action]),
				Body: "oleh " + sess.Name, Href: "/app/contracts/" + c.ID}); err != nil {
				return err
			}
		}
		view = c.as(side)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return api.ApplyContractAction200JSONResponse(view), nil
}

// placeContractOrder places the next run of a contract the caller has locked: a trade titled
// `<item> · order n/runs (<code>)`, nextAt advanced by the period, `ended` after the last run; both users notified.
func placeContractOrder(ctx context.Context, tx pgx.Tx, contractID string, actor *string) error {
	var code, item, unit, terms, every, buyer, supplier, address string
	var runs, n int
	var qty float64
	var price int64
	var marketID *string
	if err := tx.QueryRow(ctx, `
		SELECT c.code, c.item, c.unit, c.terms, c.every, c.buyer_party_id::text, c.supplier_party_id::text, c.runs, c.quantity::float8, c.unit_price_idr,
		       (SELECT count(*) FROM contract_orders WHERE contract_id = c.id) + 1,
		       coalesce(t.delivery_address, 'Alamat pengiriman dari profil'), t.market_id::text
		FROM supply_contracts c LEFT JOIN trades t ON t.id = c.source_trade_id WHERE c.id = $1`, contractID).
		Scan(&code, &item, &unit, &terms, &every, &buyer, &supplier, &runs, &qty, &price, &n, &address, &marketID); err != nil {
		return err
	}
	if n > runs {
		return nil
	}
	var category, region string
	if marketID != nil {
		_ = tx.QueryRow(ctx, `SELECT category_id, region FROM markets WHERE id = $1`, *marketID).Scan(&category, &region)
	}
	t, err := createTrade(ctx, tx, newTrade{Title: fmt.Sprintf("%s · order %d/%d (%s)", item, n, runs, code), BuyerParty: buyer, SupplierParty: supplier,
		Quantity: qty, Unit: unit, UnitPriceIdr: price, Terms: terms, DeliveryAddress: address, Via: "contract", Category: category, Region: region,
		ActorUserID: actor})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO contract_orders (contract_id, run, trade_id) VALUES ($1, $2, $3)`, contractID, n, t.ID); err != nil {
		return err
	}
	status := "active"
	if n >= runs {
		status = "ended"
	}
	if _, err := tx.Exec(ctx, `UPDATE supply_contracts SET next_at = $2, status = CASE WHEN $3 = 'ended' THEN 'ended' ELSE status END WHERE id = $1`,
		contractID, nextRun(time.Now(), every), status); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT user_id::text FROM parties WHERE id IN ($1, $2) AND user_id IS NOT NULL`, buyer, supplier)
	if err != nil {
		return err
	}
	users, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, u := range users {
		if err := notify(ctx, tx, u, notification{Type: "transaction_update", Title: fmt.Sprintf("Order kontrak %s dibuat", code),
			Body: fmt.Sprintf("%s · order %d dari %d", item, n, runs), Href: "/app/transactions/" + t.ID}); err != nil {
			return err
		}
	}
	return emitTradeUpdated(ctx, tx, t.ID)
}
