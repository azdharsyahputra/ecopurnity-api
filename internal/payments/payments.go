// Package payments is the payment gateway seam: Midtrans Core API (midtrans.go) in production and sandbox, Fake
// (fake.go) in development without keys and in tests. The server never talks to Midtrans directly.
//
// Methods (Core API charge): bank_transfer VA (bca, bni, bri, permata, cimb), echannel (Mandiri bill), qris, gopay,
// shopeepay. No credit card yet: a card charge needs a token made in the browser by Midtrans' JS (the card number
// must never reach our servers) plus the 3DS redirect/challenge flow; that is the next step.
package payments

import (
	"context"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"slices"
	"time"
)

// Methods and the banks of bank_transfer.
var (
	Methods = []string{"bank_transfer", "echannel", "qris", "gopay", "shopeepay"}
	Banks   = []string{"bca", "bni", "bri", "permata", "cimb"}
)

// ValidMethod reports whether method (with bank for bank_transfer) can be charged.
func ValidMethod(method, bank string) bool {
	if method == "bank_transfer" {
		return slices.Contains(Banks, bank)
	}
	return slices.Contains(Methods, method)
}

// Expiry is how long a charge stays payable (Midtrans custom_expiry): a transfer needs time to reach a bank app or
// ATM (24 h); a QR or e-wallet push is paid on the spot, and GoPay/QRIS cap it at 15 minutes anyway.
func Expiry(method string) time.Duration {
	if method == "bank_transfer" || method == "echannel" {
		return 24 * time.Hour
	}
	return 15 * time.Minute
}

// Charge asks the gateway for payment instructions.
type Charge struct {
	OrderID       string // unique per attempt, forever (Midtrans rejects a reused order_id)
	Amount        int64  // Rupiah, what the buyer owes
	Method, Bank  string
	CustomerName  string
	CustomerEmail string
}

// Instructions is what the buyer needs to pay a charge.
type Instructions struct {
	TransactionID       string
	VANumber            string // bank_transfer
	BillerCode, BillKey string // echannel (Mandiri bill)
	QRURL               string // qris, gopay: an image URL
	DeeplinkURL         string // gopay, shopeepay
	ExpiresAt           time.Time
}

// Status is the gateway's view of an order (Midtrans GET /v2/{order_id}/status).
type Status struct {
	OrderID           string
	StatusCode        string
	TransactionStatus string // pending | settlement | capture | expire | cancel | deny | failure | refund | ...
	FraudStatus       string
	GrossAmount       string // "1110000.00"
	TransactionID     string
}

// Settled: the money is in (settlement, or a capture the fraud screening accepted).
func (s Status) Settled() bool {
	return s.TransactionStatus == "settlement" || s.TransactionStatus == "capture" && s.FraudStatus == "accept"
}

// ErrNotFound: the gateway does not know the order.
var ErrNotFound = errors.New("payments: unknown order")

// Gateway is a payment provider.
type Gateway interface {
	Charge(ctx context.Context, c Charge) (Instructions, error)
	Status(ctx context.Context, orderID string) (Status, error)
	Cancel(ctx context.Context, orderID string) error
	// VerifySignature checks a notification's signature_key.
	VerifySignature(orderID, statusCode, grossAmount, signature string) bool
}

// Signature is Midtrans' notification signature: hex SHA512(order_id + status_code + gross_amount + server key).
func Signature(orderID, statusCode, grossAmount, key string) string {
	h := sha512.Sum512([]byte(orderID + statusCode + grossAmount + key))
	return hex.EncodeToString(h[:])
}

func verify(orderID, statusCode, grossAmount, signature, key string) bool {
	want := Signature(orderID, statusCode, grossAmount, key)
	return subtle.ConstantTimeCompare([]byte(want), []byte(signature)) == 1
}
