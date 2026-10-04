package payments

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Fake struct {
	Key         string
	SettleAfter time.Duration

	mu     sync.Mutex
	orders map[string]*fakeOrder
}

type fakeOrder struct {
	status  string
	amount  int64
	at, exp time.Time
}

func NewFake(settleAfter time.Duration) *Fake {
	return &Fake{Key: "fake-server-key", SettleAfter: settleAfter, orders: map[string]*fakeOrder{}}
}

var vaPrefix = map[string]string{"bca": "39021", "bni": "9880", "bri": "88608", "permata": "8562", "cimb": "7031"}

func (f *Fake) Charge(_ context.Context, c Charge) (Instructions, error) {
	if !ValidMethod(c.Method, c.Bank) {
		return Instructions{}, fmt.Errorf("fake: unsupported method %q/%q", c.Method, c.Bank)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.orders[c.OrderID]; ok {
		return Instructions{}, fmt.Errorf("fake: order_id %s already used", c.OrderID)
	}
	now := time.Now()
	o := &fakeOrder{status: "pending", amount: c.Amount, at: now, exp: now.Add(Expiry(c.Method))}
	f.orders[c.OrderID] = o
	h := sha256.Sum256([]byte(c.OrderID))
	digits := func(n int) string {
		var b strings.Builder
		for i := range n {
			b.WriteByte('0' + h[i]%10)
		}
		return b.String()
	}
	in := Instructions{TransactionID: "fake-" + digits(12), ExpiresAt: o.exp}
	switch c.Method {
	case "bank_transfer":
		p := vaPrefix[c.Bank]
		in.VANumber = p + digits(16-len(p))
	case "echannel":
		in.BillerCode, in.BillKey = "70012", digits(12)
	case "qris":
		in.QRURL = fakeQR(h)
	case "gopay":
		in.QRURL, in.DeeplinkURL = fakeQR(h), "gojek://gopay/merchanttransfer?tref="+c.OrderID
	case "shopeepay":
		in.DeeplinkURL = "shopeeid://main?order=" + c.OrderID
	}
	return in, nil
}

func (f *Fake) Status(_ context.Context, orderID string) (Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.orders[orderID]
	if !ok {
		return Status{}, ErrNotFound
	}
	if o.status == "pending" && f.SettleAfter > 0 && time.Since(o.at) >= f.SettleAfter {
		o.status = "settlement"
	}
	if o.status == "pending" && time.Now().After(o.exp) {
		o.status = "expire"
	}
	code := map[string]string{"pending": "201", "expire": "407", "deny": "202", "failure": "202"}[o.status]
	if code == "" {
		code = "200"
	}
	return Status{OrderID: orderID, StatusCode: code, TransactionStatus: o.status, FraudStatus: "accept",
		GrossAmount: fmt.Sprintf("%d.00", o.amount), TransactionID: "fake-" + orderID}, nil
}

func (f *Fake) Cancel(_ context.Context, orderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.orders[orderID]
	if !ok {
		return ErrNotFound
	}
	if o.status != "pending" {
		return fmt.Errorf("fake: cannot cancel a %s order", o.status)
	}
	o.status = "cancel"
	return nil
}

func (f *Fake) SetStatus(orderID, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o, ok := f.orders[orderID]; ok {
		o.status = status
	}
}

func (f *Fake) VerifySignature(orderID, statusCode, grossAmount, signature string) bool {
	return verify(orderID, statusCode, grossAmount, signature, f.Key)
}

func fakeQR(h [32]byte) string {
	const n = 25
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="-2 -2 %d %d" shape-rendering="crispEdges"><rect x="-2" y="-2" width="%d" height="%d" fill="#fff"/>`, n+4, n+4, n+4, n+4)
	finder := func(x, y int) bool {
		for _, o := range [][2]int{{0, 0}, {n - 7, 0}, {0, n - 7}} {
			if dx, dy := x-o[0], y-o[1]; dx >= 0 && dx < 7 && dy >= 0 && dy < 7 {
				return dx == 0 || dx == 6 || dy == 0 || dy == 6 || dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4
			}
		}
		return false
	}
	inFinder := func(x, y int) bool { return (x < 8 || x >= n-8) && y < 8 || x < 8 && y >= n-8 }
	for y := range n {
		for x := range n {
			on := finder(x, y) || !inFinder(x, y) && (h[(x*7+y*13)%32]>>((x*3+y)%8))&1 == 1
			if on {
				fmt.Fprintf(&b, `<rect x="%d" y="%d" width="1" height="1"/>`, x, y)
			}
		}
	}
	b.WriteString(`</svg>`)
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(b.String()))
}
