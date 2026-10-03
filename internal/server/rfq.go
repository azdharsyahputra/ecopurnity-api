package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// RFQs and quotes (PRD F6 direct trade): a buyer asks, suppliers quote, the two negotiate (counter / revise), and
// accepting a quote creates the trade (createTrade). Every RFQ has its conversation (conversations.go). Fictional
// suppliers that quote and answer counters are a demo simulation in counterparties.go, off unless
// SIMULATE_COUNTERPARTIES=true.

// ── Negotiation rules (frontend src/domain/rfq.ts) ───────────────

type quoteRule struct {
	by   string // buyer | supplier
	from []string
	to   string
}

var quoteFlow = map[api.QuoteAction]quoteRule{
	"accept":         {"buyer", []string{"submitted"}, "accepted"},
	"counter":        {"buyer", []string{"submitted"}, "countered"},
	"decline":        {"buyer", []string{"submitted", "countered"}, "declined"},
	"revise":         {"supplier", []string{"submitted", "countered"}, "submitted"},
	"accept_counter": {"supplier", []string{"countered"}, "accepted"},
	"withdraw":       {"supplier", []string{"submitted", "countered"}, "withdrawn"},
}

// quoteTransition is the status a quote moves to, or "" when side may not take the action now.
func quoteTransition(status, side string, a api.QuoteAction, rfqOpen bool) string {
	r, ok := quoteFlow[a]
	if !ok || !rfqOpen || r.by != side || !slices.Contains(r.from, status) {
		return ""
	}
	return r.to
}

// dealPrice is the unit price the deal closes at: the counter when the supplier accepted it, else the quoted price.
func dealPrice(price int64, counter *int64, a api.QuoteAction) int64 {
	if a == "accept_counter" && counter != nil {
		return *counter
	}
	return price
}

// botCounterReply is how a simulated supplier answers a counter: accept within 5% of its price, else meet halfway.
func botCounterReply(quoted, counter int64) (accept bool, revise int64) {
	if float64(counter) >= float64(quoted)*0.95 {
		return true, 0
	}
	return false, int64(math.Round(float64(quoted+counter) / 2))
}

var categoryLabels = map[string]string{
	"agri": "Pertanian", "food": "Pangan", "packaging": "Kemasan", "manufacturing": "Manufaktur",
	"logistics": "Logistik", "it": "Jasa IT", "energy": "Energi",
}

var (
	errRfqNotFound   = &Error{Status: http.StatusNotFound, Code: "not_found", Message: "RFQ tidak ditemukan"}
	errQuoteNotFound = &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Penawaran tidak ditemukan"}
	errRfqClosed     = &Error{Status: http.StatusConflict, Code: "closed", Message: "RFQ sudah ditutup"}
)

// ── Read model ───────────────────────────────────────────────────

const partyCols = `p.name, p.display_kind, p.verified, p.user_id::text`

func scanParty(p *api.TradeParty) []any { return []any{&p.Name, &p.Kind, &p.Verified, &p.UserId} }

// partyOf is the user's party id, nil when they never needed one.
func partyOf(ctx context.Context, q dbtx, userID string) (*string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id::text FROM parties WHERE user_id = $1`, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &id, err
}

// rfqRelevant: $1 (the caller's party) is invited, already quoted, or holds supply listings in the RFQ's category.
const rfqRelevant = `(EXISTS (SELECT 1 FROM rfq_invitations ri WHERE ri.rfq_id = r.id AND ri.party_id = $1::uuid)
	OR EXISTS (SELECT 1 FROM quotes rq WHERE rq.rfq_id = r.id AND rq.supplier_party_id = $1::uuid)
	OR EXISTS (SELECT 1 FROM listings rl WHERE rl.owner_party_id = $1::uuid AND rl.kind = 'supply' AND rl.category_id = r.category_id))`

// loadRfqs returns the RFQs matching where ($1 = me, the caller's party, may be nil; more args from $2) as the caller
// sees them: the buyer sees every quote, a supplier only their own.
func loadRfqs(ctx context.Context, q dbtx, me *string, where string, args ...any) ([]api.RfqView, error) {
	rows, err := q.Query(ctx, `
		SELECT r.id::text, r.code, `+strings.ReplaceAll(partyCols, "p.", "bp.")+`, r.item, r.category_id, r.quantity::float8, r.unit,
		       r.target_price_idr, r.deadline, r.location, r.spec, r.status, r.conversation_id::text, r.created_at, r.source_kind,
		       coalesce(r.source_trade_id, r.source_listing_id, r.source_match_id)::text,
		       (SELECT t.id::text FROM trades t JOIN quotes aq ON aq.id = t.source_quote_id WHERE aq.rfq_id = r.id),
		       coalesce(r.buyer_party_id = $1::uuid, false)
		FROM rfqs r JOIN parties bp ON bp.id = r.buyer_party_id
		WHERE `+where+` ORDER BY r.created_at DESC, r.id`, append([]any{me}, args...)...)
	if err != nil {
		return nil, err
	}
	out := []api.RfqView{}
	idx := map[string]int{}
	var ids, mine []string
	for rows.Next() {
		var v api.RfqView
		var srcKind, srcID *string
		var isBuyer bool
		dest := append([]any{&v.Id, &v.Code}, scanParty(&v.Buyer)...)
		dest = append(dest, &v.Item, &v.CategoryId, &v.Quantity.Value, &v.Quantity.Unit, &v.TargetPriceIdr, &v.Deadline, &v.Location,
			&v.Spec, &v.Status, &v.ConversationId, &v.CreatedAt, &srcKind, &srcID, &v.TransactionId, &isBuyer)
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return nil, err
		}
		if srcKind != nil && srcID != nil {
			v.Source = &struct {
				Id   string                `json:"id"`
				Kind api.RfqViewSourceKind `json:"kind"`
			}{*srcID, api.RfqViewSourceKind(*srcKind)}
		}
		v.Side = "supplier"
		if isBuyer {
			v.Side = "buyer"
			mine = append(mine, v.Id)
		}
		v.Invited, v.Quotes = []api.TradeParty{}, []api.Quote{}
		idx[v.Id] = len(out)
		ids = append(ids, v.Id)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}

	rows, err = q.Query(ctx, `
		SELECT i.rfq_id::text, `+partyCols+` FROM rfq_invitations i JOIN parties p ON p.id = i.party_id
		WHERE i.rfq_id = ANY($1::uuid[]) ORDER BY i.invited_at, p.name`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var p api.TradeParty
		if err := rows.Scan(append([]any{&id}, scanParty(&p)...)...); err != nil {
			rows.Close()
			return nil, err
		}
		out[idx[id]].Invited = append(out[idx[id]].Invited, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `
		SELECT q.rfq_id::text, q.id::text, `+partyCols+`, q.price_idr, q.quantity::float8, q.lead_time_days, q.terms, q.note, q.status,
		       q.counter_price_idr, q.created_at,
		       (SELECT coalesce(json_agg(json_build_object('at', e.at, 'by', e.actor_label, 'text', e.text) ORDER BY e.at, e.id), '[]')
		        FROM quote_events e WHERE e.quote_id = q.id)
		FROM quotes q JOIN parties p ON p.id = q.supplier_party_id
		WHERE q.rfq_id = ANY($1::uuid[]) AND (q.rfq_id = ANY($2::uuid[]) OR q.supplier_party_id = $3::uuid)
		ORDER BY q.created_at, q.id`, ids, mine, me)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var qt api.Quote
		var history []byte
		dest := append([]any{&id, &qt.Id}, scanParty(&qt.Supplier)...)
		dest = append(dest, &qt.PriceIdr, &qt.Quantity, &qt.LeadTimeDays, &qt.Terms, &qt.Note, &qt.Status, &qt.CounterPriceIdr, &qt.At, &history)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(history, &qt.History); err != nil {
			return nil, err
		}
		out[idx[id]].Quotes = append(out[idx[id]].Quotes, qt)
	}
	return out, rows.Err()
}

func loadRfq(ctx context.Context, q dbtx, me *string, id string) (api.RfqView, error) {
	list, err := loadRfqs(ctx, q, me, "r.id = $2", id)
	if err != nil {
		return api.RfqView{}, err
	}
	if len(list) == 0 {
		return api.RfqView{}, errRfqNotFound
	}
	return list[0], nil
}

// rfqRow is what the write paths need of an RFQ, read under its row lock.
type rfqRow struct {
	ID, Code, BuyerParty, BuyerName, Item, Category, Unit, Location, Status, ConversationID string
	BuyerUser                                                                               *string
	Quantity                                                                                float64
	CreatedAt                                                                               time.Time
	TargetPrice                                                                             *int64
}

func lockRfq(ctx context.Context, q dbtx, id string) (rfqRow, error) {
	var r rfqRow
	if !uuidPattern.MatchString(id) {
		return r, errRfqNotFound
	}
	err := q.QueryRow(ctx, `
		SELECT r.id::text, r.code, r.buyer_party_id::text, p.name, p.user_id::text, r.item, r.category_id, r.quantity::float8, r.unit,
		       r.location, r.status, r.conversation_id::text, r.created_at, r.target_price_idr
		FROM rfqs r JOIN parties p ON p.id = r.buyer_party_id WHERE r.id = $1 FOR UPDATE OF r`, id).
		Scan(&r.ID, &r.Code, &r.BuyerParty, &r.BuyerName, &r.BuyerUser, &r.Item, &r.Category, &r.Quantity, &r.Unit,
			&r.Location, &r.Status, &r.ConversationID, &r.CreatedAt, &r.TargetPrice)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, errRfqNotFound
	}
	return r, err
}

type quoteRow struct {
	ID, SupplierParty, SupplierName, Terms, Status string
	SupplierUser                                   *string
	Price                                          int64
	Counter                                        *int64
	Quantity                                       float64
}

func lockQuote(ctx context.Context, q dbtx, rfqID, id string) (quoteRow, error) {
	var r quoteRow
	if !uuidPattern.MatchString(id) {
		return r, errQuoteNotFound
	}
	err := q.QueryRow(ctx, `
		SELECT q.id::text, q.supplier_party_id::text, p.name, p.user_id::text, q.terms, q.status, q.price_idr, q.counter_price_idr, q.quantity::float8
		FROM quotes q JOIN parties p ON p.id = q.supplier_party_id WHERE q.id = $1 AND q.rfq_id = $2 FOR UPDATE OF q`, id, rfqID).
		Scan(&r.ID, &r.SupplierParty, &r.SupplierName, &r.SupplierUser, &r.Terms, &r.Status, &r.Price, &r.Counter, &r.Quantity)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, errQuoteNotFound
	}
	return r, err
}

func derefSlice[T any](p *[]T) []T {
	if p == nil {
		return nil
	}
	return *p
}

func unitPrice(v int64, unit string) string { return rupiah(v) + "/" + unit }

// ── Handlers ─────────────────────────────────────────────────────

func (s *Server) ListRfqs(ctx context.Context, req api.ListRfqsRequestObject) (api.ListRfqsResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	db := s.DB.Reader()
	me, err := partyOf(ctx, db, sess.UserID)
	if err != nil {
		return nil, err
	}
	where := "r.buyer_party_id = $1::uuid"
	if p := req.Params.Side; p != nil && *p == "supplier" {
		where = "r.buyer_party_id IS DISTINCT FROM $1::uuid AND " + rfqRelevant + ` AND (r.status = 'open'
			OR EXISTS (SELECT 1 FROM quotes mq WHERE mq.rfq_id = r.id AND mq.supplier_party_id = $1::uuid))`
	}
	list, err := loadRfqs(ctx, db, me, where)
	if err != nil {
		return nil, err
	}
	return api.ListRfqs200JSONResponse(list), nil
}

func (s *Server) GetRfq(ctx context.Context, req api.GetRfqRequestObject) (api.GetRfqResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if !uuidPattern.MatchString(req.Id) {
		return nil, errRfqNotFound
	}
	db := s.DB.Reader()
	me, err := partyOf(ctx, db, sess.UserID)
	if err != nil {
		return nil, err
	}
	list, err := loadRfqs(ctx, db, me, "r.id = $2 AND (r.buyer_party_id = $1::uuid OR "+rfqRelevant+")", req.Id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, errRfqNotFound
	}
	return api.GetRfq200JSONResponse(list[0]), nil
}

func (s *Server) CreateRfq(ctx context.Context, req api.CreateRfqRequestObject) (api.CreateRfqResponseObject, error) {
	sess, err := requireActive(ctx)
	if err != nil {
		return nil, err
	}
	in := req.Body
	in.Item = strings.TrimSpace(in.Item)
	f := map[string]string{}
	if in.Item == "" {
		f["item"] = "Isi barang/jasa yang dicari"
	}
	if !(in.Quantity.Value > 0) {
		f["quantity"] = "Kuantitas harus lebih dari 0"
	}
	if in.Deadline.IsZero() {
		f["deadline"] = "Isi batas waktu"
	}
	if len(f) > 0 {
		return nil, &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: "Lengkapi RFQ", Fields: f}
	}
	var out api.RfqView
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		buyer, err := userParty(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		var srcKind, srcTrade, srcListing, srcMatch *string
		if src := in.Source; src != nil {
			kind := string(src.Kind)
			var ok bool
			if uuidPattern.MatchString(src.Id) {
				switch kind {
				case "repeat", "logistics":
					srcTrade = &src.Id
					err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM trades WHERE id = $1 AND $2 IN (buyer_party_id, supplier_party_id))`, src.Id, buyer).Scan(&ok)
				case "listing":
					srcListing = &src.Id
					err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM listings WHERE id = $1)`, src.Id).Scan(&ok)
				case "match":
					srcMatch = &src.Id
					err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM matches WHERE id = $1)`, src.Id).Scan(&ok)
				}
				if err != nil {
					return err
				}
			}
			if !ok {
				return &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: "Lengkapi RFQ",
					Fields: map[string]string{"source": "Sumber RFQ tidak ditemukan"}}
			}
			srcKind = &kind
		}

		// Participants: the buyer, invited platform users (not the caller), invited external businesses by name.
		parties := []convParty{{PartyID: buyer, UserID: &sess.UserID}}
		var inviteIDs []string
		for _, id := range derefSlice(in.InviteUserIds) {
			if id = strings.ToLower(id); uuidPattern.MatchString(id) && id != sess.UserID {
				inviteIDs = append(inviteIDs, id)
			}
		}
		rows, _ := tx.Query(ctx, `SELECT id::text FROM users WHERE id = ANY($1::uuid[]) ORDER BY id`, inviteIDs)
		users, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, u := range users {
			p, err := userParty(ctx, tx, u)
			if err != nil {
				return err
			}
			parties = append(parties, convParty{PartyID: p, UserID: &u})
		}
		for _, name := range derefSlice(in.InviteNames) {
			if name = strings.TrimSpace(name); name != "" {
				p, err := externalParty(ctx, tx, name, "business", true)
				if err != nil {
					return err
				}
				parties = append(parties, convParty{PartyID: p})
			}
		}

		var id string
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
			return err
		}
		conv, err := createConversation(ctx, tx, "RFQ "+in.Item, &sess.UserID, "rfq", id, parties)
		if err != nil {
			return err
		}
		var code string
		if err := tx.QueryRow(ctx, `
			INSERT INTO rfqs (id, buyer_party_id, item, category_id, quantity, unit, target_price_idr, deadline, location, spec,
			                  conversation_id, source_kind, source_trade_id, source_listing_id, source_match_id, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16) RETURNING code`,
			id, buyer, in.Item, in.CategoryId, in.Quantity.Value, in.Quantity.Unit, in.TargetPriceIdr, in.Deadline, in.Location, in.Spec,
			conv, srcKind, srcTrade, srcListing, srcMatch, sess.UserID).Scan(&code); err != nil {
			return err
		}
		for _, p := range parties[1:] {
			if _, err := tx.Exec(ctx, `INSERT INTO rfq_invitations (rfq_id, party_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, p.PartyID); err != nil {
				return err
			}
		}
		if err := writeAudit(ctx, tx, audit{ActorUserID: &sess.UserID, ActorLabel: sess.Name, Action: "Buat RFQ",
			EntityType: "procurement", EntityID: id, EntityLabel: code + " · " + in.Item}); err != nil {
			return err
		}
		// Every platform supplier relevant to it. ponytail: one notification per supplier in the category; a digest
		// (or a cap) when categories hold thousands of suppliers.
		rows, _ = tx.Query(ctx, `
			SELECT DISTINCT p.user_id::text FROM parties p JOIN users u ON u.id = p.user_id
			WHERE p.user_id <> $2 AND u.status <> 'suspended' AND (
			  EXISTS (SELECT 1 FROM rfq_invitations i WHERE i.rfq_id = $1 AND i.party_id = p.id) OR
			  EXISTS (SELECT 1 FROM listings l WHERE l.owner_party_id = p.id AND l.kind = 'supply' AND l.category_id = $3))`,
			id, sess.UserID, in.CategoryId)
		notifyIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		body := fmt.Sprintf("%s butuh %s %s · %s", sess.Name, qtyLabel(in.Quantity.Value), in.Quantity.Unit, categoryLabels[string(in.CategoryId)])
		for _, u := range notifyIDs {
			if err := notify(ctx, tx, u, notification{Type: "auction_invitation", Title: "RFQ baru: " + in.Item, Body: body, Href: "/app/rfq/" + id}); err != nil {
				return err
			}
		}
		out, err = loadRfq(ctx, tx, &buyer, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.CreateRfq201JSONResponse(out), nil
}

func (s *Server) SubmitQuote(ctx context.Context, req api.SubmitQuoteRequestObject) (api.SubmitQuoteResponseObject, error) {
	sess, err := requireActive(ctx)
	if err != nil {
		return nil, err
	}
	in := req.Body
	var out api.RfqView
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		r, err := lockRfq(ctx, tx, req.Id)
		if err != nil {
			return err
		}
		me, err := userParty(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		var relevant bool
		if err := tx.QueryRow(ctx, `SELECT r.buyer_party_id <> $1 AND `+rfqRelevant+` FROM rfqs r WHERE r.id = $2`, me, r.ID).Scan(&relevant); err != nil {
			return err
		}
		if !relevant {
			return errRfqNotFound
		}
		if r.Status != "open" {
			return errRfqClosed
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM quotes WHERE rfq_id = $1 AND supplier_party_id = $2 AND status IN ('submitted','countered'))`,
			r.ID, me).Scan(&active); err != nil {
			return err
		}
		if active {
			return &Error{Status: http.StatusConflict, Code: "duplicate", Message: "Kamu sudah punya penawaran aktif; revisi saja"}
		}
		if err := s.commitGuard(ctx, tx, sess.UserID, int64(math.Round(float64(in.PriceIdr)*in.Quantity))); err != nil {
			return err
		}
		var qid string
		if err := tx.QueryRow(ctx, `
			INSERT INTO quotes (rfq_id, supplier_party_id, price_idr, quantity, lead_time_days, terms, note, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id::text`,
			r.ID, me, in.PriceIdr, in.Quantity, in.LeadTimeDays, in.Terms, in.Note, sess.UserID).Scan(&qid); err != nil {
			return err
		}
		if err := quoteEvent(ctx, tx, qid, &sess.UserID, sess.Name, "Penawaran "+unitPrice(int64(in.PriceIdr), r.Unit)); err != nil {
			return err
		}
		if err := addParticipant(ctx, tx, r.ConversationID, convParty{PartyID: me, UserID: &sess.UserID}); err != nil {
			return err
		}
		if r.BuyerUser != nil {
			if err := notify(ctx, tx, *r.BuyerUser, notification{Type: "transaction_update", Title: "Penawaran baru untuk " + r.Code,
				Body: sess.Name + ": " + unitPrice(int64(in.PriceIdr), r.Unit), Href: "/app/rfq/" + r.ID}); err != nil {
				return err
			}
		}
		out, err = loadRfq(ctx, tx, &me, r.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.SubmitQuote201JSONResponse(out), nil
}

func quoteEvent(ctx context.Context, q dbtx, quoteID string, actor *string, label, text string) error {
	_, err := q.Exec(ctx, `INSERT INTO quote_events (quote_id, actor_user_id, actor_label, text) VALUES ($1, $2, $3, $4)`, quoteID, actor, label, text)
	return err
}

func (s *Server) ActOnQuote(ctx context.Context, req api.ActOnQuoteRequestObject) (api.ActOnQuoteResponseObject, error) {
	sess, err := requireActive(ctx)
	if err != nil {
		return nil, err
	}
	in := req.Body
	var out api.RfqView
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		r, err := lockRfq(ctx, tx, req.Id)
		if errors.Is(err, errRfqNotFound) {
			return errQuoteNotFound
		}
		if err != nil {
			return err
		}
		qt, err := lockQuote(ctx, tx, r.ID, req.Qid)
		if err != nil {
			return err
		}
		me, err := partyOf(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		side := ""
		switch {
		case me == nil:
		case *me == r.BuyerParty:
			side = "buyer"
		case *me == qt.SupplierParty:
			side = "supplier"
		}
		if side == "" {
			return &Error{Status: http.StatusForbidden, Code: "forbidden", Message: "Bukan penawaranmu"}
		}
		next := quoteTransition(qt.Status, side, in.Action, r.Status == "open")
		if next == "" {
			return &Error{Status: http.StatusConflict, Code: "invalid_transition", Message: "Aksi ini tidak tersedia sekarang"}
		}
		var price int64
		if in.PriceIdr != nil {
			price = int64(*in.PriceIdr)
		}
		if (in.Action == "counter" || in.Action == "revise") && price <= 0 {
			return &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: "Isi harga", Fields: map[string]string{"priceIdr": "Isi harga per unit"}}
		}
		// The buyer commits at accept, and already at counter (the supplier can accept a counter alone); the
		// supplier at revise, as at submit.
		guard := func(userID *string, unit int64) error {
			if userID == nil {
				return nil
			}
			return s.commitGuard(ctx, tx, *userID, int64(math.Round(float64(unit)*qt.Quantity)))
		}
		switch in.Action {
		case "accept", "accept_counter":
			deal := dealPrice(qt.Price, qt.Counter, in.Action)
			if err := guard(r.BuyerUser, deal); err != nil {
				return err
			}
			if _, err := acceptQuote(ctx, tx, r, qt, deal, &sess.UserID, sess.Name); err != nil {
				return err
			}
		case "counter":
			if err := guard(r.BuyerUser, price); err != nil {
				return err
			}
			fallthrough
		default:
			if in.Action == "revise" {
				if err := guard(&sess.UserID, price); err != nil {
					return err
				}
			}
			if err := negotiate(ctx, tx, r, qt, in.Action, price, deref(in.Note), &sess.UserID, sess.Name); err != nil {
				return err
			}
		}
		out, err = loadRfq(ctx, tx, me, r.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.ActOnQuote200JSONResponse(out), nil
}

// negotiate applies counter / revise / decline / withdraw (already checked by quoteTransition), records it in the
// quote history and notifies the other side.
func negotiate(ctx context.Context, tx pgx.Tx, r rfqRow, qt quoteRow, a api.QuoteAction, price int64, note string, actor *string, actorName string) error {
	var err error
	switch a {
	case "counter":
		_, err = tx.Exec(ctx, `UPDATE quotes SET status = 'countered', counter_price_idr = $2 WHERE id = $1`, qt.ID, price)
	case "revise":
		_, err = tx.Exec(ctx, `UPDATE quotes SET status = 'submitted', price_idr = $2, counter_price_idr = NULL WHERE id = $1`, qt.ID, price)
	default:
		_, err = tx.Exec(ctx, `UPDATE quotes SET status = $2 WHERE id = $1`, qt.ID, quoteFlow[a].to)
	}
	if err != nil {
		return err
	}
	text := map[api.QuoteAction]string{"counter": "Tawar balik", "revise": "Revisi", "decline": "Tolak", "withdraw": "Tarik"}[a]
	if price > 0 {
		text += " " + rupiah(price)
	}
	if note = strings.TrimSpace(note); note != "" {
		text += " · " + note
	}
	if err := quoteEvent(ctx, tx, qt.ID, actor, actorName, text); err != nil {
		return err
	}
	other := qt.SupplierUser
	if quoteFlow[a].by == "supplier" {
		other = r.BuyerUser
	}
	if other == nil {
		return nil
	}
	verb := map[api.QuoteAction]string{"counter": "menawar balik", "revise": "merevisi harga", "decline": "menolak", "withdraw": "menarik penawaran"}[a]
	body := r.Item
	if price > 0 {
		body = unitPrice(price, r.Unit)
	}
	return notify(ctx, tx, *other, notification{Type: "transaction_update", Title: fmt.Sprintf("%s: %s %s", r.Code, actorName, verb), Body: body, Href: "/app/rfq/" + r.ID})
}

// acceptQuote closes the deal at price: the quote is accepted, the other open quotes declined, the RFQ awarded, and
// the trade created (createTrade, linked through trades.source_quote_id). The caller ran the buyer's commitGuard.
func acceptQuote(ctx context.Context, tx pgx.Tx, r rfqRow, qt quoteRow, price int64, actor *string, actorName string) (string, error) {
	if _, err := tx.Exec(ctx, `UPDATE quotes SET status = 'accepted' WHERE id = $1`, qt.ID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE quotes SET status = 'declined' WHERE rfq_id = $1 AND id <> $2 AND status IN ('submitted','countered')`, r.ID, qt.ID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE rfqs SET status = 'awarded' WHERE id = $1`, r.ID); err != nil {
		return "", err
	}
	t, err := createTrade(ctx, tx, newTrade{Title: fmt.Sprintf("%s · %s %s", r.Item, qtyLabel(qt.Quantity), r.Unit),
		BuyerParty: r.BuyerParty, SupplierParty: qt.SupplierParty, Quantity: qt.Quantity, Unit: r.Unit, UnitPriceIdr: price,
		Terms: qt.Terms, DeliveryAddress: r.Location, Via: "rfq", Category: r.Category, Region: r.Location, ActorUserID: actor})
	if err != nil {
		return "", err
	}
	// createTrade has no quote field; link it here rather than widen the shared helper.
	if _, err := tx.Exec(ctx, `UPDATE trades SET source_quote_id = $2 WHERE id = $1`, t.ID, qt.ID); err != nil {
		return "", err
	}
	if err := writeAudit(ctx, tx, audit{ActorUserID: actor, ActorLabel: actorName, Action: "Terima penawaran " + qt.SupplierName,
		EntityType: "procurement", EntityID: r.ID, EntityLabel: r.Code + " · " + r.Item,
		Changes: []change{{Field: "Harga", After: unitPrice(price, r.Unit)}}}); err != nil {
		return "", err
	}
	href := "/app/transactions/" + t.ID
	if qt.SupplierUser != nil {
		if err := notify(ctx, tx, *qt.SupplierUser, notification{Type: "winning_bid", Title: "Penawaranmu diterima: " + r.Code,
			Body: fmt.Sprintf("%s · %s. Setujui agreement-nya.", r.BuyerName, unitPrice(price, r.Unit)), Href: href}); err != nil {
			return "", err
		}
	}
	if r.BuyerUser != nil && (actor == nil || *actor != *r.BuyerUser) {
		if err := notify(ctx, tx, *r.BuyerUser, notification{Type: "transaction_update", Title: qt.SupplierName + " menerima tawaran balikmu",
			Body: r.Code + " · " + unitPrice(price, r.Unit), Href: href}); err != nil {
			return "", err
		}
	}
	return t.ID, nil
}

func (s *Server) CloseRfq(ctx context.Context, req api.CloseRfqRequestObject) (api.CloseRfqResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	var out api.RfqView
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		r, err := lockRfq(ctx, tx, req.Id)
		if err != nil {
			return err
		}
		if r.BuyerUser == nil || *r.BuyerUser != sess.UserID {
			return errRfqNotFound
		}
		if r.Status != "open" {
			return errRfqClosed
		}
		if _, err := tx.Exec(ctx, `UPDATE rfqs SET status = 'closed' WHERE id = $1`, r.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE quotes SET status = 'declined' WHERE rfq_id = $1 AND status IN ('submitted','countered')`, r.ID); err != nil {
			return err
		}
		out, err = loadRfq(ctx, tx, &r.BuyerParty, r.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.CloseRfq200JSONResponse(out), nil
}
