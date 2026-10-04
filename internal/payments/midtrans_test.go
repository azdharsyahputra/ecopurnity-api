package payments

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

type toServer struct{ u *url.URL }

func (t toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r.URL.Scheme, r.URL.Host = t.u.Scheme, t.u.Host
	return http.DefaultTransport.RoundTrip(r)
}

func fakeMidtrans(t *testing.T, h http.HandlerFunc) *Midtrans {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return NewMidtrans("SB-Mid-server-test", false, &http.Client{Transport: toServer{u}})
}

func TestMidtransChargeRequestAndInstructions(t *testing.T) {
	var got map[string]any
	var path, user string
	m := fakeMidtrans(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.Method + " " + r.URL.Path
		user, _, _ = r.BasicAuth()
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		switch got["payment_type"] {
		case "bank_transfer":
			_, _ = io.WriteString(w, `{"status_code":"201","transaction_id":"tx-1","order_id":"TRX-1-1","gross_amount":"1110000.00","transaction_status":"pending",
				"va_numbers":[{"bank":"bca","va_number":"12345678901"}],"expiry_time":"2026-10-04 10:00:00"}`)
		case "echannel":
			_, _ = io.WriteString(w, `{"status_code":"201","transaction_status":"pending","bill_key":"990011","biller_code":"70012"}`)
		case "gopay":
			_, _ = io.WriteString(w, `{"status_code":"201","transaction_status":"pending","actions":[
				{"name":"generate-qr-code","method":"GET","url":"https://api.sandbox.midtrans.com/v2/gopay/tx/qr-code"},
				{"name":"deeplink-redirect","method":"GET","url":"https://simulator.sandbox.midtrans.com/gopay/ui/checkout?ref=x"}]}`)
		default:
			_, _ = io.WriteString(w, `{"status_code":"400","status_message":"One or more parameters in the payload is invalid.","validation_messages":["bank is invalid"]}`)
		}
	})

	in, err := m.Charge(context.Background(), Charge{OrderID: "TRX-1-1", Amount: 1_110_000, Method: "bank_transfer", Bank: "bca", CustomerName: "Rina", CustomerEmail: "rina@example.id"})
	if err != nil {
		t.Fatal(err)
	}
	if path != "POST /v2/charge" || user != "SB-Mid-server-test" {
		t.Fatal(path, user)
	}
	td := got["transaction_details"].(map[string]any)
	if td["order_id"] != "TRX-1-1" || td["gross_amount"] != float64(1_110_000) || got["bank_transfer"].(map[string]any)["bank"] != "bca" {
		t.Fatalf("request: %v", got)
	}
	if ex := got["custom_expiry"].(map[string]any); ex["expiry_duration"] != float64(1440) || ex["unit"] != "minute" {
		t.Fatalf("expiry: %v", ex)
	}
	if got["customer_details"].(map[string]any)["email"] != "rina@example.id" {
		t.Fatal(got["customer_details"])
	}
	if in.VANumber != "12345678901" || in.TransactionID != "tx-1" || !in.ExpiresAt.Equal(time.Date(2026, 10, 4, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("instructions: %+v", in)
	}

	if in, err = m.Charge(context.Background(), Charge{OrderID: "TRX-1-2", Amount: 5, Method: "echannel"}); err != nil || in.BillKey != "990011" || in.BillerCode != "70012" {
		t.Fatal(in, err)
	}
	if got["echannel"].(map[string]any)["bill_info2"] != "TRX-1-2" {
		t.Fatal(got)
	}
	if in, err = m.Charge(context.Background(), Charge{OrderID: "TRX-1-3", Amount: 5, Method: "gopay"}); err != nil ||
		!strings.HasSuffix(in.QRURL, "/qr-code") || !strings.Contains(in.DeeplinkURL, "checkout") || time.Until(in.ExpiresAt) > 16*time.Minute {
		t.Fatal(in, err)
	}
	if ex := got["custom_expiry"].(map[string]any); ex["expiry_duration"] != float64(15) {
		t.Fatal(ex)
	}

	if _, err := m.Charge(context.Background(), Charge{OrderID: "TRX-1-4", Amount: 5, Method: "qris"}); err == nil || !strings.Contains(err.Error(), "bank is invalid") {
		t.Fatal(err)
	}
}

func TestMidtransStatusAndCancel(t *testing.T) {
	m := fakeMidtrans(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v2/TRX-1-1/status":
			_, _ = io.WriteString(w, `{"status_code":"200","order_id":"TRX-1-1","transaction_status":"settlement","fraud_status":"accept","gross_amount":"1110000.00","transaction_id":"tx-1","signature_key":"x"}`)
		case "GET /v2/TRX-1-2/status":
			_, _ = io.WriteString(w, `{"status_code":"201","order_id":"TRX-1-2","transaction_status":"capture","fraud_status":"challenge","gross_amount":"5.00"}`)
		case "GET /v2/nope/status":
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"status_code":"404","status_message":"Transaction doesn't exist."}`)
		case "POST /v2/TRX-1-2/cancel":
			_, _ = io.WriteString(w, `{"status_code":"200","transaction_status":"cancel"}`)
		case "POST /v2/TRX-1-1/cancel":
			_, _ = io.WriteString(w, `{"status_code":"412","status_message":"Merchant cannot modify the status of the transaction"}`)
		}
	})
	st, err := m.Status(context.Background(), "TRX-1-1")
	if err != nil || !st.Settled() || st.GrossAmount != "1110000.00" || st.StatusCode != "200" {
		t.Fatal(st, err)
	}
	if st, _ = m.Status(context.Background(), "TRX-1-2"); st.Settled() {
		t.Fatal("capture under fraud challenge is not settled", st)
	}
	if _, err := m.Status(context.Background(), "nope"); err != ErrNotFound {
		t.Fatal(err)
	}
	if err := m.Cancel(context.Background(), "TRX-1-2"); err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel(context.Background(), "TRX-1-1"); err == nil {
		t.Fatal("412 must fail")
	}
}

func TestSignature(t *testing.T) {
	m := NewMidtrans("key", false, nil)
	sig := Signature("TRX-1-1", "200", "1110000.00", "key")
	if !m.VerifySignature("TRX-1-1", "200", "1110000.00", sig) || m.VerifySignature("TRX-1-1", "200", "1.00", sig) {
		t.Fatal("signature")
	}
}

func TestFake(t *testing.T) {
	f := NewFake(20 * time.Millisecond)
	in, err := f.Charge(context.Background(), Charge{OrderID: "o1", Amount: 10, Method: "bank_transfer", Bank: "bni"})
	if err != nil || !strings.HasPrefix(in.VANumber, "9880") || len(in.VANumber) != 16 {
		t.Fatal(in, err)
	}
	if st, _ := f.Status(context.Background(), "o1"); st.TransactionStatus != "pending" {
		t.Fatal(st)
	}
	time.Sleep(25 * time.Millisecond)
	if st, _ := f.Status(context.Background(), "o1"); !st.Settled() || st.GrossAmount != "10.00" {
		t.Fatal(st)
	}
	if _, err := f.Charge(context.Background(), Charge{OrderID: "o1", Amount: 10, Method: "qris"}); err == nil {
		t.Fatal("reused order id")
	}
}

func TestMidtransSandboxLive(t *testing.T) {
	key := os.Getenv("MIDTRANS_SERVER_KEY")
	if os.Getenv("MIDTRANS_LIVE_TEST") != "1" || key == "" || os.Getenv("MIDTRANS_ENV") == "production" {
		t.Skip("set MIDTRANS_LIVE_TEST=1 and a sandbox MIDTRANS_SERVER_KEY")
	}
	m := NewMidtrans(key, false, nil)
	ctx := context.Background()
	for _, c := range []Charge{{Method: "bank_transfer", Bank: "bca"}, {Method: "qris"}} {
		c.OrderID, c.Amount, c.CustomerName = "ECP-LIVE-"+c.Method+"-"+time.Now().Format("20060102150405.000"), 10_000, "Smoke Test"
		in, err := m.Charge(ctx, c)
		if err != nil {
			t.Fatalf("%s charge: %v", c.Method, err)
		}
		st, err := m.Status(ctx, c.OrderID)
		if err != nil || st.TransactionStatus != "pending" || st.GrossAmount != "10000.00" {
			t.Fatalf("%s status: %+v %v", c.Method, st, err)
		}
		t.Logf("%s: charged (va=%t qr=%t expires in %s), status %s %s", c.Method, in.VANumber != "", in.QRURL != "", time.Until(in.ExpiresAt).Round(time.Minute), st.StatusCode, st.TransactionStatus)
		if err := m.Cancel(ctx, c.OrderID); err != nil {
			t.Fatalf("%s cancel: %v", c.Method, err)
		}
		if st, err = m.Status(ctx, c.OrderID); err != nil || st.TransactionStatus != "cancel" {
			t.Fatalf("%s after cancel: %+v %v", c.Method, st, err)
		}
		t.Logf("%s: cancelled, status %s %s", c.Method, st.StatusCode, st.TransactionStatus)
	}
}
