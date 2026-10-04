package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

const activeAuctionStatuses = `('scheduled','qualification','live','extended')`

func auctionFindings(ctx context.Context, q dbtx, where string, args ...any) (map[string][]api.Finding, error) {
	out := map[string][]api.Finding{}
	rows, err := q.Query(ctx, `
		SELECT a.id::text, al.id::text, al.source, al.code, al.score, al.title
		FROM fraud_alert_subjects s JOIN fraud_alerts al ON al.id = s.alert_id JOIN auctions a ON a.id = s.subject_id
		WHERE s.subject_type = 'auction' AND al.status <> 'dismissed' AND (`+where+`) ORDER BY al.detected_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var auctionID, id, source, code, title string
		var score float64
		if err := rows.Scan(&auctionID, &id, &source, &code, &score, &title); err != nil {
			return nil, err
		}
		rule, sev := "Rekomendasi sistem "+code, api.FindingSeverityLow
		if source == "manual" {
			rule = "Kasus manual " + code
		}
		switch {
		case score >= 80:
			sev = api.FindingSeverityHigh
		case score >= 60:
			sev = api.FindingSeverityMedium
		}
		out[auctionID] = append(out[auctionID], api.Finding{Id: id, Rule: rule, Severity: sev, Detail: title})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	rules, err := q.Query(ctx, `
		SELECT auction_id::text, (array_agg(bidder ORDER BY seq) FILTER (WHERE run3))[1], (array_agg(jump ORDER BY seq) FILTER (WHERE jump > lim))[1]
		FROM (SELECT b.auction_id, b.seq, `+bidderName+` AS bidder,
		             b.bidder_party_id = lag(b.bidder_party_id) OVER w AND b.bidder_party_id = lag(b.bidder_party_id, 2) OVER w AS run3,
		             abs(b.price_idr - lag(b.price_idr) OVER w) AS jump, 20 * greatest(1, a.min_step_idr) AS lim
		      FROM bids b JOIN auctions a ON a.id = b.auction_id JOIN parties p ON p.id = b.bidder_party_id
		      WHERE b.status <> 'withdrawn' AND NOT (a.type = 'sealed' AND a.status IN `+activeAuctionStatuses+`) AND (`+where+`)
		      WINDOW w AS (PARTITION BY b.auction_id ORDER BY b.seq)) x
		GROUP BY auction_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rules.Close()
	for rules.Next() {
		var id string
		var runBy *string
		var jump *int64
		if err := rules.Scan(&id, &runBy, &jump); err != nil {
			return nil, err
		}
		if runBy != nil {
			out[id] = append(out[id], api.Finding{Id: id + "-run", Rule: "Bid beruntun", Severity: api.FindingSeverityMedium,
				Detail: *runBy + " memasang ≥ 3 bid berturut-turut tanpa ada pesaing di antaranya."})
		}
		if jump != nil {
			out[id] = append(out[id], api.Finding{Id: id + "-jump", Rule: "Lonjakan harga", Severity: api.FindingSeverityLow,
				Detail: fmt.Sprintf("Selisih bid %s > 20× langkah minimum.", rupiah(*jump))})
		}
	}
	return out, rules.Err()
}

const bidderName = `CASE WHEN p.kind = 'user' THEN p.name || ' (akun pengguna)' ELSE p.name END`

func adminAuctions(ctx context.Context, q dbtx, where string, args ...any) ([]api.AdminAuction, error) {
	list, err := loadAuctions(ctx, q, where, args...)
	if err != nil {
		return nil, err
	}
	findings, err := auctionFindings(ctx, q, `a.id IN (SELECT a.id FROM auctions a WHERE `+where+`)`, args...)
	if err != nil {
		return nil, err
	}
	out := make([]api.AdminAuction, len(list))
	for i, a := range list {
		if err := widen(a, &out[i]); err != nil {
			return nil, err
		}
		out[i].Findings = nonNil(findings[a.Id])
	}
	return out, nil
}

func adminMarkets(ctx context.Context, q dbtx, where string, args ...any) ([]api.AdminMarket, error) {
	markets, err := loadMarkets(ctx, q, where, args...)
	if err != nil {
		return nil, err
	}
	out := make([]api.AdminMarket, len(markets))
	index := map[string]int{}
	ids := make([]string, len(markets))
	for i, m := range markets {
		if err := widen(m, &out[i]); err != nil {
			return nil, err
		}
		out[i].Flags, out[i].Reports = []api.MarketFlag{}, []api.UserReport{}
		index[m.Id], ids[i] = i, m.Id
	}
	rows, err := q.Query(ctx, `
		SELECT m.id::text, m.reviewed_at, (SELECT count(*) FROM disputes d WHERE d.market_id = m.id AND d.status <> 'resolved')
		FROM markets m WHERE m.id::text = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var n int64
		var m api.AdminMarket
		if err := rows.Scan(&id, &m.ReviewedAt, &n); err != nil {
			return nil, err
		}
		out[index[id]].ReviewedAt, out[index[id]].Disputes = m.ReviewedAt, int(n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `
		SELECT f.market_id::text, f.id::text, coalesce(u.name || ' (Admin)', 'Sistem'), f.label, f.created_at
		FROM market_flags f LEFT JOIN users u ON u.id = f.flagged_by WHERE f.market_id::text = ANY($1) ORDER BY f.created_at DESC`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var f api.MarketFlag
		if err := rows.Scan(&id, &f.Id, &f.By, &f.Label, &f.At); err != nil {
			return nil, err
		}
		out[index[id]].Flags = append(out[index[id]].Flags, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `
		SELECT r.market_id::text, r.id::text, p.name, r.reason, r.created_at, r.context
		FROM market_reports r JOIN parties p ON p.id = r.reporter_party_id WHERE r.market_id::text = ANY($1) ORDER BY r.created_at DESC`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var r api.UserReport
		if err := rows.Scan(&id, &r.Id, &r.Reporter, &r.Reason, &r.At, &r.Context); err != nil {
			return nil, err
		}
		out[index[id]].Reports = append(out[index[id]].Reports, r)
	}
	return out, rows.Err()
}

func (s *Server) ListAdminMarkets(ctx context.Context, _ api.ListAdminMarketsRequestObject) (api.ListAdminMarketsResponseObject, error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	out, err := adminMarkets(ctx, s.DB.Reader(), `true ORDER BY m.created_at DESC`)
	if err != nil {
		return nil, err
	}
	return api.ListAdminMarkets200JSONResponse(out), nil
}

func (s *Server) GetAdminMarket(ctx context.Context, req api.GetAdminMarketRequestObject) (api.GetAdminMarketResponseObject, error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	q := s.DB.Reader()
	ms, err := adminMarkets(ctx, q, `m.id::text = $1`, req.Id)
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, notFound("Market tidak ditemukan")
	}
	var d api.AdminMarketDetail
	if err := widen(ms[0], &d); err != nil {
		return nil, err
	}
	if d.Auctions, err = adminAuctions(ctx, q, `a.market_id = $1 ORDER BY a.starts_at DESC`, d.Id); err != nil {
		return nil, err
	}
	if d.Audit, err = loadAudit(ctx, q, `entity_type = 'market' AND entity_id = $1`, 200, d.Id); err != nil {
		return nil, err
	}
	return api.GetAdminMarket200JSONResponse(d), nil
}

type marketRef struct{ ID, Code, Name, Status string }

func lockMarket(ctx context.Context, tx pgx.Tx, id string) (marketRef, error) {
	var m marketRef
	err := tx.QueryRow(ctx, `SELECT id, code, name, status FROM markets WHERE id::text = $1 FOR UPDATE`, id).Scan(&m.ID, &m.Code, &m.Name, &m.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, notFound("Market tidak ditemukan")
	}
	return m, err
}

func (m marketRef) label() string { return m.Code + " · " + m.Name }

func setMarketStatus(ctx context.Context, tx pgx.Tx, a adminActor, m marketRef, to, reason, via string) error {
	if _, err := tx.Exec(ctx, `UPDATE markets SET status = $2 WHERE id = $1`, m.ID, to); err != nil {
		return err
	}
	action := map[string]string{"suspended": "Suspend market", "active": "Pulihkan market"}[to]
	if via != "" {
		action += " (eskalasi " + via + ")"
	}
	return a.record(ctx, tx, audit{Action: action, EntityType: "market", EntityID: m.ID, EntityLabel: m.label(), MarketID: &m.ID,
		Reason: &reason, Changes: []change{diff("status", m.Status, to)}})
}

func (s *Server) ActOnAdminMarket(ctx context.Context, req api.ActOnAdminMarketRequestObject) (api.ActOnAdminMarketResponseObject, error) {
	a, err := s.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		m, err := lockMarket(ctx, tx, req.Id)
		if err != nil {
			return err
		}
		if req.Body.Action == api.MarketActionReview {
			if _, err := tx.Exec(ctx, `UPDATE markets SET reviewed_at = now() WHERE id = $1`, m.ID); err != nil {
				return err
			}
			return a.record(ctx, tx, audit{Action: "Review market", EntityType: "market", EntityID: m.ID, EntityLabel: m.label(), MarketID: &m.ID,
				Reason: optReason(req.Body.Reason)})
		}
		reason, err := needReason(req.Body.Reason)
		if err != nil {
			return err
		}
		switch req.Body.Action {
		case api.MarketActionFlag:
			if _, err := tx.Exec(ctx, `INSERT INTO market_flags (market_id, label, flagged_by) VALUES ($1, $2, $3)`, m.ID, reason, a.ID); err != nil {
				return err
			}
			return a.record(ctx, tx, audit{Action: "Flag market", EntityType: "market", EntityID: m.ID, EntityLabel: m.label(), MarketID: &m.ID, Reason: &reason})
		case api.MarketActionSuspend:
			if m.Status == "suspended" {
				return conflict("no_change", "Market sudah disuspend")
			}
			return setMarketStatus(ctx, tx, a, m, "suspended", reason, "")
		default:
			if m.Status != "suspended" {
				return conflict("no_change", "Market tidak sedang disuspend")
			}
			return setMarketStatus(ctx, tx, a, m, "active", reason, "")
		}
	})
	if err != nil {
		return nil, err
	}
	return api.ActOnAdminMarket200JSONResponse{Ok: api.AdminOkOkTrue}, nil
}

func (s *Server) ListAdminAuctions(ctx context.Context, _ api.ListAdminAuctionsRequestObject) (api.ListAdminAuctionsResponseObject, error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	out, err := adminAuctions(ctx, s.DB.Reader(), `true ORDER BY a.starts_at DESC`)
	if err != nil {
		return nil, err
	}
	return api.ListAdminAuctions200JSONResponse(out), nil
}

func isActiveAuction(status string) bool {
	switch status {
	case "scheduled", "qualification", "live", "extended":
		return true
	}
	return false
}

func (s *Server) GetAdminAuction(ctx context.Context, req api.GetAdminAuctionRequestObject) (api.GetAdminAuctionResponseObject, error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	q := s.DB.Reader()
	r, err := loadAuction(ctx, q, req.Id, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound("Auction tidak ditemukan")
	}
	if err != nil {
		return nil, err
	}
	var d api.AdminAuctionDetail
	if err := widen(r.api(), &d); err != nil {
		return nil, err
	}
	d.MinStepIdr = int(r.MinStep)
	findings, err := auctionFindings(ctx, q, `a.id = $1`, r.ID)
	if err != nil {
		return nil, err
	}
	d.Findings = nonNil(findings[r.ID])
	if !(r.Type == "sealed" && isActiveAuction(r.Status)) {
		rows, err := q.Query(ctx, `
			SELECT b.id::text, `+bidderName+`, b.bidder_no, b.price_idr, b.created_at
			FROM bids b JOIN parties p ON p.id = b.bidder_party_id
			WHERE b.auction_id = $1 AND b.status <> 'withdrawn' ORDER BY b.seq DESC LIMIT 500`, r.ID)
		if err != nil {
			return nil, err
		}
		bids := []api.AdminBid{}
		for rows.Next() {
			var b api.AdminBid
			var no int32
			var price int64
			if err := rows.Scan(&b.Id, &b.Bidder, &no, &price, &b.At); err != nil {
				return nil, err
			}
			b.Masked, b.PriceIdr = r.bidderLabel(no), int(price)
			bids = append(bids, b)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		d.Bids = &bids
	}
	err = q.QueryRow(ctx, `
		SELECT al.id::text FROM fraud_alerts al JOIN fraud_alert_subjects s ON s.alert_id = al.id
		WHERE s.subject_type = 'auction' AND s.subject_id = $1 AND al.status = 'investigating' ORDER BY al.detected_at DESC LIMIT 1`, r.ID).Scan(&d.CaseId)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if d.Audit, err = loadAudit(ctx, q, `entity_type = 'auction' AND entity_id = $1`, 200, r.ID); err != nil {
		return nil, err
	}
	return api.GetAdminAuction200JSONResponse(d), nil
}

func (r auctionRow) label() string { return r.Code + " · " + r.Title }

func freezeAuction(ctx context.Context, tx pgx.Tx, a adminActor, r auctionRow, reason, via string) error {
	if _, err := tx.Exec(ctx, `UPDATE auctions SET frozen_from = status, status = 'frozen' WHERE id = $1`, r.ID); err != nil {
		return err
	}
	if err := emitAuctionState(ctx, tx, r.ID); err != nil {
		return err
	}
	action := "Freeze auction"
	if via != "" {
		action += " (eskalasi " + via + ")"
	}
	if err := a.record(ctx, tx, audit{Action: action, EntityType: "auction", EntityID: r.ID, EntityLabel: r.label(), MarketID: r.MarketID,
		Reason: &reason, Changes: []change{diff("status", r.Status, "frozen")}}); err != nil {
		return err
	}
	if r.OwnerUserID == nil {
		return nil
	}
	return notify(ctx, tx, *r.OwnerUserID, notification{Type: "auction_ending", Title: r.Title + " dibekukan Admin",
		Body: "Bid dan penetapan pemenang ditahan selama pemeriksaan.", Href: "/auctions/" + r.ID})
}

func lockAuction(ctx context.Context, tx pgx.Tx, id string) (auctionRow, error) {
	r, err := loadAuction(ctx, tx, id, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, notFound("Auction tidak ditemukan")
	}
	return r, err
}

func (s *Server) ActOnAdminAuction(ctx context.Context, req api.ActOnAdminAuctionRequestObject) (api.ActOnAdminAuctionResponseObject, error) {
	a, err := s.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	var out api.ActOnAdminAuction200JSONResponse
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		r, err := lockAuction(ctx, tx, req.Id)
		if err != nil {
			return err
		}
		reason, err := needReason(req.Body.Reason)
		if err != nil {
			return err
		}
		switch req.Body.Action {
		case api.AuctionActionFreeze:
			if !isActiveAuction(r.Status) {
				return conflict("not_active", "Hanya auction yang belum ditutup yang bisa dibekukan")
			}
			if err := freezeAuction(ctx, tx, a, r, reason, ""); err != nil {
				return err
			}
		case api.AuctionActionUnfreeze:
			if r.Status != "frozen" {
				return conflict("not_frozen", "Auction tidak sedang dibekukan")
			}

			var to string
			if err := tx.QueryRow(ctx, `UPDATE auctions SET status = frozen_from, frozen_from = NULL WHERE id = $1 RETURNING status`, r.ID).Scan(&to); err != nil {
				return err
			}
			if err := emitAuctionState(ctx, tx, r.ID); err != nil {
				return err
			}
			if err := a.record(ctx, tx, audit{Action: "Cabut freeze auction", EntityType: "auction", EntityID: r.ID, EntityLabel: r.label(),
				MarketID: r.MarketID, Reason: &reason, Changes: []change{diff("status", "frozen", to)}}); err != nil {
				return err
			}
		default:
			id, err := openAuctionCase(ctx, tx, a, r, req.Body.Type, reason)
			if err != nil {
				return err
			}
			return out.FromAdminCaseOpened(api.AdminCaseOpened{CaseId: id})
		}
		return out.FromAdminOk(api.AdminOk{Ok: api.AdminOkOkTrue})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func openAuctionCase(ctx context.Context, tx pgx.Tx, a adminActor, r auctionRow, typ *api.AlertType, reason string) (string, error) {
	t := api.AlertTypeBidManipulation
	if typ != nil {
		t = *typ
	}
	findings, err := auctionFindings(ctx, tx, `a.id = $1`, r.ID)
	if err != nil {
		return "", err
	}
	evidence := []string{}
	for _, f := range findings[r.ID] {
		evidence = append(evidence, f.Rule+": "+f.Detail)
	}
	var id, code, title string
	if err := tx.QueryRow(ctx, `
		INSERT INTO fraud_alerts (type, source, title, status, evidence, investigation_opened_at, investigation_opened_by)
		VALUES ($1, 'manual', $2, 'investigating', $3, now(), $4) RETURNING id, code, title`,
		t, fmt.Sprintf("Pemeriksaan %s: %s", r.Code, r.Title), evidence, a.ID).Scan(&id, &code, &title); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO fraud_alert_subjects (alert_id, subject_type, subject_id, label) VALUES ($1, 'auction', $2, $3)`,
		id, r.ID, r.label()); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO fraud_alert_notes (alert_id, text, created_by) VALUES ($1, $2, $3)`, id, reason, a.ID); err != nil {
		return "", err
	}
	return id, a.record(ctx, tx, audit{Action: "Buka kasus " + code, EntityType: "alert", EntityID: id, EntityLabel: code + " · " + title,
		MarketID: r.MarketID, Reason: &reason})
}

func loadAlerts(ctx context.Context, q dbtx, where string, args ...any) ([]api.FraudAlert, error) {
	rows, err := q.Query(ctx, `
		SELECT json_build_object(
		  'id', a.id, 'code', a.code, 'type', a.type, 'source', a.source, 'title', a.title, 'score', a.score, 'confidence', a.confidence,
		  'status', a.status, 'detectedAt', a.detected_at, 'evidence', a.evidence, 'graph', a.graph,
		  'subjects', coalesce((SELECT json_agg(json_build_object('type', s.subject_type, 'id', s.subject_id, 'label', s.label) ORDER BY s.label)
		                        FROM fraud_alert_subjects s WHERE s.alert_id = a.id), '[]'),
		  'investigation', CASE WHEN a.investigation_opened_at IS NOT NULL THEN json_build_object(
		     'openedAt', a.investigation_opened_at, 'by', ou.name || ' (Admin)',
		     'notes', coalesce((SELECT json_agg(json_build_object('at', n.created_at, 'by', nu.name || ' (Admin)', 'text', n.text) ORDER BY n.created_at)
		                        FROM fraud_alert_notes n JOIN users nu ON nu.id = n.created_by WHERE n.alert_id = a.id), '[]')) END,
		  'resolution', CASE WHEN a.resolved_at IS NOT NULL THEN json_build_object(
		     'at', a.resolved_at, 'by', ru.name || ' (Admin)', 'outcome', a.resolution_outcome, 'reason', a.resolution_reason) END)
		FROM fraud_alerts a LEFT JOIN users ou ON ou.id = a.investigation_opened_by LEFT JOIN users ru ON ru.id = a.resolved_by
		WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.FraudAlert{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var al api.FraudAlert
		if err := json.Unmarshal(raw, &al); err != nil {
			return nil, err
		}
		out = append(out, al)
	}
	return out, rows.Err()
}

func loadAlert(ctx context.Context, q dbtx, id string) (api.FraudAlert, error) {
	as, err := loadAlerts(ctx, q, `a.id::text = $1`, id)
	if err != nil {
		return api.FraudAlert{}, err
	}
	if len(as) == 0 {
		return api.FraudAlert{}, notFound("Alert tidak ditemukan")
	}
	return as[0], nil
}

func (s *Server) ListAdminAlerts(ctx context.Context, _ api.ListAdminAlertsRequestObject) (api.ListAdminAlertsResponseObject, error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	out, err := loadAlerts(ctx, s.DB.Reader(), `true ORDER BY a.detected_at DESC`)
	if err != nil {
		return nil, err
	}
	return api.ListAdminAlerts200JSONResponse(out), nil
}

func (s *Server) GetAdminAlert(ctx context.Context, req api.GetAdminAlertRequestObject) (api.GetAdminAlertResponseObject, error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	q := s.DB.Reader()
	al, err := loadAlert(ctx, q, req.Id)
	if err != nil {
		return nil, err
	}
	var d api.AdminAlertDetail
	if err := widen(al, &d); err != nil {
		return nil, err
	}

	if d.Audit, err = loadAudit(ctx, q, `(entity_type = 'alert' AND entity_id = $1) OR action LIKE '%(eskalasi ' || $2 || ')'`, 200, al.Id, al.Code); err != nil {
		return nil, err
	}
	return api.GetAdminAlert200JSONResponse(d), nil
}

var alertLabel = map[string]string{
	"bid_manipulation": "Bid manipulation", "collusion": "Collusion pattern", "fake_accounts": "Fake accounts", "abnormal_bidding": "Abnormal bidding",
	"wash_trading": "Wash trading", "price_manipulation": "Sudden price manipulation", "transaction_network": "Suspicious transaction network",
}

var escalations = map[string]map[api.EscalationAction]string{
	"user":    {api.EscalationActionSuspendUser: "Suspend", api.EscalationActionRestrictUser: "Batasi"},
	"auction": {api.EscalationActionFreezeAuction: "Freeze"},
	"market":  {api.EscalationActionSuspendMarket: "Suspend"},
}

func (s *Server) ActOnAdminAlert(ctx context.Context, req api.ActOnAdminAlertRequestObject) (api.ActOnAdminAlertResponseObject, error) {
	a, err := s.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	action, err := req.Body.Discriminator()
	if err != nil {
		return nil, err
	}
	var out api.FraudAlert
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var id, code, typ, title, status string
		err := tx.QueryRow(ctx, `SELECT id, code, type, title, status FROM fraud_alerts WHERE id::text = $1 FOR UPDATE`, req.Id).
			Scan(&id, &code, &typ, &title, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound("Alert tidak ditemukan")
		}
		if err != nil {
			return err
		}
		entity := audit{EntityType: "alert", EntityID: id, EntityLabel: code + " · " + title}
		record := func(act string, reason *string, ch ...change) error {
			e := entity
			e.Action, e.Reason, e.Changes = act, reason, ch
			return a.record(ctx, tx, e)
		}
		if action == "investigate" {
			if status != "new" {
				return conflict("invalid_state", "Alert ini sudah ditangani")
			}
			if _, err := tx.Exec(ctx, `UPDATE fraud_alerts SET status = 'investigating', investigation_opened_at = now(), investigation_opened_by = $2 WHERE id = $1`,
				id, a.ID); err != nil {
				return err
			}
			return record(fmt.Sprintf("Buka investigasi %s (%s)", code, alertLabel[typ]), nil, diff("status alert", "new", "investigating"))
		}
		if status != "investigating" {
			return conflict("no_case", "Buka investigasi dulu sebelum mengambil tindakan")
		}
		if action == "note" {
			in, err := req.Body.AsAlertActionNote()
			if err != nil {
				return err
			}
			text := strings.TrimSpace(in.Text)
			if text == "" {
				return &Error{Status: 422, Code: "validation", Message: "Catatan kosong", Fields: map[string]string{"text": "Tulis catatan"}}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO fraud_alert_notes (alert_id, text, created_by) VALUES ($1, $2, $3)`, id, text, a.ID); err != nil {
				return err
			}
			return record(fmt.Sprintf("Catatan %s: %s", code, text), nil)
		}
		var rawReason string
		if action == "escalate" {
			in, err := req.Body.AsAlertActionEscalate()
			if err != nil {
				return err
			}
			rawReason = in.Reason
			reason, err := needReason(&rawReason)
			if err != nil {
				return err
			}
			var subjectType, subjectID, subjectLabel string
			err = tx.QueryRow(ctx, `SELECT subject_type, subject_id, label FROM fraud_alert_subjects WHERE alert_id = $1 AND subject_id::text = $2`,
				id, in.SubjectId).Scan(&subjectType, &subjectID, &subjectLabel)
			verb, ok := escalations[subjectType][in.Escalation]
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !ok) {
				return &Error{Status: 422, Code: "validation", Message: "Pilih subjek dan tindakan yang sesuai",
					Fields: map[string]string{"subjectId": "Tindakan tidak cocok untuk subjek ini"}}
			}
			if err != nil {
				return err
			}
			if err := escalate(ctx, tx, a, subjectType, subjectID, in.Escalation, reason, code); err != nil {
				return err
			}
			outcome := verb + " " + subjectLabel
			if _, err := tx.Exec(ctx, `
				UPDATE fraud_alerts SET status = 'escalated', resolved_at = now(), resolved_by = $2, resolution_outcome = $3, resolution_reason = $4,
				  escalation = $5, escalated_subject_id = $6 WHERE id = $1`, id, a.ID, outcome, reason, in.Escalation, subjectID); err != nil {
				return err
			}
			return record(fmt.Sprintf("Eskalasi %s: %s", code, outcome), &reason, diff("status alert", "investigating", "escalated"))
		}
		in, err := req.Body.AsAlertActionClose()
		if err != nil {
			return err
		}
		reason, err := needReason(&in.Reason)
		if err != nil {
			return err
		}
		to, outcome, verb := "closed", "Ditutup tanpa tindakan", "Tutup"
		if action == "dismiss" {
			to, outcome, verb = "dismissed", "Ditolak: bukan pelanggaran", "Dismiss"
		}
		if _, err := tx.Exec(ctx, `UPDATE fraud_alerts SET status = $2, resolved_at = now(), resolved_by = $3, resolution_outcome = $4, resolution_reason = $5 WHERE id = $1`,
			id, to, a.ID, outcome, reason); err != nil {
			return err
		}
		return record(verb+" "+code, &reason, diff("status alert", "investigating", to))
	})
	if err != nil {
		return nil, err
	}
	if out, err = loadAlert(ctx, s.DB.Primary(), req.Id); err != nil {
		return nil, err
	}
	return api.ActOnAdminAlert200JSONResponse(out), nil
}

func escalate(ctx context.Context, tx pgx.Tx, a adminActor, subjectType, subjectID string, e api.EscalationAction, reason, code string) error {
	switch subjectType {
	case "user":
		u, err := lockAdminUser(ctx, tx, subjectID)
		if err != nil {
			return err
		}
		to := "restricted"
		if e == api.EscalationActionSuspendUser {
			to = "suspended"
		}
		return setUserStatus(ctx, tx, a, u, to, reason, code)
	case "auction":
		r, err := lockAuction(ctx, tx, subjectID)
		if err != nil {
			return err
		}
		if !isActiveAuction(r.Status) {
			return conflict("not_active", "Auction ini sudah tidak aktif; tidak bisa dibekukan")
		}
		return freezeAuction(ctx, tx, a, r, reason, code)
	default:
		m, err := lockMarket(ctx, tx, subjectID)
		if err != nil {
			return err
		}
		return setMarketStatus(ctx, tx, a, m, "suspended", reason, code)
	}
}
