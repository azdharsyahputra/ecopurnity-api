package server

import (
	"testing"
)

// Port of the frontend's src/domain/matching.test.ts, plus the detection rules and the gazetteer.

func baseMatch() matchInput {
	one := 1.0
	return matchInput{CategoryMatch: true, DistanceKm: 10, RadiusKm: 50, Coverage: &one, Confidence: 1}
}

func TestScoreMatch(t *testing.T) {
	score, p := scoreMatch(baseMatch())
	if score != 100 || p.Category+p.Distance+p.Coverage+p.Confidence != score {
		t.Fatalf("perfect: %v %+v", score, p)
	}
	m := baseMatch()
	m.CategoryMatch = false
	if s, _ := scoreMatch(m); s != 100-matchWeights.Category {
		t.Fatalf("category mismatch: %v", s)
	}
	for km, want := range map[float64]float64{50: 25, 100: 13, 150: 0, 900: 0} {
		m := baseMatch()
		m.DistanceKm = km
		if _, p := scoreMatch(m); p.Distance != want {
			t.Fatalf("distance %v: %v, want %v", km, p.Distance, want)
		}
	}
	for _, c := range []struct {
		cov  *float64
		want float64
	}{{ptr(5.0), 25}, {ptr(0.2), 5}, {nil, 13}} {
		m := baseMatch()
		m.Coverage = c.cov
		if _, p := scoreMatch(m); p.Coverage != c.want {
			t.Fatalf("coverage %v: %v", c.cov, p.Coverage)
		}
	}
}

func TestItemCategoryAndValue(t *testing.T) {
	if got := itemCategory("Truk engkel", "2 ton", ptr("agri")); got != "agri" {
		t.Fatal(got)
	}
	if got := itemCategory("Truk engkel", "2 ton", nil); got != "logistics" {
		t.Fatal(got)
	}
	if got := itemCategory("Desain grafis", "Mahir", nil); got != "" {
		t.Fatal(got)
	}
	if estimateMatchValue(500, 200, 1000) != 200_000 || estimateMatchValue(100, 200, 1000) != 100_000 || estimateMatchValue(100, -50, 1000) != 0 {
		t.Fatal("estimateMatchValue")
	}
}

func TestPlaces(t *testing.T) {
	if regionOf("Garut, Jawa Barat") != "Jawa Barat" || regionOf("Kota Medan") != "Sumatera Utara" || regionOf(" Atlantis ") != "Atlantis" {
		t.Fatal("regionOf")
	}
	if d := distanceKm("Bandung", "Jawa Barat"); d != 0 {
		t.Fatalf("same place: %v", d)
	}
	if d := distanceKm("Jakarta", "Bandung"); d < 100 || d > 140 {
		t.Fatalf("Jakarta-Bandung: %v", d)
	}
	if d := distanceKm("Atlantis", "Bali"); d != unknownDistanceKm {
		t.Fatalf("unknown: %v", d)
	}
}

func TestClassify(t *testing.T) {
	group := func(demand, supply float64, buyers, suppliers int) *gapGroup {
		g := &gapGroup{category: "food", region: "Bali", unit: "kg", demand: demand, supply: supply, buyers: map[string]bool{}, suppliers: map[string]bool{}}
		for i := range buyers {
			g.buyers[string(rune('a'+i))] = true
		}
		for i := range suppliers {
			g.suppliers[string(rune('A'+i))] = true
		}
		return g
	}
	cases := []struct {
		g         *gapGroup
		market    bool
		kind, mec string
	}{
		{group(100, 50, 1, 1), false, "market_gap", "reverse_auction"},
		{group(100, 150, 1, 1), false, "market_gap", "forward_auction"},
		{group(300, 0, 3, 0), false, "collective_demand", "collective_procurement"},
		{group(100, 50, 1, 1), true, "supply_gap", "reverse_auction"},
		{group(100, 90, 1, 1), true, "", ""},
		{group(100, 120, 1, 1), true, "capacity_match", "forward_auction"},
		{group(100, 200, 1, 1), true, "capacity_match", "dutch_auction"},
		{group(100, 0, 1, 0), false, "", ""}, // one party: too thin
		{group(0, 100, 0, 2), false, "", ""}, // no demand
	}
	for i, c := range cases {
		kind, mec, ok := classify(c.g, c.market)
		if kind != c.kind || mec != c.mec || ok != (c.kind != "") {
			t.Errorf("case %d: %s %s %v", i, kind, mec, ok)
		}
	}
	d := describe(group(300, 0, 3, 0), "collective_demand", "collective_procurement")
	if d.reason != "3 peserta dengan demand 100% di atas supply: collective procurement paling cepat menemukan harga." {
		t.Fatal(d.reason)
	}
}

func TestMaskName(t *testing.T) {
	for in, want := range map[string]string{"CV Sumber Pangan": "C••• S••• P•••", "Rina W.": "R••• W.", "PT X-9": "P••• X-9"} {
		if got := maskName(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}
