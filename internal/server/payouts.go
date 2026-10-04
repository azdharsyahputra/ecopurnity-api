package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
	"github.com/azdharsyahputra/ecopurnity-api/internal/secure"
)

const withdrawalSLA = 24 * time.Hour

const withdrawalDue = `(w.created_at + CASE extract(isodow FROM w.created_at AT TIME ZONE 'Asia/Jakarta') WHEN 5 THEN 3 WHEN 6 THEN 2 ELSE 1 END * interval '1 day')`

const withdrawalSelect = `
	SELECT w.id::text, w.code, w.status, w.amount_idr, w.created_at, ` + withdrawalDue + `,
	       coalesce(u.id::text, ''), coalesce(u.name, ''), coalesce(u.email::text, ''), p.id::text, p.name, b.bank, b.holder, b.account_last4,
	       w.transfer_ref, w.paid_at, w.admin_note, w.reject_reason, w.decided_at, du.name || ' (Admin)'
	FROM withdrawals w JOIN parties p ON p.id = w.party_id JOIN bank_accounts b ON b.id = w.bank_account_id
	LEFT JOIN users u ON u.id = coalesce(w.requested_by, p.user_id) LEFT JOIN users du ON du.id = w.decided_by`

func loadWithdrawals(ctx context.Context, q dbtx, where string, args ...any) ([]api.AdminWithdrawal, error) {
	rows, err := q.Query(ctx, withdrawalSelect+" WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.AdminWithdrawal{}
	for rows.Next() {
		var w api.AdminWithdrawal
		if err := rows.Scan(&w.Id, &w.Code, &w.Status, &w.AmountIdr, &w.RequestedAt, &w.DueAt, &w.Requester.Id, &w.Requester.Name,
			&w.Requester.Email, &w.Party.Id, &w.Party.Name, &w.Bank, &w.Holder, &w.AccountLast4, &w.TransferRef, &w.PaidAt, &w.Note,
			&w.Reason, &w.DecidedAt, &w.DecidedBy); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Server) ListAdminWithdrawals(ctx context.Context, req api.ListAdminWithdrawalsRequestObject) (api.ListAdminWithdrawalsResponseObject, error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}

	where, args := `true ORDER BY w.created_at DESC LIMIT 500`, []any{}
	if st := req.Params.Status; st != nil {
		order := "DESC"
		if *st == api.WithdrawalStatusProcessing {
			order = "ASC"
		}
		where, args = `w.status = $1 ORDER BY w.created_at `+order+` LIMIT 500`, []any{string(*st)}
	}
	out, err := loadWithdrawals(ctx, s.DB.Reader(), where, args...)
	if err != nil {
		return nil, err
	}
	return api.ListAdminWithdrawals200JSONResponse(out), nil
}

func normName(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToUpper(s), func(r rune) bool { return !unicode.IsLetter(r) }), " ")
}

func (s *Server) GetAdminWithdrawal(ctx context.Context, req api.GetAdminWithdrawalRequestObject) (api.GetAdminWithdrawalResponseObject, error) {
	admin, err := s.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Reader()
	rows, err := loadWithdrawals(ctx, q, `w.id::text = $1`, req.Id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, notFound("Pencairan tidak ditemukan")
	}
	w := rows[0]
	d := api.AdminWithdrawalDetail{Id: w.Id, Code: w.Code, Status: w.Status, AmountIdr: w.AmountIdr, RequestedAt: w.RequestedAt, DueAt: w.DueAt,
		Requester: w.Requester, Party: w.Party, Bank: w.Bank, Holder: w.Holder, AccountLast4: w.AccountLast4, TransferRef: w.TransferRef,
		PaidAt: w.PaidAt, Note: w.Note, Reason: w.Reason, DecidedAt: w.DecidedAt, DecidedBy: w.DecidedBy}

	var name string
	err = q.QueryRow(ctx, `
		SELECT k.full_name FROM kyc_submissions k JOIN verification_requests r ON r.id = k.verification_request_id
		WHERE r.submitted_by::text = $1 AND r.kind = 'personal' AND r.status = 'approved' ORDER BY r.decided_at DESC LIMIT 1`, w.Requester.Id).Scan(&name)
	switch {
	case err == nil:
		d.IdentityName = &name
		d.NameMismatch = normName(name) != normName(w.Holder)
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	if d.Recent, err = loadWithdrawals(ctx, q, `w.party_id::text = $1 AND w.id::text <> $2 ORDER BY w.created_at DESC LIMIT 10`, w.Party.Id, w.Id); err != nil {
		return nil, err
	}

	if w.Status == api.WithdrawalStatusProcessing {
		var sealed []byte
		if err := q.QueryRow(ctx, `SELECT b.account_no_enc FROM withdrawals w JOIN bank_accounts b ON b.id = w.bank_account_id WHERE w.id::text = $1`,
			w.Id).Scan(&sealed); err != nil {
			return nil, err
		}
		no, err := secure.Decrypt(s.Keys.BankCipher, sealed, []byte(w.Party.Id))
		if err != nil {
			return nil, err
		}
		d.AccountNo = ptr(string(no))
		if err := admin.record(ctx, s.DB.Primary(), audit{Action: "Melihat nomor rekening " + w.Code, EntityType: "user", EntityID: w.Requester.Id,
			EntityLabel: w.Requester.Name}); err != nil {
			return nil, err
		}
	}
	return api.GetAdminWithdrawal200JSONResponse(d), nil
}

func (s *Server) ActOnAdminWithdrawal(ctx context.Context, req api.ActOnAdminWithdrawalRequestObject) (api.ActOnAdminWithdrawalResponseObject, error) {
	a, err := s.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	var out api.AdminWithdrawal
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM withdrawals WHERE id::text = $1 FOR UPDATE`, req.Id).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound("Pencairan tidak ditemukan")
		}
		if err != nil {
			return err
		}
		if status != "processing" {
			return conflict("decided", "Pencairan ini sudah diproses")
		}
		rows, err := loadWithdrawals(ctx, tx, `w.id::text = $1`, req.Id)
		if err != nil {
			return err
		}
		w, b := rows[0], req.Body
		amount := int64(w.AmountIdr)
		dest := w.Bank + " ••" + w.AccountLast4
		e := audit{EntityType: "user", EntityID: w.Requester.Id, EntityLabel: w.Requester.Name}
		var n notification

		switch b.Action {
		case api.WithdrawalActionInputActionMarkPaid:
			ref := strings.TrimSpace(deref(b.TransferRef))
			paidAt := time.Now()
			fields := map[string]string{}
			switch {
			case ref == "":
				fields["transferRef"] = "Isi nomor referensi transfer"
			case utf8.RuneCountInString(ref) > 100:
				fields["transferRef"] = "Maksimal 100 karakter"
			}
			if b.PaidAt != nil {
				paidAt = *b.PaidAt
				if paidAt.After(time.Now().Add(5 * time.Minute)) {
					fields["paidAt"] = "Waktu transfer tidak boleh di masa depan"
				} else if paidAt.Before(w.RequestedAt) {
					fields["paidAt"] = "Waktu transfer sebelum pengajuan"
				}
			}
			note := optReason(b.Note)
			if note != nil && utf8.RuneCountInString(*note) > 500 {
				fields["note"] = "Maksimal 500 karakter"
			}
			if len(fields) > 0 {
				return &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: "Data transfer belum lengkap", Fields: fields}
			}
			if _, err := tx.Exec(ctx, `UPDATE withdrawals SET status = 'paid', transfer_ref = $2, paid_at = $3, admin_note = $4, decided_by = $5,
				decided_at = now() WHERE id = $1`, w.Id, ref, paidAt, note, a.ID); err != nil {
				return err
			}
			acc, err := accounts(ctx, tx, "payout_pending", "bank_clearing")
			if err != nil {
				return err
			}
			if err := post(ctx, tx, journal{Label: "Transfer " + w.Code + " ke " + dest + " · ref " + ref, WithdrawalID: &w.Id, CreatedBy: &a.ID,
				Lines: []ledgerLine{{acc["payout_pending"], amount, "withdrawal"}, {acc["bank_clearing"], -amount, "withdrawal"}}}); err != nil {
				return err
			}
			e.Action = "Tandai pencairan " + w.Code + " dibayar"
			e.Changes = []change{diff("status", "processing", "paid"), {Field: "Ref transfer", After: ref}}
			n = notification{Type: "payment", Title: fmt.Sprintf("Dana %s sudah ditransfer ke %s", rupiah(amount), dest),
				Body: "Ref transfer " + ref, Href: "/app/finance"}

		case api.WithdrawalActionInputActionReject:
			reason, err := needReason(b.Reason)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE withdrawals SET status = 'rejected', reject_reason = $2, decided_by = $3, decided_at = now() WHERE id = $1`,
				w.Id, reason, a.ID); err != nil {
				return err
			}

			taken, err := tx.Query(ctx, `
				SELECT e.account_id::text, sum(e.amount)::bigint FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.account_id
				WHERE e.withdrawal_id = $1 AND la.owner_party_id IS NOT NULL GROUP BY e.account_id`, w.Id)
			if err != nil {
				return err
			}
			var lines []ledgerLine
			var total int64
			for taken.Next() {
				var l ledgerLine
				if err := taken.Scan(&l.Account, &l.Amount); err != nil {
					return err
				}
				total += l.Amount
				l.Amount, l.Kind = -l.Amount, "withdrawal"
				lines = append(lines, l)
			}
			if err := taken.Err(); err != nil {
				return err
			}
			if total != amount {
				return fmt.Errorf("withdrawal %s: request journal took %d, amount %d", w.Code, total, amount)
			}
			acc, err := accounts(ctx, tx, "payout_pending")
			if err != nil {
				return err
			}
			lines = append(lines, ledgerLine{acc["payout_pending"], amount, "withdrawal"})
			if err := post(ctx, tx, journal{Label: "Pencairan " + w.Code + " ditolak · dana kembali ke saldo", WithdrawalID: &w.Id, CreatedBy: &a.ID,
				Lines: lines}); err != nil {
				return err
			}
			e.Action, e.Reason = "Tolak pencairan "+w.Code, &reason
			e.Changes = []change{diff("status", "processing", "rejected")}
			n = notification{Type: "payment", Title: fmt.Sprintf("Pencairan %s ditolak", rupiah(amount)),
				Body: reason + " Dana sudah kembali ke saldo yang bisa ditarik.", Href: "/app/finance"}

		default:
			return &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: "Aksi tidak dikenal",
				Fields: map[string]string{"action": "Pilih mark_paid atau reject"}}
		}

		if err := a.record(ctx, tx, e); err != nil {
			return err
		}
		if w.Requester.Id != "" {
			if err := notify(ctx, tx, w.Requester.Id, n); err != nil {
				return err
			}
		}
		rows, err = loadWithdrawals(ctx, tx, `w.id = $1`, w.Id)
		if err != nil {
			return err
		}
		out = rows[0]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return api.ActOnAdminWithdrawal200JSONResponse(out), nil
}
