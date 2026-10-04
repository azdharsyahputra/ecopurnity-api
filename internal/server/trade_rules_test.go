package server

import (
	"slices"
	"testing"
	"time"
)

func TestTradeActions(t *testing.T) {
	base := tradeState{Status: "agreement", Terms: "escrow", Agreement: map[string]bool{}, UnscheduledQty: 100, Reviewed: map[string]bool{}}
	eq := func(got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	eq(tradeActions(base, "supplier"), []string{"accept_agreement", "cancel"})
	both := base
	both.Agreement = map[string]bool{"buyer": true, "supplier": true}
	if !slices.Contains(tradeActions(both, "supplier"), "issue_invoice") || slices.Contains(tradeActions(both, "buyer"), "issue_invoice") {
		t.Fatal("issue_invoice is the supplier's, after both accepted")
	}

	inv := both
	inv.Status = "invoiced"
	if !slices.Contains(tradeActions(inv, "buyer"), "pay") || slices.Contains(tradeActions(inv, "supplier"), "ship") {
		t.Fatal("escrow: pay, then ship")
	}
	net := inv
	net.Terms = "net30"
	if slices.Contains(tradeActions(net, "buyer"), "pay") || !slices.Contains(tradeActions(net, "supplier"), "ship") {
		t.Fatal("net: ship first")
	}
	acc := net
	acc.Status = "accepted"
	if !slices.Contains(tradeActions(acc, "buyer"), "pay") || nextStatus(net, "pay", false, "") != "completed" {
		t.Fatal("net: pay after acceptance completes")
	}

	f := base
	f.Status, f.UnscheduledQty, f.OpenShipments = "fulfilling", 40, 1
	eq(tradeActions(f, "supplier"), []string{"ship", "upload_proof", "dispute"})
	if nextStatus(f, "upload_proof", false, "") != "fulfilling" || nextStatus(f, "upload_proof", true, "") != "delivered" {
		t.Fatal("upload_proof")
	}
	d := f
	d.Status = "delivered"
	if nextStatus(d, "confirm_receipt", false, "partial") != "completed" || nextStatus(d, "confirm_receipt", false, "rejected") != "disputed" {
		t.Fatal("confirm_receipt escrow")
	}
	d.Terms = "net14"
	if nextStatus(d, "confirm_receipt", false, "accepted") != "accepted" {
		t.Fatal("confirm_receipt net")
	}

	done := base
	done.Status, done.Reviewed = "completed", map[string]bool{"buyer": true}
	eq(tradeActions(done, "buyer"), nil)
	eq(tradeActions(done, "supplier"), []string{"review"})
}

func TestTradeMoney(t *testing.T) {
	b := breakdown(1_000_000, 0.01, 0.005)
	if b != (money{Subtotal: 1_000_000, VAT: 110_000, BuyerPays: 1_110_000, PlatformFee: 10_000, MakerFee: 5_000, SupplierReceives: 1_095_000}) {
		t.Fatalf("%+v", b)
	}
	if partialRefund(1_000, 100, 90) != 11_100 || partialRefund(1_000, 100, 120) != 0 {
		t.Fatal("partial refund")
	}
}

func TestContractRules(t *testing.T) {
	at := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	if got := nextRun(at, "weekly"); !got.Equal(time.Date(2026, 2, 7, 0, 0, 0, 0, time.UTC)) {
		t.Fatal(got)
	}
	if got := nextRun(time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), "monthly"); got.Format("2006-01-02") != "2026-02-10" {
		t.Fatal(got)
	}
	if !slices.Equal(contractActions("proposed", "buyer", "supplier"), []string{"accept", "decline"}) ||
		!slices.Equal(contractActions("proposed", "buyer", "buyer"), []string{"end"}) {
		t.Fatal("only the other side accepts")
	}
	if contractTransition("active", "pause") != "paused" || contractTransition("ended", "resume") != "" || contractTransition("paused", "run_now") != "" {
		t.Fatal("transitions")
	}
}
