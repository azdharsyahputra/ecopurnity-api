package server

import (
	"math"
	"slices"
	"time"
)

// Two-sided trade rules (PRD F6), ported from the frontend's src/domain/trade.ts and src/domain/contract.ts (tests
// ported too: trade_rules_test.go). Pure functions: the engine (trade_engine.go) loads a tradeState and asks them.

const (
	vatRate     = 0.11 // PPN, paid by the buyer on top of the subtotal
	day         = 24 * time.Hour
	carrierDflt = "Armada supplier"
)

// termsDays: net terms payment window after receipt (escrow pays before shipping).
var termsDays = map[string]int{"escrow": 0, "net14": 14, "net30": 30}

var tradeActionLabel = map[string]string{
	"accept_agreement": "Setujui agreement",
	"issue_invoice":    "Terbitkan invoice",
	"pay":              "Bayar",
	"ship":             "Jadwalkan pengiriman",
	"upload_proof":     "Konfirmasi terkirim",
	"confirm_receipt":  "Periksa & terima barang",
	"cancel":           "Batalkan",
	"dispute":          "Ajukan dispute",
	"add_evidence":     "Kirim bukti",
	"review":           "Beri ulasan",
}

type tradeState struct {
	Status, Terms  string
	Agreement      map[string]bool // side -> accepted
	UnscheduledQty float64         // quantity not yet on a shipment
	OpenShipments  int             // scheduled or in transit
	Reviewed       map[string]bool // side -> reviewed
}

// timelineFor is the happy-path order for the progress timeline.
func timelineFor(terms string) []string {
	if terms == "escrow" {
		return []string{"agreement", "invoiced", "paid", "fulfilling", "delivered", "completed"}
	}
	return []string{"agreement", "invoiced", "fulfilling", "delivered", "accepted", "completed"}
}

// tradeActions lists what `side` may do now, in the frontend's order.
func tradeActions(s tradeState, side string) []string {
	escrow := s.Terms == "escrow"
	payFrom := "accepted" // net terms: pay after receipt
	if escrow {
		payFrom = "invoiced"
	}
	in := func(xs ...string) bool { return slices.Contains(xs, s.Status) }
	var out []string
	add := func(a string, ok bool) {
		if ok {
			out = append(out, a)
		}
	}
	add("accept_agreement", s.Status == "agreement" && !s.Agreement[side])
	add("issue_invoice", s.Status == "agreement" && side == "supplier" && s.Agreement["buyer"] && s.Agreement["supplier"])
	add("pay", side == "buyer" && s.Status == payFrom)
	add("ship", side == "supplier" && s.UnscheduledQty > 0 && (escrow && in("paid", "fulfilling") || !escrow && in("invoiced", "fulfilling")))
	add("upload_proof", side == "supplier" && s.Status == "fulfilling" && s.OpenShipments > 0)
	add("confirm_receipt", side == "buyer" && s.Status == "delivered")
	add("cancel", in("agreement", "invoiced"))
	add("dispute", escrow && in("paid", "fulfilling", "delivered") || !escrow && in("fulfilling", "delivered", "accepted"))
	add("add_evidence", s.Status == "disputed")
	add("review", s.Status == "completed" && !s.Reviewed[side])
	return out
}

// nextStatus is the status after an action; allDelivered for upload_proof, qc for confirm_receipt.
func nextStatus(s tradeState, action string, allDelivered bool, qc string) string {
	escrow := s.Terms == "escrow"
	switch action {
	case "issue_invoice":
		return "invoiced"
	case "pay":
		if escrow {
			return "paid"
		}
		return "completed"
	case "ship":
		return "fulfilling"
	case "upload_proof":
		if allDelivered {
			return "delivered"
		}
		return "fulfilling"
	case "confirm_receipt":
		if qc == "rejected" {
			return "disputed"
		}
		if escrow {
			return "completed"
		}
		return "accepted"
	case "cancel":
		return "cancelled"
	case "dispute":
		return "disputed"
	}
	return s.Status
}

// money is one invoice's breakdown: the buyer pays subtotal + PPN; fees come out of the supplier's side.
type money struct {
	Subtotal, VAT, BuyerPays, PlatformFee, MakerFee, SupplierReceives int64
}

func round(v float64) int64 { return int64(math.Round(v)) }

func breakdown(subtotal int64, platformRate, makerRate float64) money {
	m := money{Subtotal: subtotal, VAT: round(float64(subtotal) * vatRate), PlatformFee: round(float64(subtotal) * platformRate),
		MakerFee: round(float64(subtotal) * makerRate)}
	m.BuyerPays = subtotal + m.VAT
	m.SupplierReceives = m.BuyerPays - m.PlatformFee - m.MakerFee
	return m
}

// partialRefund is owed to the buyer when QC accepts fewer units than were paid for (escrow only): the short
// quantity's subtotal plus its PPN.
func partialRefund(unitPrice int64, paidQty, acceptedQty float64) int64 {
	short := math.Max(0, paidQty-acceptedQty)
	return breakdown(round(short*float64(unitPrice)), 0, 0).BuyerPays
}

// ── Standing contracts (src/domain/contract.ts) ─────────────────

var contractEvery = map[string]string{"weekly": "Tiap minggu", "biweekly": "Tiap 2 minggu", "monthly": "Tiap bulan"}

func nextRun(from time.Time, every string) time.Time {
	switch every {
	case "monthly":
		return from.AddDate(0, 1, 0)
	case "weekly":
		return from.AddDate(0, 0, 7)
	}
	return from.AddDate(0, 0, 14)
}

func contractActions(status, proposedBy, side string) []string {
	switch status {
	case "proposed":
		if side == proposedBy {
			return []string{"end"}
		}
		return []string{"accept", "decline"}
	case "active":
		if side == "buyer" {
			return []string{"run_now", "pause", "end"}
		}
		return []string{"pause", "end"}
	case "paused":
		return []string{"resume", "end"}
	}
	return []string{}
}

// contractTransition returns the status after action, or "" when it is not allowed from status.
func contractTransition(status, action string) string {
	to := map[string]struct {
		from []string
		to   string
	}{
		"accept": {[]string{"proposed"}, "active"}, "decline": {[]string{"proposed"}, "declined"},
		"pause": {[]string{"active"}, "paused"}, "resume": {[]string{"paused"}, "active"},
		"end": {[]string{"proposed", "active", "paused"}, "ended"}, "run_now": {[]string{"active"}, "active"},
	}
	if t, ok := to[action]; ok && slices.Contains(t.from, status) {
		return t.to
	}
	return ""
}

func otherSide(side string) string {
	if side == "buyer" {
		return "supplier"
	}
	return "buyer"
}
