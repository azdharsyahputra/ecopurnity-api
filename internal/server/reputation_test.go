package server

import (
	"math"
	"testing"
	"time"
)

// Port of the frontend's src/domain/reputation.test.ts.

func repAt(day float64) time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(day * 24 * float64(time.Hour)))
}

func repFixture(id, status string, day float64, cp string, due float64, dispute bool) repTx {
	if cp == "" {
		cp = id
	}
	if due == 0 {
		due = day + 10
	}
	t := repTx{ID: id, Title: id, Status: status, Counterparty: cp, TotalIdr: 1_000_000,
		CreatedAt: repAt(day), UpdatedAt: repAt(day + 5), DueAt: repAt(due),
		Timeline: []repStep{{"agreement", repAt(day)}, {"invoiced", repAt(day).Add(2 * time.Hour)}}}
	if status == "completed" {
		t.Timeline = append(t.Timeline, repStep{"completed", repAt(day + 5)})
	}
	if dispute {
		t.Dispute = "resolved"
	}
	return t
}

func near(a *float64, b float64) bool { return a != nil && math.Abs(*a-b) < 1e-9 }

func TestReputationBaseline(t *testing.T) {
	if s, _, _ := reputationScore(nil); s != baselineScore {
		t.Fatal(s)
	}
	if s, _, _ := reputationScore([]repTx{repFixture("a", "paid", 0, "", 0, false)}); s != baselineScore {
		t.Fatal(s)
	}
}

func TestReputationBreakdown(t *testing.T) {
	_, b, c := reputationScore([]repTx{
		repFixture("a", "completed", 0, "X", 0, false),
		repFixture("b", "completed", 10, "X", 12, false),
		repFixture("c", "cancelled", 20, "", 0, false),
		repFixture("d", "completed", 30, "", 0, true),
	})
	if c.Transactions != 4 || c.Successful != 3 || c.Disputes != 1 || c.Cancelled != 1 {
		t.Fatalf("counts %+v", c)
	}
	if !near(b.FulfillmentRate, 0.75) || !near(b.OnTimeRate, 2.0/3) || !near(b.CancellationRate, 0.25) ||
		!near(b.DisputeRate, 0.25) || !near(b.RepeatRate, 2.0/3) || b.VolumeIdr != 3_000_000 || !near(b.ResponseHours, 2) {
		t.Fatalf("breakdown %+v", b)
	}
}

func TestReputationCleanBeatsMessy(t *testing.T) {
	clean, _, _ := reputationScore([]repTx{repFixture("a", "completed", 0, "", 0, false), repFixture("b", "completed", 5, "", 0, false)})
	messy, _, _ := reputationScore([]repTx{repFixture("a", "completed", 0, "", 0, false), repFixture("b", "cancelled", 5, "", 0, false),
		repFixture("c", "completed", 9, "", 0, true)})
	if clean <= messy || clean > 100 {
		t.Fatal(clean, messy)
	}
}

func TestReputationReport(t *testing.T) {
	r := reputationReport([]repTx{repFixture("a", "completed", 0, "", 0, false), repFixture("b", "cancelled", 40, "", 0, false)},
		time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if len(r.Trend) != 12 || r.Trend[11].Month != "2026-06" || r.Trend[0].Month != "2025-07" || r.Trend[0].Score != baselineScore {
		t.Fatalf("trend %+v", r.Trend)
	}
	if len(r.Events) != 2 || r.Events[0].Id != "b" || r.Events[1].Id != "a" {
		t.Fatalf("events %+v", r.Events)
	}
	if r.Events[0].Delta >= 0 || r.Events[1].Score-r.Events[1].Delta != baselineScore {
		t.Fatalf("deltas %+v", r.Events)
	}
}

func TestReputationReviews(t *testing.T) {
	five, one := 5.0, 1.0
	good := repFixture("a", "completed", 0, "", 0, false)
	good.Rating = &five
	bad := good
	bad.Rating = &one
	gs, gb, _ := reputationScore([]repTx{good})
	bs, _, _ := reputationScore([]repTx{bad})
	if !near(gb.RatingAvg, 5) || gb.RatingCount != 1 || gs <= bs {
		t.Fatalf("good %d %+v bad %d", gs, gb, bs)
	}
	if _, b, _ := reputationScore([]repTx{repFixture("a", "completed", 0, "", 0, false)}); b.RatingAvg != nil {
		t.Fatal("rating without reviews")
	}
}

func TestReputationGate(t *testing.T) {
	for _, c := range []struct {
		score, trades int
		ok            bool
	}{{80, 0, true}, {40, 0, true}, {80, 3, true}, {79, 3, false}, {95, 1, true}} {
		if ok, detail := reputationGate(c.score, c.trades); ok != c.ok || detail == "" {
			t.Errorf("reputationGate(%d, %d) = %v %q, want %v", c.score, c.trades, ok, detail, c.ok)
		}
	}
}
