package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
	"github.com/azdharsyahputra/ecopurnity-api/internal/payments"
)

var (
	errPayViaGateway = &Error{Status: http.StatusConflict, Code: "payment_required", Message: "Bayar lewat tombol Bayar: pilih metode pembayaran (virtual account, QRIS, atau e-wallet)"}
	errGateway       = &Error{Status: http.StatusBadGateway, Code: "payment_gateway", Message: "Gateway pembayaran sedang bermasalah, coba lagi sebentar lagi"}
	errAlreadyPaid   = &Error{Status: http.StatusConflict, Code: "already_paid", Message: "Pembayaran sebelumnya sudah diterima"}
	errNoPending     = &Error{Status: http.StatusConflict, Code: "invalid_transition", Message: "Tidak ada pembayaran yang menunggu"}
)

var paymentClosedLabel = map[string]string{"expire": "Pembayaran kedaluwarsa", "cancel": "Pembayaran dibatalkan", "deny": "Pembayaran ditolak", "failure": "Pembayaran gagal"}

func methodLabel(method, bank string) string {
	switch method {
	case "bank_transfer":
		return "VA " + strings.ToUpper(bank)
	case "echannel":
		return "Mandiri Bill"
	}
	return map[string]string{"qris": "QRIS", "gopay": "GoPay", "shopeepay": "ShopeePay"}[method]
}

func tradeSide(ctx context.Context, q dbtx, id, party string) (string, error) {
	var side string
	if party == "" || !isUUID(id) {
		return "", errTradeNotFound
	}
	err := q.QueryRow(ctx, `
		SELECT CASE WHEN buyer_party_id = $2 THEN 'buyer' ELSE 'supplier' END FROM trades
		WHERE id = $1 AND $2 IN (buyer_party_id, supplier_party_id)`, id, party).Scan(&side)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errTradeNotFound
	}
	return side, err
}

type payer struct {
	party, side, userID, label string
	orgID                      *string
}

type payerFunc func(q dbtx) (payer, error)

func myPayer(ctx context.Context, tradeID string) payerFunc {
	return func(q dbtx) (payer, error) {
		sess, err := requireUser(ctx)
		if err != nil {
			return payer{}, err
		}
		party, err := myPartyID(ctx, q, sess.UserID)
		if err != nil {
			return payer{}, err
		}
		side, err := tradeSide(ctx, q, tradeID, party)
		return payer{party, side, sess.UserID, sess.Name, nil}, err
	}
}

func orgPayer(ctx context.Context, orgID, tradeID string, write bool) payerFunc {
	return func(q dbtx) (payer, error) {
		c, err := orgAccess(ctx, q, orgID)
		if err != nil {
			return payer{}, err
		}
		if !write {
			if err := c.need("transactions", "view"); err != nil {
				return payer{}, err
			}
		}
		party, err := orgPartyID(ctx, q, c.OrgID)
		if err != nil {
			return payer{}, err
		}
		side, err := tradeSide(ctx, q, tradeID, party)
		if err != nil {
			return payer{}, err
		}
		if msg := txDeniedReason(c.Perms, c.Role, c.RoleLabel, "pay"); write && msg != "" {
			return payer{}, &Error{Status: http.StatusForbidden, Code: "forbidden", Message: msg}
		}
		return payer{party, side, c.sess.UserID, c.actor(), &c.OrgID}, nil
	}
}

const paymentCols = `id::text, trade_id::text, order_id, method, bank, gross_amount_idr, status, va_number, biller_code, bill_key,
	qr_url, deeplink_url, expires_at, created_at, paid_at`

func scanPayment(row pgx.Row) (api.Payment, error) {
	var p api.Payment
	err := row.Scan(&p.Id, &p.TransactionId, &p.OrderId, &p.Method, &p.Bank, &p.AmountIdr, &p.Status, &p.VaNumber, &p.BillerCode, &p.BillKey,
		&p.QrUrl, &p.DeeplinkUrl, &p.ExpiresAt, &p.CreatedAt, &p.PaidAt)
	return p, err
}

type paymentRow struct {
	ID, TradeID, TradeCode, TradeTitle, OrderID, Method, Bank, Status, PayerParty, PayerLabel, CreatedBy string
	OrgID                                                                                                *string
	Amount                                                                                               int64
	ExpiresAt                                                                                            time.Time
}

func lockPayment(ctx context.Context, tx pgx.Tx, where string, args ...any) (r paymentRow, ok bool, err error) {
	var tradeID string
	if err = tx.QueryRow(ctx, `SELECT p.trade_id::text FROM payments p WHERE `+where+` LIMIT 1`, args...).Scan(&tradeID); errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	} else if err != nil {
		return r, false, err
	}
	if _, err = tx.Exec(ctx, `SELECT 1 FROM trades WHERE id = $1 FOR UPDATE`, tradeID); err != nil {
		return r, false, err
	}
	err = tx.QueryRow(ctx, `
		SELECT p.id::text, p.trade_id::text, t.code, t.title, p.order_id, p.method, coalesce(p.bank, ''), p.status, p.payer_party_id::text,
		       p.payer_label, p.created_by::text, pp.org_id::text, p.gross_amount_idr, p.expires_at
		FROM payments p JOIN trades t ON t.id = p.trade_id JOIN parties pp ON pp.id = p.payer_party_id
		WHERE `+where+` LIMIT 1 FOR UPDATE OF p`, args...).Scan(&r.ID, &r.TradeID, &r.TradeCode, &r.TradeTitle, &r.OrderID, &r.Method, &r.Bank, &r.Status,
		&r.PayerParty, &r.PayerLabel, &r.CreatedBy, &r.OrgID, &r.Amount, &r.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

func (s *Server) applyPaymentStatus(ctx context.Context, tx pgx.Tx, r paymentRow, st payments.Status) (string, error) {
	switch {
	case st.Settled():
		if r.Status == "settlement" {
			return r.Status, nil
		}

		if strings.TrimSuffix(st.GrossAmount, ".00") != strconv.FormatInt(r.Amount, 10) {
			s.Log.Error("payment settled with a different amount: not applied", "order", r.OrderID, "gateway", st.GrossAmount, "expected", r.Amount)
			return r.Status, nil
		}
		if _, err := tx.Exec(ctx, `UPDATE payments SET status = 'settlement', paid_at = now(), gateway_transaction_id = coalesce(nullif($2, ''), gateway_transaction_id) WHERE id = $1`,
			r.ID, st.TransactionID); err != nil {
			return "", err
		}
		t, err := lockTrade(ctx, tx, r.TradeID)
		if err != nil {
			return "", err
		}
		if t.Party["buyer"] != r.PayerParty || !slices.Contains(tradeActions(t.state(), "buyer"), "pay") {

			s.Log.Error("payment settled but the invoice is no longer payable: refund it at Midtrans", "order", r.OrderID, "trade", r.TradeCode)
			return "settlement", emitTradeUpdated(ctx, tx, r.TradeID)
		}
		ref := "Midtrans " + methodLabel(r.Method, r.Bank) + " · " + r.OrderID
		return "settlement", applyTradeAction(ctx, tx, r.TradeID, tradeActor{Side: "buyer", UserID: &r.CreatedBy, Name: r.PayerLabel, OrgID: r.OrgID},
			api.TradeActionInput{Action: "pay", Note: &ref})
	case paymentClosedLabel[st.TransactionStatus] != "":
		if r.Status != "pending" {
			return r.Status, nil
		}
		if _, err := tx.Exec(ctx, `UPDATE payments SET status = $2 WHERE id = $1`, r.ID, st.TransactionStatus); err != nil {
			return "", err
		}
		return st.TransactionStatus, fanoutTrade(ctx, tx, r.TradeID, "buyer", notification{Type: "payment",
			Title: r.TradeCode + ": " + paymentClosedLabel[st.TransactionStatus], Body: "Pilih metode lagi untuk membayar · " + r.TradeTitle,
			Href: "/app/transactions/" + r.TradeID})
	}
	return r.Status, nil
}

func (s *Server) closeAtGateway(ctx context.Context, tx pgx.Tx, r paymentRow) (string, error) {
	cerr := s.Payments.Cancel(ctx, r.OrderID)
	if cerr != nil {
		st, err := s.Payments.Status(ctx, r.OrderID)
		switch {
		case errors.Is(err, payments.ErrNotFound):
		case err != nil || st.TransactionStatus == "pending":
			s.Log.Error("payment cancel", "order", r.OrderID, "err", cerr, "status_err", err)
			return "", errGateway
		default:
			return s.applyPaymentStatus(ctx, tx, r, st)
		}
	}
	_, err := tx.Exec(ctx, `UPDATE payments SET status = 'cancel' WHERE id = $1`, r.ID)
	return "cancel", err
}

func (s *Server) createPayment(ctx context.Context, tradeID string, in api.PaymentInput, who payerFunc) (api.Payment, error) {
	var out api.Payment
	paid := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		p, err := who(tx)
		if err != nil {
			return err
		}
		method, bank := string(in.Method), ""
		if in.Bank != nil && method == "bank_transfer" {
			bank = string(*in.Bank)
		}
		if !payments.ValidMethod(method, bank) {
			return fieldErr("bank", "Pilih bank untuk virtual account")
		}

		t, err := lockTrade(ctx, tx, tradeID)
		if err != nil {
			return err
		}
		if p.side != "buyer" || !slices.Contains(tradeActions(t.state(), "buyer"), "pay") {
			return errInvalidTransition
		}
		var invoiceID string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM invoices WHERE trade_id = $1`, t.ID).Scan(&invoiceID); err != nil {
			return err
		}
		if old, ok, err := lockPayment(ctx, tx, `p.invoice_id = $1 AND p.status = 'pending'`, invoiceID); err != nil {
			return err
		} else if ok {
			st, err := s.closeAtGateway(ctx, tx, old)
			if err != nil {
				return err
			}
			if paid = st == "settlement"; paid {
				return nil
			}
		}
		var attempt int
		if err := tx.QueryRow(ctx, `SELECT count(*) + 1 FROM payments WHERE invoice_id = $1`, invoiceID).Scan(&attempt); err != nil {
			return err
		}
		var name, email string
		if err := tx.QueryRow(ctx, `SELECT name, email FROM users WHERE id = $1`, p.userID).Scan(&name, &email); err != nil {
			return err
		}

		orderID := fmt.Sprintf("%s-%d-%s", t.Code, attempt, strings.ToLower(rand.Text()[:4]))
		amount := breakdown(t.Total, t.PlatformRate, t.MakerRate).BuyerPays
		ins, err := s.Payments.Charge(ctx, payments.Charge{OrderID: orderID, Amount: amount, Method: method, Bank: bank, CustomerName: name, CustomerEmail: email})
		if err != nil {
			s.Log.Error("payment charge", "order", orderID, "err", err)
			return errGateway
		}
		if method == "echannel" {
			bank = "mandiri"
		}
		out, err = scanPayment(tx.QueryRow(ctx, `
			INSERT INTO payments (trade_id, invoice_id, payer_party_id, payer_label, created_by, method, bank, order_id, gross_amount_idr,
			                      gateway_transaction_id, va_number, biller_code, bill_key, qr_url, deeplink_url, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), $8, $9, nullif($10, ''), nullif($11, ''), nullif($12, ''), nullif($13, ''), nullif($14, ''), nullif($15, ''), $16)
			RETURNING `+paymentCols, t.ID, invoiceID, p.party, p.label, p.userID, method, bank, orderID, amount,
			ins.TransactionID, ins.VANumber, ins.BillerCode, ins.BillKey, ins.QRURL, ins.DeeplinkURL, ins.ExpiresAt))
		if err != nil {
			return err
		}
		return emitTradeUpdated(ctx, tx, t.ID)
	})
	if err == nil && paid {
		err = errAlreadyPaid
	}
	return out, err
}

func (s *Server) currentPayment(ctx context.Context, tradeID string, who payerFunc) (paymentOrNull, error) {
	q := s.DB.Primary()
	p, err := who(q)
	if err != nil {
		return paymentOrNull{}, err
	}
	pay, err := scanPayment(q.QueryRow(ctx, `SELECT `+paymentCols+` FROM payments WHERE trade_id = $1 AND payer_party_id = $2 ORDER BY created_at DESC LIMIT 1`,
		tradeID, p.party))
	if errors.Is(err, pgx.ErrNoRows) {
		return paymentOrNull{}, nil
	}
	return paymentOrNull{&pay}, err
}

func (s *Server) cancelPayment(ctx context.Context, tradeID string, who payerFunc) (api.Payment, error) {
	var out api.Payment
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		p, err := who(tx)
		if err != nil {
			return err
		}
		r, ok, err := lockPayment(ctx, tx, `p.trade_id = $1 AND p.payer_party_id = $2 AND p.status = 'pending'`, tradeID, p.party)
		if err != nil {
			return err
		}
		if !ok {
			return errNoPending
		}
		if _, err := s.closeAtGateway(ctx, tx, r); err != nil {
			return err
		}
		if err := emitTradeUpdated(ctx, tx, r.TradeID); err != nil {
			return err
		}
		out, err = scanPayment(tx.QueryRow(ctx, `SELECT `+paymentCols+` FROM payments WHERE id = $1`, r.ID))
		return err
	})
	return out, err
}

func (s *Server) syncPayment(ctx context.Context, orderID string, notification []byte) error {
	st, gerr := s.Payments.Status(ctx, orderID)
	if gerr != nil && !errors.Is(gerr, payments.ErrNotFound) {
		return gerr
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		r, ok, err := lockPayment(ctx, tx, `p.order_id = $1`, orderID)
		if err != nil || !ok {
			return err
		}
		if notification != nil {
			if _, err := tx.Exec(ctx, `UPDATE payments SET last_notification = $2 WHERE id = $1`, r.ID, notification); err != nil {
				return err
			}
		}
		if gerr != nil || st.TransactionStatus == "pending" {
			if r.Status != "pending" || time.Since(r.ExpiresAt) < time.Minute {
				return nil
			}
			st = payments.Status{TransactionStatus: "expire"}
		}
		_, err = s.applyPaymentStatus(ctx, tx, r, st)
		return err
	})
}

func (s *Server) RunPaymentReconciler(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.ReconcilePayments(ctx); err != nil && ctx.Err() == nil {
			s.Log.Error("payment reconciler", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) ReconcilePayments(ctx context.Context) error {
	rows, err := s.DB.Primary().Query(ctx, `SELECT order_id FROM payments WHERE status = 'pending' ORDER BY updated_at LIMIT 200`)
	if err != nil {
		return err
	}
	orders, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, o := range orders {
		if err := s.syncPayment(ctx, o, nil); err != nil {
			s.Log.Warn("payment sync", "order", o, "err", err)
		}
	}
	return nil
}

type paymentOrNull struct{ p *api.Payment }

func (r paymentOrNull) write(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.p == nil {
		_, err := w.Write([]byte("null\n"))
		return err
	}
	return json.NewEncoder(w).Encode(r.p)
}

func (r paymentOrNull) VisitGetMyTransactionPaymentResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r paymentOrNull) VisitGetOrgTransactionPaymentResponse(w http.ResponseWriter) error {
	return r.write(w)
}

func (s *Server) CreateMyTransactionPayment(ctx context.Context, req api.CreateMyTransactionPaymentRequestObject) (api.CreateMyTransactionPaymentResponseObject, error) {
	p, err := s.createPayment(ctx, req.Id, *req.Body, myPayer(ctx, req.Id))
	if err != nil {
		return nil, err
	}
	return api.CreateMyTransactionPayment201JSONResponse(p), nil
}

func (s *Server) GetMyTransactionPayment(ctx context.Context, req api.GetMyTransactionPaymentRequestObject) (api.GetMyTransactionPaymentResponseObject, error) {
	p, err := s.currentPayment(ctx, req.Id, myPayer(ctx, req.Id))
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Server) CancelMyTransactionPayment(ctx context.Context, req api.CancelMyTransactionPaymentRequestObject) (api.CancelMyTransactionPaymentResponseObject, error) {
	p, err := s.cancelPayment(ctx, req.Id, myPayer(ctx, req.Id))
	if err != nil {
		return nil, err
	}
	return api.CancelMyTransactionPayment200JSONResponse(p), nil
}

func (s *Server) CreateOrgTransactionPayment(ctx context.Context, req api.CreateOrgTransactionPaymentRequestObject) (api.CreateOrgTransactionPaymentResponseObject, error) {
	p, err := s.createPayment(ctx, req.Tid, *req.Body, orgPayer(ctx, req.OrgId, req.Tid, true))
	if err != nil {
		return nil, err
	}
	return api.CreateOrgTransactionPayment201JSONResponse(p), nil
}

func (s *Server) GetOrgTransactionPayment(ctx context.Context, req api.GetOrgTransactionPaymentRequestObject) (api.GetOrgTransactionPaymentResponseObject, error) {
	p, err := s.currentPayment(ctx, req.Tid, orgPayer(ctx, req.OrgId, req.Tid, false))
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Server) CancelOrgTransactionPayment(ctx context.Context, req api.CancelOrgTransactionPaymentRequestObject) (api.CancelOrgTransactionPaymentResponseObject, error) {
	p, err := s.cancelPayment(ctx, req.Tid, orgPayer(ctx, req.OrgId, req.Tid, true))
	if err != nil {
		return nil, err
	}
	return api.CancelOrgTransactionPayment200JSONResponse(p), nil
}

func (s *Server) MidtransNotification(ctx context.Context, req api.MidtransNotificationRequestObject) (api.MidtransNotificationResponseObject, error) {
	b := req.Body
	if !s.Payments.VerifySignature(b.OrderId, b.StatusCode, b.GrossAmount, b.SignatureKey) {
		s.Log.Warn("midtrans notification: bad signature", "order", b.OrderId)
		return nil, &Error{Status: http.StatusForbidden, Code: "forbidden", Message: "Invalid signature"}
	}
	var known bool
	if err := s.DB.Primary().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payments WHERE order_id = $1)`, b.OrderId).Scan(&known); err != nil {
		return nil, err
	}
	if !known {
		s.Log.Info("midtrans notification for an unknown order", "order", b.OrderId)
		return api.MidtransNotification200JSONResponse{Received: true}, nil
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	delete(body, "signature_key")
	if raw, err = json.Marshal(body); err != nil {
		return nil, err
	}
	if err := s.syncPayment(ctx, b.OrderId, raw); err != nil {
		s.Log.Error("midtrans notification", "order", b.OrderId, "err", err)
		return nil, errGateway
	}
	return api.MidtransNotification200JSONResponse{Received: true}, nil
}
