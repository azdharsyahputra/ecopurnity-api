package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Public markets (spec tag Public) and the caller's market membership (tag Personal). Draft markets are not
// published: they are absent from every list here and answer 404.

var errMarketNotFound = &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Market tidak ditemukan"}

const marketsOrder = ` ORDER BY m.volume_30d_idr DESC, m.id`

func (s *Server) ListMarkets(ctx context.Context, req api.ListMarketsRequestObject) (api.ListMarketsResponseObject, error) {
	p := req.Params
	page, size := pageParams(p.Page, p.PageSize)
	where := []string{`m.status <> 'draft'`}
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	if p.Q != nil && strings.TrimSpace(*p.Q) != "" {
		ph := arg("%" + likeEscape(strings.TrimSpace(*p.Q)) + "%")
		where = append(where, fmt.Sprintf(`(m.name ILIKE %[1]s OR m.code ILIKE %[1]s OR p.name ILIKE %[1]s)`, ph))
	}
	if p.Category != nil {
		where = append(where, "m.category_id = "+arg(string(*p.Category)))
	}
	if p.Region != nil && strings.TrimSpace(*p.Region) != "" {
		where = append(where, "lower(m.region) = lower("+arg(strings.TrimSpace(*p.Region))+")")
	}
	if p.Status != nil && strings.TrimSpace(*p.Status) != "" {
		var sts []string
		for _, st := range strings.Split(*p.Status, ",") {
			if st = strings.TrimSpace(st); st != "" {
				sts = append(sts, st)
			}
		}
		where = append(where, "m.status = ANY("+arg(sts)+")")
	}
	cond := strings.Join(where, " AND ")
	q := s.DB.Reader()
	out := api.ListMarkets200JSONResponse{Meta: api.PageMeta{Page: page, PageSize: size}}
	if err := q.QueryRow(ctx, `SELECT count(*) FROM markets m JOIN parties p ON p.id = m.maker_party_id WHERE `+cond, args...).
		Scan(&out.Meta.Total); err != nil {
		return nil, err
	}
	data, err := loadMarkets(ctx, q, cond+marketsOrder+" LIMIT "+arg(size)+" OFFSET "+arg((page-1)*size), args...)
	if err != nil {
		return nil, err
	}
	out.Data = data
	return out, nil
}

func (s *Server) GetMarket(ctx context.Context, req api.GetMarketRequestObject) (api.GetMarketResponseObject, error) {
	q := s.DB.Reader()
	ms, err := loadMarkets(ctx, q, `m.id::text = $1 AND m.status <> 'draft'`, req.Id)
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, errMarketNotFound
	}
	var d api.MarketDetail
	if err := widen(ms[0], &d); err != nil {
		return nil, err
	}
	var rules []byte
	// Rules shown are the version governing the current round (latest started round; v1 before any round ran).
	err = q.QueryRow(ctx, `
		WITH r AS (SELECT coalesce(max(round_no), 0) AS n FROM auctions
		           WHERE market_id = $1 AND status NOT IN ('scheduled','qualification','cancelled'))
		SELECT m.description, v.rules
		FROM markets m CROSS JOIN r
		LEFT JOIN LATERAL (
			SELECT rules FROM market_rule_versions v WHERE v.market_id = m.id
			ORDER BY v.effective_from_round <= r.n DESC, CASE WHEN v.effective_from_round <= r.n THEN -v.version ELSE v.version END
			LIMIT 1) v ON true
		WHERE m.id = $1`, d.Id).Scan(&d.Description, &rules)
	if err != nil {
		return nil, err
	}
	d.Rules = []api.LabeledValue{}
	if rules != nil {
		var r marketRules
		if err := json.Unmarshal(rules, &r); err != nil {
			return nil, fmt.Errorf("market %s rules: %w", d.Id, err)
		}
		d.Rules = r.labeled(d.PriceRange.Unit)
	}
	// ponytail: every round of the market, unpaged; add a limit when markets run for years of weekly rounds.
	if d.Auctions, err = loadAuctions(ctx, q, `a.market_id = $1
		ORDER BY CASE a.status WHEN 'live' THEN 0 WHEN 'extended' THEN 0 WHEN 'qualification' THEN 1 WHEN 'scheduled' THEN 2 ELSE 3 END,
		         a.ends_at, a.id`, d.Id); err != nil {
		return nil, err
	}
	d.PriceHistory, d.Activity = s.marketAnalytics(ctx, d.Id)
	return api.GetMarket200JSONResponse(d), nil
}

type pricePoint = struct {
	HighIdr   int    `json:"highIdr"`
	LowIdr    int    `json:"lowIdr"`
	MedianIdr int    `json:"medianIdr"`
	Week      string `json:"week"`
}

// marketAnalytics reads the 12-week price history and the recent activity from ClickHouse. Analytics is never the
// system of record: when it is down or slow the market page shows empty charts instead of failing.
func (s *Server) marketAnalytics(ctx context.Context, marketID string) ([]pricePoint, []api.ActivityEvent) {
	history, activity := []pricePoint{}, []api.ActivityEvent{}
	if s.Analytics == nil {
		return history, activity
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	weeks, err := s.Analytics.PriceHistory(ctx, marketID, 12)
	if err == nil {
		for _, w := range weeks {
			history = append(history, pricePoint{Week: w.Week.Format(time.DateOnly), MedianIdr: int(math.Round(w.Median)), LowIdr: int(w.Low), HighIdr: int(w.High)})
		}
		events, aerr := s.Analytics.MarketActivity(ctx, marketID, 6)
		if err = aerr; err == nil {
			for _, a := range events {
				ev := api.ActivityEvent{Id: a.ID, Type: api.ActivityType(a.Type), Title: a.Title, At: a.At}
				if a.AmountIdr != nil {
					ev.AmountIdr = ptr(int(*a.AmountIdr))
				}
				activity = append(activity, ev)
			}
		}
	}
	if err != nil && s.Log != nil {
		s.Log.Warn("market detail: clickhouse", "market", marketID, "err", err)
	}
	return history, activity
}

// ListMyMarkets: every published market with the caller's state. Read on the primary: the UI refetches right after
// join/leave/watch.
func (s *Server) ListMyMarkets(ctx context.Context, _ api.ListMyMarketsRequestObject) (api.ListMyMarketsResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Primary()
	ms, err := loadMarkets(ctx, q, `m.status <> 'draft'`+marketsOrder)
	if err != nil {
		return nil, err
	}
	type state struct {
		joined   bool
		watch    *int64
		approval *string
		listings int64
	}
	rows, err := q.Query(ctx, `
		SELECT m.id::text, w.joined_at IS NOT NULL, w.watch_price_idr, mp.status,
		       (SELECT count(*) FROM listings l WHERE l.market_id = m.id AND l.owner_party_id = pt.id)
		FROM markets m
		LEFT JOIN parties pt ON pt.user_id = $1
		LEFT JOIN watchlist w ON w.market_id = m.id AND w.user_id = $1
		LEFT JOIN market_participants mp ON mp.market_id = m.id AND mp.party_id = pt.id
		WHERE m.status <> 'draft'`, sess.UserID)
	if err != nil {
		return nil, err
	}
	states := map[string]state{}
	for rows.Next() {
		var id string
		var st state
		if err := rows.Scan(&id, &st.joined, &st.watch, &st.approval, &st.listings); err != nil {
			rows.Close()
			return nil, err
		}
		states[id] = st
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(api.ListMyMarkets200JSONResponse, len(ms))
	for i, m := range ms {
		if err := widen(m, &out[i]); err != nil {
			return nil, err
		}
		st := states[m.Id]
		out[i].Joined, out[i].MyListings = st.joined, int(st.listings)
		if st.watch != nil {
			out[i].WatchPriceIdr = ptr(int(*st.watch))
		}
		if st.joined && st.approval != nil {
			out[i].Approval = ptr(api.MyMarketApproval(*st.approval))
		}
	}
	return out, nil
}

// JoinMarket marks the market joined and queues the caller as a buyer participant (same rules as putting a listing
// into a market). Operators are notified when the request waits for their approval. Idempotent.
func (s *Server) JoinMarket(ctx context.Context, req api.JoinMarketRequestObject) (api.JoinMarketResponseObject, error) {
	sess, err := requireActive(ctx)
	if err != nil {
		return nil, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var marketID, name, status string
		var approval *string
		err := tx.QueryRow(ctx, `
			SELECT m.id, m.name, m.status, st.approval FROM markets m LEFT JOIN market_settings st ON st.market_id = m.id
			WHERE m.id::text = $1 AND m.status <> 'draft'`, req.Id).Scan(&marketID, &name, &status, &approval)
		if errors.Is(err, pgx.ErrNoRows) {
			return errMarketNotFound
		}
		if err != nil {
			return err
		}
		if status != "active" && status != "formation" {
			return &Error{Status: http.StatusConflict, Code: "market_closed", Message: "Market ini sedang tidak menerima peserta baru"}
		}
		party, err := userParty(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		participant := "pending"
		if approval != nil && *approval == "auto" {
			participant = "active"
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO market_participants (market_id, party_id, role, status) VALUES ($1, $2, 'buyer', $3)
			ON CONFLICT (market_id, party_id) DO NOTHING`, marketID, party, participant)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO watchlist (user_id, market_id, joined_at) VALUES ($1, $2, now())
			ON CONFLICT (user_id, market_id) DO UPDATE SET joined_at = coalesce(watchlist.joined_at, now())`, sess.UserID, marketID); err != nil {
			return err
		}
		if tag.RowsAffected() == 0 || participant != "pending" {
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT user_id::text FROM market_operators WHERE market_id = $1 AND user_id <> $2`, marketID, sess.UserID)
		if err != nil {
			return err
		}
		ops, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		n := notification{Type: "new_market", Title: name + ": permintaan bergabung",
			Body: sess.Name + " ingin bergabung sebagai pembeli dan menunggu persetujuan.", Href: "/mm/markets/" + marketID + "?tab=participants"}
		for _, op := range ops {
			if err := notify(ctx, tx, op, n); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return api.JoinMarket204Response{}, nil
}

// LeaveMarket clears the caller's joined flag and price watch, and withdraws a participant request still pending.
// Active, rejected and suspended participant rows are the market maker's record and stay.
func (s *Server) LeaveMarket(ctx context.Context, req api.LeaveMarketRequestObject) (api.LeaveMarketResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		marketID, err := publishedMarket(ctx, tx, req.Id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM watchlist WHERE user_id = $1 AND market_id = $2`, sess.UserID, marketID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			DELETE FROM market_participants mp USING parties p
			WHERE mp.market_id = $2 AND mp.party_id = p.id AND p.user_id = $1 AND mp.status = 'pending'`, sess.UserID, marketID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.LeaveMarket204Response{}, nil
}

// SetMarketWatchPrice sets (or with 0 / no price clears) the median price alert; the joined flag is unchanged.
func (s *Server) SetMarketWatchPrice(ctx context.Context, req api.SetMarketWatchPriceRequestObject) (api.SetMarketWatchPriceResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	var price *int
	if req.Body.PriceIdr != nil && *req.Body.PriceIdr > 0 {
		price = req.Body.PriceIdr
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		marketID, err := publishedMarket(ctx, tx, req.Id)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO watchlist (user_id, market_id, watch_price_idr) VALUES ($1, $2, $3)
			ON CONFLICT (user_id, market_id) DO UPDATE SET watch_price_idr = EXCLUDED.watch_price_idr`, sess.UserID, marketID, price)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.SetMarketWatchPrice204Response{}, nil
}

func publishedMarket(ctx context.Context, q dbtx, id string) (string, error) {
	var marketID string
	err := q.QueryRow(ctx, `SELECT id FROM markets WHERE id::text = $1 AND status <> 'draft'`, id).Scan(&marketID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errMarketNotFound
	}
	return marketID, err
}

// widen copies a Market into a type that extends it (MarketDetail, MyMarket): the generated structs repeat the fields.
func widen(src, dst any) error {
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// marketRules is a market_rule_versions.rules snapshot (MarketRules); dates stay strings so a blank renders as "—".
type marketRules struct {
	Eligibility string  `json:"eligibility"`
	Visibility  string  `json:"visibility"`
	MinStepPct  float64 `json:"minStepPct"`
	MinQuantity float64 `json:"minQuantity"`
	MaxQuantity float64 `json:"maxQuantity"`
	WindowStart string  `json:"windowStart"`
	WindowEnd   string  `json:"windowEnd"`
	Region      string  `json:"region"`
	RadiusKm    float64 `json:"radiusKm"`
	Award       string  `json:"award"`
}

// Copy from the frontend's src/domain/marketRules.ts (RULE_FIELDS, ELIGIBILITY, VISIBILITY, AWARD).
var (
	ruleEligibility = map[string]string{"open": "Terbuka untuk semua akun", "verified": "Akun terverifikasi",
		"verified_docs": "Akun terverifikasi + dokumen legal usaha"}
	ruleVisibility = map[string]string{"full": "Harga terlihat, identitas disamarkan", "rank_only": "Peserta hanya melihat peringkat",
		"sealed": "Tertutup sampai penutupan"}
	ruleAward = map[string]string{"lowest_price": "Harga terendah", "highest_price": "Harga tertinggi",
		"score": "Skor harga 70% + kualitas 30%", "pro_rata": "Pro-rata ke semua penawar yang lolos"}
	idMonths = [...]string{"Jan", "Feb", "Mar", "Apr", "Mei", "Jun", "Jul", "Agu", "Sep", "Okt", "Nov", "Des"}
)

// labeled is rulesToLabeled: the public rule list in display order.
func (r marketRules) labeled(unit string) []api.LabeledValue {
	label := func(m map[string]string, k string) string {
		if v, ok := m[k]; ok {
			return v
		}
		return k
	}
	return []api.LabeledValue{
		{Label: "Eligibility", Value: label(ruleEligibility, r.Eligibility)},
		{Label: "Visibilitas bid", Value: label(ruleVisibility, r.Visibility)},
		{Label: "Kenaikan/penurunan minimum", Value: strings.Replace(strconv.FormatFloat(r.MinStepPct, 'f', -1, 64), ".", ",", 1) + "% dari harga pembuka"},
		{Label: "Kuantitas minimum", Value: idNumber(r.MinQuantity) + " " + unit + " per order"},
		{Label: "Kuantitas maksimum", Value: idNumber(r.MaxQuantity) + " " + unit + " per peserta"},
		{Label: "Jendela mulai", Value: idDate(r.WindowStart)},
		{Label: "Jendela selesai", Value: idDate(r.WindowEnd)},
		{Label: "Wilayah", Value: r.Region},
		{Label: "Radius", Value: idNumber(r.RadiusKm) + " km"},
		{Label: "Penetapan pemenang", Value: label(ruleAward, r.Award)},
	}
}

// idNumber formats like Intl.NumberFormat('id-ID'): 12.345,5 (at most 3 decimals).
func idNumber(v float64) string {
	s := strconv.FormatFloat(math.Round(v*1000)/1000, 'f', -1, 64)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}
	out := b.String()
	if frac != "" {
		out += "," + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}

// idDate formats YYYY-MM-DD like the frontend's formatDate: "5 Mar 2026"; blank is "—".
func idDate(d string) string {
	if d == "" {
		return "—"
	}
	t, err := time.Parse(time.DateOnly, d)
	if err != nil {
		return d
	}
	return fmt.Sprintf("%d %s %d", t.Day(), idMonths[t.Month()-1], t.Year())
}
