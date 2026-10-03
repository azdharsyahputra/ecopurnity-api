package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Market maker workspace (/mm, PRD §10). Access: the market_maker capability (403 otherwise); market endpoints also need
// the caller to operate the market — a market_operators row, or active membership of the maker's org — else 404.
// Every mutation is audited; the caller's actions also go out live as `mm.activity` frames on user:{id}.

var errNotMaker = &Error{Status: http.StatusForbidden, Code: "forbidden", Message: "Butuh capability Market Maker"}

func (s *Server) requireMaker(ctx context.Context) (*session, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	var ok bool
	if err := s.DB.Primary().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_capabilities WHERE user_id = $1 AND capability = 'market_maker')`,
		sess.UserID).Scan(&ok); err != nil {
		return nil, err
	}
	if !ok {
		return nil, errNotMaker
	}
	return sess, nil
}

// operates is a WHERE fragment over markets m: the user in placeholder $n operates m.
func operates(n int) string {
	return fmt.Sprintf(`(EXISTS (SELECT 1 FROM market_operators mo WHERE mo.market_id = m.id AND mo.user_id = $%[1]d)
		OR EXISTS (SELECT 1 FROM parties mk JOIN org_members om ON om.org_id = mk.org_id
		           WHERE mk.id = m.maker_party_id AND om.user_id = $%[1]d AND om.status = 'active'))`, n)
}

type mmMarket struct {
	ID, Code, Name, Status, Unit, Category, Region, Mechanism string
	PriceMax                                                  int64
	Demand, Supply                                            float64
}

// operatedMarket loads a market the user operates (404 otherwise), optionally locked for an update.
func operatedMarket(ctx context.Context, q dbtx, userID, id string, lock bool) (mmMarket, error) {
	sql := `SELECT m.id, m.code, m.name, m.status, m.unit, m.category_id, m.region, m.mechanism, m.price_max_idr,
		       m.demand_value, m.supply_value
		FROM markets m WHERE m.id::text = $1 AND ` + operates(2)
	if lock {
		sql += ` FOR UPDATE OF m`
	}
	var m mmMarket
	err := q.QueryRow(ctx, sql, id, userID).Scan(&m.ID, &m.Code, &m.Name, &m.Status, &m.Unit, &m.Category, &m.Region, &m.Mechanism, &m.PriceMax, &m.Demand, &m.Supply)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, errMarketNotFound
	}
	return m, err
}

func mmActor(sess *session) string { return sess.Name + " (Market Maker)" }

// mmEventType maps an audited market maker action to the ActivityEvent type of its feed item.
func mmEventType(entityType, action string) string {
	switch {
	case entityType == "opportunity":
		return "opportunity_detected"
	case entityType == "auction":
		return "auction_closed"
	case strings.HasPrefix(action, "Buka round"):
		return "auction_started"
	}
	return "market_formed"
}

// mmAudit writes the audit entry of a market maker action and pushes it as the actor's `mm.activity` feed item (the
// overview reads the same entries back, titled "<action> · <entity>"). publicTitle != "" also puts it on the public
// activity feed (outbox topic `activity` for ClickHouse, frame on public:activity).
func mmAudit(ctx context.Context, q dbtx, sess *session, a audit, publicTitle string, amountIdr *int64) error {
	a.ActorUserID, a.ActorLabel = &sess.UserID, mmActor(sess)
	if err := writeAudit(ctx, q, a); err != nil {
		return err
	}
	var id int64
	if err := q.QueryRow(ctx, `SELECT currval(pg_get_serial_sequence('audit_log', 'id'))`).Scan(&id); err != nil {
		return err
	}
	at := time.Now().UTC()
	ev := map[string]any{"id": fmt.Sprintf("aud-%d", id), "type": mmEventType(a.EntityType, a.Action), "title": a.Action + " · " + a.EntityLabel, "at": at}
	if amountIdr != nil {
		ev["amountIdr"] = *amountIdr
	}
	if err := emitFrame(ctx, q, "user:"+sess.UserID, "mm.activity", nil, ev); err != nil {
		return err
	}
	if publicTitle == "" {
		return nil
	}
	actID := fmt.Sprintf("act-aud-%d", id)
	pub := map[string]any{"id": actID, "type": ev["type"], "title": publicTitle, "at": at}
	if amountIdr != nil {
		pub["amountIdr"] = *amountIdr
	}
	fact, _ := json.Marshal(map[string]any{"type": ev["type"], "title": publicTitle, "amountIdr": amountIdr, "marketId": a.MarketID})
	if err := emit(ctx, q, "activity", actID, fact); err != nil {
		return err
	}
	return emitFrame(ctx, q, "public:activity", "activity.created", nil, pub)
}

// notifyMarket notifies the platform accounts in a market: active participants plus users who joined it.
func notifyMarket(ctx context.Context, q dbtx, marketID string, n notification) error {
	rows, err := q.Query(ctx, `
		SELECT p.user_id::text FROM market_participants mp JOIN parties p ON p.id = mp.party_id
		WHERE mp.market_id = $1 AND mp.status = 'active' AND p.user_id IS NOT NULL
		UNION
		SELECT user_id::text FROM watchlist WHERE market_id = $1 AND joined_at IS NOT NULL`, marketID)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := notify(ctx, q, id, n); err != nil {
			return err
		}
	}
	return nil
}

// ── Alerts ───────────────────────────────────────────────────────

type marketQueues struct{ pending, disputes int }

// queues counts pending participants and open disputes (an escalated one follows its admin case) per market.
func queues(ctx context.Context, q dbtx, ids []string) (map[string]marketQueues, error) {
	rows, err := q.Query(ctx, `
		SELECT m.id::text,
		       (SELECT count(*) FROM market_participants x WHERE x.market_id = m.id AND x.status = 'pending'),
		       (SELECT count(*) FROM market_disputes md LEFT JOIN disputes d ON d.id = md.escalated_to
		        WHERE md.market_id = m.id AND coalesce(d.status, md.status) <> 'resolved')
		FROM markets m WHERE m.id::text = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]marketQueues{}
	for rows.Next() {
		var id string
		var c marketQueues
		if err := rows.Scan(&id, &c.pending, &c.disputes); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

func mmAlerts(m api.Market, c marketQueues) []api.MmAlert {
	out := []api.MmAlert{}
	if m.Status == "closed" {
		return out
	}
	// lowLiquidity (domain/mm.ts): shared by the overview and detail views.
	if m.Suppliers < 8 || float64(m.Buyers)/math.Max(1, float64(m.Suppliers)) > 10 {
		out = append(out, api.MmAlert{Kind: "low_liquidity", Label: fmt.Sprintf("Likuiditas rendah (%d:%d)", m.Buyers, m.Suppliers)})
	}
	if c.disputes > 0 {
		out = append(out, api.MmAlert{Kind: "disputes", Label: fmt.Sprintf("%d dispute terbuka", c.disputes)})
	}
	if c.pending > 0 {
		out = append(out, api.MmAlert{Kind: "approvals", Label: fmt.Sprintf("%d menunggu approval", c.pending)})
	}
	return out
}

// ── Overview ─────────────────────────────────────────────────────

func (s *Server) GetMmOverview(ctx context.Context, _ api.GetMmOverviewRequestObject) (api.GetMmOverviewResponseObject, error) {
	sess, err := s.requireMaker(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Primary()
	ms, err := loadMarkets(ctx, q, operates(1)+marketsOrder, sess.UserID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.Id
	}
	qs, err := queues(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	out := api.GetMmOverview200JSONResponse{Markets: []api.MmMarketRow{}, Events: []api.ActivityEvent{}}
	out.LiveAuctions = []struct {
		Id    string `json:"id"`
		Title string `json:"title"`
	}{}
	for _, m := range ms {
		var row api.MmMarketRow
		if err := widen(m, &row); err != nil {
			return nil, err
		}
		row.LiveRounds, row.Alerts = m.ActiveAuctions, mmAlerts(m, qs[m.Id])
		out.Markets = append(out.Markets, row)
		if m.Status == "active" {
			out.Stats.ActiveMarkets++
		}
		out.Stats.Participants += m.Buyers + m.Suppliers
		out.Stats.ActiveAuctions += m.ActiveAuctions
		out.Stats.VolumeIdr += m.Volume30dIdr
	}

	// Feed: the latest two bids of each live round (public prices only) and the caller's own market maker actions.
	rows, err := q.Query(ctx, auctionSelect+` WHERE a.market_id::text = ANY($1) AND a.status IN ('live','extended') ORDER BY a.ends_at, a.id`, ids)
	if err != nil {
		return nil, err
	}
	var live []auctionRow
	for rows.Next() {
		r, err := scanAuction(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		live = append(live, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range live {
		out.LiveAuctions = append(out.LiveAuctions, struct {
			Id    string `json:"id"`
			Title string `json:"title"`
		}{r.ID, r.Title})
		if r.Visibility != "full" {
			continue
		}
		bids, err := q.Query(ctx, `SELECT id, bidder_no, price_idr, created_at FROM bids WHERE auction_id = $1 AND status <> 'withdrawn' ORDER BY seq DESC LIMIT 2`, r.ID)
		if err != nil {
			return nil, err
		}
		for bids.Next() {
			var id string
			var no int32
			var price int64
			var at time.Time
			if err := bids.Scan(&id, &no, &price, &at); err != nil {
				bids.Close()
				return nil, err
			}
			out.Events = append(out.Events, api.ActivityEvent{Id: "bid-" + id, Type: "bid_placed", Title: r.bidderLabel(no) + " bid di " + r.Title,
				AmountIdr: ptr(int(math.Round(float64(price) * r.Quantity))), At: at})
		}
		bids.Close()
		if err := bids.Err(); err != nil {
			return nil, err
		}
	}
	// ponytail: filtered scan of the newest audit rows; add an (actor_user_id, at) index when the log gets big.
	arows, err := q.Query(ctx, `
		SELECT id, entity_type, action, entity_label, at FROM audit_log
		WHERE actor_user_id = $1 AND actor_label LIKE '%(Market Maker)' ORDER BY at DESC, id DESC LIMIT 15`, sess.UserID)
	if err != nil {
		return nil, err
	}
	for arows.Next() {
		var id int64
		var entity, action, label string
		var at time.Time
		if err := arows.Scan(&id, &entity, &action, &label, &at); err != nil {
			arows.Close()
			return nil, err
		}
		out.Events = append(out.Events, api.ActivityEvent{Id: fmt.Sprintf("aud-%d", id), Type: api.ActivityType(mmEventType(entity, action)),
			Title: action + " · " + label, At: at})
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out.Events, func(i, j int) bool { return out.Events[i].At.After(out.Events[j].At) })
	if len(out.Events) > 15 {
		out.Events = out.Events[:15]
	}
	if eff, _ := s.mmWeeks(ctx, ids); len(eff) > 0 {
		out.Stats.MatchedDemand = eff[len(eff)-1].Matched
	}
	return out, nil
}

// ── Pipeline ─────────────────────────────────────────────────────

func (s *Server) ListMmPipeline(ctx context.Context, _ api.ListMmPipelineRequestObject) (api.ListMmPipelineResponseObject, error) {
	if _, err := s.requireMaker(ctx); err != nil {
		return nil, err
	}
	q := s.DB.Primary()
	os, err := loadOpportunities(ctx, q, `true ORDER BY o.detected_at DESC, o.id`)
	if err != nil {
		return nil, err
	}
	type card struct {
		reason, stage  string
		dismiss, mktID *string
	}
	rows, err := q.Query(ctx, `
		SELECT o.id::text, o.mechanism_reason, coalesce(p.stage, CASE o.status WHEN 'closed' THEN 'dismissed' ELSE o.status END),
		       p.dismiss_reason, coalesce(p.market_id, (SELECT mk.id FROM markets mk WHERE mk.opportunity_id = o.id ORDER BY mk.created_at LIMIT 1))::text
		FROM opportunities o LEFT JOIN mm_pipeline p ON p.opportunity_id = o.id`)
	if err != nil {
		return nil, err
	}
	cards := map[string]card{}
	for rows.Next() {
		var id string
		var c card
		if err := rows.Scan(&id, &c.reason, &c.stage, &c.dismiss, &c.mktID); err != nil {
			rows.Close()
			return nil, err
		}
		cards[id] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(api.ListMmPipeline200JSONResponse, len(os))
	for i, o := range os {
		if err := widen(o, &out[i]); err != nil {
			return nil, err
		}
		c := cards[o.Id]
		out[i].MechanismReason, out[i].Stage, out[i].DismissReason, out[i].MarketId = c.reason, api.PipelineStage(c.stage), c.dismiss, c.mktID
	}
	return out, nil
}

func (s *Server) MoveMmPipelineStage(ctx context.Context, req api.MoveMmPipelineStageRequestObject) (api.MoveMmPipelineStageResponseObject, error) {
	sess, err := s.requireMaker(ctx)
	if err != nil {
		return nil, err
	}
	to := string(req.Body.Stage)
	reason := strings.TrimSpace(deref(req.Body.Reason))
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var id, code, title, from string
		err := tx.QueryRow(ctx, `
			SELECT o.id, o.code, o.title, coalesce(p.stage, CASE o.status WHEN 'closed' THEN 'dismissed' ELSE o.status END)
			FROM opportunities o LEFT JOIN mm_pipeline p ON p.opportunity_id = o.id
			WHERE o.id::text = $1 FOR UPDATE OF o`, req.Id).Scan(&id, &code, &title, &from)
		if errors.Is(err, pgx.ErrNoRows) {
			return &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Opportunity tidak ditemukan"}
		}
		if err != nil {
			return err
		}
		if !canMove(from, to) {
			return &Error{Status: http.StatusConflict, Code: "invalid_transition", Message: "Perpindahan tahap ini tidak diizinkan"}
		}
		var dismiss *string
		if to == "dismissed" {
			if reason == "" {
				return &Error{Status: 422, Code: "validation", Message: "Alasan wajib diisi", Fields: map[string]string{"reason": "Jelaskan kenapa opportunity ini di-dismiss"}}
			}
			dismiss = &reason
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO mm_pipeline (opportunity_id, stage, dismiss_reason, updated_by) VALUES ($1, $2, $3, $4)
			ON CONFLICT (opportunity_id) DO UPDATE SET stage = EXCLUDED.stage, dismiss_reason = EXCLUDED.dismiss_reason, updated_by = EXCLUDED.updated_by`,
			id, to, dismiss, sess.UserID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE opportunities SET status = CASE WHEN $2 = 'evaluating' THEN 'detected' ELSE $2 END WHERE id = $1`, id, to); err != nil {
			return err
		}
		before := pipelineLabel[from]
		return mmAudit(ctx, tx, sess, audit{Action: "Pipeline: " + before + " → " + pipelineLabel[to], EntityType: "opportunity", EntityID: id,
			EntityLabel: code + " · " + title, Reason: dismiss, Changes: []change{{Field: "Tahap", Before: &before, After: pipelineLabel[to]}}}, "", nil)
	})
	if err != nil {
		return nil, err
	}
	return api.MoveMmPipelineStage204Response{}, nil
}

// ── Analytics ────────────────────────────────────────────────────

type mmWeek struct {
	Week                                            string
	Matched, Utilization                            float64
	Participants, Transactions, Connections, Repeat int
}

// mmWeeks is 8 weeks (Mondays, oldest first) of efficiency and growth from ClickHouse, zero-filled. Analytics is never
// the system of record: when it is down the weeks stay zero (the error is logged and returned for the caller to ignore).
func (s *Server) mmWeeks(ctx context.Context, marketIDs []string) ([]mmWeek, error) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	monday := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))
	out := make([]mmWeek, 8)
	idx := map[string]int{}
	for i := range out {
		out[i].Week = monday.AddDate(0, 0, -7*(7-i)).Format(time.DateOnly)
		idx[out[i].Week] = i
	}
	if s.Analytics == nil || len(marketIDs) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	eff, err := s.Analytics.MmEfficiency(ctx, marketIDs)
	if err == nil {
		for _, w := range eff {
			if i, ok := idx[w.Week.Format(time.DateOnly)]; ok {
				out[i].Matched, out[i].Utilization = math.Min(1, w.Matched), math.Min(1, w.Utilization)
			}
		}
		growth, gerr := s.Analytics.MmGrowth(ctx, marketIDs)
		if err = gerr; err == nil {
			for _, w := range growth {
				if i, ok := idx[w.Week.Format(time.DateOnly)]; ok {
					out[i].Participants, out[i].Transactions, out[i].Connections, out[i].Repeat = int(w.Participants), int(w.Transactions), int(w.Connections), int(w.Repeat)
				}
			}
		}
	}
	if err != nil && s.Log != nil {
		s.Log.Warn("mm analytics: clickhouse", "err", err)
	}
	return out, err
}

func (s *Server) GetMmAnalytics(ctx context.Context, req api.GetMmAnalyticsRequestObject) (api.GetMmAnalyticsResponseObject, error) {
	sess, err := s.requireMaker(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Primary()
	where, args := operates(1), []any{sess.UserID}
	if req.Params.Market != nil {
		where, args = where+` AND m.id::text = $2`, append(args, *req.Params.Market)
	}
	ms, err := loadMarkets(ctx, q, where+marketsOrder, args...)
	if err != nil {
		return nil, err
	}
	if req.Params.Market != nil && len(ms) == 0 {
		return nil, errMarketNotFound
	}
	out := api.GetMmAnalytics200JSONResponse{}
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.Id
		out.Liquidity.Buyers += m.Buyers
		out.Liquidity.Suppliers += m.Suppliers
		out.ByMarket = append(out.ByMarket, struct {
			Buyers    int    `json:"buyers"`
			Id        string `json:"id"`
			Name      string `json:"name"`
			Suppliers int    `json:"suppliers"`
		}{m.Buyers, m.Id, m.Name, m.Suppliers})
		results, err := roundResults(ctx, q, `a.market_id = $1`, m.Id)
		if err != nil {
			return nil, err
		}
		out.PriceDiscovery = append(out.PriceDiscovery, struct {
			MarketId string            `json:"marketId"`
			Name     string            `json:"name"`
			Rounds   []api.RoundResult `json:"rounds"`
			Unit     string            `json:"unit"`
		}{m.Id, m.Name, results, m.PriceRange.Unit})
	}
	if out.Liquidity.Suppliers > 0 {
		out.Liquidity.Ratio = float64(out.Liquidity.Buyers) / float64(out.Liquidity.Suppliers)
	}
	// Active orders: bids standing in live rounds plus listings placed in the markets.
	if err := q.QueryRow(ctx, `
		SELECT (SELECT coalesce(sum(bid_count), 0) FROM auctions WHERE market_id::text = ANY($1) AND status IN ('live','extended'))
		     + (SELECT count(*) FROM listings WHERE market_id::text = ANY($1) AND status = 'in_market')`, ids).Scan(&out.Liquidity.ActiveOrders); err != nil {
		return nil, err
	}
	weeks, _ := s.mmWeeks(ctx, ids)
	for _, w := range weeks {
		out.Efficiency = append(out.Efficiency, struct {
			Matched     float64 `json:"matched"`
			Unmatched   float64 `json:"unmatched"`
			Utilization float64 `json:"utilization"`
			Week        string  `json:"week"`
		}{w.Matched, 1 - w.Matched, w.Utilization, w.Week})
		out.Growth = append(out.Growth, struct {
			Connections  int    `json:"connections"`
			Participants int    `json:"participants"`
			Repeat       int    `json:"repeat"`
			Transactions int    `json:"transactions"`
			Week         string `json:"week"`
		}{w.Connections, w.Participants, w.Repeat, w.Transactions, w.Week})
	}
	out.ByMarket = nonNil(out.ByMarket)
	out.PriceDiscovery = nonNil(out.PriceDiscovery)
	return out, nil
}
