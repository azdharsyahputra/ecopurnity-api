package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Supplier directory as an org sees it (PRD 9.7) and purchase analytics (PRD 9.9).

var errSupplierNotFound = notFound("Supplier tidak ditemukan")

// loadOrgSuppliers: directory suppliers matching cond (alias s, args from $2) with this org's relation and rating, the
// platform rating (seed = 10 reviews, plus buyers' reviews and this org's rating) and this org's trades + history.
func loadOrgSuppliers(ctx context.Context, q dbtx, orgID, cond string, args ...any) ([]api.OrgSupplier, error) {
	rows, err := q.Query(ctx, `
		WITH me AS (SELECT id FROM parties WHERE org_id = $1)
		SELECT s.id::text, s.name, s.categories::text[], s.region, s.seed_rating::float8, s.verified, s.documents, s.capacity,
		       coalesce(os.relation, 'none'), os.my_rating::int, rv.n, rv.total,
		       (SELECT count(*) FROM trades t WHERE t.buyer_party_id IN (SELECT id FROM me) AND t.supplier_party_id = s.party_id)
		         + (SELECT count(*) FROM org_purchase_history h WHERE h.org_id = $1 AND h.supplier_id = s.id),
		       (SELECT coalesce(sum(t.total_idr), 0)::bigint FROM trades t WHERE t.buyer_party_id IN (SELECT id FROM me) AND t.supplier_party_id = s.party_id)
		         + (SELECT coalesce(sum(round(h.unit_price_idr * h.quantity)), 0)::bigint FROM org_purchase_history h WHERE h.org_id = $1 AND h.supplier_id = s.id)
		FROM suppliers s LEFT JOIN org_suppliers os ON os.supplier_id = s.id AND os.org_id = $1
		LEFT JOIN LATERAL (SELECT count(*) AS n, coalesce(sum(r.rating), 0) AS total FROM reviews r JOIN trades t ON t.id = r.trade_id
		                   WHERE r.side = 'buyer' AND t.supplier_party_id = s.party_id) rv ON true
		WHERE `+cond+` ORDER BY s.name, s.id`, append([]any{orgID}, args...)...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (api.OrgSupplier, error) {
		var s api.OrgSupplier
		var cats []string
		var seed float64
		var n, total int64
		err := r.Scan(&s.Id, &s.Name, &cats, &s.Region, &seed, &s.Verified, &s.Documents, &s.Capacity, &s.Relation, &s.MyRating, &n, &total,
			&s.Transactions, &s.SpendIdr)
		for _, c := range cats {
			s.Categories = append(s.Categories, api.CategoryId(c))
		}
		s.Categories, s.Documents = nonNil(s.Categories), nonNil(s.Documents)
		sum, count := seed*10+float64(total), 10+float64(n)
		if s.MyRating != nil {
			sum, count = sum+float64(*s.MyRating), count+1
		}
		s.Rating = math.Round(sum/count*10) / 10
		s.Scorecard = []api.ScorePoint{}
		return s, err
	})
	if err != nil || len(out) == 0 {
		return nonNil(out), err
	}
	ids := make([]string, len(out))
	idx := map[string]int{}
	for i, s := range out {
		ids[i], idx[s.Id] = s.Id, i
	}
	rows, err = q.Query(ctx, `
		SELECT supplier_id::text, to_char(month, 'YYYY-MM'), price::float8, reliability::float8, quality::float8, delivery::float8
		FROM supplier_scorecards WHERE supplier_id::text = ANY($1) ORDER BY month`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var p api.ScorePoint
		if err := rows.Scan(&id, &p.Month, &p.Price, &p.Reliability, &p.Quality, &p.Delivery); err != nil {
			return nil, err
		}
		out[idx[id]].Scorecard = append(out[idx[id]].Scorecard, p)
	}
	return out, rows.Err()
}

func (s *Server) ListOrgSuppliers(ctx context.Context, req api.ListOrgSuppliersRequestObject) (api.ListOrgSuppliersResponseObject, error) {
	q := s.DB.Primary()
	c, err := orgAccess(ctx, q, req.OrgId)
	if err != nil {
		return nil, err
	}
	p := req.Params
	where, args := []string{"true"}, []any{}
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)+1) }
	if p.Q != nil && strings.TrimSpace(*p.Q) != "" {
		where = append(where, "s.name ILIKE "+arg("%"+likeEscape(strings.TrimSpace(*p.Q))+"%"))
	}
	if p.Category != nil {
		where = append(where, arg(string(*p.Category))+" = ANY(s.categories::text[])")
	}
	if p.Region != nil && strings.TrimSpace(*p.Region) != "" {
		where = append(where, "s.region = "+arg(strings.TrimSpace(*p.Region)))
	}
	if p.Verified != nil {
		if v, err := strconv.ParseBool(*p.Verified); err == nil { // the frontend sends verified=1
			where = append(where, "s.verified = "+arg(v))
		}
	}
	if p.Relation != nil {
		where = append(where, "coalesce(os.relation, 'none') = "+arg(string(*p.Relation)))
	}
	all, err := loadOrgSuppliers(ctx, q, c.OrgID, strings.Join(where, " AND "), args...)
	if err != nil {
		return nil, err
	}
	out := api.ListOrgSuppliers200JSONResponse{}
	for _, sup := range all {
		if p.MinRating == nil || sup.Rating >= *p.MinRating {
			out = append(out, sup)
		}
	}
	return out, nil
}

func loadOrgSupplier(ctx context.Context, q dbtx, orgID, id string) (api.OrgSupplier, error) {
	ss, err := loadOrgSuppliers(ctx, q, orgID, `s.id::text = $2`, id)
	if err == nil && len(ss) == 0 {
		err = errSupplierNotFound
	}
	if err != nil {
		return api.OrgSupplier{}, err
	}
	return ss[0], nil
}

func (s *Server) GetOrgSupplier(ctx context.Context, req api.GetOrgSupplierRequestObject) (api.GetOrgSupplierResponseObject, error) {
	q := s.DB.Primary()
	c, err := orgAccess(ctx, q, req.OrgId)
	if err != nil {
		return nil, err
	}
	sup, err := loadOrgSupplier(ctx, q, c.OrgID, req.Sid)
	if err != nil {
		return nil, err
	}
	out := api.GetOrgSupplier200JSONResponse{Id: sup.Id, Name: sup.Name, Categories: sup.Categories, Region: sup.Region, Rating: sup.Rating,
		Verified: sup.Verified, Documents: sup.Documents, Capacity: sup.Capacity, Scorecard: sup.Scorecard, Relation: sup.Relation, MyRating: sup.MyRating,
		Transactions: sup.Transactions, SpendIdr: sup.SpendIdr}
	if out.History, err = supplierHistory(ctx, q, c.OrgID, sup.Id); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
		SELECT code, to_char(month, 'YYYY-MM'), item, category_id, quantity, unit, unit_price_idr, round(unit_price_idr * quantity)::bigint, via
		FROM org_purchase_history WHERE org_id = $1 AND supplier_id::text = $2 ORDER BY month DESC, code DESC LIMIT 12`, c.OrgID, sup.Id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p struct {
			CategoryId   api.CategoryId               `json:"categoryId"`
			Code         string                       `json:"code"`
			Item         string                       `json:"item"`
			Month        string                       `json:"month"`
			Quantity     api.Quantity                 `json:"quantity"`
			Supplier     string                       `json:"supplier"`
			TotalIdr     int                          `json:"totalIdr"`
			UnitPriceIdr int                          `json:"unitPriceIdr"`
			Via          api.SupplierPagePurchasesVia `json:"via"`
		}
		if err := rows.Scan(&p.Code, &p.Month, &p.Item, &p.CategoryId, &p.Quantity.Value, &p.Quantity.Unit, &p.UnitPriceIdr, &p.TotalIdr, &p.Via); err != nil {
			rows.Close()
			return nil, err
		}
		p.Supplier = sup.Name
		out.Purchases = append(out.Purchases, p)
	}
	rows.Close()
	out.Purchases = nonNil(out.Purchases)
	if out.Activity, err = loadAuditEntries(ctx, q, `org_id = $1 AND entity_type = 'supplier' AND entity_id = $2 ORDER BY at DESC, id DESC`, c.OrgID, sup.Id); err != nil {
		return nil, err
	}
	return out, nil
}

var (
	supplierRelationAfter = map[string]string{"shortlist": "shortlisted", "invite": "invited", "verify": "verified", "block": "blocked", "unblock": "none"}
	supplierAuditLabel    = map[string]string{"shortlist": "Shortlist supplier", "invite": "Undang supplier", "verify": "Verifikasi supplier",
		"block": "Blokir supplier", "unblock": "Buka blokir supplier", "rate": "Beri rating supplier"}
)

func (s *Server) ActOnOrgSupplier(ctx context.Context, req api.ActOnOrgSupplierRequestObject) (api.ActOnOrgSupplierResponseObject, error) {
	in := req.Body
	action := string(in.Action)
	var out api.OrgSupplier
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		var id, name string
		var verified bool
		var docs int
		if err := tx.QueryRow(ctx, `SELECT id::text, name, verified, cardinality(documents) FROM suppliers WHERE id::text = $1`, req.Sid).
			Scan(&id, &name, &verified, &docs); errors.Is(err, pgx.ErrNoRows) {
			return errSupplierNotFound
		} else if err != nil {
			return err
		}
		needs := "manage"
		if action == "shortlist" || action == "invite" {
			needs = "create"
		}
		if err := c.need("suppliers", needs); err != nil {
			return err
		}
		reason := strings.TrimSpace(deref(in.Reason))
		if action == "block" && reason == "" {
			return invalid("Tulis alasan blokir", map[string]string{"reason": "Alasan wajib diisi"})
		}
		if action == "rate" && (in.Rating == nil || *in.Rating < 1 || *in.Rating > 5) {
			return invalid("Rating 1–5", map[string]string{"rating": "Pilih 1–5 bintang"})
		}
		if action == "verify" && !verified && docs < 2 {
			return conflict("docs_missing", "Dokumen legal supplier belum lengkap untuk diverifikasi")
		}
		before := "none"
		if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT relation FROM org_suppliers WHERE org_id = $1 AND supplier_id = $2 FOR UPDATE), 'none')`,
			c.OrgID, id).Scan(&before); err != nil {
			return err
		}
		if before == "blocked" && action != "unblock" {
			return conflict("blocked", "Supplier diblokir. Buka blokir dulu.")
		}
		if action == "unblock" && before != "blocked" {
			return conflict("not_blocked", "Supplier ini tidak sedang diblokir")
		}
		var ch change
		var auditReason *string
		if action == "rate" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO org_suppliers (org_id, supplier_id, my_rating) VALUES ($1, $2, $3) ON CONFLICT (org_id, supplier_id) DO UPDATE SET my_rating = EXCLUDED.my_rating`,
				c.OrgID, id, *in.Rating); err != nil {
				return err
			}
			ch = change{Field: "Rating", After: fmt.Sprintf("%d/5", *in.Rating)}
		} else {
			after := supplierRelationAfter[action]
			if action == "block" {
				auditReason = &reason
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO org_suppliers (org_id, supplier_id, relation, block_reason) VALUES ($1, $2, $3, $4)
				ON CONFLICT (org_id, supplier_id) DO UPDATE SET relation = EXCLUDED.relation, block_reason = EXCLUDED.block_reason`,
				c.OrgID, id, after, auditReason); err != nil {
				return err
			}
			ch = change{Field: "Relasi", Before: &before, After: after}
		}
		if err := c.audit(ctx, tx, supplierAuditLabel[action], "supplier", id, name, auditReason, ch); err != nil {
			return err
		}
		out, err = loadOrgSupplier(ctx, tx, c.OrgID, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.ActOnOrgSupplier200JSONResponse(out), nil
}

// supplierHistory: the org's trades with a directory supplier, newest first, as the transactions area renders them.
func supplierHistory(ctx context.Context, q dbtx, orgID, supplierID string) ([]api.TransactionDetail, error) {
	var party string
	err := q.QueryRow(ctx, `SELECT id::text FROM parties WHERE org_id = $1`, orgID).Scan(&party)
	if errors.Is(err, pgx.ErrNoRows) {
		return []api.TransactionDetail{}, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
		SELECT t.id::text FROM trades t JOIN suppliers s ON s.party_id IN (t.buyer_party_id, t.supplier_party_id)
		WHERE s.id::text = $2 AND $1::uuid IN (t.buyer_party_id, t.supplier_party_id) ORDER BY t.created_at DESC, t.id`, party, supplierID)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := []api.TransactionDetail{}
	for _, id := range ids {
		d, err := loadTransaction(ctx, q, party, id, nil) // supplier history: no file links (not gated by transactions.view)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// ── Analytics ────────────────────────────────────────────────────

// purchase is one aggregated group of an org's purchases; purchaseLine one purchase. Supplier keys are "s:<supplier id>"
// (directory, purchase history) or "p:<party id>" (trades).
type purchase struct {
	Month, Category, Item, Unit, Supplier, Via string
	Qty                                        float64
	Spend, Budget, Market                      int64
	Count                                      int
}

type purchaseLine struct {
	purchase
	Code             string
	At               time.Time
	UnitPrice        int64
	Bidders, Opening int64
}

// pgPurchases: purchase history plus every line of the org's awarded procurement auctions (the Postgres source, also the
// fallback when ClickHouse is down), oldest first.
func (s *Server) pgPurchases(ctx context.Context, q dbtx, orgID string) ([]purchaseLine, error) {
	rows, err := q.Query(ctx, `
		SELECT code, to_char(month, 'YYYY-MM'), item, category_id, 's:' || supplier_id, via, quantity, unit, unit_price_idr,
		       round(unit_price_idr * quantity)::bigint, round(budget_unit_idr * quantity)::bigint, round(market_unit_idr * quantity)::bigint,
		       coalesce(bidders, 0), coalesce(opening_idr, market_unit_idr), month::timestamptz
		FROM org_purchase_history WHERE org_id = $1
		UNION ALL
		SELECT coalesce(po.po_number, a.code) || '-' || lot.position ||
		         CASE WHEN count(*) OVER (PARTITION BY wl.org_award_id, lot.id) > 1 THEN chr(97 + wl.position) ELSE '' END,
		       to_char(aw.awarded_at, 'YYYY-MM'), lot.item, a.category_id, coalesce('s:' || o.supplier_id, 'p:' || o.party_id), 'auction', wl.quantity, lot.unit,
		       wl.price_idr, round(wl.price_idr * wl.quantity)::bigint, round(lot.reserve_price_idr * wl.quantity)::bigint,
		       round(lot.reserve_price_idr * wl.quantity)::bigint, coalesce(e.participant_count, 0), lot.reserve_price_idr, aw.awarded_at
		FROM org_award_lines wl JOIN org_awards aw ON aw.id = wl.org_award_id JOIN org_auctions a ON a.id = aw.org_auction_id
		JOIN org_auction_lots lot ON lot.id = wl.org_auction_lot_id JOIN org_auction_offers o ON o.id = wl.org_auction_offer_id
		LEFT JOIN purchase_orders po ON po.org_award_id = aw.id LEFT JOIN auctions e ON e.id = lot.auction_id
		WHERE a.org_id = $1 AND a.objective = 'procurement'
		ORDER BY 2, 15, 1`, orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (purchaseLine, error) {
		var l purchaseLine
		err := r.Scan(&l.Code, &l.Month, &l.Item, &l.Category, &l.Supplier, &l.Via, &l.Qty, &l.Unit, &l.UnitPrice, &l.Spend, &l.Budget, &l.Market,
			&l.Bidders, &l.Opening, &l.At)
		l.Count = 1
		return l, err
	})
}

// chPurchases: the same from ClickHouse (org_purchase_monthly for the aggregates, trades for the lines).
func (s *Server) chPurchases(ctx context.Context, orgID string) ([]purchase, []purchaseLine, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second) // a hung ClickHouse must not hang the page
	defer cancel()
	ps, err := s.Analytics.OrgPurchases(ctx, orgID)
	if err != nil {
		return nil, nil, err
	}
	ts, err := s.Analytics.OrgTrades(ctx, orgID, 500)
	if err != nil {
		return nil, nil, err
	}
	aggs := make([]purchase, len(ps))
	for i, p := range ps {
		aggs[i] = purchase{Month: p.Month.Format("2006-01"), Category: p.Category, Item: p.Item, Unit: p.Unit, Supplier: supplierKey(p.SupplierParty), Via: p.Via,
			Qty: p.Quantity, Spend: int64(p.Spend), Budget: int64(p.Budget), Market: int64(p.Market), Count: int(p.Purchases)}
	}
	lines := make([]purchaseLine, len(ts))
	for i, t := range ts {
		opening := int64(t.Opening)
		if opening == 0 {
			opening = int64(t.Budget)
		}
		lines[i] = purchaseLine{purchase: purchase{Month: t.At.UTC().Format("2006-01"), Category: t.Category, Item: t.Item, Unit: t.Unit,
			Supplier: supplierKey(t.SupplierParty), Via: t.Via, Qty: t.Quantity, Spend: int64(t.Value), Count: 1},
			Code: t.Code, At: t.At, UnitPrice: int64(t.UnitPrice), Bidders: int64(t.Bidders), Opening: opening}
	}
	slices.Reverse(lines) // oldest first, like the Postgres source
	return aggs, lines, nil
}

// supplierKey: a ClickHouse supplier is a party id, or "s:<directory id>" for imported history whose supplier has no party
// yet (migrations/postgres/00023).
func supplierKey(party string) string {
	if strings.HasPrefix(party, "s:") {
		return party
	}
	return "p:" + party
}

type supplierInfo struct {
	ID, Name      string
	Score, OnTime float64
}

// resolveSuppliers names supplier keys: directory suppliers (by id or by their party) with their latest scorecard,
// other parties by name with an 80 baseline.
func resolveSuppliers(ctx context.Context, q dbtx, keys []string) (map[string]supplierInfo, error) {
	var ids, parties []string
	for _, k := range keys {
		if id, ok := strings.CutPrefix(k, "s:"); ok {
			ids = append(ids, id)
		} else if p, ok := strings.CutPrefix(k, "p:"); ok && p != "" {
			parties = append(parties, p)
		}
	}
	out := map[string]supplierInfo{}
	rows, err := q.Query(ctx, `
		SELECT s.id::text, s.party_id::text, s.name, coalesce(sc.price + sc.reliability + sc.quality + sc.delivery, 320)::float8 / 4, coalesce(sc.delivery, 80)::float8 / 100
		FROM suppliers s LEFT JOIN LATERAL (SELECT * FROM supplier_scorecards x WHERE x.supplier_id = s.id ORDER BY month DESC LIMIT 1) sc ON true
		WHERE s.id::text = ANY($1) OR s.party_id::text = ANY($2)`, ids, parties)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var i supplierInfo
		var party *string
		if err := rows.Scan(&i.ID, &party, &i.Name, &i.Score, &i.OnTime); err != nil {
			rows.Close()
			return nil, err
		}
		i.Score = math.Round(i.Score)
		out["s:"+i.ID] = i
		if party != nil {
			out["p:"+*party] = i
		}
	}
	rows.Close()
	rows, err = q.Query(ctx, `SELECT id::text, name FROM parties WHERE id::text = ANY($1)`, parties)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var fresh []supplierInfo
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		if _, ok := out["p:"+id]; !ok {
			fresh = append(fresh, supplierInfo{ID: id, Name: name, OnTime: 0.8})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// Bidders outside the directory: their platform reputation.
	for _, i := range fresh {
		score, _, err := reputationOf(ctx, q, i.ID)
		if err != nil {
			return nil, err
		}
		i.Score = float64(score)
		out["p:"+i.ID] = i
	}
	return out, nil
}

func (s *Server) GetOrgAnalytics(ctx context.Context, req api.GetOrgAnalyticsRequestObject) (api.GetOrgAnalyticsResponseObject, error) {
	q := s.DB.Reader()
	c, err := orgAccess(ctx, s.DB.Primary(), req.OrgId)
	if err != nil {
		return nil, err
	}
	months := 12
	if m := req.Params.Months; m != nil {
		months = min(max(*m, 1), 36)
	}
	category := ""
	if req.Params.Category != nil {
		category = string(*req.Params.Category)
	}
	var aggs []purchase
	var lines []purchaseLine
	if s.Analytics != nil {
		aggs, lines, err = s.chPurchases(ctx, c.OrgID)
		if err != nil && s.Log != nil {
			// Analytics down degrades to the Postgres figures; it never fails the page.
			s.Log.Warn("org analytics: clickhouse unavailable, using postgres", "err", err)
		}
	}
	if s.Analytics == nil || err != nil {
		if lines, err = s.pgPurchases(ctx, q, c.OrgID); err != nil {
			return nil, err
		}
		aggs = make([]purchase, len(lines))
		for i, l := range lines {
			aggs[i] = l.purchase
		}
	}
	var catOrder []string
	if err := q.QueryRow(ctx, `SELECT categories::text[] FROM org_profiles WHERE org_id = $1`, c.OrgID).Scan(&catOrder); err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, a := range aggs {
		keys[a.Supplier] = true
	}
	for _, l := range lines {
		keys[l.Supplier] = true
	}
	sups, err := resolveSuppliers(ctx, q, slices.Collect(maps.Keys(keys)))
	if err != nil {
		return nil, err
	}
	return api.GetOrgAnalytics200JSONResponse(orgAnalytics(aggs, lines, months, category, catOrder, sups)), nil
}

// orgAnalytics is the frontend mock's analytics(): over the latest `months` months that have data, optionally one category.
func orgAnalytics(aggs []purchase, lines []purchaseLine, months int, category string, catOrder []string, sups map[string]supplierInfo) api.OrgAnalytics {
	var keys []string
	for _, a := range aggs {
		if !slices.Contains(keys, a.Month) {
			keys = append(keys, a.Month)
		}
	}
	slices.Sort(keys)
	keys = keys[max(0, len(keys)-months):]
	in := func(month, cat string) bool {
		return slices.Contains(keys, month) && (category == "" || cat == category)
	}
	var rows []purchase
	for _, a := range aggs {
		if in(a.Month, a.Category) {
			rows = append(rows, a)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Month < rows[j].Month })
	out := api.OrgAnalytics{Categories: []api.CategoryId{}, Spend: []api.OrgAnalytics_Spend_Item{}}
	var cats, items []string
	for _, r := range rows {
		if !slices.Contains(cats, r.Category) {
			cats = append(cats, r.Category)
		}
		if !slices.Contains(items, r.Item) {
			items = append(items, r.Item)
		}
	}
	sort.SliceStable(cats, func(i, j int) bool { return slices.Index(catOrder, cats[i]) < slices.Index(catOrder, cats[j]) })
	for _, c := range cats {
		out.Categories = append(out.Categories, api.CategoryId(c))
	}
	sum := func(f func(purchase) bool, v func(purchase) float64) float64 {
		t := 0.0
		for _, r := range rows {
			if f(r) {
				t += v(r)
			}
		}
		return t
	}
	spend := func(r purchase) float64 { return float64(r.Spend) }
	qty := func(r purchase) float64 { return r.Qty }
	market := func(r purchase) float64 { return float64(r.Market) }
	itemTotal := map[string]float64{}
	for _, it := range items {
		itemTotal[it] = sum(func(r purchase) bool { return r.Item == it }, spend)
	}
	top := ""
	if len(items) > 0 {
		sorted := slices.Clone(items)
		sort.SliceStable(sorted, func(i, j int) bool { return itemTotal[sorted[i]] > itemTotal[sorted[j]] })
		top = sorted[0]
	}
	out.PriceTrend.Item, out.Demand.Item = top, top
	var firstOurs, firstMarket float64
	for _, m := range keys {
		inMonth := func(r purchase) bool { return r.Month == m }
		item := api.OrgAnalytics_Spend_Item{Month: m}
		for _, c := range cats {
			item.Set(c, int(sum(func(r purchase) bool { return inMonth(r) && r.Category == c }, spend)))
		}
		out.Spend = append(out.Spend, item)
		out.Savings = append(out.Savings, struct {
			BudgetIdr int    `json:"budgetIdr"`
			MarketIdr int    `json:"marketIdr"`
			Month     string `json:"month"`
			SpendIdr  int    `json:"spendIdr"`
		}{BudgetIdr: int(sum(inMonth, func(r purchase) float64 { return float64(r.Budget) })), MarketIdr: int(sum(inMonth, market)), Month: m,
			SpendIdr: int(sum(inMonth, spend))})
		topMonth := func(r purchase) bool { return inMonth(r) && r.Item == top }
		q := sum(topMonth, qty)
		if q > 0 {
			ours, mkt := sum(topMonth, spend)/q, sum(topMonth, market)/q
			if firstOurs == 0 {
				firstOurs, firstMarket = ours, mkt
			}
			p := struct {
				Market float64 `json:"market"`
				Month  string  `json:"month"`
				Ours   float64 `json:"ours"`
			}{Month: m, Ours: math.Round(ours/firstOurs*1000) / 10}
			if firstMarket > 0 {
				p.Market = math.Round(mkt/firstMarket*1000) / 10
			}
			out.PriceTrend.Points = append(out.PriceTrend.Points, p)
		}
		out.Demand.Points = append(out.Demand.Points, struct {
			Month    string  `json:"month"`
			Quantity float64 `json:"quantity"`
			Requests int     `json:"requests"`
		}{Month: m, Quantity: q, Requests: int(sum(inMonth, func(r purchase) float64 { return float64(r.Count) }))})
	}
	for _, r := range rows {
		if r.Item == top {
			out.Demand.Unit = r.Unit
			break
		}
	}
	for _, it := range items {
		isItem := func(r purchase) bool { return r.Item == it }
		q := sum(isItem, qty)
		unit := ""
		for _, r := range rows {
			if r.Item == it {
				unit = r.Unit
				break
			}
		}
		u := struct {
			AvgIdr    int    `json:"avgIdr"`
			Item      string `json:"item"`
			MarketIdr int    `json:"marketIdr"`
			Unit      string `json:"unit"`
		}{Item: it, Unit: unit}
		if q > 0 {
			u.AvgIdr, u.MarketIdr = int(math.Round(sum(isItem, spend)/q)), int(math.Round(sum(isItem, market)/q))
		}
		out.UnitPrices = append(out.UnitPrices, u)
	}
	type supRow = struct {
		Id       string  `json:"id"`
		Name     string  `json:"name"`
		OnTime   float64 `json:"onTime"`
		Score    float64 `json:"score"`
		SpendIdr int     `json:"spendIdr"`
	}
	bySup := map[string]*supRow{}
	var supOrder []string
	for _, r := range rows {
		info, ok := sups[r.Supplier]
		if !ok {
			info = supplierInfo{ID: r.Supplier[min(2, len(r.Supplier)):], Name: "-", Score: 80, OnTime: 0.8}
		}
		if bySup[info.ID] == nil {
			bySup[info.ID] = &supRow{Id: info.ID, Name: info.Name, OnTime: info.OnTime, Score: info.Score}
			supOrder = append(supOrder, info.ID)
		}
		bySup[info.ID].SpendIdr += int(r.Spend)
	}
	for _, id := range supOrder {
		out.Suppliers = append(out.Suppliers, *bySup[id])
	}
	sort.SliceStable(out.Suppliers, func(i, j int) bool { return out.Suppliers[i].SpendIdr > out.Suppliers[j].SpendIdr })
	// Lines newest first.
	var hist []purchaseLine
	for i := len(lines) - 1; i >= 0; i-- {
		if in(lines[i].Month, lines[i].Category) {
			hist = append(hist, lines[i])
		}
	}
	for _, l := range hist {
		if l.Via == "auction" && len(out.Auctions) < 8 {
			out.Auctions = append(out.Auctions, struct {
				Bidders     int    `json:"bidders"`
				ClearingIdr int    `json:"clearingIdr"`
				Code        string `json:"code"`
				OpeningIdr  int    `json:"openingIdr"`
				Title       string `json:"title"`
			}{Bidders: int(l.Bidders), ClearingIdr: int(l.UnitPrice), Code: l.Code, OpeningIdr: int(l.Opening), Title: l.Item + " · " + l.Month})
		}
		name := sups[l.Supplier].Name
		via := l.Via
		if via != "auction" && via != "collective" {
			via = "direct"
		}
		out.History = append(out.History, struct {
			CategoryId   api.CategoryId             `json:"categoryId"`
			Code         string                     `json:"code"`
			Item         string                     `json:"item"`
			Month        string                     `json:"month"`
			Quantity     api.Quantity               `json:"quantity"`
			Supplier     string                     `json:"supplier"`
			TotalIdr     int                        `json:"totalIdr"`
			UnitPriceIdr int                        `json:"unitPriceIdr"`
			Via          api.OrgAnalyticsHistoryVia `json:"via"`
		}{CategoryId: api.CategoryId(l.Category), Code: l.Code, Item: l.Item, Month: l.Month, Quantity: api.Quantity{Value: l.Qty, Unit: l.Unit},
			Supplier: nonEmpty(name, "-"), TotalIdr: int(l.Spend), UnitPriceIdr: int(l.UnitPrice), Via: api.OrgAnalyticsHistoryVia(via)})
	}
	out.Savings, out.UnitPrices, out.PriceTrend.Points, out.Demand.Points = nonNil(out.Savings), nonNil(out.UnitPrices), nonNil(out.PriceTrend.Points), nonNil(out.Demand.Points)
	out.Suppliers, out.Auctions, out.History = nonNil(out.Suppliers), nonNil(out.Auctions), nonNil(out.History)
	return out
}
