package server

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/azdharsyahputra/ecopurnity-api/internal/analytics"
	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Public network stats, activity feed, explorer (ClickHouse) and search (Postgres). Analytics is never the system of
// record: when ClickHouse is down or slow these answer zeros / empty lists and log a warning instead of failing.

const analyticsTimeout = 2 * time.Second

func (s *Server) chWarn(what string, err error) {
	if err != nil && s.Log != nil {
		s.Log.Warn(what+": clickhouse", "err", err)
	}
}

// analyticsCtx bounds a ClickHouse read; ok=false when analytics is not configured.
func (s *Server) analyticsCtx(ctx context.Context) (context.Context, context.CancelFunc, bool) {
	if s.Analytics == nil {
		return ctx, func() {}, false
	}
	c, cancel := context.WithTimeout(ctx, analyticsTimeout)
	return c, cancel, true
}

func (s *Server) publicStats(ctx context.Context) api.PublicStats {
	ctx, cancel, ok := s.analyticsCtx(ctx)
	defer cancel()
	if !ok {
		return api.PublicStats{}
	}
	st, err := s.Analytics.PublicStats(ctx)
	s.chWarn("public stats", err)
	return api.PublicStats{ActiveParticipants: int(st.ActiveParticipants), ActiveMarkets: int(st.ActiveMarkets),
		OpportunitiesDetected: int(st.OpportunitiesDetected), TransactionVolumeIdr: int(st.TransactionVolumeIdr)}
}

func (s *Server) GetPublicStats(ctx context.Context, _ api.GetPublicStatsRequestObject) (api.GetPublicStatsResponseObject, error) {
	return api.GetPublicStats200JSONResponse(s.publicStats(ctx)), nil
}

// publicActivity is the newest n events of the public feed ([] when ClickHouse is unavailable).
func (s *Server) publicActivity(ctx context.Context, n int) []api.ActivityEvent {
	out := []api.ActivityEvent{}
	ctx, cancel, ok := s.analyticsCtx(ctx)
	defer cancel()
	if !ok {
		return out
	}
	events, err := s.Analytics.PublicActivity(ctx, n)
	s.chWarn("public activity", err)
	for _, a := range events {
		ev := api.ActivityEvent{Id: a.ID, Type: api.ActivityType(a.Type), Title: a.Title, At: a.At}
		if a.AmountIdr != nil {
			ev.AmountIdr = ptr(int(*a.AmountIdr))
		}
		out = append(out, ev)
	}
	return out
}

func (s *Server) ListPublicActivity(ctx context.Context, req api.ListPublicActivityRequestObject) (api.ListPublicActivityResponseObject, error) {
	n := 20
	if req.Params.Limit != nil {
		n = min(*req.Params.Limit, 100)
	}
	return api.ListPublicActivity200JSONResponse(s.publicActivity(ctx, n)), nil
}

var rangeDays = map[api.GetExplorerOverviewParamsRange]int{"7d": 7, "30d": 30, "90d": 90}

func or0(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func (s *Server) GetExplorerOverview(ctx context.Context, req api.GetExplorerOverviewRequestObject) (api.GetExplorerOverviewResponseObject, error) {
	days := 30
	if req.Params.Range != nil {
		days = rangeDays[*req.Params.Range]
	}
	category := ""
	if req.Params.Category != nil {
		category = string(*req.Params.Category)
	}
	out := api.GetExplorerOverview200JSONResponse{Stats: s.publicStats(ctx)}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	for k := range days { // a zero series when analytics is down
		out.Volume = append(out.Volume, struct {
			Date      openapi_types.Date `json:"date"`
			VolumeIdr int                `json:"volumeIdr"`
		}{Date: openapi_types.Date{Time: today.AddDate(0, 0, k-days+1)}})
	}
	out.DemandSupply = []struct {
		CategoryId api.CategoryId `json:"categoryId"`
		DemandIdr  int            `json:"demandIdr"`
		SupplyIdr  int            `json:"supplyIdr"`
	}{}
	var index []map[string]any
	if cctx, cancel, ok := s.analyticsCtx(ctx); ok {
		defer cancel()
		err := func() error {
			d, err := s.Analytics.ExplorerDeltas(cctx, days, category)
			if err != nil {
				return err
			}
			out.Deltas.Participants, out.Deltas.Markets, out.Deltas.Opportunities, out.Deltas.Volume =
				or0(d.Participants), or0(d.Markets), or0(d.Opportunities), or0(d.Volume)
			vol, err := s.Analytics.VolumeSeries(cctx, days, category)
			if err != nil {
				return err
			}
			byDay := map[string]int{}
			for _, v := range vol {
				byDay[v.Day.Format(time.DateOnly)] = int(v.Volume)
			}
			for i := range out.Volume {
				out.Volume[i].VolumeIdr = byDay[out.Volume[i].Date.Format(time.DateOnly)]
			}
			points, err := s.Analytics.PriceIndex(cctx, days, category)
			if err != nil {
				return err
			}
			index = pivotIndex(points, today, days)
			ds, err := s.Analytics.DemandSupply(cctx, days, category)
			if err != nil {
				return err
			}
			for _, v := range ds {
				out.DemandSupply = append(out.DemandSupply, struct {
					CategoryId api.CategoryId `json:"categoryId"`
					DemandIdr  int            `json:"demandIdr"`
					SupplyIdr  int            `json:"supplyIdr"`
				}{api.CategoryId(v.Category), int(v.Demand), int(v.Supply)})
			}
			return nil
		}()
		s.chWarn("explorer overview", err)
	}
	if index == nil {
		index = pivotIndex(nil, today, days)
	}
	if err := widen(index, &out.PriceIndex); err != nil {
		return nil, err
	}
	return out, nil
}

// pivotIndex turns long (day, category, index) rows into one point per day of the range with one key per category,
// carrying a category's last index forward over days without deals (absent before its first deal).
func pivotIndex(points []analytics.IndexPoint, today time.Time, days int) []map[string]any {
	byDay := map[string]map[string]float64{}
	for _, p := range points {
		d := p.Day.Format(time.DateOnly)
		if byDay[d] == nil {
			byDay[d] = map[string]float64{}
		}
		byDay[d][p.Category] = p.Index
	}
	last := map[string]float64{}
	out := make([]map[string]any, 0, days)
	for k := range days {
		d := today.AddDate(0, 0, k-days+1).Format(time.DateOnly)
		for c, v := range byDay[d] {
			last[c] = v
		}
		point := map[string]any{"date": d}
		for c, v := range last {
			point[c] = v
		}
		out = append(out, point)
	}
	return out
}

func (s *Server) ListExplorerAggregates(ctx context.Context, req api.ListExplorerAggregatesRequestObject) (api.ListExplorerAggregatesResponseObject, error) {
	side := string(req.Side)
	if side != "demand" && side != "supply" {
		return nil, &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Tidak ditemukan"}
	}
	out := api.ListExplorerAggregates200JSONResponse{}
	cctx, cancel, ok := s.analyticsCtx(ctx)
	defer cancel()
	if !ok {
		return out, nil
	}
	category := ""
	if req.Params.Category != nil {
		category = string(*req.Params.Category)
	}
	rows, err := s.Analytics.Aggregates(cctx, side, category)
	s.chWarn("explorer aggregates", err)
	for _, a := range rows {
		out = append(out, api.AggregateRow{CategoryId: api.CategoryId(a.Category), Item: a.Item, Region: a.Region,
			Quantity: api.Quantity{Value: a.Quantity, Unit: a.Unit}, Listings: int(a.Listings), Trend: or0(a.Trend)})
	}
	return out, nil
}

// ── Search ───────────────────────────────────────────────────────

// slugify is the business profile slug: lower case, runs of anything but a-z0-9 become "-" (same as orgs.slug).
// ponytail: accented letters are dropped instead of folded (the frontend NFKD-normalises first).
func slugify(name string) string {
	return strings.Trim(notSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

// Search: case-insensitive substring over title and subtitle of opportunities, markets, auctions, businesses (orgs and
// market makers) and products/services (auction lots and offered supply listings). Users are not searchable: the
// SearchType enum has no person type.
func (s *Server) Search(ctx context.Context, req api.SearchRequestObject) (api.SearchResponseObject, error) {
	out := api.Search200JSONResponse{}
	term := ""
	if req.Params.Q != nil {
		term = strings.TrimSpace(*req.Params.Q)
	}
	if term == "" {
		return out, nil
	}
	limit := 50
	if req.Params.Limit != nil {
		limit = min(*req.Params.Limit, 100)
	}
	typ := ""
	if req.Params.Type != nil {
		typ = string(*req.Params.Type)
	}
	like := "%" + likeEscape(term) + "%"
	// Each source builds title and subtitle and matches on both; sources come in the mock's order.
	for _, src := range searchSources {
		if len(out) >= limit {
			break
		}
		if typ != "" && !slices.Contains(src.types, typ) {
			continue
		}
		rows, err := s.DB.Reader().Query(ctx, `SELECT type, id, title, subtitle, href FROM (`+src.sql+`) h
			WHERE (h.title || ' ' || h.subtitle) ILIKE $1 AND ($3 = '' OR h.type = $3) ORDER BY h.sort, h.title LIMIT $2`, like, limit-len(out), typ)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var h api.SearchHit
			if err := rows.Scan(&h.Type, &h.Id, &h.Title, &h.Subtitle, &h.Href); err != nil {
				rows.Close()
				return nil, err
			}
			if h.Id == "biz-" { // external market maker: no org slug, derive it from the name
				slug := slugify(h.Title)
				h.Id, h.Href = "biz-"+slug, "/b/"+slug
			}
			out = append(out, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// searchSources: each yields (type, id, title, subtitle, href, sort).
var searchSources = []struct {
	types []string
	sql   string
}{
	{[]string{"opportunity"}, `SELECT 'opportunity' AS type, o.id::text AS id, o.title, o.code || ' · ' || o.region AS subtitle,
		'/opportunities/' || o.id AS href, -o.potential_value_idr::float8 AS sort FROM opportunities o`},
	{[]string{"market"}, `SELECT 'market' AS type, m.id::text AS id, m.name AS title, m.code || ' · ' || m.region AS subtitle,
		'/markets/' || m.id AS href, -m.volume_30d_idr::float8 AS sort FROM markets m WHERE m.status <> 'draft'`},
	{[]string{"auction"}, `SELECT 'auction' AS type, a.id::text AS id, a.title, a.code || ' · ' || coalesce(m.name, 'Pengadaan langsung') AS subtitle,
		'/auctions/' || a.id AS href, -extract(epoch FROM a.ends_at)::float8 AS sort FROM auctions a LEFT JOIN markets m ON m.id = a.market_id`},
	{[]string{"business"}, `SELECT DISTINCT ON (o.id) 'business' AS type, 'biz-' || o.slug AS id, o.name AS title,
		CASE WHEN m.id IS NOT NULL THEN 'Market maker · ' || m.region
		     ELSE 'Bisnis · ' || coalesce(nullif(pr.region, ''), nullif(pr.location, ''), 'Indonesia') END AS subtitle,
		'/b/' || o.slug AS href, 0::float8 AS sort
		FROM orgs o LEFT JOIN org_profiles pr ON pr.org_id = o.id LEFT JOIN parties p ON p.org_id = o.id
		LEFT JOIN markets m ON m.maker_party_id = p.id AND m.status <> 'draft'
		ORDER BY o.id, m.id IS NULL, m.created_at`},
	{[]string{"business"}, `SELECT DISTINCT ON (p.id) 'business' AS type, 'biz-' AS id, p.name AS title, 'Market maker · ' || m.region AS subtitle,
		'' AS href, 0::float8 AS sort
		FROM parties p JOIN markets m ON m.maker_party_id = p.id AND m.status <> 'draft' WHERE p.kind = 'external' ORDER BY p.id, m.created_at`},
	{[]string{"product", "service"}, `SELECT CASE WHEN a.category_id IN ('it','logistics') THEN 'service' ELSE 'product' END AS type,
		'item-' || a.id AS id, a.lot_item AS title, a.lot_spec AS subtitle, '/auctions/' || a.id AS href, -extract(epoch FROM a.ends_at)::float8 AS sort
		FROM auctions a WHERE a.status <> 'cancelled'`},
	{[]string{"product", "service"}, `SELECT CASE WHEN l.category_id IN ('it','logistics') THEN 'service' ELSE 'product' END AS type,
		l.id::text AS id, l.item AS title, l.code || ' · ' || l.location AS subtitle, '/listings?q=' || l.code AS href,
		-extract(epoch FROM l.created_at)::float8 AS sort
		FROM listings l WHERE l.kind = 'supply' AND l.status IN ('available','in_market')`},
}
