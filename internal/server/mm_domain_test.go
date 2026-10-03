package server

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

// Ports of src/domain/settlement.test.ts and marketRules.test.ts.

func TestSplitProRata(t *testing.T) {
	q := func(s []share) (out []float64) {
		for _, x := range s {
			out = append(out, x.Quantity)
		}
		return
	}
	if got := q(splitProRata(1000, []member{{"a", 2}, {"b", 1}, {"c", 1}})); fmt.Sprint(got) != "[500 250 250]" {
		t.Fatal(got)
	}
	odd := q(splitProRata(10, []member{{"a", 1}, {"b", 1}, {"c", 1}}))
	sort.Float64s(odd)
	if odd[0]+odd[1]+odd[2] != 10 || odd[2]-odd[0] > 1 {
		t.Fatal(odd)
	}
	if got := splitProRata(100, []member{{"a", 0}}); got[0] != (share{ID: "a"}) {
		t.Fatal(got)
	}
	// Pool lot back to its members (splitPool): 60/40 of 50.
	got := splitProRata(50, []member{{"0", 60}, {"1", 40}})
	if got[0].Quantity != 30 || got[0].Share != 0.6 || got[1].Quantity != 20 || got[1].Share != 0.4 {
		t.Fatal(got)
	}
	// The largest remainder gets the leftover unit.
	if got := q(splitProRata(3, []member{{"s", 1}, {"b", 1.0000001}})); fmt.Sprint(got) != "[1 2]" {
		t.Fatal(got)
	}
}

func TestMembersForLot(t *testing.T) {
	m := membersForLot(1000, []member{{"rina", 200}}, []string{"p1", "p2"})
	if fmt.Sprint(m) != "[{rina 200} {p1 400} {p2 400}]" {
		t.Fatal(m)
	}
	if m := membersForLot(100, []member{{"x", 150}}, []string{"p1"}); fmt.Sprint(m) != "[{x 150}]" {
		t.Fatal(m)
	}
}

var baseRules = marketRules{Eligibility: "verified", Visibility: "full", MinStepPct: 1, MinQuantity: 100, MaxQuantity: 5000,
	WindowStart: "2026-10-05", WindowEnd: "2026-10-09", Region: "Jawa Barat", RadiusKm: 75, Award: "highest_price"}

func TestMarketRules(t *testing.T) {
	if d := diffRules(baseRules, baseRules, "kg"); len(d) != 0 {
		t.Fatal(d)
	}
	after := baseRules
	after.MaxQuantity, after.Award = 8000, "score"
	d := diffRules(baseRules, after, "kg")
	if len(d) != 2 || d[0].Field != "Kuantitas maksimum" || *d[0].Before != "5.000 kg per peserta" || d[0].After != "8.000 kg per peserta" ||
		d[1].Field != "Penetapan pemenang" || *d[1].Before != "Harga tertinggi" || d[1].After != "Skor harga 70% + kualitas 30%" {
		t.Fatalf("%+v", d)
	}

	if e := baseRules.validate(); len(e) != 0 {
		t.Fatal(e)
	}
	bad := baseRules
	bad.MinQuantity, bad.MaxQuantity, bad.WindowEnd, bad.RadiusKm, bad.Region = 0, -1, "2026-10-01", 0, " "
	var keys []string
	for k := range bad.validate() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if fmt.Sprint(keys) != "[maxQuantity minQuantity radiusKm region windowEnd]" {
		t.Fatal(keys)
	}

	// New versions only take effect from the next round.
	v := []ruleVersion{{Version: 1, EffectiveFromRound: 1}}
	if activeVersion(v, 0).Version != 1 {
		t.Fatal("v1 before any round")
	}
	v = append(v, ruleVersion{Version: 2, EffectiveFromRound: 4})
	if activeVersion(v, 3).Version != 1 || activeVersion(v, 4).Version != 2 {
		t.Fatal("v2 from round 4")
	}
	v = append(v, ruleVersion{Version: 3, EffectiveFromRound: 4})
	if activeVersion(v, 4).Version != 3 {
		t.Fatal("v3 supersedes v2 for the same round")
	}
	if l := baseRules.labeled("kg"); len(l) != 10 || l[0].Label != "Eligibility" {
		t.Fatal(l)
	}
}

func TestPipelineMovesAndDefaults(t *testing.T) {
	if !canMove("detected", "evaluating") || canMove("forming", "market_live") || canMove("market_live", "dismissed") || !canMove("dismissed", "detected") {
		t.Fatal("moves")
	}
	// Thursday 2026-10-08 in WIB → window from today to Fri 9; on a weekend (Sat 10) → next Mon 12 .. Fri 16.
	if r := defaultRules(1, 1, "x", "dutch_auction", time.Date(2026, 10, 10, 5, 0, 0, 0, time.UTC)); r.WindowStart != "2026-10-12" || r.WindowEnd != "2026-10-16" {
		t.Fatalf("weekend window %+v", r)
	}
	r := defaultRules(1000, 600, "Garut", "collective_procurement", time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC))
	if r.WindowStart != "2026-10-08" || r.WindowEnd != "2026-10-09" || r.MinQuantity != 10 || r.MaxQuantity != 240 || r.Award != "pro_rata" || r.Visibility != "full" {
		t.Fatalf("%+v", r)
	}
	if r := defaultRules(10, 1, "x", "sealed_bid", time.Now()); r.Visibility != "sealed" || r.Award != "score" || r.MinQuantity != 1 || r.MaxQuantity != 1 {
		t.Fatalf("%+v", r)
	}
}
