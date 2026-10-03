package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
	"github.com/azdharsyahputra/ecopurnity-api/internal/secure"
)

// Money: double-entry ledger (00004_trade.sql) and personal finance (GET/PUT /me/finance...).
//
// Signs: debit > 0, credit < 0; every journal sums to zero (deferred trigger). Liability accounts (wallet_available,
// escrow, ppn_payable, platform_revenue, maker_commission, payout_pending) carry negative balances; bank_clearing (money
// at the bank) is the platform's asset. Postings per trade action are listed in trade_engine.go; withdrawals: request
// wallet → payout_pending (here), then an admin marks it paid (payout_pending → bank_clearing) or rejects it
// (payout_pending → back to the wallet lines), payouts.go.
//
// Finance (one party):
//   - escrowHeldIdr  = -balance(escrow)                          what the buyer has in escrow
//   - availableIdr   = -(balance(wallet_available) + balance(ppn_payable))   withdrawable: released earnings and refunds;
//     the supplier's collected PPN is paid out with them (the supplier remits it), but booked apart so it is visible
//   - receivableIdr  = supplier's claims not yet released: escrowed invoices (frozen supplier_receives) and net-terms
//     trades delivered/accepted but unpaid. Not a ledger balance: for net terms no money exists yet, and escrowed money
//     already sits in the buyer's escrow account (one amount cannot be in two liability accounts).
//   - withdrawnIdr   = withdrawals not rejected (processing or paid)
//   - entries        = the party's statement, cash view (as the frontend shows it): lines on its accounts grouped per
//     journal and kind; wallet/ppn/commission credits show positive; escrow lines show only when money goes in (as the
//     negative payment), its outflows (release, refund) are not the party's cash and are hidden.

type ledgerLine struct {
	Account string
	Amount  int64
	Kind    string // escrow | payout | refund | payment | withdrawal | fee
}

type journal struct {
	Label        string
	TradeID      *string
	WithdrawalID *string
	CreatedBy    *string
	Lines        []ledgerLine
}

// ledgerAccount returns the account of (party, kind), creating it on first use. party "" = platform account.
func ledgerAccount(ctx context.Context, q dbtx, party, kind string) (string, error) {
	var owner *string
	if party != "" {
		owner = &party
	}
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO ledger_accounts (owner_party_id, kind) VALUES ($1, $2)
		ON CONFLICT (owner_party_id, kind) DO UPDATE SET kind = EXCLUDED.kind
		RETURNING id`, owner, kind).Scan(&id)
	return id, err
}

// post writes one journal; zero lines are dropped. Balance is checked by the database at commit.
func post(ctx context.Context, q dbtx, j journal) error {
	var sum int64
	for _, l := range j.Lines {
		sum += l.Amount
	}
	if sum != 0 {
		return fmt.Errorf("journal %q does not balance: %d", j.Label, sum)
	}
	var id string
	if err := q.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		return err
	}
	for _, l := range j.Lines {
		if l.Amount == 0 {
			continue
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO ledger_entries (journal_id, account_id, amount, kind, label, trade_id, withdrawal_id, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, id, l.Account, l.Amount, l.Kind, j.Label, j.TradeID, j.WithdrawalID, j.CreatedBy); err != nil {
			return err
		}
	}
	return nil
}

// accounts resolves several (party, kind) accounts at once: keys "kind" (platform) or "kind:party".
func accounts(ctx context.Context, q dbtx, keys ...string) (map[string]string, error) {
	out := map[string]string{}
	for _, k := range keys {
		kind, party, _ := strings.Cut(k, ":")
		id, err := ledgerAccount(ctx, q, party, kind)
		if err != nil {
			return nil, err
		}
		out[k] = id
	}
	return out, nil
}

func balance(ctx context.Context, q dbtx, party, kind string) (int64, error) {
	var v int64
	err := q.QueryRow(ctx, `
		SELECT coalesce(sum(e.amount), 0) FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id
		WHERE a.owner_party_id = $1 AND a.kind = $2`, party, kind).Scan(&v)
	return v, err
}

// ── Finance read model ───────────────────────────────────────────

// myPartyID returns the user's party without creating it ("" when the user never traded).
func myPartyID(ctx context.Context, q dbtx, userID string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id::text FROM parties WHERE user_id = $1`, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// financeWithdrawal is the generated Finance.withdrawals element (an inline struct in the spec).
type financeWithdrawal = struct {
	AmountIdr   int                  `json:"amountIdr"`
	At          time.Time            `json:"at"`
	Id          string               `json:"id"`
	PaidAt      *time.Time           `json:"paidAt,omitempty"`
	Reason      *string              `json:"reason,omitempty"`
	Status      api.WithdrawalStatus `json:"status"`
	TransferRef *string              `json:"transferRef,omitempty"`
}

func loadFinance(ctx context.Context, q dbtx, party string) (api.Finance, error) {
	f := api.Finance{Entries: []struct {
		AmountIdr int                    `json:"amountIdr"`
		At        time.Time              `json:"at"`
		Id        string                 `json:"id"`
		Kind      api.FinanceEntriesKind `json:"kind"`
		Label     string                 `json:"label"`
	}{}, Withdrawals: []financeWithdrawal{}}
	if party == "" {
		return f, nil
	}
	var escrow, avail, receivable, withdrawn int64
	if err := q.QueryRow(ctx, `
		SELECT coalesce(-sum(e.amount) FILTER (WHERE a.kind = 'escrow'), 0),
		       coalesce(-sum(e.amount) FILTER (WHERE a.kind IN ('wallet_available','ppn_payable')), 0),
		       (SELECT coalesce(sum(CASE WHEN i.status = 'escrow' THEN i.supplier_receives_idr
		                                 ELSE t.total_idr + round(t.total_idr * 0.11) - round(t.total_idr * t.platform_fee_rate) - round(t.total_idr * t.maker_fee_rate) END), 0)
		          FROM trades t LEFT JOIN invoices i ON i.trade_id = t.id
		         WHERE t.supplier_party_id = $1
		           AND (i.status = 'escrow' OR (t.terms <> 'escrow' AND t.status IN ('delivered','accepted') AND coalesce(i.status, 'unpaid') = 'unpaid'))),
		       (SELECT coalesce(sum(amount_idr), 0) FROM withdrawals WHERE party_id = $1 AND status <> 'rejected')
		FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id
		WHERE a.owner_party_id = $1`, party).Scan(&escrow, &avail, &receivable, &withdrawn); err != nil {
		return f, err
	}
	f.EscrowHeldIdr, f.AvailableIdr, f.ReceivableIdr, f.WithdrawnIdr = int(escrow), int(avail), int(receivable), int(withdrawn)

	var bank, holder, last4 string
	err := q.QueryRow(ctx, `SELECT bank, holder, account_last4 FROM bank_accounts WHERE party_id = $1 AND replaced_at IS NULL`, party).
		Scan(&bank, &holder, &last4)
	switch {
	case err == nil:
		// Only the last 4 digits ever leave the API; the full number is decrypted for payouts only.
		f.Bank = &struct {
			AccountNo string `json:"accountNo"`
			Bank      string `json:"bank"`
			Holder    string `json:"holder"`
		}{AccountNo: "••••" + last4, Bank: bank, Holder: holder}
	case !errors.Is(err, pgx.ErrNoRows):
		return f, err
	}

	rows, err := q.Query(ctx, `SELECT id::text, amount_idr, created_at, status, transfer_ref, paid_at, reject_reason
		FROM withdrawals WHERE party_id = $1 ORDER BY created_at DESC`, party)
	if err != nil {
		return f, err
	}
	for rows.Next() {
		var w financeWithdrawal
		if err := rows.Scan(&w.Id, &w.AmountIdr, &w.At, &w.Status, &w.TransferRef, &w.PaidAt, &w.Reason); err != nil {
			return f, err
		}
		f.Withdrawals = append(f.Withdrawals, w)
	}
	if err := rows.Err(); err != nil {
		return f, err
	}

	// ponytail: newest 500 statement lines; paginate when the contract grows a cursor.
	rows, err = q.Query(ctx, `
		SELECT e.journal_id::text || ':' || e.kind, e.kind, min(e.label), min(e.created_at),
		       sum(CASE WHEN a.kind = 'escrow' THEN e.amount ELSE -e.amount END)
		FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id
		WHERE a.owner_party_id = $1 AND NOT (a.kind = 'escrow' AND e.amount > 0)
		GROUP BY e.journal_id, e.kind
		ORDER BY min(e.created_at) DESC, min(e.id) DESC LIMIT 500`, party)
	if err != nil {
		return f, err
	}
	for rows.Next() {
		var e struct {
			AmountIdr int                    `json:"amountIdr"`
			At        time.Time              `json:"at"`
			Id        string                 `json:"id"`
			Kind      api.FinanceEntriesKind `json:"kind"`
			Label     string                 `json:"label"`
		}
		if err := rows.Scan(&e.Id, &e.Kind, &e.Label, &e.At, &e.AmountIdr); err != nil {
			return f, err
		}
		if e.AmountIdr != 0 {
			f.Entries = append(f.Entries, e)
		}
	}
	return f, rows.Err()
}

func (s *Server) GetMyFinance(ctx context.Context, _ api.GetMyFinanceRequestObject) (api.GetMyFinanceResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	party, err := myPartyID(ctx, s.DB.Reader(), sess.UserID)
	if err != nil {
		return nil, err
	}
	f, err := loadFinance(ctx, s.DB.Reader(), party)
	if err != nil {
		return nil, err
	}
	return api.GetMyFinance200JSONResponse(f), nil
}

func (s *Server) SaveMyBankAccount(ctx context.Context, req api.SaveMyBankAccountRequestObject) (api.SaveMyBankAccountResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	bank, holder := strings.TrimSpace(b.Bank), strings.TrimSpace(b.Holder)
	fields := map[string]string{}
	if bank == "" {
		fields["bank"] = "Pilih bank"
	}
	if !accountNoPattern(b.AccountNo) {
		fields["accountNo"] = "8–16 digit angka"
	}
	if holder == "" {
		fields["holder"] = "Isi nama pemilik rekening"
	}
	if len(fields) > 0 {
		return nil, &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: "Data rekening belum lengkap", Fields: fields}
	}
	var f api.Finance
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		party, err := userParty(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		// The party id is the AAD: a ciphertext copied to another party's row does not decrypt.
		sealed, err := secure.Encrypt(s.Keys.BankCipher, []byte(b.AccountNo), []byte(party))
		if err != nil {
			return err
		}
		last4 := b.AccountNo[len(b.AccountNo)-4:]
		if _, err := tx.Exec(ctx, `UPDATE bank_accounts SET replaced_at = now() WHERE party_id = $1 AND replaced_at IS NULL`, party); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO bank_accounts (party_id, bank, holder, account_no_enc, account_last4, created_by) VALUES ($1, $2, $3, $4, $5, $6)`,
			party, bank, holder, sealed, last4, sess.UserID); err != nil {
			return err
		}
		if err := writeAudit(ctx, tx, audit{ActorUserID: &sess.UserID, ActorLabel: sess.Name, Action: "Ubah rekening pencairan",
			EntityType: "user", EntityID: sess.UserID, EntityLabel: sess.Name,
			Changes: []change{{Field: "Rekening", After: bank + " ••" + last4}}}); err != nil {
			return err
		}
		f, err = loadFinance(ctx, tx, party)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.SaveMyBankAccount200JSONResponse(f), nil
}

func accountNoPattern(s string) bool {
	if len(s) < 8 || len(s) > 16 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// CreateMyWithdrawal pays available funds out to the active bank account. The wallet account row is locked first, so
// concurrent withdrawals of one party serialize and never overdraw; the journal moves the money to payout_pending (owed,
// not yet transferred). It stays `processing` until an admin transfers it by hand and records it (payouts.go).
func (s *Server) CreateMyWithdrawal(ctx context.Context, req api.CreateMyWithdrawalRequestObject) (api.CreateMyWithdrawalResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	amount := int64(req.Body.AmountIdr)
	var f api.Finance
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		party, err := userParty(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		acc, err := accounts(ctx, tx, "wallet_available:"+party, "ppn_payable:"+party, "payout_pending")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM ledger_accounts WHERE id = $1 FOR UPDATE`, acc["wallet_available:"+party]); err != nil {
			return err
		}
		fail := func(msg string) error {
			return &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: msg, Fields: map[string]string{"amountIdr": msg}}
		}
		var bankID, bank string
		if err := tx.QueryRow(ctx, `SELECT id::text, bank FROM bank_accounts WHERE party_id = $1 AND replaced_at IS NULL`, party).Scan(&bankID, &bank); errors.Is(err, pgx.ErrNoRows) {
			return fail("Tambahkan rekening pencairan dulu")
		} else if err != nil {
			return err
		}
		wallet, err := balance(ctx, tx, party, "wallet_available")
		if err != nil {
			return err
		}
		ppn, err := balance(ctx, tx, party, "ppn_payable")
		if err != nil {
			return err
		}
		available := -(wallet + ppn)
		if amount <= 0 || amount > available {
			return fail("Maksimal " + rupiah(max(0, available)))
		}
		var wid string
		if err := tx.QueryRow(ctx, `INSERT INTO withdrawals (party_id, bank_account_id, amount_idr, requested_by) VALUES ($1, $2, $3, $4) RETURNING id`,
			party, bankID, amount, sess.UserID).Scan(&wid); err != nil {
			return err
		}
		fromWallet := min(amount, max(0, -wallet)) // the wallet first, then the collected PPN
		if err := post(ctx, tx, journal{Label: "Tarik dana ke " + bank, WithdrawalID: &wid, CreatedBy: &sess.UserID, Lines: []ledgerLine{
			{acc["wallet_available:"+party], fromWallet, "withdrawal"},
			{acc["ppn_payable:"+party], amount - fromWallet, "withdrawal"},
			{acc["payout_pending"], -amount, "withdrawal"},
		}}); err != nil {
			return err
		}
		if err := writeAudit(ctx, tx, audit{ActorUserID: &sess.UserID, ActorLabel: sess.Name, Action: "Tarik dana " + rupiah(amount),
			EntityType: "user", EntityID: sess.UserID, EntityLabel: sess.Name}); err != nil {
			return err
		}
		f, err = loadFinance(ctx, tx, party)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.CreateMyWithdrawal201JSONResponse(f), nil
}
