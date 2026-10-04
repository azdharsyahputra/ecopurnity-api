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

var (
	Methods = []string{"bank_transfer", "echannel", "qris", "gopay", "shopeepay"}
	Banks   = []string{"bca", "bni", "bri", "permata", "cimb"}
)

func ValidMethod(method, bank string) bool {
	if method == "bank_transfer" {
		return slices.Contains(Banks, bank)
	}
	return slices.Contains(Methods, method)
}

func Expiry(method string) time.Duration {
	if method == "bank_transfer" || method == "echannel" {
		return 24 * time.Hour
	}
	return 15 * time.Minute
}

type Charge struct {
	OrderID       string
	Amount        int64
	Method, Bank  string
	CustomerName  string
	CustomerEmail string
}

type Instructions struct {
	TransactionID       string
	VANumber            string
	BillerCode, BillKey string
	QRURL               string
	DeeplinkURL         string
	ExpiresAt           time.Time
}

type Status struct {
	OrderID           string
	StatusCode        string
	TransactionStatus string
	FraudStatus       string
	GrossAmount       string
	TransactionID     string
}

func (s Status) Settled() bool {
	return s.TransactionStatus == "settlement" || s.TransactionStatus == "capture" && s.FraudStatus == "accept"
}

var ErrNotFound = errors.New("payments: unknown order")

type Gateway interface {
	Charge(ctx context.Context, c Charge) (Instructions, error)
	Status(ctx context.Context, orderID string) (Status, error)
	Cancel(ctx context.Context, orderID string) error

	VerifySignature(orderID, statusCode, grossAmount, signature string) bool
}

func Signature(orderID, statusCode, grossAmount, key string) string {
	h := sha512.Sum512([]byte(orderID + statusCode + grossAmount + key))
	return hex.EncodeToString(h[:])
}

func verify(orderID, statusCode, grossAmount, signature, key string) bool {
	want := Signature(orderID, statusCode, grossAmount, key)
	return subtle.ConstantTimeCompare([]byte(want), []byte(signature)) == 1
}
