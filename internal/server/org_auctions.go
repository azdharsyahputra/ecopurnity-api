package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Business auctions (PRD 9.6). An org auction is a header with lots; once approved each lot runs as an economy auction
// (auctions row owned by the org, no market), so bidding, anti-sniping, the clock and the realtime room are the
// economy's. Award and PO happen here: the PO turns each award line into a trade with the org as one party.

var errOrgAuctionNotFound = notFound("Auction tidak ditemukan")

type liveLot = struct {
	AuctionId    string            `json:"auctionId"`
	BestPriceIdr *int              `json:"bestPriceIdr,omitempty"`
	BidCount     int               `json:"bidCount"`
	EndsAt       time.Time         `json:"endsAt"`
	Participants int               `json:"participants"`
	Status       api.AuctionStatus `json:"status"`
}

type orgAward = struct {
	At             time.Time              `json:"at"`
	By             string                 `json:"by"`
	Lines          [][]api.AllocationLine `json:"lines"`
	PoNumber       *string                `json:"poNumber,omitempty"`
	Reason         string                 `json:"reason"`
	TransactionIds *[]string              `json:"transactionIds,omitempty"`
}

// higherWins: selling auctions (forward/Dutch) are won by the highest price.
func higherWins(typ string) bool { return typ == "forward" || typ == "dutch" }

// orgAuctionStatus: org-level status from its lots (src/domain/org.ts orgAuctionStatus).
func orgAuctionStatus(stored string, awarded bool, lots []string) string {
	switch {
	case stored == "pending_approval" || stored == "rejected":
		return stored
	case awarded:
		return "awarded"
	case len(lots) == 0:
		return stored
	case slices.ContainsFunc(lots, func(s string) bool { return s == "live" || s == "extended" }):
		return "live"
	case !slices.ContainsFunc(lots, func(s string) bool { return s != "scheduled" && s != "qualification" }):
		return "scheduled"
	}
	return "closed"
}

// loadOrgAuctions: the org's auctions (newest first) matching cond over alias a, with lots, live facts, approvals and award.
func loadOrgAuctions(ctx context.Context, q dbtx, orgID, cond string, args ...any) ([]api.OrgAuctionView, error) {
	rows, err := q.Query(ctx, `
		SELECT a.id::text, a.code, a.title, a.category_id, a.type, a.objective, a.min_step_idr, a.bid_visibility, a.auto_extension, a.withdraw_rule,
		       a.award_rule, a.weight_price::float8, a.weight_quality::float8, a.weight_delivery::float8, a.weight_reliability::float8, a.qual_documents,
		       a.qual_min_rating::float8, a.qual_regions, a.starts_at, a.duration_minutes, a.procurement_request_id::text, a.status, a.value_idr,
		       a.required_approvers, a.created_at, `+memberLabelSQL("u.name", "u.id", "a.org_id")+`,
		       aw.reason, aw.awarded_at, `+memberLabelSQL("awu.name", "awu.id", "a.org_id")+`, po.po_number
		FROM org_auctions a JOIN users u ON u.id = a.created_by
		LEFT JOIN org_awards aw ON aw.org_auction_id = a.id LEFT JOIN users awu ON awu.id = aw.awarded_by
		LEFT JOIN purchase_orders po ON po.org_award_id = aw.id
		WHERE a.org_id::text = $1 AND `+cond+` ORDER BY a.created_at DESC, a.id`, append([]any{orgID}, args...)...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (api.OrgAuctionView, error) {
		var a api.OrgAuctionView
		var reason, by, po *string
		var at *time.Time
		err := r.Scan(&a.Id, &a.Code, &a.Title, &a.CategoryId, &a.Type, &a.Objective, &a.Rules.MinStepIdr, &a.Rules.Visibility, &a.Rules.AutoExtension,
			&a.Rules.Withdraw, &a.Rules.Award, &a.Rules.Weights.Price, &a.Rules.Weights.Quality, &a.Rules.Weights.Delivery, &a.Rules.Weights.Reliability,
			&a.Qualification.Documents, &a.Qualification.MinRating, &a.Qualification.Regions, &a.Schedule.StartsAt, &a.Schedule.DurationMinutes,
			&a.ProcurementId, &a.Status, &a.ValueIdr, &a.RequiredApprovers, &a.CreatedAt, &a.CreatedBy, &reason, &at, &by, &po)
		if at != nil {
			a.Award = &orgAward{At: *at, By: deref(by), Reason: deref(reason), PoNumber: po, Lines: [][]api.AllocationLine{}}
		}
		a.Lots, a.Live, a.Approvals, a.Invited = []api.OrgLot{}, []liveLot{}, []api.Approval{}, []string{}
		return a, err
	})
	if err != nil || len(out) == 0 {
		return nonNil(out), err
	}
	ids := make([]string, len(out))
	idx := map[string]int{}
	for i, a := range out {
		ids[i], idx[a.Id] = a.Id, i
	}
	lotStatus := make([][]string, len(out))
	rows, err = q.Query(ctx, `
		SELECT l.org_auction_id::text, l.position, l.item, l.quantity, l.unit, l.spec, l.reserve_price_idr, l.auction_id::text,
		       e.status, e.bid_count, e.participant_count, e.current_price_idr, e.ends_at
		FROM org_auction_lots l LEFT JOIN auctions e ON e.id = l.auction_id
		WHERE l.org_auction_id::text = ANY($1) ORDER BY l.position`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var pos int
		var l api.OrgLot
		var status *string
		var bids, parts *int
		var best *int64
		var ends *time.Time
		if err := rows.Scan(&id, &pos, &l.Item, &l.Quantity.Value, &l.Quantity.Unit, &l.Spec, &l.ReservePriceIdr, &l.AuctionId, &status, &bids, &parts, &best, &ends); err != nil {
			rows.Close()
			return nil, err
		}
		i := idx[id]
		a := &out[i]
		l.Id = fmt.Sprintf("lot-%d", pos)
		a.Lots = append(a.Lots, l)
		if l.AuctionId != nil {
			live := liveLot{AuctionId: *l.AuctionId, Status: api.AuctionStatus(*status), BidCount: *bids, Participants: *parts, EndsAt: *ends}
			// The org owns the auction, so it sees the best price, except a sealed one while it runs.
			if best != nil && !(a.Type == "sealed" && (*status == "live" || *status == "extended")) {
				live.BestPriceIdr = ptr(int(*best))
			}
			a.Live = append(a.Live, live)
			lotStatus[i] = append(lotStatus[i], *status)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `SELECT org_auction_id::text, supplier_id::text FROM org_auction_invites WHERE org_auction_id::text = ANY($1) ORDER BY 1, 2`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, sup string
		if err := rows.Scan(&id, &sup); err != nil {
			rows.Close()
			return nil, err
		}
		out[idx[id]].Invited = append(out[idx[id]].Invited, sup)
	}
	rows.Close()
	rows, err = q.Query(ctx, `
		SELECT a.org_auction_id::text, a.role, a.decision, a.note, a.decided_at,
		       `+approvalBySQL("oa.org_id")+`
		FROM org_auction_approvals a JOIN org_auctions oa ON oa.id = a.org_auction_id JOIN users u ON u.id = a.decided_by
		WHERE a.org_auction_id::text = ANY($1) ORDER BY a.decided_at, a.id`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var ap api.Approval
		if err := rows.Scan(&id, &ap.Role, &ap.Decision, &ap.Note, &ap.At, &ap.By); err != nil {
			rows.Close()
			return nil, err
		}
		out[idx[id]].Approvals = append(out[idx[id]].Approvals, ap)
	}
	rows.Close()
	rows, err = q.Query(ctx, `
		SELECT aw.org_auction_id::text, lot.position, wl.org_auction_offer_id::text, coalesce(sup.name, p.name), wl.quantity, wl.price_idr, wl.trade_id::text
		FROM org_award_lines wl JOIN org_awards aw ON aw.id = wl.org_award_id JOIN org_auction_lots lot ON lot.id = wl.org_auction_lot_id
		JOIN org_auction_offers o ON o.id = wl.org_auction_offer_id JOIN parties p ON p.id = o.party_id LEFT JOIN suppliers sup ON sup.id = o.supplier_id
		WHERE aw.org_auction_id::text = ANY($1) ORDER BY lot.position, wl.position`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var pos int
		var l api.AllocationLine
		var trade *string
		if err := rows.Scan(&id, &pos, &l.OfferId, &l.Supplier, &l.Quantity, &l.PriceIdr, &trade); err != nil {
			return nil, err
		}
		aw := out[idx[id]].Award
		for len(aw.Lines) < pos {
			aw.Lines = append(aw.Lines, []api.AllocationLine{})
		}
		aw.Lines[pos-1] = append(aw.Lines[pos-1], l)
		if trade != nil {
			if aw.TransactionIds == nil {
				aw.TransactionIds = &[]string{}
			}
			*aw.TransactionIds = append(*aw.TransactionIds, *trade)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		a := &out[i]
		a.MultiLot = len(a.Lots) > 1
		if a.Award != nil {
			for len(a.Award.Lines) < len(a.Lots) {
				a.Award.Lines = append(a.Award.Lines, []api.AllocationLine{})
			}
		}
		a.Status = api.OrgAuctionStatus(orgAuctionStatus(string(a.Status), a.Award != nil, lotStatus[i]))
	}
	return out, nil
}

func loadOrgAuction(ctx context.Context, q dbtx, orgID, id string) (api.OrgAuctionView, error) {
	as, err := loadOrgAuctions(ctx, q, orgID, `a.id::text = $2`, id)
	if err == nil && len(as) == 0 {
		err = errOrgAuctionNotFound
	}
	if err != nil {
		return api.OrgAuctionView{}, err
	}
	return as[0], nil
}

// lockOrgAuction locks the header row (404 when not this org's).
func lockOrgAuction(ctx context.Context, tx pgx.Tx, orgID, id string) (string, error) {
	var got string
	err := tx.QueryRow(ctx, `SELECT id::text FROM org_auctions WHERE org_id = $1 AND id::text = $2 FOR UPDATE`, orgID, id).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errOrgAuctionNotFound
	}
	return got, err
}

func (s *Server) ListOrgAuctions(ctx context.Context, req api.ListOrgAuctionsRequestObject) (api.ListOrgAuctionsResponseObject, error) {
	q := s.DB.Primary()
	c, err := orgAccess(ctx, q, req.OrgId)
	if err != nil {
		return nil, err
	}
	out, err := loadOrgAuctions(ctx, q, c.OrgID, "true")
	if err != nil {
		return nil, err
	}
	return api.ListOrgAuctions200JSONResponse(out), nil
}

// withdrawRuleLabel: src/domain/org.ts WITHDRAW_RULES (the lot's "Penarikan bid" rule; enforced by withdrawBlock).
var withdrawRuleLabel = map[string]string{"anytime": "Boleh tarik kapan saja", "before_last_30": "Boleh tarik sampai 30 menit terakhir",
	"never": "Bid mengikat, tidak bisa ditarik"}

var auctionTypeLabel = map[string]string{"reverse": "Reverse", "forward": "Forward", "sealed": "Sealed bid", "dutch": "Dutch"}

func awardRuleLabel(rule string, higher bool) string {
	if higher {
		return map[string]string{"lowest": "Harga tertinggi", "weighted": "Weighted score", "split": "Split award", "bundled": "Bundled"}[rule]
	}
	return map[string]string{"lowest": "Harga terendah", "weighted": "Weighted score", "split": "Split award", "bundled": "Bundled"}[rule]
}

func (s *Server) CreateOrgAuction(ctx context.Context, req api.CreateOrgAuctionRequestObject) (api.CreateOrgAuctionResponseObject, error) {
	in := req.Body
	var out api.OrgAuctionView
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("auctions", "create"); err != nil {
			return err
		}
		if c.sess.Status == "restricted" {
			return errRestricted
		}
		f := map[string]string{}
		if strings.TrimSpace(in.Title) == "" {
			f["title"] = "Judul wajib diisi"
		}
		switch {
		case len(in.Lots) == 0:
			f["lots"] = "Tambah minimal satu lot"
		case len(in.Lots) > 20:
			f["lots"] = "Maksimal 20 lot"
		}
		var value float64
		for i, l := range in.Lots {
			if strings.TrimSpace(l.Item) == "" || !(l.Quantity.Value > 0) || strings.TrimSpace(l.Quantity.Unit) == "" || l.ReservePriceIdr <= 0 {
				f[fmt.Sprintf("lot-%d", i)] = "Lengkapi item, kuantitas, dan harga"
			}
			value += l.Quantity.Value * float64(l.ReservePriceIdr)
		}
		if d := in.Schedule.DurationMinutes; d <= 0 || d > 30*24*60 {
			f["duration"] = "Pilih durasi"
		}
		if st := in.Schedule.StartsAt; st != nil && st.Before(time.Now().Add(-time.Minute)) {
			f["startsAt"] = "Waktu mulai sudah lewat"
		}
		if in.Type == "dutch" {
			// ponytail: accepting a Dutch ask creates a trade with the market's maker; org lots have no market. Allow once
			// AcceptDutchPrice can sell on behalf of an org.
			f["type"] = "Dutch auction belum tersedia untuk auction bisnis"
		}
		if in.Rules.MinStepIdr < 0 {
			f["minStepIdr"] = "Langkah minimum harus ≥ 0"
		}
		if w := in.Rules.Weights; w.Price < 0 || w.Quality < 0 || w.Delivery < 0 || w.Reliability < 0 {
			f["weights"] = "Bobot harus ≥ 0"
		}
		if r := in.Qualification.MinRating; r < 0 || r > 5 {
			f["minRating"] = "Rating minimum 0–5"
		}
		invited := []string{}
		for _, id := range in.Invited {
			if !slices.Contains(invited, id) {
				invited = append(invited, id)
			}
		}
		if len(invited) > 0 {
			var known int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM suppliers WHERE id::text = ANY($1)`, invited).Scan(&known); err != nil {
				return err
			}
			if known != len(invited) {
				f["invited"] = "Supplier tidak ditemukan"
			}
		}
		var procurementID *string
		if p := in.ProcurementId; p != nil && *p != "" {
			var id string
			err := tx.QueryRow(ctx, `SELECT id::text FROM procurement_requests WHERE org_id = $1 AND id::text = $2 AND status IN ('approved','published') FOR UPDATE`,
				c.OrgID, *p).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				f["procurementId"] = "Procurement ini tidak bisa dilelang (harus approved atau published)"
			} else if err != nil {
				return err
			}
			procurementID = &id
		}
		if err := invalid("Periksa kembali isian auction", f); err != nil {
			return err
		}
		rules, err := loadApprovalRules(ctx, tx, c.OrgID)
		if err != nil {
			return err
		}
		valueIdr := int64(math.Round(value))
		required := requiredApprovers(valueIdr, "auction", rules)
		visibility := string(in.Rules.Visibility)
		if in.Type == "sealed" {
			visibility = "sealed"
		}
		var id, code string
		w := in.Rules.Weights
		if err := tx.QueryRow(ctx, `
			INSERT INTO org_auctions (org_id, title, category_id, type, objective, min_step_idr, bid_visibility, auto_extension, withdraw_rule, award_rule,
			  weight_price, weight_quality, weight_delivery, weight_reliability, qual_documents, qual_min_rating, qual_regions, starts_at, duration_minutes,
			  procurement_request_id, status, value_idr, required_approvers, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, 'pending_approval', $21, $22, $23)
			RETURNING id::text, code`,
			c.OrgID, strings.TrimSpace(in.Title), in.CategoryId, in.Type, in.Objective, in.Rules.MinStepIdr, visibility, in.Rules.AutoExtension,
			in.Rules.Withdraw, in.Rules.Award, w.Price, w.Quality, w.Delivery, w.Reliability, nonNil(in.Qualification.Documents), in.Qualification.MinRating,
			nonNil(in.Qualification.Regions), in.Schedule.StartsAt, in.Schedule.DurationMinutes, procurementID, valueIdr, required, c.sess.UserID).Scan(&id, &code); err != nil {
			return err
		}
		for i, l := range in.Lots {
			if _, err := tx.Exec(ctx, `
				INSERT INTO org_auction_lots (org_auction_id, position, item, quantity, unit, spec, reserve_price_idr) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				id, i+1, strings.TrimSpace(l.Item), l.Quantity.Value, strings.TrimSpace(l.Quantity.Unit), strings.TrimSpace(l.Spec), l.ReservePriceIdr); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO org_auction_invites (org_auction_id, supplier_id) SELECT $1, unnest($2::uuid[])`, id, invited); err != nil {
			return err
		}
		if procurementID != nil {
			if _, err := tx.Exec(ctx, `UPDATE procurement_requests SET status = 'in_auction' WHERE id = $1`, *procurementID); err != nil {
				return err
			}
		}
		action := "Buka auction"
		if len(required) == 0 {
			if err := goLive(ctx, tx, c, id); err != nil {
				return err
			}
		} else {
			action = "Ajukan auction untuk approval"
			if err := askApprovers(ctx, tx, c, "auction", id, code, strings.TrimSpace(in.Title), valueIdr, required, nil); err != nil {
				return err
			}
		}
		if err := c.audit(ctx, tx, action, "auction", id, code+" "+strings.TrimSpace(in.Title), nil); err != nil {
			return err
		}
		out, err = loadOrgAuction(ctx, tx, c.OrgID, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.CreateOrgAuction201JSONResponse(out), nil
}

// goLive opens an approved org auction: one economy auction per lot, owned by the org (members may not bid and
// evaluate it from the org workspace), starting at the scheduled time or now.
func goLive(ctx context.Context, tx pgx.Tx, c *orgCtx, id string) error {
	a, err := loadOrgAuction(ctx, tx, c.OrgID, id)
	if err != nil {
		return err
	}
	var createdBy string
	if err := tx.QueryRow(ctx, `SELECT created_by::text FROM org_auctions WHERE id = $1`, id).Scan(&createdBy); err != nil {
		return err
	}
	var invitees []string
	if err := tx.QueryRow(ctx, `SELECT array(SELECT s.name FROM org_auction_invites i JOIN suppliers s ON s.id = i.supplier_id WHERE i.org_auction_id = $1 ORDER BY s.name)`,
		id).Scan(&invitees); err != nil {
		return err
	}
	start := time.Now()
	if st := a.Schedule.StartsAt; st != nil && st.After(start) {
		start = *st
	}
	status := "live"
	if start.After(time.Now()) {
		status = "scheduled"
	}
	typ, higher := string(a.Type), higherWins(string(a.Type))
	ext, extMin := 0, 0
	if a.Rules.AutoExtension {
		ext, extMin = 2, 5
	}
	for i, l := range a.Lots {
		title, typeLabel := a.Title, auctionTypeLabel[typ]+" auction"
		if a.MultiLot {
			title = fmt.Sprintf("%s · Lot %d: %s", a.Title, i+1, l.Item)
			typeLabel += fmt.Sprintf(" · lot %d dari %d", i+1, len(a.Lots))
		}
		reserve, step := "Harga target", "Penurunan minimum"
		if typ == "forward" {
			reserve, step = "Reserve", "Kenaikan minimum"
		}
		rules := []api.LabeledValue{{Label: "Tipe", Value: typeLabel}, {Label: "Penyelenggara", Value: c.OrgName},
			{Label: reserve, Value: fmt.Sprintf("%s per %s", rupiah(int64(l.ReservePriceIdr)), l.Quantity.Unit)}}
		if a.Rules.MinStepIdr > 0 {
			rules = append(rules, api.LabeledValue{Label: step, Value: rupiah(int64(a.Rules.MinStepIdr))})
		}
		extLabel := "Tidak ada"
		if a.Rules.AutoExtension {
			extLabel = "+5 menit jika ada bid di 2 menit terakhir"
		}
		qual := fmt.Sprintf("Rating ≥ %g", a.Qualification.MinRating)
		if len(a.Qualification.Documents) > 0 {
			qual += ", dokumen: " + strings.Join(a.Qualification.Documents, ", ")
		}
		if len(a.Qualification.Regions) > 0 {
			qual += ", wilayah: " + strings.Join(a.Qualification.Regions, ", ")
		}
		rules = append(rules, api.LabeledValue{Label: "Perpanjangan otomatis", Value: extLabel}, api.LabeledValue{Label: "Kualifikasi", Value: qual},
			api.LabeledValue{Label: "Penarikan bid", Value: withdrawRuleLabel[string(a.Rules.Withdraw)]},
			api.LabeledValue{Label: "Penetapan pemenang", Value: awardRuleLabel(string(a.Rules.Award), higher)})
		var auctionID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO auctions (title, owner_org_id, category_id, type, status, visibility, lot_item, lot_spec, quantity, unit, opening_price_idr, min_step_idr,
			                      starts_at, ends_at, ext_window_minutes, ext_minutes, rules, invitees, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::timestamptz, $13::timestamptz + make_interval(mins => $14), $15, $16, $17, $18, $19) RETURNING id::text`,
			title, c.OrgID, a.CategoryId, typ, status, a.Rules.Visibility, l.Item, l.Spec, l.Quantity.Value, l.Quantity.Unit, l.ReservePriceIdr,
			a.Rules.MinStepIdr, start, a.Schedule.DurationMinutes, ext, extMin, rules, nonNil(invitees), createdBy).Scan(&auctionID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE org_auction_lots SET auction_id = $3 WHERE org_auction_id = $1 AND position = $2`, id, i+1, auctionID); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE org_auctions SET status = $2 WHERE id = $1`, id, status)
	return err
}

func (s *Server) DecideOrgAuction(ctx context.Context, req api.DecideOrgAuctionRequestObject) (api.DecideOrgAuctionResponseObject, error) {
	var out api.OrgAuctionView
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		id, err := lockOrgAuction(ctx, tx, c.OrgID, req.Aid)
		if err != nil {
			return err
		}
		a, err := loadOrgAuction(ctx, tx, c.OrgID, id)
		if err != nil {
			return err
		}
		if a.Status != "pending_approval" {
			return conflict("invalid_action", "Auction ini tidak menunggu approval")
		}
		approvals := apiApprovals(a.Approvals)
		active, err := activeRoles(ctx, tx, c.OrgID)
		if err != nil {
			return err
		}
		roles := signingRoles(c.Role, a.RequiredApprovers, approvals, active)
		if len(roles) == 0 {
			return &Error{Status: http.StatusForbidden, Code: "forbidden", Message: "Approval ini tidak menunggu peran " + c.RoleLabel}
		}
		decision := "approved"
		note := strings.TrimSpace(deref(req.Body.Note))
		if req.Body.Action == "reject" {
			decision = "rejected"
			if note == "" {
				return invalid("Tulis alasan penolakan", map[string]string{"note": "Alasan wajib diisi saat menolak"})
			}
		}
		signed, err := sign(ctx, tx, "org_auction_approvals", "org_auction_id", id, c, roles, decision, note)
		if err != nil {
			return err
		}
		_, rejected, approved := approvalState(a.RequiredApprovers, append(approvals, signed...))
		switch {
		case approved:
			if err := goLive(ctx, tx, c, id); err != nil {
				return err
			}
		case rejected:
			// A rejected auction hands its procurement back so it can be re-run or sourced another way.
			if _, err := tx.Exec(ctx, `UPDATE org_auctions SET status = 'rejected' WHERE id = $1`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE procurement_requests SET status = 'approved' WHERE id = (SELECT procurement_request_id FROM org_auctions WHERE id = $1) AND status = 'in_auction'`,
				id); err != nil {
				return err
			}
		}
		var reason *string
		action := "Approve auction"
		if rejected {
			reason, action = &note, "Tolak auction"
		}
		if err := c.audit(ctx, tx, action, "auction", id, a.Code, reason); err != nil {
			return err
		}
		out, err = loadOrgAuction(ctx, tx, c.OrgID, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.DecideOrgAuction200JSONResponse(out), nil
}

// notifyOrgLotsClosed tells the org's members who can view auctions (owner always) that a business auction is ready to
// evaluate: once, in the transaction that closes its last lot, with one line per lot. Lots close in separate clock
// transactions (possibly on different instances); the advisory lock serialises them per business auction, so exactly
// one sees every sibling closed (each statement reads the latest commits). Not the header row lock: award holds that
// while it locks the lots, which would deadlock with a closing lot.
func notifyOrgLotsClosed(ctx context.Context, tx pgx.Tx, orgID, orgAuctionID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('org_auction_close:' || $1, 0))`, orgAuctionID); err != nil {
		return err
	}
	var title string
	if err := tx.QueryRow(ctx, `SELECT title FROM org_auctions WHERE id = $1`, orgAuctionID).Scan(&title); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		SELECT l.position, l.item, e.status IN ('live','extended','scheduled','qualification'),
		       (SELECT count(DISTINCT b.bidder_party_id) FROM bids b WHERE b.auction_id = e.id AND b.status <> 'withdrawn')
		FROM org_auction_lots l JOIN auctions e ON e.id = l.auction_id WHERE l.org_auction_id = $1 ORDER BY l.position`, orgAuctionID)
	if err != nil {
		return err
	}
	type lot struct {
		pos    int
		item   string
		open   bool
		offers int
	}
	lots, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (lot, error) {
		var l lot
		return l, r.Scan(&l.pos, &l.item, &l.open, &l.offers)
	})
	if err != nil || slices.ContainsFunc(lots, func(l lot) bool { return l.open }) {
		return err
	}
	var lines []string
	for _, l := range lots {
		name := l.item
		if len(lots) > 1 {
			name = fmt.Sprintf("%d %s", l.pos, l.item)
		}
		lines = append(lines, fmt.Sprintf("Lot %s ditutup, %d penawaran.", name, l.offers))
	}
	rows, err = tx.Query(ctx, `
		SELECT m.user_id::text FROM org_members m JOIN org_roles ro ON ro.org_id = m.org_id AND ro.key = m.role
		WHERE m.org_id = $1 AND m.status = 'active' AND m.user_id IS NOT NULL AND (m.role = 'owner' OR 'auctions.view' = ANY(ro.permissions))
		ORDER BY m.created_at`, orgID)
	if err != nil {
		return err
	}
	users, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	n := notification{Type: "auction_ending", Title: title + " ditutup", Body: strings.Join(lines, " ") + " Evaluasi dan tetapkan pemenang.",
		Href: "/org/" + orgID + "/auctions/" + orgAuctionID + "/evaluate"}
	for _, u := range users {
		if err := notify(ctx, tx, u, n); err != nil {
			return err
		}
	}
	return nil
}

// lotOffer is an evaluated offer plus what award and PO need.
type lotOffer struct {
	api.LotOffer
	PartyID    string
	SupplierID *string // directory match
	UserID     *string // bidder account, for notifications
}

// evalOffers: one offer per bidder of a lot auction (their best active bid), best price first, mapped onto the supplier
// directory (scorecard) by party; bidders not in the directory get their party id and an 80 baseline.
func evalOffers(ctx context.Context, q dbtx, r auctionRow, higher bool) ([]lotOffer, error) {
	src := r
	if higher { // offers() keeps each bidder's highest bid only for forward; Dutch sells too
		src.Type = "forward"
	}
	os, err := offers(ctx, q, src)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(os, func(i, j int) bool {
		if higher {
			return os[i].Price > os[j].Price
		}
		return os[i].Price < os[j].Price
	})
	parties := make([]string, len(os))
	for i, o := range os {
		parties[i] = o.PartyID
	}
	type dir struct {
		id, name                   string
		verified                   bool
		price, rel, qual, delivery *float64
	}
	byParty := map[string]dir{}
	rows, err := q.Query(ctx, `
		SELECT s.party_id::text, s.id::text, s.name, s.verified, sc.price::float8, sc.reliability::float8, sc.quality::float8, sc.delivery::float8
		FROM suppliers s LEFT JOIN LATERAL (SELECT * FROM supplier_scorecards x WHERE x.supplier_id = s.id ORDER BY month DESC LIMIT 1) sc ON true
		WHERE s.party_id::text = ANY($1)`, parties)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p string
		var d dir
		if err := rows.Scan(&p, &d.id, &d.name, &d.verified, &d.price, &d.rel, &d.qual, &d.delivery); err != nil {
			rows.Close()
			return nil, err
		}
		byParty[p] = d
	}
	rows.Close()
	out := make([]lotOffer, 0, len(os))
	for _, o := range os {
		lo := lotOffer{PartyID: o.PartyID, UserID: o.UserID}
		lo.Id, lo.PriceIdr, lo.SubmittedAt, lo.SupplierId = o.BidID, int(o.Price), o.At, o.PartyID
		lo.Supplier.Name, lo.Supplier.Kind, lo.Supplier.Verified, lo.Supplier.Reputation = o.Name, api.LotOfferSupplierKind(o.Kind), o.Verified, 80
		lo.Capacity = api.Quantity{Value: o.Capacity, Unit: r.Unit}
		lo.Quality, lo.Delivery, lo.Reliability = 80, 80, 80
		if d, ok := byParty[o.PartyID]; ok {
			lo.SupplierId, lo.SupplierID, lo.Supplier.Name, lo.Supplier.Verified = d.id, &d.id, d.name, d.verified
			if d.price != nil {
				lo.Quality, lo.Delivery, lo.Reliability = *d.qual, *d.delivery, *d.rel
				lo.Supplier.Reputation = math.Round((*d.price + *d.rel + *d.qual + *d.delivery) / 4)
			}
		}
		out = append(out, lo)
	}
	return out, nil
}

type lotRow struct {
	ID, Item, Unit string
	Position       int
	Qty            float64
	Reserve        int64
	AuctionID      *string
}

func orgLots(ctx context.Context, q dbtx, orgAuctionID string) ([]lotRow, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, item, unit, position, quantity, reserve_price_idr, auction_id::text FROM org_auction_lots WHERE org_auction_id = $1 ORDER BY position`, orgAuctionID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (lotRow, error) {
		var l lotRow
		return l, r.Scan(&l.ID, &l.Item, &l.Unit, &l.Position, &l.Qty, &l.Reserve, &l.AuctionID)
	})
}

func (s *Server) GetOrgAuctionEvaluation(ctx context.Context, req api.GetOrgAuctionEvaluationRequestObject) (api.GetOrgAuctionEvaluationResponseObject, error) {
	q := s.DB.Primary()
	c, err := orgAccess(ctx, q, req.OrgId)
	if err != nil {
		return nil, err
	}
	a, err := loadOrgAuction(ctx, q, c.OrgID, req.Aid)
	if err != nil {
		return nil, err
	}
	p, err := loadOrgProfile(ctx, q, c.OrgID)
	if err != nil {
		return nil, err
	}
	out := api.GetOrgAuctionEvaluation200JSONResponse{Auction: a, Lots: []api.LotEvaluation{}}
	out.Org.Name, out.Org.Location, out.Org.Npwp = p.Name, p.Location, p.Legal.Npwp
	for _, l := range a.Lots {
		ev := api.LotEvaluation{Lot: l, Status: "scheduled", Offers: []api.LotOffer{}}
		if l.AuctionId != nil {
			r, err := loadAuction(ctx, q, *l.AuctionId, false)
			if err != nil {
				return nil, err
			}
			ev.Status = api.AuctionStatus(r.Status)
			os, err := evalOffers(ctx, q, r, higherWins(string(a.Type)))
			if err != nil {
				return nil, err
			}
			for _, o := range os {
				ev.Offers = append(ev.Offers, o.LotOffer)
			}
		}
		out.Lots = append(out.Lots, ev)
	}
	return out, nil
}

func (s *Server) AwardOrgAuction(ctx context.Context, req api.AwardOrgAuctionRequestObject) (api.AwardOrgAuctionResponseObject, error) {
	in := req.Body
	var out api.OrgAuctionView
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("auctions", "manage"); err != nil {
			return err
		}
		id, err := lockOrgAuction(ctx, tx, c.OrgID, req.Aid)
		if err != nil {
			return err
		}
		a, err := loadOrgAuction(ctx, tx, c.OrgID, id)
		if err != nil {
			return err
		}
		if a.Award != nil {
			return conflict("awarded", "Pemenang sudah ditetapkan")
		}
		lots, err := orgLots(ctx, tx, id)
		if err != nil {
			return err
		}
		econ := make([]auctionRow, len(lots))
		for i, l := range lots {
			if l.AuctionID != nil {
				if econ[i], err = loadAuction(ctx, tx, *l.AuctionID, true); err != nil {
					return err
				}
			}
			if l.AuctionID == nil || econ[i].Status != "closed" {
				return conflict("not_closed", "Award hanya setelah semua lot ditutup")
			}
		}
		reason := strings.TrimSpace(in.Reason)
		if reason == "" {
			return invalid("Tulis alasan award", map[string]string{"reason": "Alasan award wajib diisi (tercatat di audit trail)"})
		}
		if len(in.Lines) != len(lots) {
			return invalid("Setiap lot harus punya pemenang", map[string]string{"lines": "Setiap lot harus punya pemenang"})
		}
		f := map[string]string{}
		higher := higherWins(string(a.Type))
		offersByLot := make([]map[string]lotOffer, len(lots))
		allOffers := make([][]lotOffer, len(lots))
		for i, l := range lots {
			os, err := evalOffers(ctx, tx, econ[i], higher)
			if err != nil {
				return err
			}
			allOffers[i], offersByLot[i] = os, map[string]lotOffer{}
			for _, o := range os {
				offersByLot[i][o.Id] = o
			}
			if len(in.Lines[i]) == 0 {
				f[fmt.Sprintf("lines.%d", i)] = "Setiap lot harus punya pemenang"
			}
			var sum float64
			seen := map[string]bool{}
			for j, ln := range in.Lines[i] {
				key := fmt.Sprintf("lines.%d.%d", i, j)
				o, ok := offersByLot[i][ln.OfferId]
				switch {
				case !ok:
					f[key] = "Penawaran tidak ditemukan"
				case seen[ln.OfferId]:
					f[key] = "Penawaran dipilih dua kali"
				case !(ln.Quantity > 0):
					f[key] = "Jumlah harus lebih dari 0"
				case ln.Quantity > o.Capacity.Value+1e-9:
					f[key] = "Melebihi kapasitas penawaran"
				}
				seen[ln.OfferId] = true
				sum += ln.Quantity
			}
			if sum > l.Qty+1e-9 {
				f[fmt.Sprintf("lines.%d", i)] = "Total alokasi melebihi kuantitas lot"
			}
		}
		if err := invalid("Periksa kembali alokasi pemenang", f); err != nil {
			return err
		}

		// Snapshot every offer of every lot (the evaluation the award was made on); award lines point at them.
		for i, l := range lots {
			for _, o := range allOffers[i] {
				if _, err := tx.Exec(ctx, `
					INSERT INTO org_auction_offers (id, org_auction_lot_id, party_id, supplier_id, price_idr, capacity, submitted_at)
					VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`, o.Id, l.ID, o.PartyID, o.SupplierID, o.PriceIdr, o.Capacity.Value, o.SubmittedAt); err != nil {
					return err
				}
			}
		}
		var awardID string
		if err := tx.QueryRow(ctx, `INSERT INTO org_awards (org_auction_id, reason, awarded_by) VALUES ($1, $2, $3) RETURNING id`, id, reason, c.sess.UserID).Scan(&awardID); err != nil {
			return err
		}
		var changes []change
		for i, l := range lots {
			winners := map[string]bool{}
			var parts []string
			for j, ln := range in.Lines[i] {
				o := offersByLot[i][ln.OfferId]
				winners[o.PartyID] = true
				if _, err := tx.Exec(ctx, `
					INSERT INTO org_award_lines (org_award_id, org_auction_lot_id, position, org_auction_offer_id, quantity, price_idr) VALUES ($1, $2, $3, $4, $5, $6)`,
					awardID, l.ID, j, o.Id, ln.Quantity, o.PriceIdr); err != nil {
					return err
				}
				parts = append(parts, fmt.Sprintf("%s %s × %s", o.Supplier.Name, qtyLabel(ln.Quantity), rupiah(int64(o.PriceIdr))))
				if o.UserID != nil {
					if err := notify(ctx, tx, *o.UserID, notification{Type: "winning_bid", Title: "Penawaranmu dipilih: " + econ[i].Title,
						Body: fmt.Sprintf("%s %s × %s. PO menyusul dari %s.", qtyLabel(ln.Quantity), l.Unit, rupiah(int64(o.PriceIdr)), c.OrgName),
						Href: "/auctions/" + econ[i].ID}); err != nil {
						return err
					}
				}
			}
			changes = append(changes, change{Field: l.Item, After: strings.Join(parts, "; ")})
			if err := settleLotAuction(ctx, tx, econ[i], winners, allOffers[i]); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE org_auctions SET status = 'awarded' WHERE id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE procurement_requests SET status = 'awarded' WHERE id = (SELECT procurement_request_id FROM org_auctions WHERE id = $1) AND status = 'in_auction'`, id); err != nil {
			return err
		}
		if err := c.audit(ctx, tx, "Award auction", "auction", id, a.Code+" "+a.Title, &reason, changes...); err != nil {
			return err
		}
		out, err = loadOrgAuction(ctx, tx, c.OrgID, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.AwardOrgAuction200JSONResponse(out), nil
}

// settleLotAuction marks a lot's economy auction awarded and its bids won/lost, and tells the losing bidders.
func settleLotAuction(ctx context.Context, tx pgx.Tx, r auctionRow, winners map[string]bool, os []lotOffer) error {
	if _, err := tx.Exec(ctx, `UPDATE auctions SET status = 'awarded' WHERE id = $1`, r.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE bids SET status = CASE WHEN bidder_party_id::text = ANY($2) THEN 'won' ELSE 'lost' END WHERE auction_id = $1 AND status <> 'withdrawn'`,
		r.ID, keys(winners)); err != nil {
		return err
	}
	r.Status = "awarded"
	for _, o := range os {
		if o.UserID == nil {
			continue
		}
		if !winners[o.PartyID] {
			if err := notify(ctx, tx, *o.UserID, notification{Type: "auction_ending", Title: r.Title + " selesai", Body: "Penawaranmu belum terpilih kali ini.",
				Href: "/auctions/" + r.ID}); err != nil {
				return err
			}
		}
		if err := emitBidStatus(ctx, tx, r, *o.UserID); err != nil {
			return err
		}
	}
	return emitAuctionState(ctx, tx, r.ID)
}

// poInitials: capitalised words of the org name, first letters, at most 4 ("PT Solusi Kemasan Nusantara" -> PSKN).
func poInitials(name string) string {
	var b strings.Builder
	for _, w := range strings.Fields(name) {
		if r := []rune(w)[0]; unicode.IsUpper(r) && b.Len() < 4 {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "ORG"
	}
	return b.String()
}

func (s *Server) IssueOrgPurchaseOrder(ctx context.Context, req api.IssueOrgPurchaseOrderRequestObject) (api.IssueOrgPurchaseOrderResponseObject, error) {
	var out api.OrgPoIssued
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("auctions", "manage"); err != nil {
			return err
		}
		id, err := lockOrgAuction(ctx, tx, c.OrgID, req.Aid)
		if err != nil {
			return err
		}
		var awardID, objective, category, code, title string
		var hasPO bool
		err = tx.QueryRow(ctx, `
			SELECT aw.id::text, a.objective, a.category_id, a.code, a.title, EXISTS (SELECT 1 FROM purchase_orders po WHERE po.org_award_id = aw.id)
			FROM org_auctions a JOIN org_awards aw ON aw.org_auction_id = a.id WHERE a.id = $1`, id).Scan(&awardID, &objective, &category, &code, &title, &hasPO)
		if errors.Is(err, pgx.ErrNoRows) {
			return conflict("not_awarded", "Tetapkan pemenang dulu")
		}
		if err != nil {
			return err
		}
		if hasPO {
			return conflict("exists", "PO sudah diterbitkan")
		}
		for attempt := 0; ; attempt++ {
			out.PoNumber = fmt.Sprintf("PO-%s-%04d", poInitials(c.OrgName), rand.IntN(10000))
			err := pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
				_, err := sp.Exec(ctx, `INSERT INTO purchase_orders (org_id, org_award_id, po_number, issued_by) VALUES ($1, $2, $3, $4)`,
					c.OrgID, awardID, out.PoNumber, c.sess.UserID)
				return err
			})
			if uniqueViolation(err, "purchase_orders_org_id_po_number_key") && attempt < 20 {
				continue
			}
			if err != nil {
				return err
			}
			break
		}
		party, err := orgParty(ctx, tx, c.OrgID)
		if err != nil {
			return err
		}
		var location string
		if err := tx.QueryRow(ctx, `SELECT location FROM org_profiles WHERE org_id = $1`, c.OrgID).Scan(&location); err != nil {
			return err
		}
		region, err := orgRegion(ctx, tx, c.OrgID)
		if err != nil {
			return err
		}
		type poLine struct {
			ID, Party, Item, Unit string
			UserID, AuctionID     *string
			Qty                   float64
			Price, Reserve        int64
		}
		rows, err := tx.Query(ctx, `
			SELECT wl.id::text, o.party_id::text, lot.item, lot.unit, p.user_id::text, lot.auction_id::text, wl.quantity, wl.price_idr, lot.reserve_price_idr
			FROM org_award_lines wl JOIN org_auction_offers o ON o.id = wl.org_auction_offer_id JOIN org_auction_lots lot ON lot.id = wl.org_auction_lot_id
			JOIN parties p ON p.id = o.party_id WHERE wl.org_award_id = $1 ORDER BY lot.position, wl.position`, awardID)
		if err != nil {
			return err
		}
		lines, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (poLine, error) {
			var l poLine
			return l, r.Scan(&l.ID, &l.Party, &l.Item, &l.Unit, &l.UserID, &l.AuctionID, &l.Qty, &l.Price, &l.Reserve)
		})
		if err != nil {
			return err
		}
		out.TransactionIds = []string{}
		for _, l := range lines {
			t := newTrade{Title: fmt.Sprintf("%s · %s %s", l.Item, qtyLabel(l.Qty), l.Unit), BuyerParty: party, SupplierParty: l.Party, Quantity: l.Qty,
				Unit: l.Unit, UnitPriceIdr: l.Price, AuctionID: l.AuctionID, DeliveryAddress: nonEmpty(location, "-"), Via: "auction", Category: category,
				Region: region, ActorUserID: &c.sess.UserID, BuyerOrgID: &c.OrgID, Item: l.Item,
				// ponytail: the lot's target price stands in for budget and market until awards carry a market reference.
				BudgetUnitIdr: l.Reserve, MarketUnitIdr: l.Reserve}
			if objective == "selling" {
				t.BuyerParty, t.SupplierParty, t.BuyerOrgID, t.SupplierOrgID = l.Party, party, nil, &c.OrgID
			}
			tr, err := createTrade(ctx, tx, t)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE org_award_lines SET trade_id = $2 WHERE id = $1`, l.ID, tr.ID); err != nil {
				return err
			}
			out.TransactionIds = append(out.TransactionIds, tr.ID)
			if l.UserID != nil {
				if err := notify(ctx, tx, *l.UserID, notification{Type: "transaction_update", Title: fmt.Sprintf("%s dari %s", out.PoNumber, c.OrgName),
					Body: t.Title + ". Setujui agreement-nya.", Href: "/app/transactions/" + tr.ID}); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE procurement_requests SET status = 'po_issued' WHERE id = (SELECT procurement_request_id FROM org_auctions WHERE id = $1) AND status = 'awarded'`, id); err != nil {
			return err
		}
		return c.audit(ctx, tx, "Terbitkan "+out.PoNumber, "transaction", id, code+" "+title, nil,
			change{Field: "Transaksi", After: fmt.Sprintf("%d PO", len(lines))})
	})
	if err != nil {
		return nil, err
	}
	return api.IssueOrgPurchaseOrder200JSONResponse(out), nil
}
