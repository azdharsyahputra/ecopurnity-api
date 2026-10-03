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

// The opportunity engine: groups the open listings by category, region (province, from the free-text location) and
// unit, and turns a group whose demand and supply are out of balance into an opportunity. Existing open opportunities
// of the same group get fresh totals. Runs on every API instance; a transaction-scoped advisory lock lets one pass run
// at a time.
//
// Rules (the mock seeds its opportunities, so these are the BE's; thresholds are the knobs below):
//   - a group needs demand and at least minParties distinct parties;
//   - market_gap:        both sides listed but no active/forming market in the category and region;
//   - collective_demand: demand > supply with at least collectiveBuyers distinct buyers;
//   - supply_gap:        supply covers less than supplyGapRatio of demand;
//   - capacity_match:    supply exceeds demand (idle capacity looking for buyers);
//   - otherwise balanced: nothing to detect.
//
// A group with an opportunity in any status but closed is never detected again (a dismissed one stays dismissed).

const (
	minParties       = 2
	collectiveBuyers = 3
	supplyGapRatio   = 0.7
	engineLockKey    = "opportunity_engine"
)

// RunOpportunityEngine ticks until ctx ends.
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

// item is the most listed item name (ties: first seen).
func (g *gapGroup) item() string {
	best := ""
	for _, it := range g.itemOrder {
		if best == "" || g.items[it] > g.items[best] {
			best = it
		}
	}
	return best
}

// unitPrice is the volume-weighted supply price, else the demand budget per unit.
func (g *gapGroup) unitPrice() float64 {
	if g.supply > 0 && g.supplyValue > 0 {
		return g.supplyValue / g.supply
	}
	if g.demandQty > 0 {
		return g.demandBudgetIdr / g.demandQty
	}
	return 0
}

// classify applies the rules above; ok=false when the group is balanced or too thin.
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
			return "supply_gap", "direct_market", true // services by the hour sell at a posted rate
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

// mechanismLabel (frontend MECHANISMS labels) lives in mm_domain.go.

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
	// Same copy as the mock's mechanismReason (mocks/economy.ts).
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

// OpportunityTick runs one detection pass (exported for tests).
func (s *Server) OpportunityTick(ctx context.Context) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext($1))`, engineLockKey).Scan(&locked); err != nil || !locked {
			return err
		}
		groups, err := loadGroups(ctx, tx)
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
		existing := map[string]struct {
			id     string
			active bool
		}{}
		rows, err = tx.Query(ctx, `SELECT id::text, category_id, region, unit, status NOT IN ('dismissed') FROM opportunities WHERE status <> 'closed'`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, c, r, u string
			var active bool
			if err := rows.Scan(&id, &c, &r, &u, &active); err != nil {
				rows.Close()
				return err
			}
			existing[groupKey(c, r, u)] = struct {
				id     string
				active bool
			}{id, active}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		keys := make([]string, 0, len(groups))
		for k := range groups {
			keys = append(keys, k)
		}
		slices.Sort(keys) // deterministic order: stable codes and lock order
		for _, k := range keys {
			g := groups[k]
			if ex, ok := existing[k]; ok {
				if ex.active {
					if err := refreshOpportunity(ctx, tx, ex.id, g); err != nil {
						return err
					}
				}
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
		return nil
	})
}

// loadGroups reads the open listings into groups.
// ponytail: a full scan of open listings per pass; make it incremental (listings.updated_at watermark) when the open
// book passes ~100k rows.
func loadGroups(ctx context.Context, q dbtx) (map[string]*gapGroup, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, kind, category_id, location, unit, item, quantity::float8, coalesce(price_idr, 0), coalesce(budget_idr, 0), owner_party_id::text
		FROM listings WHERE status IN ('open','matched','available','in_market') AND quantity > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := map[string]*gapGroup{}
	for rows.Next() {
		var id, kind, cat, location, unit, item, party string
		var qty float64
		var price, budget int64
		if err := rows.Scan(&id, &kind, &cat, &location, &unit, &item, &qty, &price, &budget, &party); err != nil {
			return nil, err
		}
		region := regionOf(location)
		if region == "" {
			continue
		}
		k := groupKey(cat, region, unit)
		g := groups[k]
		if g == nil {
			g = &gapGroup{category: cat, region: region, unit: unit, buyers: map[string]bool{}, suppliers: map[string]bool{}, items: map[string]int{}}
			groups[k] = g
		}
		g.listings = append(g.listings, id)
		if g.items[item] == 0 {
			g.itemOrder = append(g.itemOrder, item)
		}
		g.items[item]++
		if kind == "demand" {
			g.demand += qty
			g.demandQty += qty
			g.demandBudgetIdr += float64(budget)
			g.buyers[party] = true
		} else {
			g.supply += qty
			g.supplyValue += qty * float64(price)
			g.suppliers[party] = true
		}
	}
	return groups, rows.Err()
}

// refreshOpportunity sets the engine totals plus the platform contributions that are not already among the group's
// listings (POST /me/opportunities/{id}/join), only when something changed.
func refreshOpportunity(ctx context.Context, tx pgx.Tx, id string, g *gapGroup) error {
	_, err := tx.Exec(ctx, `
		WITH n AS (
		  SELECT ($2::numeric + coalesce((SELECT sum(quantity) FROM opportunity_participants p WHERE p.opportunity_id = $1 AND p.role = 'buyer'
		                                  AND (p.listing_id IS NULL OR NOT p.listing_id = ANY($5::uuid[]))), 0))::numeric(18,3) AS d,
		         ($3::numeric + coalesce((SELECT sum(quantity) FROM opportunity_participants p WHERE p.opportunity_id = $1 AND p.role = 'supplier'
		                                  AND (p.listing_id IS NULL OR NOT p.listing_id = ANY($5::uuid[]))), 0))::numeric(18,3) AS s,
		         ($4::int + (SELECT count(*) FROM opportunity_participants p WHERE p.opportunity_id = $1 AND NOT p.party_id = ANY($6::uuid[])))::int AS c)
		UPDATE opportunities o SET demand_value = n.d, supply_value = n.s, participant_count = n.c,
		       potential_value_idr = greatest(o.potential_value_idr, $7)
		FROM n WHERE o.id = $1
		  AND (o.demand_value, o.supply_value, o.participant_count) IS DISTINCT FROM (n.d::numeric, n.s::numeric, n.c)`,
		id, g.demand, g.supply, len(g.parties()), g.listings, g.parties(), int64(jsRound(math.Max(g.demand, g.supply)*g.unitPrice())))
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
	fact, _ := json.Marshal(map[string]any{"categoryId": g.category, "region": g.region, "kind": kind})
	if err := emit(ctx, tx, "opportunity.detected", id, fact); err != nil {
		return err
	}
	if err := emitActivity(ctx, tx, "opportunity_detected", "Opportunity baru: "+d.title, &d.potential, nil); err != nil {
		return err
	}
	// Interested: owners of the listings in the group, and users whose preferences name the category (and the region,
	// when they set any locations).
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
	// ponytail: one notification per interested user inside the pass; batch it when a category has thousands of fans.
	for _, u := range ids {
		if err := notify(ctx, tx, u, notification{Type: "opportunity_detected", Title: "Opportunity baru cocok untukmu", Body: body,
			Href: "/opportunities/" + id}); err != nil {
			return err
		}
	}
	return nil
}

// emitActivity appends one public activity event (ActivityEvent) for the ClickHouse feed (topic `activity`) and pushes
// it on the public:activity channel. Other areas call it from their write paths (see the report / docs).
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
