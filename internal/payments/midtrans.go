package payments

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/midtrans/midtrans-go"
	"github.com/midtrans/midtrans-go/coreapi"
)

// Midtrans is the Core API gateway (github.com/midtrans/midtrans-go). The SDK's own logger is off: in sandbox it
// prints request headers, including the Basic auth made from the server key.
type Midtrans struct {
	c   coreapi.Client
	key string
}

// wib: Midtrans reports times (expiry_time) in Jakarta time without a zone.
var wib = time.FixedZone("WIB", 7*3600)

// NewMidtrans: hc nil uses a client with a 20 s timeout (tests pass one that points at an httptest server).
func NewMidtrans(serverKey string, production bool, hc *http.Client) *Midtrans {
	env := midtrans.Sandbox
	if production {
		env = midtrans.Production
	}
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	m := &Midtrans{key: serverKey}
	m.c.New(serverKey, env)
	m.c.HttpClient = &midtrans.HttpClientImplementation{HttpClient: hc, Logger: &midtrans.LoggerImplementation{LogLevel: midtrans.NoLogging}}
	return m
}

func (m *Midtrans) Charge(_ context.Context, c Charge) (Instructions, error) {
	// ponytail: the SDK ignores request contexts (it drops req.WithContext); the http.Client timeout bounds each call.
	req := &coreapi.ChargeReq{
		PaymentType:        coreapi.CoreapiPaymentType(c.Method),
		TransactionDetails: midtrans.TransactionDetails{OrderID: c.OrderID, GrossAmt: c.Amount},
		CustomerDetails:    &midtrans.CustomerDetails{FName: c.CustomerName, Email: c.CustomerEmail},
		CustomExpiry:       &coreapi.CustomExpiry{ExpiryDuration: int(Expiry(c.Method).Minutes()), Unit: "minute"},
	}
	switch c.Method {
	case "bank_transfer":
		req.BankTransfer = &coreapi.BankTransferDetails{Bank: midtrans.Bank(c.Bank)}
	case "echannel":
		req.EChannel = &coreapi.EChannelDetail{BillInfo1: "Pembayaran:", BillInfo2: c.OrderID}
	case "qris":
		req.Qris = &coreapi.QrisDetails{Acquirer: "gopay"}
	case "gopay":
		req.Gopay = &coreapi.GopayDetails{}
	case "shopeepay":
		req.ShopeePay = &coreapi.ShopeePayDetails{}
	default:
		return Instructions{}, fmt.Errorf("midtrans: unsupported method %q", c.Method)
	}
	res, e := m.c.ChargeTransaction(req)
	if e != nil {
		return Instructions{}, fmt.Errorf("midtrans charge: %s", e.GetMessage())
	}
	if res.StatusCode != "200" && res.StatusCode != "201" {
		return Instructions{}, fmt.Errorf("midtrans charge: %s %s %v", res.StatusCode, res.StatusMessage, res.ValidationMessages)
	}
	in := Instructions{TransactionID: res.TransactionID, BillerCode: res.BillerCode, BillKey: res.BillKey, VANumber: res.PermataVaNumber}
	if len(res.VaNumbers) > 0 {
		in.VANumber = res.VaNumbers[0].VANumber
	}
	for _, a := range res.Actions {
		switch a.Name {
		case "generate-qr-code":
			in.QRURL = a.URL
		case "deeplink-redirect":
			in.DeeplinkURL = a.URL
		}
	}
	in.ExpiresAt = time.Now().Add(Expiry(c.Method))
	if t, err := time.ParseInLocation(time.DateTime, res.ExpiryTime, wib); err == nil {
		in.ExpiresAt = t
	}
	return in, nil
}

func (m *Midtrans) Status(_ context.Context, orderID string) (Status, error) {
	res, e := m.c.CheckTransaction(orderID)
	if e != nil {
		if e.StatusCode == http.StatusNotFound {
			return Status{}, ErrNotFound
		}
		return Status{}, fmt.Errorf("midtrans status: %s", e.GetMessage())
	}
	return Status{OrderID: res.OrderID, StatusCode: res.StatusCode, TransactionStatus: res.TransactionStatus, FraudStatus: res.FraudStatus,
		GrossAmount: res.GrossAmount, TransactionID: res.TransactionID}, nil
}

func (m *Midtrans) Cancel(_ context.Context, orderID string) error {
	res, e := m.c.CancelTransaction(orderID)
	if e != nil {
		return fmt.Errorf("midtrans cancel: %s", e.GetMessage())
	}
	if res.StatusCode != "200" {
		return fmt.Errorf("midtrans cancel: %s %s", res.StatusCode, res.StatusMessage)
	}
	return nil
}

func (m *Midtrans) VerifySignature(orderID, statusCode, grossAmount, signature string) bool {
	return verify(orderID, statusCode, grossAmount, signature, m.key)
}
