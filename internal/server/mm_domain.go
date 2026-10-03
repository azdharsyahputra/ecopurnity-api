package server

import (
	"math"
	"sort"
	"strings"
	"time"
)

// Market maker domain rules, ported from the frontend's src/domain/{mm,marketRules,settlement}.ts (tests in
// mm_domain_test.go mirror theirs).

// pipelineMoves: manual moves. market_live is only reached by publishing a market; dismissing needs a reason.
var pipelineMoves = map[string][]string{
	"detected":    {"evaluating", "forming", "dismissed"},
	"evaluating":  {"detected", "forming", "dismissed"},
	"forming":     {"evaluating", "dismissed"},
	"market_live": {},
	"dismissed":   {"detected"},
}

func canMove(from, to string) bool {
	for _, s := range pipelineMoves[from] {
		if s == to {
			return true
		}
	}
	return false
}

var (
	pipelineLabel = map[string]string{"detected": "Detected", "evaluating": "Evaluating", "forming": "Forming",
		"market_live": "Market Live", "dismissed": "Dismissed"}
	marketStatusLabel = map[string]string{"draft": "Draft", "formation": "Formation", "active": "Active", "paused": "Paused",
		"closed": "Closed", "suspended": "Suspended"}
	mechanismLabel = map[string]string{"forward_auction": "Forward auction", "reverse_auction": "Reverse auction",
		"sealed_bid": "Sealed bid", "dutch_auction": "Dutch auction", "direct_market": "Direct market",
		"collective_procurement": "Collective procurement"}
	objectiveLabel = map[string]string{"procurement": "Procurement", "selling": "Selling", "resource_exchange": "Resource exchange",
		"service_exchange": "Service exchange"}
	// roundType: auction type a market's rounds run as (ROUND_TYPE).
	roundType = map[string]string{"forward_auction": "forward", "reverse_auction": "reverse", "sealed_bid": "sealed",
		"dutch_auction": "dutch", "direct_market": "reverse", "collective_procurement": "reverse"}
)

// validate is validateRules: field errors keyed like the form inputs; empty = valid.
func (r marketRules) validate() map[string]string {
	e := map[string]string{}
	if !(r.MinQuantity > 0) {
		e["minQuantity"] = "Harus lebih dari 0"
	}
	if !(r.MaxQuantity >= r.MinQuantity) {
		e["maxQuantity"] = "Tidak boleh di bawah kuantitas minimum"
	}
	if !(r.MinStepPct >= 0 && r.MinStepPct <= 20) {
		e["minStepPct"] = "Antara 0 dan 20%"
	}
	if r.WindowStart == "" {
		e["windowStart"] = "Isi tanggal mulai"
	}
	if r.WindowEnd == "" || r.WindowEnd < r.WindowStart {
		e["windowEnd"] = "Harus setelah tanggal mulai"
	}
	if strings.TrimSpace(r.Region) == "" {
		e["region"] = "Isi wilayah"
	}
	if !(r.RadiusKm > 0) {
		e["radiusKm"] = "Harus lebih dari 0"
	}
	return e
}

// raw is the rule values in RULE_FIELDS order (the order of labeled).
func (r marketRules) raw() []any {
	return []any{r.Eligibility, r.Visibility, r.MinStepPct, r.MinQuantity, r.MaxQuantity, r.WindowStart, r.WindowEnd,
		r.Region, r.RadiusKm, r.Award}
}

// diffRules: field-level before → after with display text; empty when nothing changed.
func diffRules(before, after marketRules, unit string) []change {
	b, a := before.labeled(unit), after.labeled(unit)
	rb, ra := before.raw(), after.raw()
	var out []change
	for i := range rb {
		if rb[i] != ra[i] {
			out = append(out, change{Field: b[i].Label, Before: &b[i].Value, After: a[i].Value})
		}
	}
	return out
}

type ruleVersion struct {
	Version, EffectiveFromRound int
	Rules                       marketRules
	ID                          string
}

// activeVersion: the version governing round `round` (latest that has taken effect); v1 before any round has run.
func activeVersion(versions []ruleVersion, round int) ruleVersion {
	for i := len(versions) - 1; i >= 0; i-- {
		if versions[i].EffectiveFromRound <= round {
			return versions[i]
		}
	}
	return versions[0]
}

// defaultRules: today until this week's Friday (next week's Mon–Fri on a weekend), 1% of demand as minimum order, 40%
// of supply as cap (frontend domain/marketRules.ts).
func defaultRules(demand, supply float64, region, mechanism string, today time.Time) marketRules {
	today = today.In(wib) // the frontend's clock for date-only rule windows (wib: contracts.go)
	weekend := today.Weekday() == time.Saturday || today.Weekday() == time.Sunday
	monday := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))
	start := today
	if weekend {
		monday = monday.AddDate(0, 0, 7)
		start = monday
	}
	visibility := "full"
	if mechanism == "sealed_bid" {
		visibility = "sealed"
	}
	award := map[string]string{"forward_auction": "highest_price", "sealed_bid": "score", "collective_procurement": "pro_rata"}[mechanism]
	if award == "" {
		award = "lowest_price"
	}
	return marketRules{
		Eligibility: "verified_docs", Visibility: visibility, MinStepPct: 1,
		MinQuantity: math.Max(1, math.Round(demand*0.01)), MaxQuantity: math.Max(1, math.Round(supply*0.4)),
		WindowStart: start.Format(time.DateOnly), WindowEnd: monday.AddDate(0, 0, 4).Format(time.DateOnly),
		Region: region, RadiusKm: 75, Award: award,
	}
}

// ── Aggregated settlement (settlement.ts) ────────────────────────

type member struct {
	ID       string
	Quantity float64 // what the member contributed (demand or supply), in lot units
}

type share struct {
	ID              string
	Quantity, Share float64
}

// splitProRata splits `total` units across members in proportion to their contribution, in whole units
// (largest-remainder method, ties go to the bigger contributor). Members with nothing get nothing.
func splitProRata(total float64, members []member) []share {
	out := make([]share, len(members))
	var pool float64
	for i, m := range members {
		out[i].ID = m.ID
		pool += math.Max(0, m.Quantity)
	}
	whole := math.Round(total)
	if pool <= 0 || total <= 0 || whole <= 0 {
		return out
	}
	exact := make([]float64, len(members))
	left := whole
	for i, m := range members {
		exact[i] = math.Max(0, m.Quantity) / pool * total
		out[i].Quantity = math.Floor(exact[i])
		left -= out[i].Quantity
	}
	order := make([]int, len(members))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(x, y int) bool {
		a, b := order[x], order[y]
		ra, rb := exact[a]-out[a].Quantity, exact[b]-out[b].Quantity
		if ra != rb {
			return ra > rb
		}
		return members[a].Quantity > members[b].Quantity
	})
	for _, i := range order {
		if left <= 0 {
			break
		}
		out[i].Quantity++
		left--
	}
	for i := range out {
		out[i].Share = out[i].Quantity / whole
	}
	return out
}

// membersForLot: real contributions first, then the rest of the lot spread evenly over `fillers` (participants
// whose volumes aren't tracked as listings).
func membersForLot(lotQty float64, contributions []member, fillers []string) []member {
	var contributed float64
	for _, c := range contributions {
		contributed += c.Quantity
	}
	out := append([]member{}, contributions...)
	rest := math.Max(0, lotQty-contributed)
	if len(fillers) > 0 && rest > 0 {
		each := rest / float64(len(fillers))
		for _, f := range fillers {
			out = append(out, member{ID: f, Quantity: each})
		}
	}
	return out
}
