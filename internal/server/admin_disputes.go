package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Admin dispute cases (PRD §11). The workflow and the resolution arithmetic are ported from the frontend's
// src/domain/dispute.ts (tests in admin_disputes_test.go). The money side of a resolution is settled by the transactions
// area through settleDisputeResolution, in the same transaction.

var disputeFlow = map[string]struct {
	from []string
	to   string
}{
	"request_evidence": {[]string{"open", "evidence"}, "evidence"},
	"start_review":     {[]string{"open", "evidence"}, "review"},
	"resolve":          {[]string{"review"}, "resolved"},
}

// disputeTransition is the next status, or "" when the action is not available in this status.
func disputeTransition(status, action string) string {
	f, ok := disputeFlow[action]
	if !ok || !slices.Contains(f.from, status) {
		return ""
	}
	return f.to
}

type resolution struct {
	Kind      string // refund | release | partial
	RefundIdr int64  // partial only
}

// validateResolution is the error message for an invalid resolution, or "".
func validateResolution(totalIdr int64, r resolution) string {
	if r.Kind != "partial" {
		return ""
	}
	if r.RefundIdr <= 0 {
		return "Isi nominal refund lebih dari 0"
	}
	if r.RefundIdr >= totalIdr {
		return fmt.Sprintf("Refund sebagian harus kurang dari total %s; pakai refund penuh", rupiah(totalIdr))
	}
	return ""
}

type resolutionOutcome struct {
	Status, Payment       string // trade status, invoice payment state
	RefundIdr, ReleaseIdr int64
	Note                  string // timeline and notification summary
}

// resolveOutcome: refund -> buyer gets everything back, the order is cancelled; release -> supplier gets everything,
// the order completes; partial -> buyer gets RefundIdr back, supplier the rest, the order completes.
func resolveOutcome(totalIdr int64, r resolution) resolutionOutcome {
	switch r.Kind {
	case "refund":
		return resolutionOutcome{"cancelled", "refunded", totalIdr, 0, fmt.Sprintf("Refund penuh %s ke pembeli", rupiah(totalIdr))}
	case "release":
		return resolutionOutcome{"completed", "released", 0, totalIdr, fmt.Sprintf("Dana %s dilepas ke supplier", rupiah(totalIdr))}
	}
	release := totalIdr - r.RefundIdr
	return resolutionOutcome{"completed", "released", r.RefundIdr, release,
		fmt.Sprintf("%s dikembalikan ke pembeli, %s dilepas ke supplier", rupiah(r.RefundIdr), rupiah(release))}
}

// disputeSettlement is a decided dispute's effect on its trade.
type disputeSettlement struct {
	DisputeID, TradeID    string
	Kind                  string // refund | release | partial
	RefundIdr, ReleaseIdr int64  // out of trades.total_idr (pre-tax), as the frontend computes it
	TradeStatus           string // cancelled (refund) | completed
	Payment               string // invoice payment state: refunded | released
	Note                  string // "Putusan dispute: <Note>" on the trade's completed timeline step
	ActorUserID           string
}

// settleDisputeResolution (the money side of a resolution) lives with the trade engine: trade_engine.go.

// ── Read model ───────────────────────────────────────────────────

type disputeRow struct {
	Summary                     api.DisputeSummary
	TradeID, BuyerParty, Status string
	Address                     string
	TotalIdr                    int64
	Resolution                  []byte // JSON of the resolution, nil while open
}

const disputeSelect = `
	SELECT d.id, d.code, d.status, d.reason, d.opened_at, coalesce(ou.name, 'Market Maker'), d.opened_by_side, d.market_id::text,
	       d.resolution_kind, d.refund_idr, d.release_idr, d.resolution_reason, d.resolved_at, ru.name || ' (Admin)',
	       t.id, t.title, t.total_idr, t.buyer_party_id, t.delivery_address,
	       bp.name, bp.display_kind, bp.verified, bp.user_id::text, sp.name, sp.display_kind, sp.verified, sp.user_id::text
	FROM disputes d JOIN trades t ON t.id = d.trade_id
	JOIN parties bp ON bp.id = t.buyer_party_id JOIN parties sp ON sp.id = t.supplier_party_id
	LEFT JOIN users ou ON ou.id = d.opened_by LEFT JOIN users ru ON ru.id = d.resolved_by`

func loadDisputes(ctx context.Context, q dbtx, where string, args ...any) ([]disputeRow, error) {
	rows, err := q.Query(ctx, disputeSelect+" WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []disputeRow{}
	for rows.Next() {
		var r disputeRow
		d := &r.Summary
		var side, kind, reason, by *string
		var refund, release *int64
		var at *time.Time
		var buyer, supplier api.DisputeParty
		if err := rows.Scan(&d.Id, &d.Code, &d.Status, &d.Reason, &d.OpenedAt, &d.OpenedBy, &side, &d.MarketId,
			&kind, &refund, &release, &reason, &at, &by,
			&r.TradeID, &d.Title, &r.TotalIdr, &r.BuyerParty, &r.Address,
			&buyer.Name, &buyer.Kind, &buyer.Verified, &buyer.UserId, &supplier.Name, &supplier.Kind, &supplier.Verified, &supplier.UserId); err != nil {
			return nil, err
		}
		buyer.Role, supplier.Role = api.RoleBuyer, api.RoleSupplier
		d.Parties = []api.DisputeParty{buyer, supplier}
		if side != nil && *side == "supplier" {
			d.Parties = []api.DisputeParty{supplier, buyer} // the party who opened the case first
		}
		d.TotalIdr, r.Status = int(r.TotalIdr), string(d.Status)
		if kind != nil {
			res := map[string]any{"kind": *kind, "refundIdr": *refund, "releaseIdr": *release, "reason": deref(reason), "at": at, "by": deref(by)}
			r.Resolution, _ = json.Marshal(res)
			d.Resolution = &api.DisputeSummary_Resolution{}
			if err := json.Unmarshal(r.Resolution, d.Resolution); err != nil {
				return nil, err
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func loadDisputeSummaries(ctx context.Context, q dbtx, where string, args ...any) ([]api.DisputeSummary, error) {
	rows, err := loadDisputes(ctx, q, where, args...)
	if err != nil {
		return nil, err
	}
	out := make([]api.DisputeSummary, len(rows))
	for i, r := range rows {
		out[i] = r.Summary
	}
	return out, nil
}

func loadDisputeRow(ctx context.Context, q dbtx, id string, forUpdate bool) (disputeRow, error) {
	where := `d.id::text = $1`
	if forUpdate {
		where += ` FOR UPDATE OF d`
	}
	rows, err := loadDisputes(ctx, q, where, id)
	if err != nil {
		return disputeRow{}, err
	}
	if len(rows) == 0 {
		return disputeRow{}, notFound("Kasus tidak ditemukan")
	}
	return rows[0], nil
}

// disputeCase is the full case: the trade from the buyer's side, evidence and the case timeline.
func disputeCase(ctx context.Context, q dbtx, r disputeRow) (api.DisputeCase, error) {
	var c api.DisputeCase
	if err := widen(r.Summary, &c); err != nil {
		return c, err
	}
	if r.Resolution != nil {
		c.Resolution = &api.DisputeCase_Resolution{}
		if err := json.Unmarshal(r.Resolution, c.Resolution); err != nil {
			return c, err
		}
	}
	// The trade from the buyer's side, with the parties' evidence of its latest dispute (transactions read model).
	var err error
	if c.Transaction, err = loadTransaction(ctx, q, r.BuyerParty, r.TradeID); err != nil {
		return c, err
	}

	c.Evidence = []api.Evidence{}
	rows, err := q.Query(ctx, `SELECT id::text, side, author_name, text, file_name, created_at FROM dispute_evidence WHERE dispute_id = $1 ORDER BY created_at`, c.Id)
	if err != nil {
		return c, err
	}
	for rows.Next() {
		var e api.Evidence
		if err := rows.Scan(&e.Id, &e.Side, &e.By, &e.Text, &e.File, &e.At); err != nil {
			return c, err
		}
		c.Evidence = append(c.Evidence, e)
	}
	if err := rows.Err(); err != nil {
		return c, err
	}
	// The opening is part of the case row; dispute_events holds the steps after it.
	type step = struct {
		At    time.Time `json:"at"`
		By    string    `json:"by"`
		Label string    `json:"label"`
	}
	c.Timeline = []step{{At: c.OpenedAt, By: c.OpenedBy, Label: "Dispute dibuka"}}
	rows, err = q.Query(ctx, `SELECT at, actor_label, label FROM dispute_events WHERE dispute_id = $1 ORDER BY at, id`, c.Id)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var s step
		if err := rows.Scan(&s.At, &s.By, &s.Label); err != nil {
			return c, err
		}
		c.Timeline = append(c.Timeline, s)
	}
	return c, rows.Err()
}

// ── Handlers ─────────────────────────────────────────────────────

func (s *Server) ListAdminDisputes(ctx context.Context, _ api.ListAdminDisputesRequestObject) (api.ListAdminDisputesResponseObject, error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	out, err := loadDisputeSummaries(ctx, s.DB.Reader(), `true ORDER BY d.opened_at DESC`)
	if err != nil {
		return nil, err
	}
	return api.ListAdminDisputes200JSONResponse(out), nil
}

func (s *Server) GetAdminDispute(ctx context.Context, req api.GetAdminDisputeRequestObject) (api.GetAdminDisputeResponseObject, error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	q := s.DB.Reader()
	r, err := loadDisputeRow(ctx, q, req.Id, false)
	if err != nil {
		return nil, err
	}
	c, err := disputeCase(ctx, q, r)
	if err != nil {
		return nil, err
	}
	return api.GetAdminDispute200JSONResponse(c), nil
}

var roleLabel = map[string]string{"buyer": "pembeli", "supplier": "supplier", "both": "kedua pihak"}

func (s *Server) ActOnAdminDispute(ctx context.Context, req api.ActOnAdminDisputeRequestObject) (api.ActOnAdminDisputeResponseObject, error) {
	a, err := s.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	action, err := req.Body.Discriminator()
	if err != nil {
		return nil, err
	}
	var out api.DisputeCase
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		r, err := loadDisputeRow(ctx, tx, req.Id, true)
		if err != nil {
			return err
		}
		c := r.Summary
		next := disputeTransition(r.Status, action)
		if next == "" {
			return conflict("invalid_transition", "Aksi ini tidak tersedia untuk status kasus sekarang")
		}
		event := func(label string) error {
			_, err := tx.Exec(ctx, `INSERT INTO dispute_events (dispute_id, actor_user_id, actor_label, label) VALUES ($1, $2, $3, $4)`, c.Id, a.ID, a.Label, label)
			return err
		}
		notifyParties := func(roles []api.Role, title, body string) error {
			for _, p := range c.Parties {
				if p.UserId != nil && slices.Contains(roles, p.Role) {
					if err := notify(ctx, tx, *p.UserId, notification{Type: "transaction_update", Title: title, Body: body, Href: "/app/transactions/" + r.TradeID}); err != nil {
						return err
					}
				}
			}
			return nil
		}
		e := audit{EntityType: "dispute", EntityID: c.Id, EntityLabel: c.Code + " · " + c.Title, MarketID: c.MarketId}
		if r.Status != next {
			e.Changes = []change{diff("status kasus", r.Status, next)}
		}
		switch action {
		case "request_evidence":
			in, err := req.Body.AsDisputeActionInputRequestEvidence()
			if err != nil {
				return err
			}
			reason, err := needReason(&in.Reason)
			if err != nil {
				return err
			}
			if err := event(fmt.Sprintf("Bukti diminta dari %s: %s", roleLabel[string(in.From)], reason)); err != nil {
				return err
			}
			roles := []api.Role{api.Role(in.From)}
			if in.From == api.DisputeActionInputRequestEvidenceFromBoth {
				roles = []api.Role{api.RoleBuyer, api.RoleSupplier}
			}
			if err := notifyParties(roles, c.Code+": Admin meminta bukti", reason); err != nil {
				return err
			}
			e.Action, e.Reason = "Minta bukti dispute", &reason
		case "start_review":
			in, err := req.Body.AsDisputeActionInputStartReview()
			if err != nil {
				return err
			}
			if err := event("Review dimulai"); err != nil {
				return err
			}
			e.Action, e.Reason = "Mulai review dispute", optReason(in.Reason)
		default: // resolve
			in, err := req.Body.AsDisputeActionInputResolve()
			if err != nil {
				return err
			}
			reason, err := needReason(&in.Reason)
			if err != nil {
				return err
			}
			kind, err := in.Resolution.Discriminator()
			if err != nil {
				return err
			}
			res := resolution{Kind: kind}
			if kind == "partial" {
				p, err := in.Resolution.AsResolutionPartial()
				if err != nil {
					return err
				}
				res.RefundIdr = int64(p.RefundIdr)
			}
			if msg := validateResolution(r.TotalIdr, res); msg != "" {
				return &Error{Status: 422, Code: "validation", Message: msg, Fields: map[string]string{"refundIdr": msg}}
			}
			o := resolveOutcome(r.TotalIdr, res)
			var tradeStatus, payment string
			if err := tx.QueryRow(ctx, `SELECT t.status, coalesce(i.status, 'unpaid') FROM trades t LEFT JOIN invoices i ON i.trade_id = t.id WHERE t.id = $1`,
				r.TradeID).Scan(&tradeStatus, &payment); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE disputes SET status = 'resolved', resolution_kind = $2, refund_idr = $3, release_idr = $4, resolution_reason = $5,
				  resolved_at = now(), resolved_by = $6 WHERE id = $1`, c.Id, kind, o.RefundIdr, o.ReleaseIdr, reason, a.ID); err != nil {
				return err
			}
			if err := event("Diputuskan: " + o.Note); err != nil {
				return err
			}
			if err := s.settleDisputeResolution(ctx, tx, disputeSettlement{DisputeID: c.Id, TradeID: r.TradeID, Kind: kind, RefundIdr: o.RefundIdr,
				ReleaseIdr: o.ReleaseIdr, TradeStatus: o.Status, Payment: o.Payment, Note: o.Note, ActorUserID: a.ID}); err != nil {
				return err
			}
			if err := notifyParties([]api.Role{api.RoleBuyer, api.RoleSupplier}, c.Code+" diputuskan", o.Note); err != nil {
				return err
			}
			verb := map[string]string{"refund": "refund penuh", "release": "lepas dana", "partial": "refund sebagian"}[kind]
			e.Action, e.Reason = "Putuskan dispute ("+verb+")", &reason
			e.Changes = append(e.Changes, diff("status transaksi", tradeStatus, o.Status), diff("pembayaran", payment, o.Payment))
		}
		if action != "resolve" {
			if _, err := tx.Exec(ctx, `UPDATE disputes SET status = $2 WHERE id = $1`, c.Id, next); err != nil {
				return err
			}
		}
		if err := a.record(ctx, tx, e); err != nil {
			return err
		}
		r, err = loadDisputeRow(ctx, tx, c.Id, false)
		if err != nil {
			return err
		}
		out, err = disputeCase(ctx, tx, r)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.ActOnAdminDispute200JSONResponse(out), nil
}
