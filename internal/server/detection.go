package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	minParties       = 2
	collectiveBuyers = 3
	supplyGapRatio   = 0.7
	engineLockKey    = "opportunity_engine"
)

func (s *Server) RunOpportunityEngine(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.OpportunityTick(ctx); err != nil && ctx.Err() == nil && s.Log != nil {
			s.Log.Error("opportunity engine", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type gapGroup struct {
	category, region, unit       string
	demand, supply               float64
	buyers, suppliers            map[string]bool
	listings                     []string
	items                        map[string]int
	itemOrder                    []string
	supplyValue, demandBudgetIdr float64
	demandQty                    float64
	words                        []string
	taken                        bool
}

func (g *gapGroup) parties() []string {
	var out []string
	for p := range g.buyers {
		out = append(out, p)
	}
	for p := range g.suppliers {
		if !g.buyers[p] {
			out = append(out, p)
		}
	}
	return out
}

func (g *gapGroup) item() string {
	best := ""
	for _, it := range g.itemOrder {
		if best == "" || g.items[it] > g.items[best] {
			best = it
		}
	}
	return best
}

func (g *gapGroup) unitPrice() float64 {
	if g.supply > 0 && g.supplyValue > 0 {
		return g.supplyValue / g.supply
	}
	if g.demandQty > 0 {
		return g.demandBudgetIdr / g.demandQty
	}
	return 0
}

func classify(g *gapGroup, hasMarket bool) (kind, mechanism string, ok bool) {
	if g.demand <= 0 || len(g.parties()) < minParties {
		return "", "", false
	}
	switch {
	case !hasMarket && g.supply > 0:
		if g.demand > g.supply {
			return "market_gap", "reverse_auction", true
		}
		return "market_gap", "forward_auction", true
	case g.demand > g.supply && len(g.buyers) >= collectiveBuyers:
		return "collective_demand", "collective_procurement", true
	case g.supply < supplyGapRatio*g.demand:
		if g.category == "it" {
			return "supply_gap", "direct_market", true
		}
		return "supply_gap", "reverse_auction", true
	case g.supply > g.demand:
		if g.supply >= 1.5*g.demand {
			return "capacity_match", "dutch_auction", true
		}
		return "capacity_match", "forward_auction", true
	}
	return "", "", false
}

var kindLabel = map[string]string{"collective_demand": "Collective demand", "supply_gap": "Supply gap", "market_gap": "Market gap",
	"capacity_match": "Capacity match"}

type detected struct {
	title, description, contribution, reason string
	confidence                               float64
	potential                                int64
}

func describe(g *gapGroup, kind, mechanism string) detected {
	item, n := g.item(), len(g.parties())
	q := func(v float64) string { return idNumber(math.Round(v)) + " " + g.unit }
	d := detected{
		title:      fmt.Sprintf("%s: %s di %s", kindLabel[kind], item, g.region),
		confidence: math.Min(0.95, 0.5+0.05*float64(n)),
		potential:  int64(jsRound(math.Max(g.demand, g.supply) * g.unitPrice())),
	}
	switch kind {
	case "market_gap":
		d.description = fmt.Sprintf("Ada %d pembeli dan %d supplier %s di %s, tetapi belum ada market yang mempertemukan mereka.",
			len(g.buyers), len(g.suppliers), item, g.region)
		d.contribution = fmt.Sprintf("Market maker: buka market %s di %s. Pembeli dan supplier: bergabung saat market dibuka.", item, g.region)
	case "capacity_match":
		d.description = fmt.Sprintf("%d supplier di %s menawarkan %s %s, melebihi permintaan %s yang tercatat.", len(g.suppliers), g.region, q(g.supply), item, q(g.demand))
		d.contribution = fmt.Sprintf("Pembeli: serapan ≥ %s per bulan.", q(g.supply-g.demand))
	case "collective_demand":
		d.description = fmt.Sprintf("%d pembeli di %s mencari %s %s secara terpisah, sementara supply yang tercatat baru %s. Digabungkan, kebutuhannya cukup besar untuk harga skala.",
			len(g.buyers), g.region, q(g.demand), item, q(g.supply))
		d.contribution = fmt.Sprintf("Supplier: kapasitas ≥ %s per bulan. Pembeli: komitmen volume bersama.", q(g.demand-g.supply))
	default:
		d.description = fmt.Sprintf("Permintaan %s di %s (%s) melebihi supply yang tercatat (%s).", item, g.region, q(g.demand), q(g.supply))
		d.contribution = fmt.Sprintf("Supplier: pasokan %s ≥ %s per bulan di %s.", item, q(g.demand-g.supply), g.region)
	}

	if g.supply < g.demand {
		d.reason = fmt.Sprintf("%d peserta dengan demand %d%% di atas supply: %s paling cepat menemukan harga.",
			n, int(jsRound((1-g.supply/g.demand)*100)), strings.ToLower(mechanismLabel[mechanism]))
	} else {
		d.reason = fmt.Sprintf("Supply melebihi demand, jadi %s membantu penjual menemukan harga pasar.", strings.ToLower(mechanismLabel[mechanism]))
	}
	return d
}

func groupKey(category, region, unit string) string {
	return category + "|" + strings.ToLower(region) + "|" + strings.ToLower(unit)
}

var genericWords = map[string]bool{"merah": true, "putih": true, "hitam": true, "hijau": true, "kuning": true, "segar": true,
	"kering": true, "basah": true, "organik": true, "premium": true, "grade": true, "kualitas": true, "super": true, "lokal": true,
	"impor": true, "import": true, "curah": true, "besar": true, "kecil": true, "murah": true, "baru": true, "bekas": true,
	"jenis": true, "per": true, "untuk": true, "dengan": true}

func significantWords(item string) []string {
	var out []string
	for _, w := range itemWords(item) {
		if !genericWords[w] && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

func engineItem(title, region string) (string, bool) {
	for _, label := range kindLabel {
		if rest, ok := strings.CutPrefix(title, label+": "); ok {
			if item, ok := strings.CutSuffix(rest, " di "+region); ok && item != "" {
				return item, true
			}
		}
	}
	return "", false
}

type engineOpp struct {
	id, status, item string
	words            []string
	cluster          *gapGroup
}

func (s *Server) OpportunityTick(ctx context.Context) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext($1))`, engineLockKey).Scan(&locked); err != nil || !locked {
			return err
		}
		clusters, err := loadGroups(ctx, tx)
		if err != nil {
			return err
		}
		markets := map[string]bool{}
		rows, err := tx.Query(ctx, `SELECT DISTINCT category_id, region FROM markets WHERE status IN ('active','formation')`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c, r string
			if err := rows.Scan(&c, &r); err != nil {
				rows.Close()
				return err
			}
			markets[c+"|"+strings.ToLower(regionOf(r))] = true
		}
		rows.Close()
		var opps []*engineOpp
		rows, err = tx.Query(ctx, `
			SELECT id::text, category_id, region, unit, status, title FROM opportunities WHERE status <> 'closed' ORDER BY detected_at, id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var o engineOpp
			var c, r, u, title string
			if err := rows.Scan(&o.id, &c, &r, &u, &o.status, &title); err != nil {
				rows.Close()
				return err
			}
			var ok bool
			if o.item, ok = engineItem(title, r); !ok {
				continue
			}
			o.words = significantWords(o.item)

			for _, g := range clusters[groupKey(c, r, u)] {
				if g.taken || !g.matches(o.item, o.words) {
					continue
				}
				if o.cluster == nil || len(g.listings) > len(o.cluster.listings) {
					o.cluster = g
				}
			}
			if o.cluster != nil {
				o.cluster.taken = true
			}
			opps = append(opps, &o)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, o := range opps {
			g := o.cluster
			switch {
			case o.status == "dismissed":
				continue
			case g == nil:
				if o.status == "detected" {
					if _, err := tx.Exec(ctx, `UPDATE opportunities SET status = 'closed' WHERE id = $1`, o.id); err != nil {
						return err
					}
				}
				continue
			}
			if err := refreshOpportunity(ctx, tx, o.id, g); err != nil {
				return err
			}
			if err := syncListings(ctx, tx, o.id, g); err != nil {
				return err
			}
			if o.status != "detected" {
				continue
			}
			kind, mechanism, ok := classify(g, markets[g.category+"|"+strings.ToLower(g.region)])
			if !ok {
				if _, err := tx.Exec(ctx, `UPDATE opportunities SET status = 'closed' WHERE id = $1`, o.id); err != nil {
					return err
				}
				continue
			}
			d := describe(g, kind, mechanism)
			if _, err := tx.Exec(ctx, `
				UPDATE opportunities SET title = $2, kind = $3, suggested_mechanism = $4, confidence = $5, mechanism_reason = $6,
				       description = $7, required_contribution = $8
				WHERE id = $1 AND (title, kind, suggested_mechanism, confidence, mechanism_reason, description, required_contribution)
				      IS DISTINCT FROM ($2, $3, $4, $5::numeric, $6, $7, $8)`,
				o.id, d.title, kind, mechanism, math.Round(d.confidence*1000)/1000, d.reason, d.description, d.contribution); err != nil {
				return err
			}
		}

		keys := make([]string, 0, len(clusters))
		for k := range clusters {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			for _, g := range clusters[k] {
				if g.taken {
					continue
				}
				kind, mechanism, ok := classify(g, markets[g.category+"|"+strings.ToLower(g.region)])
				if !ok {
					continue
				}
				if err := createOpportunity(ctx, tx, g, kind, mechanism); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

type openListing struct {
	id, kind, item, party string
	qty                   float64
	price, budget         int64
	words                 []string
}

func loadGroups(ctx context.Context, q dbtx) (map[string][]*gapGroup, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, kind, category_id, location, unit, item, quantity::float8, coalesce(price_idr, 0), coalesce(budget_idr, 0), owner_party_id::text
		FROM listings WHERE status IN ('open','matched','available','in_market') AND quantity > 0 ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	byKey := map[string][]openListing{}
	meta := map[string][3]string{}
	for rows.Next() {
		var l openListing
		var cat, location, unit string
		if err := rows.Scan(&l.id, &l.kind, &cat, &location, &unit, &l.item, &l.qty, &l.price, &l.budget, &l.party); err != nil {
			rows.Close()
			return nil, err
		}
		region := regionOf(location)
		if region == "" {
			continue
		}
		l.words = significantWords(l.item)
		k := groupKey(cat, region, unit)
		byKey[k] = append(byKey[k], l)
		if _, ok := meta[k]; !ok {
			meta[k] = [3]string{cat, region, unit}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := map[string][]*gapGroup{}
	for k, ls := range byKey {
		parent := make([]int, len(ls))
		for i := range parent {
			parent[i] = i
		}
		var find func(int) int
		find = func(i int) int {
			for parent[i] != i {
				parent[i] = parent[parent[i]]
				i = parent[i]
			}
			return i
		}
		for i := range ls {
			for j := range i {
				same := len(ls[i].words) == 0 && len(ls[j].words) == 0 && strings.EqualFold(strings.TrimSpace(ls[i].item), strings.TrimSpace(ls[j].item))
				if same || slices.ContainsFunc(ls[i].words, func(w string) bool { return slices.Contains(ls[j].words, w) }) {
					parent[find(i)] = find(j)
				}
			}
		}
		m := meta[k]
		byRoot := map[int]*gapGroup{}
		for i, l := range ls {
			r := find(i)
			g := byRoot[r]
			if g == nil {
				g = &gapGroup{category: m[0], region: m[1], unit: m[2], buyers: map[string]bool{}, suppliers: map[string]bool{}, items: map[string]int{}}
				byRoot[r] = g
				out[k] = append(out[k], g)
			}
			g.add(l)
		}
	}
	return out, nil
}

func (g *gapGroup) add(l openListing) {
	g.listings = append(g.listings, l.id)
	if g.items[l.item] == 0 {
		g.itemOrder = append(g.itemOrder, l.item)
	}
	g.items[l.item]++
	for _, w := range l.words {
		if !slices.Contains(g.words, w) {
			g.words = append(g.words, w)
		}
	}
	if l.kind == "demand" {
		g.demand += l.qty
		g.demandQty += l.qty
		g.demandBudgetIdr += float64(l.budget)
		g.buyers[l.party] = true
	} else {
		g.supply += l.qty
		g.supplyValue += l.qty * float64(l.price)
		g.suppliers[l.party] = true
	}
}

func (g *gapGroup) matches(item string, words []string) bool {
	if len(words) == 0 {
		return slices.ContainsFunc(g.itemOrder, func(it string) bool { return strings.EqualFold(it, item) })
	}
	return slices.ContainsFunc(words, func(w string) bool { return slices.Contains(g.words, w) })
}

func refreshOpportunity(ctx context.Context, tx pgx.Tx, id string, g *gapGroup) error {
	_, err := tx.Exec(ctx, `
		WITH n AS (
		  SELECT ($2::numeric + coalesce((SELECT sum(quantity) FROM opportunity_participants p WHERE p.opportunity_id = $1 AND p.role = 'buyer'
		                                  AND (p.listing_id IS NULL OR NOT p.listing_id = ANY($5::uuid[]))), 0))::numeric(18,3) AS d,
		         ($3::numeric + coalesce((SELECT sum(quantity) FROM opportunity_participants p WHERE p.opportunity_id = $1 AND p.role = 'supplier'
		                                  AND (p.listing_id IS NULL OR NOT p.listing_id = ANY($5::uuid[]))), 0))::numeric(18,3) AS s,
		         ($4::int + (SELECT count(*) FROM opportunity_participants p WHERE p.opportunity_id = $1 AND NOT p.party_id = ANY($6::uuid[])))::int AS c)
		UPDATE opportunities o SET demand_value = n.d, supply_value = n.s, participant_count = n.c,
		       potential_value_idr = $7
		FROM n WHERE o.id = $1
		  AND (o.demand_value, o.supply_value, o.participant_count, o.potential_value_idr) IS DISTINCT FROM (n.d::numeric, n.s::numeric, n.c, $7::bigint)`,
		id, g.demand, g.supply, len(g.parties()), g.listings, g.parties(), int64(jsRound(math.Max(g.demand, g.supply)*g.unitPrice())))
	return err
}

func syncListings(ctx context.Context, tx pgx.Tx, oppID string, g *gapGroup) error {
	if _, err := tx.Exec(ctx, `DELETE FROM opportunity_listings WHERE opportunity_id = $1 AND NOT listing_id = ANY($2::uuid[])`, oppID, g.listings); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO opportunity_listings (opportunity_id, listing_id, party_id, role, quantity)
		SELECT $1, l.id, l.owner_party_id, CASE l.kind WHEN 'demand' THEN 'buyer' ELSE 'supplier' END, l.quantity
		FROM listings l WHERE l.id = ANY($2::uuid[])
		ON CONFLICT (opportunity_id, listing_id) DO UPDATE SET party_id = EXCLUDED.party_id, role = EXCLUDED.role, quantity = EXCLUDED.quantity
		WHERE (opportunity_listings.party_id, opportunity_listings.role, opportunity_listings.quantity)
		      IS DISTINCT FROM (EXCLUDED.party_id, EXCLUDED.role, EXCLUDED.quantity)`, oppID, g.listings)
	return err
}

func createOpportunity(ctx context.Context, tx pgx.Tx, g *gapGroup, kind, mechanism string) error {
	d := describe(g, kind, mechanism)
	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO opportunities (title, kind, category_id, region, unit, demand_value, supply_value, participant_count, potential_value_idr,
		                           suggested_mechanism, confidence, mechanism_reason, description, required_contribution)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING id::text`,
		d.title, kind, g.category, g.region, g.unit, g.demand, g.supply, len(g.parties()), d.potential, mechanism,
		math.Round(d.confidence*1000)/1000, d.reason, d.description, d.contribution).Scan(&id); err != nil {
		return err
	}
	if err := syncListings(ctx, tx, id, g); err != nil {
		return err
	}
	fact, _ := json.Marshal(map[string]any{"categoryId": g.category, "region": g.region, "kind": kind})
	if err := emit(ctx, tx, "opportunity.detected", id, fact); err != nil {
		return err
	}
	if err := emitActivity(ctx, tx, "opportunity_detected", "Opportunity baru: "+d.title, &d.potential, nil); err != nil {
		return err
	}

	users := map[string]bool{}
	rows, err := tx.Query(ctx, `SELECT user_id::text FROM parties WHERE id = ANY($1::uuid[]) AND user_id IS NOT NULL`, g.parties())
	if err != nil {
		return err
	}
	owners, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, u := range owners {
		users[u] = true
	}
	rows, err = tx.Query(ctx, `
		SELECT i.user_id::text, i.pref_locations FROM identities i JOIN users u ON u.id = i.user_id
		WHERE $1 = ANY(i.pref_categories::text[]) AND u.status <> 'suspended'`, g.category)
	if err != nil {
		return err
	}
	for rows.Next() {
		var u string
		var locs []string
		if err := rows.Scan(&u, &locs); err != nil {
			rows.Close()
			return err
		}
		if len(locs) == 0 || slices.ContainsFunc(locs, func(l string) bool { return strings.EqualFold(regionOf(l), g.region) }) {
			users[u] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	body := fmt.Sprintf("%s: gap %s %s/bulan.", d.title, idNumber(math.Round(g.demand-g.supply)), g.unit)
	if g.supply >= g.demand {
		body = fmt.Sprintf("%s: supply %s %s/bulan belum terserap.", d.title, idNumber(math.Round(g.supply-g.demand)), g.unit)
	}
	ids := make([]string, 0, len(users))
	for u := range users {
		ids = append(ids, u)
	}
	slices.Sort(ids)

	for _, u := range ids {
		if err := notify(ctx, tx, u, notification{Type: "opportunity_detected", Title: "Opportunity baru cocok untukmu", Body: body,
			Href: "/opportunities/" + id}); err != nil {
			return err
		}
	}
	return nil
}

func emitActivity(ctx context.Context, q dbtx, typ, title string, amountIdr *int64, marketID *string) error {
	id := "act-" + strings.ToLower(rand.Text())
	payload := map[string]any{"type": typ, "title": title}
	if amountIdr != nil {
		payload["amountIdr"] = *amountIdr
	}
	if marketID != nil {
		payload["marketId"] = *marketID
	}
	b, _ := json.Marshal(payload)
	if err := emit(ctx, q, "activity", id, b); err != nil {
		return err
	}
	frame := map[string]any{"id": id, "type": typ, "title": title, "at": time.Now().UTC().Format(time.RFC3339Nano)}
	if amountIdr != nil {
		frame["amountIdr"] = *amountIdr
	}
	return emitFrame(ctx, q, "public:activity", "activity.created", nil, frame)
}
