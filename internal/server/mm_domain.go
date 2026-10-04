package server

import (
	"math"
	"sort"
	"strings"
	"time"
)

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

	roundType = map[string]string{"forward_auction": "forward", "reverse_auction": "reverse", "sealed_bid": "sealed",
		"dutch_auction": "dutch", "direct_market": "reverse", "collective_procurement": "reverse"}
)

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

func (r marketRules) raw() []any {
	return []any{r.Eligibility, r.Visibility, r.MinStepPct, r.MinQuantity, r.MaxQuantity, r.WindowStart, r.WindowEnd,
		r.Region, r.RadiusKm, r.Award}
}

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

func activeVersion(versions []ruleVersion, round int) ruleVersion {
	for i := len(versions) - 1; i >= 0; i-- {
		if versions[i].EffectiveFromRound <= round {
			return versions[i]
		}
	}
	return versions[0]
}

func defaultRules(demand, supply float64, region, mechanism string, today time.Time) marketRules {
	today = today.In(wib)
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

type member struct {
	ID       string
	Quantity float64
}

type share struct {
	ID              string
	Quantity, Share float64
}

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
