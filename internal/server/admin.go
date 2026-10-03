package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
	"github.com/azdharsyahputra/ecopurnity-api/internal/secure"
)

// Governance (spec tag Admin, PRD §11): every operation needs the `admin` capability; every write is audited in the same
// transaction, punitive ones with a written reason. Copy and rules follow the frontend mock (src/mocks/adminHandlers.ts).

// adminActor is the signed-in admin; Label is how the audit trail and decisions show them ("Sari Kusuma (Admin)").
type adminActor struct{ ID, Label string }

var errNotAdmin = &Error{Status: http.StatusForbidden, Code: "forbidden", Message: "Butuh capability admin"}

func (s *Server) requireAdmin(ctx context.Context) (adminActor, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return adminActor{}, err
	}
	var ok bool
	if err := s.DB.Primary().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM user_capabilities WHERE user_id = $1 AND capability = 'admin')`, sess.UserID).Scan(&ok); err != nil {
		return adminActor{}, err
	}
	if !ok {
		return adminActor{}, errNotAdmin
	}
	return adminActor{ID: sess.UserID, Label: sess.Name + " (Admin)"}, nil
}

// record writes an audit entry by this admin.
func (a adminActor) record(ctx context.Context, q dbtx, e audit) error {
	e.ActorUserID, e.ActorLabel = &a.ID, a.Label
	return writeAudit(ctx, q, e)
}

const minReason = 10

// needReason returns the trimmed reason, or the 422 reason_required every punitive action answers.
func needReason(r *string) (string, error) {
	if r != nil {
		if t := strings.TrimSpace(*r); utf8.RuneCountInString(t) >= minReason {
			return t, nil
		}
	}
	return "", &Error{Status: http.StatusUnprocessableEntity, Code: "reason_required", Message: "Alasan wajib diisi",
		Fields: map[string]string{"reason": fmt.Sprintf("Tulis alasan minimal %d karakter; tercatat di audit trail", minReason)}}
}

// optReason is the trimmed reason, or nil when empty.
func optReason(r *string) *string {
	if r == nil || strings.TrimSpace(*r) == "" {
		return nil
	}
	return ptr(strings.TrimSpace(*r))
}

func diff(field, before, after string) change {
	return change{Field: field, Before: &before, After: after}
}

func notFound(msg string) error {
	return &Error{Status: http.StatusNotFound, Code: "not_found", Message: msg}
}

func conflict(code, msg string) error {
	return &Error{Status: http.StatusConflict, Code: code, Message: msg}
}

// ── Audit trail ──────────────────────────────────────────────────

func loadAudit(ctx context.Context, q dbtx, where string, limit int, args ...any) ([]api.AuditEntry, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`
		SELECT id::text, at, actor_label, action, entity_type, entity_id, entity_label, reason, changes
		FROM audit_log WHERE %s ORDER BY at DESC, id DESC LIMIT %d`, where, limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.AuditEntry{}
	for rows.Next() {
		var e api.AuditEntry
		var changes []byte
		if err := rows.Scan(&e.Id, &e.At, &e.Actor, &e.Action, &e.Entity.Type, &e.Entity.Id, &e.Entity.Label, &e.Reason, &changes); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(changes, &e.Changes); err != nil {
			return nil, err
		}
		if e.Changes != nil && len(*e.Changes) == 0 {
			e.Changes = nil
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Server) ListAdminAudit(ctx context.Context, req api.ListAdminAuditRequestObject) (api.ListAdminAuditResponseObject, error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	p := req.Params
	where, args := []string{"true"}, []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if p.Q != nil && strings.TrimSpace(*p.Q) != "" {
		// ponytail: ILIKE scan on Postgres; the ClickHouse audit mirror (ngram index) takes over when this gets slow.
		add(`(actor_label || ' ' || action || ' ' || entity_label || ' ' || coalesce(reason, '')) ILIKE '%%' || $%d || '%%'`, likeEscape(strings.TrimSpace(*p.Q)))
	}
	if p.EntityType != nil {
		add("entity_type = $%d", string(*p.EntityType))
	}
	if p.EntityId != nil {
		add("entity_id = $%d", *p.EntityId)
	}
	if p.From != nil {
		add("at >= $%d", *p.From)
	}
	if p.To != nil {
		add("at < $%d", *p.To)
	}
	limit := 500
	if p.Limit != nil {
		limit = *p.Limit
	}
	out, err := loadAudit(ctx, s.DB.Reader(), strings.Join(where, " AND "), limit, args...)
	if err != nil {
		return nil, err
	}
	return api.ListAdminAudit200JSONResponse(out), nil
}

// ── Overview ─────────────────────────────────────────────────────

const (
	verificationSLA = 48 * time.Hour
	disputeSLA      = 72 * time.Hour
)

// Users in the governance queue: reported and still active, or waiting on an appeal of their current suspension.
const userQueueCond = `((u.status = 'active' AND EXISTS (SELECT 1 FROM user_reports r WHERE r.user_id = u.id))
	OR EXISTS (SELECT 1 FROM suspension_appeals a WHERE a.user_id = u.id AND a.suspended_at = u.suspended_at AND a.status = 'pending'))`

// Markets in the moderation queue: not suspended, with a flag newer than the last review.
const marketQueueCond = `(m.status <> 'suspended' AND EXISTS (SELECT 1 FROM market_flags f WHERE f.market_id = m.id
	AND f.created_at > coalesce(m.reviewed_at, '-infinity')))`

func (s *Server) GetAdminOverview(ctx context.Context, _ api.GetAdminOverviewRequestObject) (api.GetAdminOverviewResponseObject, error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	q := s.DB.Reader()
	var o api.AdminOverview
	var verTotal, verBreached, dspTotal, dspBreached int
	var verOldest, dspOldest *time.Time
	if err := q.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM users u WHERE `+userQueueCond+`),
		       (SELECT count(*) FROM markets m WHERE `+marketQueueCond+`),
		       (SELECT count(*) FROM fraud_alerts WHERE status IN ('new','investigating')),
		       count(*) FILTER (WHERE true), count(*) FILTER (WHERE created_at < now() - $1::interval), min(created_at),
		       (SELECT count(*) FROM disputes WHERE status <> 'resolved'),
		       (SELECT count(*) FROM disputes WHERE status <> 'resolved' AND opened_at < now() - $2::interval),
		       (SELECT min(opened_at) FROM disputes WHERE status <> 'resolved')
		FROM verification_requests WHERE status = 'pending'`, verificationSLA, disputeSLA).
		Scan(&o.Queues.Users, &o.Queues.Markets, &o.Queues.Fraud, &verTotal, &verBreached, &verOldest, &dspTotal, &dspBreached, &dspOldest); err != nil {
		return nil, err
	}
	o.Queues.Verification, o.Queues.Disputes = verTotal, dspTotal
	findings, err := auctionFindings(ctx, q, `a.status IN ('scheduled','qualification','live','extended')`)
	if err != nil {
		return nil, err
	}
	o.Queues.Auctions = len(findings)
	if o.NewAlerts, err = loadAlerts(ctx, q, `a.status = 'new' ORDER BY a.score DESC, a.detected_at DESC LIMIT 4`); err != nil {
		return nil, err
	}
	if o.OpenDisputes, err = loadDisputeSummaries(ctx, q, `d.status <> 'resolved' ORDER BY d.opened_at DESC LIMIT 5`); err != nil {
		return nil, err
	}
	type slaRow = struct {
		Breached int                        `json:"breached"`
		Label    string                     `json:"label"`
		Module   api.AdminOverviewSlaModule `json:"module"`
		OldestAt *time.Time                 `json:"oldestAt,omitempty"`
		SlaHours float64                    `json:"slaHours"`
		Total    int                        `json:"total"`
	}
	o.Sla = []slaRow{
		{Module: api.AdminOverviewSlaModuleVerification, Label: "Verifikasi bisnis", SlaHours: verificationSLA.Hours(), Total: verTotal, Breached: verBreached, OldestAt: verOldest},
		{Module: api.AdminOverviewSlaModuleDisputes, Label: "Dispute", SlaHours: disputeSLA.Hours(), Total: dspTotal, Breached: dspBreached, OldestAt: dspOldest},
	}
	return api.GetAdminOverview200JSONResponse(o), nil
}

// ── Users ────────────────────────────────────────────────────────

// ponytail: reputation is the baseline score (frontend BASELINE_SCORE) until the shared reputation scorer
// (src/domain/reputation.ts) is ported by the reputation area; swap reputationBaseline for it then.
const reputationBaseline = 80

const adminUserSelect = `
	SELECT u.id, u.name, u.username, u.email, u.location, u.status, u.created_at, coalesce(i.identity_verified_at IS NOT NULL, false),
	       coalesce((SELECT array_agg(c.capability ORDER BY c.capability) FROM user_capabilities c WHERE c.user_id = u.id), '{}'),
	       (SELECT count(*) FROM parties p JOIN trades t ON p.id IN (t.buyer_party_id, t.supplier_party_id) WHERE p.user_id = u.id),
	       (SELECT count(*) FROM user_reports r WHERE r.user_id = u.id)
	FROM users u LEFT JOIN identities i ON i.user_id = u.id`

func loadAdminUsers(ctx context.Context, q dbtx, where string, args ...any) ([]api.AdminUser, error) {
	rows, err := q.Query(ctx, adminUserSelect+" WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.AdminUser{}
	for rows.Next() {
		var u api.AdminUser
		var caps []string
		var txs, reports int64
		if err := rows.Scan(&u.Id, &u.Name, &u.Username, &u.Email, &u.Location, &u.Status, &u.JoinedAt, &u.Verified, &caps, &txs, &reports); err != nil {
			return nil, err
		}
		u.Kind = api.AdminUserKindPerson
		u.Capabilities = []api.Capability{}
		for _, c := range caps {
			u.Capabilities = append(u.Capabilities, api.Capability(c))
		}
		u.Transactions, u.ReportCount, u.Reputation = int(txs), int(reports), reputationBaseline
		out = append(out, u)
	}
	return out, rows.Err()
}

func loadAdminUser(ctx context.Context, q dbtx, id string) (api.AdminUser, error) {
	us, err := loadAdminUsers(ctx, q, `u.id::text = $1`, id)
	if err != nil {
		return api.AdminUser{}, err
	}
	if len(us) == 0 {
		return api.AdminUser{}, notFound("Pengguna tidak ditemukan")
	}
	return us[0], nil
}

func (s *Server) ListAdminUsers(ctx context.Context, req api.ListAdminUsersRequestObject) (api.ListAdminUsersResponseObject, error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	q := ""
	if req.Params.Q != nil {
		q = strings.TrimSpace(*req.Params.Q)
	}
	var status *string
	if req.Params.Status != nil {
		status = ptr(string(*req.Params.Status))
	}
	out, err := loadAdminUsers(ctx, s.DB.Reader(), `
		($1 = '' OR (u.name || ' ' || u.username || ' ' || u.email) ILIKE '%' || $1 || '%')
		AND ($2::text IS NULL OR u.status = $2) ORDER BY u.created_at DESC`, likeEscape(q), status)
	if err != nil {
		return nil, err
	}
	return api.ListAdminUsers200JSONResponse(out), nil
}

func (s *Server) GetAdminUser(ctx context.Context, req api.GetAdminUserRequestObject) (api.GetAdminUserResponseObject, error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	q := s.DB.Reader()
	u, err := loadAdminUser(ctx, q, req.Id)
	if err != nil {
		return nil, err
	}
	var d api.AdminUserDetail
	if err := widen(u, &d); err != nil {
		return nil, err
	}
	var ap api.Appeal
	var decidedAt *time.Time
	var decidedBy, note *string
	err = q.QueryRow(ctx, `
		SELECT a.reason, a.created_at, a.status, a.decided_at, du.name || ' (Admin)', a.decision_note
		FROM suspension_appeals a LEFT JOIN users du ON du.id = a.decided_by
		WHERE a.user_id = $1 ORDER BY a.created_at DESC LIMIT 1`, u.Id).Scan(&ap.Reason, &ap.At, &ap.Status, &decidedAt, &decidedBy, &note)
	switch {
	case err == nil:
		if decidedAt != nil {
			ap.Decision = &struct {
				At   time.Time `json:"at"`
				By   string    `json:"by"`
				Note string    `json:"note"`
			}{*decidedAt, deref(decidedBy), deref(note)}
		}
		d.Appeal = &ap
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	if d.Reports, err = loadReports(ctx, q, `SELECT r.id, p.name, r.reason, r.created_at, r.context FROM user_reports r
		JOIN parties p ON p.id = r.reporter_party_id WHERE r.user_id = $1 ORDER BY r.created_at DESC`, u.Id); err != nil {
		return nil, err
	}
	d.Orgs = []string{}
	rows, err := q.Query(ctx, `SELECT o.name FROM org_members m JOIN orgs o ON o.id = m.org_id WHERE m.user_id = $1 AND m.status = 'active' ORDER BY o.name`, u.Id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		d.Orgs = append(d.Orgs, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if d.History, err = userTransactions(ctx, q, u.Id); err != nil {
		return nil, err
	}
	if d.Audit, err = loadAudit(ctx, q, `entity_type = 'user' AND entity_id = $1`, 200, u.Id); err != nil {
		return nil, err
	}
	return api.GetAdminUser200JSONResponse(d), nil
}

func loadReports(ctx context.Context, q dbtx, sql string, args ...any) ([]api.UserReport, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.UserReport{}
	for rows.Next() {
		var r api.UserReport
		if err := rows.Scan(&r.Id, &r.Reporter, &r.Reason, &r.At, &r.Context); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// userTransactions is the user's trades as Transactions seen from their side (AdminUserDetail.history).
func userTransactions(ctx context.Context, q dbtx, userID string) ([]api.Transaction, error) {
	var party string
	err := q.QueryRow(ctx, `SELECT id FROM parties WHERE user_id = $1`, userID).Scan(&party)
	if errors.Is(err, pgx.ErrNoRows) {
		return []api.Transaction{}, nil
	}
	if err != nil {
		return nil, err
	}
	return loadTransactions(ctx, q, party, `$1 IN (t.buyer_party_id, t.supplier_party_id) ORDER BY t.created_at DESC`)
}

// loadTransactions projects trades onto the side of viewerParty ($1 in where).
// ponytail: a minimal Transaction projection for governance screens; the transactions area owns the full read model
// (switch to it once it lands).
func loadTransactions(ctx context.Context, q dbtx, viewerParty, where string, args ...any) ([]api.Transaction, error) {
	rows, err := q.Query(ctx, `
		SELECT t.id, t.code, t.title, CASE WHEN t.buyer_party_id = $1 THEN 'buyer' ELSE 'supplier' END,
		       cp.name, cp.display_kind, cp.verified, cp.user_id::text, t.status, t.quantity, t.unit, t.unit_price_idr, t.total_idr,
		       t.created_at, t.updated_at, t.due_at, t.auction_id::text, t.terms
		FROM trades t JOIN parties cp ON cp.id = CASE WHEN t.buyer_party_id = $1 THEN t.supplier_party_id ELSE t.buyer_party_id END
		WHERE `+where, append([]any{viewerParty}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Transaction{}
	for rows.Next() {
		var t api.Transaction
		var peer *string
		var unitPrice, total int64
		var terms api.PaymentTerms
		if err := rows.Scan(&t.Id, &t.Code, &t.Title, &t.Role, &t.Counterparty.Name, &t.Counterparty.Kind, &t.Counterparty.Verified, &peer,
			&t.Status, &t.Quantity.Value, &t.Quantity.Unit, &unitPrice, &total, &t.CreatedAt, &t.UpdatedAt, &t.DueAt, &t.AuctionId, &terms); err != nil {
			return nil, err
		}
		t.UnitPriceIdr, t.TotalIdr, t.Terms = int(unitPrice), int(total), &terms
		if peer != nil {
			t.Peer = &struct {
				TxId   string `json:"txId"`
				UserId string `json:"userId"`
			}{TxId: t.Id, UserId: *peer}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

var statusVerb = map[string]string{"active": "Pulihkan akun", "restricted": "Batasi akun", "suspended": "Suspend akun"}

// setUserStatus changes an account's governance status. Suspending starts a suspension episode (suspended_at, the
// appeal key; kept when the account is already suspended) and signs the user out everywhere. via = alert code for
// escalations.
func setUserStatus(ctx context.Context, tx pgx.Tx, a adminActor, u api.AdminUser, to, reason, via string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE users SET status = $2, suspended_at = CASE WHEN $2 = 'suspended' THEN coalesce(suspended_at, now()) END WHERE id = $1`,
		u.Id, to); err != nil {
		return err
	}
	if to == "suspended" {
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, u.Id); err != nil {
			return err
		}
	}
	action := statusVerb[to]
	if via != "" {
		action += " (eskalasi " + via + ")"
	}
	return a.record(ctx, tx, audit{Action: action, EntityType: "user", EntityID: u.Id, EntityLabel: u.Name, Reason: &reason,
		Changes: []change{diff("status", string(u.Status), to)}})
}

// lockAdminUser locks the users row and returns the admin view of it (404 when missing).
func lockAdminUser(ctx context.Context, tx pgx.Tx, id string) (api.AdminUser, error) {
	var uid string
	err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id::text = $1 FOR UPDATE`, id).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.AdminUser{}, notFound("Pengguna tidak ditemukan")
	}
	if err != nil {
		return api.AdminUser{}, err
	}
	return loadAdminUser(ctx, tx, uid)
}

func (s *Server) ActOnAdminUser(ctx context.Context, req api.ActOnAdminUserRequestObject) (api.ActOnAdminUserResponseObject, error) {
	a, err := s.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	var out api.AdminUser
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		u, err := lockAdminUser(ctx, tx, req.Id)
		if err != nil {
			return err
		}
		reason, err := needReason(req.Body.Reason)
		if err != nil {
			return err
		}
		entity := func(action string, ch change) audit {
			return audit{Action: action, EntityType: "user", EntityID: u.Id, EntityLabel: u.Name, Reason: &reason, Changes: []change{ch}}
		}
		switch req.Body.Action {
		case api.UserActionVerify:
			if u.Verified {
				return conflict("already_verified", "Akun sudah terverifikasi")
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO identities (user_id, identity_verified_at, identity_verified_by) VALUES ($1, now(), $2)
				ON CONFLICT (user_id) DO UPDATE SET identity_verified_at = now(), identity_verified_by = $2`, u.Id, a.ID); err != nil {
				return err
			}
			if err := a.record(ctx, tx, entity("Verifikasi identitas", diff("verified", "tidak", "ya"))); err != nil {
				return err
			}
		case api.UserActionDenyAppeal:
			tag, err := tx.Exec(ctx, `
				UPDATE suspension_appeals a SET status = 'denied', decided_at = now(), decided_by = $2, decision_note = $3
				FROM users u WHERE u.id = a.user_id AND a.user_id = $1 AND a.suspended_at = u.suspended_at AND a.status = 'pending'`,
				u.Id, a.ID, reason)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return conflict("no_appeal", "Tidak ada banding yang menunggu")
			}
			if err := a.record(ctx, tx, entity("Tolak banding suspend", diff("banding", "pending", "ditolak"))); err != nil {
				return err
			}
		default:
			to := map[api.UserAction]string{api.UserActionSuspend: "suspended", api.UserActionRestrict: "restricted", api.UserActionRestore: "active"}[req.Body.Action]
			if string(u.Status) == to {
				return conflict("no_change", "Status akun sudah seperti itu")
			}
			if req.Body.Action == api.UserActionRestore {
				// The pending appeal of the current suspension is granted by restoring the account.
				if _, err := tx.Exec(ctx, `
					UPDATE suspension_appeals a SET status = 'granted', decided_at = now(), decided_by = $2, decision_note = $3
					FROM users u WHERE u.id = a.user_id AND a.user_id = $1 AND a.suspended_at = u.suspended_at AND a.status = 'pending'`,
					u.Id, a.ID, reason); err != nil {
					return err
				}
			}
			if err := setUserStatus(ctx, tx, a, u, to, reason, ""); err != nil {
				return err
			}
		}
		out, err = loadAdminUser(ctx, tx, u.Id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.ActOnAdminUser200JSONResponse(out), nil
}

// ── Verification queue ───────────────────────────────────────────

type verificationRow struct {
	api.VerificationRequest
	UserID string
	OrgID  *string
	Keys   []string // object keys, in Documents order
}

func loadVerifications(ctx context.Context, q dbtx, where string, args ...any) ([]verificationRow, error) {
	rows, err := q.Query(ctx, `
		SELECT r.id, r.kind, r.subject_name, su.name, r.created_at, r.status, r.form, r.decided_at, du.name || ' (Admin)', r.decision_note,
		       r.submitted_by, r.org_id::text
		FROM verification_requests r JOIN users su ON su.id = r.submitted_by LEFT JOIN users du ON du.id = r.decided_by
		WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []verificationRow{}
	index := map[string]int{}
	ids := []string{}
	for rows.Next() {
		var v verificationRow
		var kind api.VerificationRequestKind
		var form []byte
		var decidedAt *time.Time
		var by, note *string
		if err := rows.Scan(&v.Id, &kind, &v.Business, &v.Owner, &v.SubmittedAt, &v.Status, &form, &decidedAt, &by, &note, &v.UserID, &v.OrgID); err != nil {
			return nil, err
		}
		v.Kind = &kind
		if err := json.Unmarshal(form, &v.Form); err != nil {
			return nil, err
		}
		if decidedAt != nil {
			v.Decision = &struct {
				At   time.Time `json:"at"`
				By   string    `json:"by"`
				Note string    `json:"note"`
			}{*decidedAt, deref(by), deref(note)}
		}
		v.Documents = []struct {
			Fields   []api.LabeledValue `json:"fields"`
			FileName string             `json:"fileName"`
			Kind     api.DocKind        `json:"kind"`
			Url      *string            `json:"url,omitempty"`
		}{}
		index[v.Id] = len(out)
		ids = append(ids, v.Id)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	docs, err := q.Query(ctx, `SELECT request_id::text, kind, file_name, object_key, fields FROM verification_documents
		WHERE request_id::text = ANY($1) ORDER BY created_at, kind`, ids)
	if err != nil {
		return nil, err
	}
	defer docs.Close()
	for docs.Next() {
		var id, key string
		var fields []byte
		var d struct {
			Fields   []api.LabeledValue `json:"fields"`
			FileName string             `json:"fileName"`
			Kind     api.DocKind        `json:"kind"`
			Url      *string            `json:"url,omitempty"`
		}
		if err := docs.Scan(&id, &d.Kind, &d.FileName, &key, &fields); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(fields, &d.Fields); err != nil {
			return nil, err
		}
		v := &out[index[id]]
		v.Documents = append(v.Documents, d)
		v.Keys = append(v.Keys, key)
	}
	return out, docs.Err()
}

func (s *Server) ListAdminVerifications(ctx context.Context, _ api.ListAdminVerificationsRequestObject) (api.ListAdminVerificationsResponseObject, error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	rows, err := loadVerifications(ctx, s.DB.Reader(), `true ORDER BY r.created_at DESC`)
	if err != nil {
		return nil, err
	}
	out := make([]api.VerificationRequest, len(rows))
	for i, r := range rows {
		out[i] = r.VerificationRequest
	}
	return api.ListAdminVerifications200JSONResponse(out), nil
}

const reviewURLTTL = 5 * time.Minute

func (s *Server) GetAdminVerification(ctx context.Context, req api.GetAdminVerificationRequestObject) (api.GetAdminVerificationResponseObject, error) {
	admin, err := s.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Reader()
	rows, err := loadVerifications(ctx, q, `r.id::text = $1`, req.Id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, notFound("Pengajuan tidak ditemukan")
	}
	v := rows[0]
	if s.Storage != nil {
		for i, key := range v.Keys {
			url, err := s.Storage.PresignGet(ctx, key, reviewURLTTL)
			if err != nil {
				return nil, err
			}
			v.Documents[i].Url = &url
		}
	}
	if v.Kind != nil && *v.Kind == api.VerificationRequestKindPersonal {
		var sealed []byte
		err := q.QueryRow(ctx, `SELECT nik_encrypted FROM kyc_submissions WHERE verification_request_id = $1`, v.Id).Scan(&sealed)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if sealed != nil {
			nik, err := secure.Decrypt(s.Keys.NIKCipher, sealed, []byte(v.UserID))
			if err != nil {
				return nil, err
			}
			v.Nik = ptr(string(nik))
			// Every view of a decrypted NIK is on the record.
			if err := admin.record(ctx, s.DB.Primary(), audit{Action: "Melihat NIK", EntityType: "user", EntityID: v.UserID,
				EntityLabel: v.Business}); err != nil {
				return nil, err
			}
		}
	}
	return api.GetAdminVerification200JSONResponse(v.VerificationRequest), nil
}

func (s *Server) DecideAdminVerification(ctx context.Context, req api.DecideAdminVerificationRequestObject) (api.DecideAdminVerificationResponseObject, error) {
	a, err := s.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	var out api.VerificationRequest
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM verification_requests WHERE id::text = $1 FOR UPDATE`, req.Id).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound("Pengajuan tidak ditemukan")
		}
		if err != nil {
			return err
		}
		if status != "pending" {
			return conflict("decided", "Pengajuan ini sudah diputuskan")
		}
		action := req.Body.Action
		reason := optReason(req.Body.Reason)
		if action != api.VerificationActionApprove {
			r, err := needReason(req.Body.Reason)
			if err != nil {
				return err
			}
			reason = &r
		}
		to := map[api.VerificationAction]string{api.VerificationActionApprove: "approved", api.VerificationActionReject: "rejected",
			api.VerificationActionReupload: "reupload"}[action]
		note := "Dokumen sesuai"
		if reason != nil {
			note = *reason
		}
		if _, err := tx.Exec(ctx, `UPDATE verification_requests SET status = $2, decided_at = now(), decided_by = $3, decision_note = $4 WHERE id = $1`,
			req.Id, to, a.ID, note); err != nil {
			return err
		}
		rows, err := loadVerifications(ctx, tx, `r.id = $1`, req.Id)
		if err != nil {
			return err
		}
		v := rows[0]
		e := audit{Reason: reason, Changes: []change{diff("status", "pending", to)}}
		if *v.Kind == api.VerificationRequestKindPersonal {
			e.Action = map[string]string{"approved": "Setujui verifikasi KTP", "rejected": "Tolak verifikasi KTP", "reupload": "Minta unggah ulang dokumen"}[to]
			e.EntityType, e.EntityID, e.EntityLabel = "user", v.UserID, v.Owner
			if to == "approved" {
				// One verified account per NIK: the hash moves onto the identity (identities.nik_hash is UNIQUE).
				_, err := tx.Exec(ctx, `
					INSERT INTO identities (user_id, identity_verified_at, identity_verified_by, nik_hash)
					SELECT $1, now(), $2, k.nik_hash FROM kyc_submissions k WHERE k.verification_request_id = $3
					ON CONFLICT (user_id) DO UPDATE SET identity_verified_at = now(), identity_verified_by = $2, nik_hash = EXCLUDED.nik_hash`,
					v.UserID, a.ID, v.Id)
				if uniqueViolation(err, "identities_nik_hash_key") {
					return conflict("nik_in_use", "NIK ini sudah dipakai akun lain yang terverifikasi.")
				}
				if err != nil {
					return err
				}
			}
		} else {
			e.Action = map[string]string{"approved": "Setujui verifikasi bisnis", "rejected": "Tolak verifikasi bisnis", "reupload": "Minta unggah ulang dokumen"}[to]
			e.EntityType, e.EntityID, e.EntityLabel, e.OrgID = "business", v.Id, v.Business, v.OrgID
			orgStatus := map[string]string{"approved": "verified", "rejected": "rejected", "reupload": "unverified"}[to]
			if _, err := tx.Exec(ctx, `UPDATE org_profiles SET verification = $2 WHERE org_id = $1`, v.OrgID, orgStatus); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE parties SET verified = $2 WHERE org_id = $1`, v.OrgID, to == "approved"); err != nil {
				return err
			}
		}
		if err := a.record(ctx, tx, e); err != nil {
			return err
		}
		word := map[string]string{"approved": "disetujui", "rejected": "ditolak", "reupload": "perlu unggah ulang"}[to]
		if err := notify(ctx, tx, v.UserID, notification{Type: "transaction_update", Title: fmt.Sprintf("Verifikasi %s: %s", v.Business, word),
			Body: note, Href: "/app/settings"}); err != nil {
			return err
		}
		out = v.VerificationRequest
		return nil
	})
	if err != nil {
		return nil, err
	}
	return api.DecideAdminVerification200JSONResponse(out), nil
}
