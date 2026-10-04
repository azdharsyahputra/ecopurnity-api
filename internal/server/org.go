package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
	"github.com/azdharsyahputra/ecopurnity-api/internal/mail"
)

var (
	orgModules     = []string{"procurement", "auctions", "collective", "suppliers", "inventory", "transactions", "analytics", "team", "profile"}
	orgActions     = []string{"view", "create", "approve", "manage"}
	builtInRoleIDs = []string{"owner", "procurement", "finance", "operations", "sales"}
	moduleLabel    = map[string]string{"procurement": "Procurement", "auctions": "Auctions", "collective": "Collective", "suppliers": "Suppliers",
		"inventory": "Inventory", "transactions": "Transactions", "analytics": "Analytics", "team": "Tim", "profile": "Profil bisnis"}
	actionLabel  = map[string]string{"view": "Lihat", "create": "Buat", "approve": "Approve", "manage": "Kelola"}
	orgRoleLabel = map[string]string{"owner": "Owner", "procurement": "Procurement", "finance": "Finance", "operations": "Operations", "sales": "Sales"}
	categoryLbl  = map[string]string{"agri": "Pertanian", "food": "Pangan", "packaging": "Kemasan", "manufacturing": "Manufaktur", "logistics": "Logistik",
		"it": "Jasa IT", "energy": "Energi"}
)

func can(perms []string, role, module, action string) bool {
	return role == "owner" || slices.Contains(perms, module+"."+action)
}

func deniedReason(roleLabel, module, action string) string {
	return fmt.Sprintf("Peran %s tidak punya izin %s %s", roleLabel, strings.ToLower(actionLabel[action]), moduleLabel[module])
}

var txActionRoles = map[string][]string{
	"accept_agreement": {"owner", "procurement", "sales"},
	"issue_invoice":    {"owner", "finance", "sales"},
	"pay":              {"owner", "finance"},
	"ship":             {"owner", "operations"},
	"upload_proof":     {"owner", "operations"},
	"confirm_receipt":  {"owner", "procurement", "operations"},
	"cancel":           {"owner", "procurement"},
	"dispute":          {"owner", "procurement"},
	"add_evidence":     {"owner", "procurement", "operations"},
	"review":           {"owner", "procurement"},
}

func canTransact(perms []string, role, action string) bool {
	if _, builtIn := orgRoleLabel[role]; builtIn {
		return slices.Contains(txActionRoles[action], role)
	}
	return can(perms, role, "transactions", "manage")
}

func txDeniedReason(perms []string, role, label, action string) string {
	if canTransact(perms, role, action) {
		return ""
	}
	if _, builtIn := orgRoleLabel[role]; !builtIn {
		return deniedReason(label, "transactions", "manage")
	}
	var who []string
	for _, r := range txActionRoles[action] {
		who = append(who, orgRoleLabel[r])
	}
	return fmt.Sprintf("%s hanya untuk %s; peranmu %s", tradeActionLabel[action], strings.Join(who, ", "), label)
}

type approvalRule struct {
	Label     string
	MinAmount int64
	Approvers []string
	AppliesTo []string
}

type approval struct{ Role, Decision string }

func requiredApprovers(amount int64, subject string, rules []approvalRule) []string {
	out := []string{}
	for _, r := range rules {
		if slices.Contains(r.AppliesTo, subject) && amount > r.MinAmount {
			for _, a := range r.Approvers {
				if !slices.Contains(out, a) {
					out = append(out, a)
				}
			}
		}
	}
	return out
}

func approvalState(required []string, approvals []approval) (pending []string, rejected, approved bool) {
	pending = []string{}
	for _, a := range approvals {
		rejected = rejected || a.Decision == "rejected"
	}
	for _, role := range required {
		if !slices.Contains(approvals, approval{role, "approved"}) {
			pending = append(pending, role)
		}
	}
	return pending, rejected, !rejected && len(pending) == 0
}

func signingRoles(role string, required []string, approvals []approval, activeRoles []string) []string {
	pending, rejected, _ := approvalState(required, approvals)
	out := []string{}
	if rejected {
		return out
	}
	if slices.Contains(pending, role) {
		out = append(out, role)
	}
	for _, p := range pending {
		if p != role && role == "owner" && activeRoles != nil && !slices.Contains(activeRoles, p) {
			out = append(out, p)
		}
	}
	return out
}

func canApprove(role string, required []string, approvals []approval, activeRoles []string) bool {
	return len(signingRoles(role, required, approvals, activeRoles)) > 0
}

type orgMemberRole struct{ UserID, Role string }

func approverUserIDs(required []string, approvals []approval, members []orgMemberRole, actorID string) []string {
	active := []string{}
	for _, m := range members {
		active = append(active, m.Role)
	}
	out := []string{}
	for _, m := range members {
		if m.UserID != actorID && canApprove(m.Role, required, approvals, active) && !slices.Contains(out, m.UserID) {
			out = append(out, m.UserID)
		}
	}
	return out
}

func activeRoles(ctx context.Context, q dbtx, orgID string) ([]string, error) {
	var out []string
	err := q.QueryRow(ctx, `SELECT array(SELECT DISTINCT role FROM org_members WHERE org_id = $1 AND status = 'active' AND user_id IS NOT NULL ORDER BY 1)`, orgID).Scan(&out)
	return nonNil(out), err
}

func sign(ctx context.Context, tx pgx.Tx, table, idCol, id string, c *orgCtx, roles []string, decision, note string) ([]approval, error) {
	if decision == "rejected" {
		roles = roles[:1]
	}
	out := []approval{}
	for _, role := range roles {
		if _, err := tx.Exec(ctx, `INSERT INTO `+table+` (`+idCol+`, role, decision, note, decided_by, on_behalf) VALUES ($1, $2, $3, nullif($4, ''), $5, $6)`,
			id, role, decision, note, c.sess.UserID, role != c.Role); err != nil {
			return nil, err
		}
		out = append(out, approval{role, decision})
	}
	return out, nil
}

func approvalBySQL(orgCol string) string {
	return `u.name || CASE WHEN a.on_behalf THEN ' · Owner (atas nama ' ELSE ' (' END ||
		coalesce((SELECT ro.label FROM org_roles ro WHERE ro.org_id = ` + orgCol + ` AND ro.key = a.role), a.role) || ')'`
}

func statusAfterApproval(required []string, approvals []approval) string {
	_, rejected, approved := approvalState(required, approvals)
	switch {
	case rejected:
		return "rejected"
	case approved:
		return "approved"
	}
	return "pending_approval"
}

var pipelineStages = []string{"draft", "approval", "published", "auction", "awarded", "po"}

func pipelineStage(status string) string {
	return map[string]string{"draft": "draft", "pending_approval": "approval", "approved": "published", "published": "published",
		"in_collective": "published", "in_auction": "auction", "awarded": "awarded", "po_issued": "po"}[status]
}

func pipelineCounts(statuses []string) map[string]int {
	out := map[string]int{}
	for _, s := range pipelineStages {
		out[s] = 0
	}
	for _, s := range statuses {
		if st := pipelineStage(s); st != "" {
			out[st]++
		}
	}
	return out
}

func procurementActions(status string, required []string, approvals []approval, role string, perms, activeRoles []string) []string {
	out := []string{}
	manage := can(perms, role, "procurement", "manage") || can(perms, role, "procurement", "create")
	if status == "draft" && manage {
		out = append(out, "submit")
	}
	if status == "pending_approval" && canApprove(role, required, approvals, activeRoles) {
		out = append(out, "approve", "reject")
	}
	if status == "approved" && manage {
		out = append(out, "publish")
	}
	if (status == "approved" || status == "published") && can(perms, role, "collective", "create") {
		out = append(out, "collective")
	}
	if slices.Contains([]string{"draft", "pending_approval", "approved", "published"}, status) && manage {
		out = append(out, "cancel")
	}
	return out
}

type orgCtx struct {
	sess                                      *session
	OrgID, OrgName, Role, RoleLabel, MemberID string
	Perms                                     []string
}

var errNotMember = &Error{Status: http.StatusForbidden, Code: "forbidden", Message: "Kamu bukan anggota organisasi ini"}

func orgAccess(ctx context.Context, q dbtx, orgID string) (*orgCtx, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	c := &orgCtx{sess: sess}
	err = q.QueryRow(ctx, `
		SELECT o.id::text, o.name, m.id::text, m.role, r.label, r.permissions
		FROM org_members m JOIN orgs o ON o.id = m.org_id JOIN org_roles r ON r.org_id = m.org_id AND r.key = m.role
		WHERE m.org_id::text = $1 AND m.user_id = $2 AND m.status = 'active'`, orgID, sess.UserID).
		Scan(&c.OrgID, &c.OrgName, &c.MemberID, &c.Role, &c.RoleLabel, &c.Perms)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotMember
	}
	return c, err
}

func (c *orgCtx) can(module, action string) bool { return can(c.Perms, c.Role, module, action) }

func (c *orgCtx) need(module, action string) error {
	if c.can(module, action) {
		return nil
	}
	return &Error{Status: http.StatusForbidden, Code: "forbidden", Message: deniedReason(c.RoleLabel, module, action)}
}

func (c *orgCtx) actor() string { return c.sess.Name + " (" + c.RoleLabel + ")" }

func (c *orgCtx) audit(ctx context.Context, q dbtx, action, entityType, entityID, label string, reason *string, changes ...change) error {
	return writeAudit(ctx, q, audit{ActorUserID: &c.sess.UserID, ActorLabel: c.actor(), Action: action, EntityType: entityType,
		EntityID: entityID, EntityLabel: label, OrgID: &c.OrgID, Reason: reason, Changes: changes})
}

func lockOrg(ctx context.Context, tx pgx.Tx, orgID string) error {
	_, err := tx.Exec(ctx, `SELECT 1 FROM orgs WHERE id = $1 FOR UPDATE`, orgID)
	return err
}

func invalid(msg string, fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	return &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: msg, Fields: fields}
}

func orgParty(ctx context.Context, q dbtx, orgID string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO parties (kind, org_id, name, display_kind, verified)
		SELECT 'org', o.id, o.name, 'business', coalesce(p.verification = 'verified', false) FROM orgs o LEFT JOIN org_profiles p ON p.org_id = o.id WHERE o.id = $1
		ON CONFLICT (org_id) DO UPDATE SET name = EXCLUDED.name, verified = EXCLUDED.verified
		RETURNING id`, orgID).Scan(&id)
	return id, err
}

func orgMembers(ctx context.Context, q dbtx, orgID string) ([]orgMemberRole, error) {
	rows, err := q.Query(ctx, `SELECT user_id::text, role FROM org_members WHERE org_id = $1 AND status = 'active' AND user_id IS NOT NULL ORDER BY created_at`, orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (orgMemberRole, error) {
		var m orgMemberRole
		return m, r.Scan(&m.UserID, &m.Role)
	})
}

func askApprovers(ctx context.Context, tx pgx.Tx, c *orgCtx, kind, id, code, title string, value int64, required []string, approvals []approval) error {
	members, err := orgMembers(ctx, tx, c.OrgID)
	if err != nil {
		return err
	}
	href := "/org/" + c.OrgID + "/procurement/" + id
	if kind == "auction" {
		href = "/org/" + c.OrgID + "/auctions?review=" + id
	}
	for _, u := range approverUserIDs(required, approvals, members, c.sess.UserID) {
		if err := notify(ctx, tx, u, notification{Type: "transaction_update", Title: "Perlu approval kamu: " + code,
			Body: fmt.Sprintf("%s mengajukan %s \"%s\" senilai %s.", c.actor(), kind, title, rupiah(value)), Href: href}); err != nil {
			return err
		}
	}
	return nil
}

func loadApprovalRules(ctx context.Context, q dbtx, orgID string) ([]approvalRule, error) {
	rows, err := q.Query(ctx, `SELECT label, min_amount_idr, approvers, applies_to FROM org_approval_rules WHERE org_id = $1 ORDER BY position, created_at`, orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (approvalRule, error) {
		var a approvalRule
		return a, r.Scan(&a.Label, &a.MinAmount, &a.Approvers, &a.AppliesTo)
	})
}

func memberLabelSQL(userName, userID, orgID string) string {
	return fmt.Sprintf(`%[1]s || coalesce(' (' || (SELECT r.label FROM org_members m JOIN org_roles r ON r.org_id = m.org_id AND r.key = m.role
		WHERE m.org_id = %[3]s AND m.user_id = %[2]s) || ')', '')`, userName, userID, orgID)
}

func loadAuditEntries(ctx context.Context, q dbtx, where string, args ...any) ([]api.AuditEntry, error) {
	rows, err := q.Query(ctx, `SELECT id::text, actor_label, action, entity_type, entity_id, entity_label, at, reason, changes FROM audit_log WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (api.AuditEntry, error) {
		var e api.AuditEntry
		var changes []byte
		if err := r.Scan(&e.Id, &e.Actor, &e.Action, &e.Entity.Type, &e.Entity.Id, &e.Entity.Label, &e.At, &e.Reason, &changes); err != nil {
			return e, err
		}
		var cs []struct {
			After  *string `json:"after,omitempty"`
			Before *string `json:"before,omitempty"`
			Field  string  `json:"field"`
		}
		_ = json.Unmarshal(changes, &cs)
		if len(cs) > 0 {
			e.Changes = &cs
		}
		return e, nil
	})
	return nonNil(out), err
}

func loadOrgProfile(ctx context.Context, q dbtx, orgID string) (api.OrgProfile, error) {
	var p api.OrgProfile
	var cats []string
	err := q.QueryRow(ctx, `
		SELECT o.name, p.industry, p.location, p.nib, p.npwp, p.akta, p.description, p.categories::text[], p.hours_days::int[],
		       to_char(p.hours_from, 'HH24:MI'), to_char(p.hours_to, 'HH24:MI'), p.verification
		FROM orgs o JOIN org_profiles p ON p.org_id = o.id WHERE o.id = $1`, orgID).
		Scan(&p.Name, &p.Industry, &p.Location, &p.Legal.Nib, &p.Legal.Npwp, &p.Legal.Akta, &p.Description, &cats, &p.Hours.Days,
			&p.Hours.From, &p.Hours.To, &p.Verification)
	if err != nil {
		return p, err
	}
	p.Categories = make([]api.CategoryId, len(cats))
	for i, c := range cats {
		p.Categories[i] = api.CategoryId(c)
	}
	p.Documents = []struct {
		Kind       api.OrgProfileDocumentsKind `json:"kind"`
		Name       string                      `json:"name"`
		UploadedAt time.Time                   `json:"uploadedAt"`
	}{}
	rows, err := q.Query(ctx, `SELECT kind, name, uploaded_at FROM org_documents WHERE org_id = $1 ORDER BY uploaded_at, id`, orgID)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var d struct {
			Kind       api.OrgProfileDocumentsKind `json:"kind"`
			Name       string                      `json:"name"`
			UploadedAt time.Time                   `json:"uploadedAt"`
		}
		if err := rows.Scan(&d.Kind, &d.Name, &d.UploadedAt); err != nil {
			return p, err
		}
		p.Documents = append(p.Documents, d)
	}
	return p, rows.Err()
}

func loadOrgSettings(ctx context.Context, q dbtx, orgID string) (api.OrgSettings, error) {
	var s api.OrgSettings
	var err error
	if s.Profile, err = loadOrgProfile(ctx, q, orgID); err != nil {
		return s, err
	}
	s.Roles, s.Permissions = []api.OrgRoleDef{}, api.Permissions{}
	rows, err := q.Query(ctx, `SELECT key, label, custom, permissions FROM org_roles WHERE org_id = $1 ORDER BY position, created_at`, orgID)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var r api.OrgRoleDef
		var perms []string
		if err := rows.Scan(&r.Id, &r.Label, &r.Custom, &perms); err != nil {
			rows.Close()
			return s, err
		}
		if r.Id == "owner" {
			perms = allPermissions()
		}
		m := map[string][]api.Action{}
		for _, p := range perms {
			mod, act, _ := strings.Cut(p, ".")
			m[mod] = append(m[mod], api.Action(act))
		}
		s.Roles, s.Permissions[r.Id] = append(s.Roles, r), m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return s, err
	}
	if err := q.QueryRow(ctx, `SELECT coalesce((SELECT departments FROM org_settings WHERE org_id = $1), '{Direksi,Pengadaan,Keuangan,Operasional,Penjualan}')`,
		orgID).Scan(&s.Departments); err != nil {
		return s, err
	}
	rows, err = q.Query(ctx, `SELECT id::text, label, min_amount_idr, approvers, applies_to FROM org_approval_rules WHERE org_id = $1 ORDER BY position, created_at`, orgID)
	if err != nil {
		return s, err
	}
	s.ApprovalRules, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (api.ApprovalRule, error) {
		var a api.ApprovalRule
		var applies []string
		err := r.Scan(&a.Id, &a.Label, &a.MinAmountIdr, &a.Approvers, &applies)
		for _, x := range applies {
			a.AppliesTo = append(a.AppliesTo, api.ApprovalSubject(x))
		}
		a.AppliesTo = nonNil(a.AppliesTo)
		return a, err
	})
	s.ApprovalRules = nonNil(s.ApprovalRules)
	if err != nil {
		return s, err
	}
	s.ActiveRoles, err = activeRoles(ctx, q, orgID)
	return s, err
}

func (s *Server) GetOrgSettings(ctx context.Context, req api.GetOrgSettingsRequestObject) (api.GetOrgSettingsResponseObject, error) {
	q := s.DB.Primary()
	c, err := orgAccess(ctx, q, req.OrgId)
	if err != nil {
		return nil, err
	}
	out, err := loadOrgSettings(ctx, q, c.OrgID)
	if err != nil {
		return nil, err
	}
	return api.GetOrgSettings200JSONResponse(out), nil
}

var activeProcurement = []string{"pending_approval", "approved", "published", "in_auction", "in_collective"}

func (s *Server) GetOrgOverview(ctx context.Context, req api.GetOrgOverviewRequestObject) (api.GetOrgOverviewResponseObject, error) {
	q := s.DB.Primary()
	c, err := orgAccess(ctx, q, req.OrgId)
	if err != nil {
		return nil, err
	}
	var out api.OrgOverview
	st := &out.Stats
	month := time.Now().UTC().Format("2006-01")
	rows, err := s.pgPurchases(ctx, q, c.OrgID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.Month == month {
			st.SpendMonthIdr += int(r.Spend)
			st.SavingsMonthIdr += int(r.Budget - r.Spend)
		}
	}
	var auctionStatuses []string
	err = q.QueryRow(ctx, `
		SELECT coalesce((SELECT savings_target_idr FROM org_settings WHERE org_id = $1), 10000000),
		       (SELECT count(*) FROM procurement_requests WHERE org_id = $1 AND status = ANY($2)),
		       (SELECT count(*) FROM org_suppliers WHERE org_id = $1 AND relation = 'verified'),
		       (SELECT count(*) FROM trades t JOIN parties p ON p.id IN (t.buyer_party_id, t.supplier_party_id)
		         WHERE p.org_id = $1 AND t.status NOT IN ('completed','cancelled'))`, c.OrgID, activeProcurement).
		Scan(&st.SavingsTargetIdr, &st.ActiveProcurement, &st.ActiveSuppliers, &st.RunningTransactions)
	if err != nil {
		return nil, err
	}
	auctions, err := loadOrgAuctions(ctx, q, c.OrgID, "true")
	if err != nil {
		return nil, err
	}
	active, err := activeRoles(ctx, q, c.OrgID)
	if err != nil {
		return nil, err
	}
	out.Waiting = []api.WaitingItem{}
	for _, a := range auctions {
		auctionStatuses = append(auctionStatuses, string(a.Status))
		if a.Status == "live" || a.Status == "scheduled" {
			st.ActiveAuctions++
		}
		if a.Status == "pending_approval" && canApprove(c.Role, a.RequiredApprovers, apiApprovals(a.Approvals), active) {
			out.Waiting = append(out.Waiting, api.WaitingItem{Kind: "auction", Id: a.Id, Code: a.Code, Title: a.Title, ValueIdr: a.ValueIdr, Href: "auctions?review=" + a.Id})
		}
	}
	reqs, err := loadProcurements(ctx, q, `r.org_id = $1 ORDER BY r.updated_at DESC`, c.OrgID)
	if err != nil {
		return nil, err
	}
	var statuses []string
	var waitingReqs []api.WaitingItem
	for _, r := range reqs {
		statuses = append(statuses, string(r.Status))
		if r.Status == "pending_approval" && canApprove(c.Role, r.RequiredApprovers, apiApprovals(r.Approvals), active) {
			waitingReqs = append(waitingReqs, api.WaitingItem{Kind: "procurement", Id: r.Id, Code: r.Code, Title: r.Need, ValueIdr: r.BudgetIdr, Href: "procurement/" + r.Id})
		}
	}
	out.Waiting = nonNil(append(waitingReqs, out.Waiting...))
	out.Pipeline = pipelineCounts(statuses)
	if out.Activity, err = loadAuditEntries(ctx, q, `org_id = $1 ORDER BY at DESC, id DESC LIMIT 12`, c.OrgID); err != nil {
		return nil, err
	}
	return api.GetOrgOverview200JSONResponse(out), nil
}

func apiApprovals(as []api.Approval) []approval {
	out := make([]approval, len(as))
	for i, a := range as {
		out[i] = approval{a.Role, string(a.Decision)}
	}
	return out
}

var (
	npwpRe = regexp.MustCompile(`^\d{2}\.\d{3}\.\d{3}\.\d-\d{3}\.\d{3}$`)
	nibRe  = regexp.MustCompile(`^\d{13}$`)
	hhmmRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)
)

func profileFlat(p api.OrgProfile) [][2]string {
	var cats []string
	for _, c := range p.Categories {
		cats = append(cats, categoryLbl[string(c)])
	}
	return [][2]string{{"Nama", p.Name}, {"Industri", p.Industry}, {"Lokasi", p.Location}, {"NIB", p.Legal.Nib}, {"NPWP", p.Legal.Npwp},
		{"Akta", p.Legal.Akta}, {"Deskripsi", p.Description}, {"Kategori", strings.Join(cats, ", ")},
		{"Jam operasional", fmt.Sprintf("%d hari · %s–%s", len(p.Hours.Days), p.Hours.From, p.Hours.To)}}
}

func (s *Server) UpdateOrgProfile(ctx context.Context, req api.UpdateOrgProfileRequestObject) (api.UpdateOrgProfileResponseObject, error) {
	in := *req.Body
	in.Name = strings.TrimSpace(in.Name)
	f := map[string]string{}
	if in.Name == "" {
		f["name"] = "Nama perusahaan wajib diisi"
	}
	if in.Legal.Npwp != "" && !npwpRe.MatchString(in.Legal.Npwp) {
		f["npwp"] = "Format NPWP: 00.000.000.0-000.000"
	}
	if in.Legal.Nib != "" && !nibRe.MatchString(in.Legal.Nib) {
		f["nib"] = "NIB terdiri dari 13 digit"
	}
	days := []int{}
	for _, d := range in.Hours.Days {
		if d < 0 || d > 6 {
			f["hours"] = "Hari operasional tidak valid"
		} else if !slices.Contains(days, d) {
			days = append(days, d)
		}
	}
	slices.Sort(days)
	in.Hours.Days = days
	if !hhmmRe.MatchString(in.Hours.From) || !hhmmRe.MatchString(in.Hours.To) || in.Hours.From >= in.Hours.To {
		f["hours"] = "Jam buka harus sebelum jam tutup (HH:MM)"
	}
	var out api.OrgProfile
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("profile", "manage"); err != nil {
			return err
		}
		if err := invalid("Periksa kembali isian profil", f); err != nil {
			return err
		}
		prev, err := loadOrgProfile(ctx, tx, c.OrgID)
		if err != nil {
			return err
		}
		cats := []string{}
		for _, c := range in.Categories {
			if !slices.Contains(cats, string(c)) {
				cats = append(cats, string(c))
			}
		}
		err = pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
			_, err := sp.Exec(ctx, `UPDATE orgs SET name = $2 WHERE id = $1`, c.OrgID, in.Name)
			return err
		})
		if uniqueViolation(err, "orgs_name_key") {
			return invalid("Periksa kembali isian profil", map[string]string{"name": "Nama ini sudah terdaftar. Pakai nama lain."})
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE org_profiles SET industry = $2, location = $3, nib = $4, npwp = $5, akta = $6, description = $7, categories = $8::category_id[],
			       hours_days = $9::smallint[], hours_from = $10::time, hours_to = $11::time
			WHERE org_id = $1`, c.OrgID, strings.TrimSpace(in.Industry), strings.TrimSpace(in.Location), in.Legal.Nib, in.Legal.Npwp,
			strings.TrimSpace(in.Legal.Akta), strings.TrimSpace(in.Description), cats, in.Hours.Days, in.Hours.From, in.Hours.To); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE parties SET name = $2 WHERE org_id = $1`, c.OrgID, in.Name); err != nil {
			return err
		}
		if out, err = loadOrgProfile(ctx, tx, c.OrgID); err != nil {
			return err
		}
		var changes []change
		before, after := profileFlat(prev), profileFlat(out)
		for i := range after {
			if before[i][1] != after[i][1] {
				ch := change{Field: after[i][0], After: after[i][1]}
				if before[i][1] != "" {
					ch.Before = &before[i][1]
				}
				changes = append(changes, ch)
			}
		}
		return c.audit(ctx, tx, "Ubah profil bisnis", "business", c.OrgID, out.Name, nil, changes...)
	})
	if err != nil {
		return nil, err
	}
	return api.UpdateOrgProfile200JSONResponse(out), nil
}

func (s *Server) UploadOrgDocument(ctx context.Context, req api.UploadOrgDocumentRequestObject) (api.UploadOrgDocumentResponseObject, error) {
	var out api.OrgProfile
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("profile", "manage"); err != nil {
			return err
		}
		u, err := s.claimUpload(ctx, tx, c.sess.UserID, req.Body.UploadId, api.UploadPurposeOrgDocument, "uploadId")
		if err != nil {
			return err
		}
		kind := string(req.Body.Kind)
		if kind == "other" {
			_, err = tx.Exec(ctx, `INSERT INTO org_documents (org_id, kind, name, object_key, uploaded_by) VALUES ($1, $2, $3, $4, $5)`,
				c.OrgID, kind, u.FileName, u.ObjectKey, c.sess.UserID)
		} else {
			_, err = tx.Exec(ctx, `
				INSERT INTO org_documents (org_id, kind, name, object_key, uploaded_by) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (org_id, kind) WHERE kind <> 'other'
				DO UPDATE SET name = EXCLUDED.name, object_key = EXCLUDED.object_key, uploaded_by = EXCLUDED.uploaded_by, uploaded_at = now()`,
				c.OrgID, kind, u.FileName, u.ObjectKey, c.sess.UserID)
		}
		if err != nil {
			return err
		}
		if err := c.audit(ctx, tx, "Unggah dokumen verifikasi", "business", c.OrgID, u.FileName, nil); err != nil {
			return err
		}
		out, err = loadOrgProfile(ctx, tx, c.OrgID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.UploadOrgDocument200JSONResponse(out), nil
}

func (s *Server) RequestOrgVerification(ctx context.Context, req api.RequestOrgVerificationRequestObject) (api.RequestOrgVerificationResponseObject, error) {
	var out api.OrgProfile
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("profile", "manage"); err != nil {
			return err
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT verification FROM org_profiles WHERE org_id = $1 FOR UPDATE`, c.OrgID).Scan(&status); err != nil {
			return err
		}
		switch status {
		case "pending":
			return conflict("already_submitted", "Pengajuan verifikasi sedang ditinjau")
		case "verified":
			return conflict("verified", "Bisnis ini sudah terverifikasi")
		}
		p, err := loadOrgProfile(ctx, tx, c.OrgID)
		if err != nil {
			return err
		}
		var missing []string
		for _, k := range []string{"nib", "npwp", "akta"} {
			if !slices.ContainsFunc(p.Documents, func(d struct {
				Kind       api.OrgProfileDocumentsKind `json:"kind"`
				Name       string                      `json:"name"`
				UploadedAt time.Time                   `json:"uploadedAt"`
			}) bool {
				return string(d.Kind) == k
			}) {
				missing = append(missing, strings.ToUpper(k))
			}
		}
		if len(missing) > 0 {
			return &Error{Status: 422, Code: "validation", Message: "Dokumen belum lengkap: " + strings.Join(missing, ", ")}
		}
		form, _ := json.Marshal([]api.LabeledValue{{Label: "Nama badan usaha", Value: p.Name}, {Label: "NIB", Value: p.Legal.Nib},
			{Label: "NPWP", Value: p.Legal.Npwp}, {Label: "Alamat", Value: p.Location}, {Label: "Penanggung jawab", Value: c.sess.Name},
			{Label: "Bidang usaha", Value: p.Industry}})
		var requestID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO verification_requests (kind, org_id, submitted_by, subject_name, form) VALUES ('business', $1, $2, $3, $4::jsonb) RETURNING id`,
			c.OrgID, c.sess.UserID, p.Name, form).Scan(&requestID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO verification_documents (request_id, kind, file_name, object_key)
			SELECT $1, kind, name, coalesce(object_key, '') FROM org_documents WHERE org_id = $2 AND kind IN ('nib','npwp','akta')`, requestID, c.OrgID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE org_profiles SET verification = 'pending', verification_request_id = $2 WHERE org_id = $1`, c.OrgID, requestID); err != nil {
			return err
		}
		if err := notifyAdmins(ctx, tx, notification{Type: "transaction_update", Title: "Pengajuan verifikasi bisnis baru",
			Body: fmt.Sprintf("%s · %s", p.Name, c.sess.Name), Href: "/admin/verification/" + requestID}); err != nil {
			return err
		}
		if err := c.audit(ctx, tx, "Ajukan verifikasi bisnis", "business", c.OrgID, p.Name, nil,
			change{Field: "Verifikasi", Before: &status, After: "pending"}); err != nil {
			return err
		}
		out, err = loadOrgProfile(ctx, tx, c.OrgID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.RequestOrgVerification200JSONResponse(out), nil
}

func (s *Server) GetOrgTeam(ctx context.Context, req api.GetOrgTeamRequestObject) (api.GetOrgTeamResponseObject, error) {
	q := s.DB.Primary()
	c, err := orgAccess(ctx, q, req.OrgId)
	if err != nil {
		return nil, err
	}
	st, err := loadOrgSettings(ctx, q, c.OrgID)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
		SELECT id::text, name, email::text, role, coalesce(department, ''), status, coalesce(joined_at, created_at)
		FROM org_members WHERE org_id = $1 ORDER BY status, coalesce(joined_at, created_at), id`, c.OrgID)
	if err != nil {
		return nil, err
	}
	members, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (api.OrgMember, error) {
		var m api.OrgMember
		return m, r.Scan(&m.Id, &m.Name, &m.Email, &m.Role, &m.Department, &m.Status, &m.JoinedAt)
	})
	if err != nil {
		return nil, err
	}
	return api.GetOrgTeam200JSONResponse{Profile: st.Profile, Roles: st.Roles, Permissions: st.Permissions, Departments: st.Departments,
		ApprovalRules: st.ApprovalRules, Members: nonNil(members)}, nil
}

var emailRe = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

func (s *Server) InviteOrgMember(ctx context.Context, req api.InviteOrgMemberRequestObject) (api.InviteOrgMemberResponseObject, error) {
	email := strings.ToLower(strings.TrimSpace(string(req.Body.Email)))
	var invite *mail.Message
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("team", "manage"); err != nil {
			return err
		}
		if !emailRe.MatchString(email) {
			return invalid("Email tidak valid", map[string]string{"email": "Masukkan email yang valid"})
		}
		if err := lockOrg(ctx, tx, c.OrgID); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM org_members WHERE org_id = $1 AND email = $2)`, c.OrgID, email).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return &Error{Status: http.StatusConflict, Code: "exists", Message: "Sudah anggota", Fields: map[string]string{"email": "Email ini sudah ada di tim"}}
		}
		var label string
		if err := tx.QueryRow(ctx, `SELECT label FROM org_roles WHERE org_id = $1 AND key = $2`, c.OrgID, req.Body.Role).Scan(&label); errors.Is(err, pgx.ErrNoRows) {
			return invalid("Pilih peran", map[string]string{"role": "Pilih peran"})
		} else if err != nil {
			return err
		}
		var memberID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO org_members (org_id, email, name, role, department, status, invited_by) VALUES ($1, $2::text, $2::text, $3, $4, 'invited', $5) RETURNING id`,
			c.OrgID, email, req.Body.Role, strings.TrimSpace(deref(req.Body.Department)), c.sess.UserID).Scan(&memberID); err != nil {
			return err
		}
		if err := c.audit(ctx, tx, "Undang anggota", "user", email, email, nil, change{Field: "Peran", After: label}); err != nil {
			return err
		}
		n := notification{Type: "transaction_update", Title: "Undangan bergabung ke " + c.OrgName,
			Body: fmt.Sprintf("%s mengundangmu sebagai %s.", c.actor(), label), Href: "/app?invitation=" + memberID}

		var userID string
		err = tx.QueryRow(ctx, `SELECT id::text FROM users WHERE email = $1`, email).Scan(&userID)
		switch {
		case err == nil:
			return notify(ctx, tx, userID, n)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		m := mail.NotificationEmail(email, email, mail.Notification{Type: n.Type, Title: n.Title, Body: n.Body,
			URL: s.AppURL + "/register?email=" + email, Action: "Lihat undangan"}, s.AppURL+"/app/settings")
		invite = &m
		return nil
	})
	if err != nil {
		return nil, err
	}
	if invite != nil {
		s.send(ctx, *invite)
	}
	return api.InviteOrgMember204Response{}, nil
}

type memberRow struct {
	ID, Name, Role, Status string
	Department, UserID     *string
}

func lockMember(ctx context.Context, tx pgx.Tx, orgID, memberID string) (memberRow, error) {
	var m memberRow
	err := tx.QueryRow(ctx, `SELECT id::text, name, role, status, department, user_id::text FROM org_members WHERE org_id = $1 AND id::text = $2 FOR UPDATE`,
		orgID, memberID).Scan(&m.ID, &m.Name, &m.Role, &m.Status, &m.Department, &m.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, notFound("Anggota tidak ditemukan")
	}
	return m, err
}

func lastOwner(ctx context.Context, tx pgx.Tx, orgID string, m memberRow) (bool, error) {
	if m.Role != "owner" || m.Status != "active" {
		return false, nil
	}
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM org_members WHERE org_id = $1 AND role = 'owner' AND status = 'active'`, orgID).Scan(&n)
	return n < 2, err
}

func roleLabelOf(ctx context.Context, q dbtx, orgID, role string) string {
	label := role
	_ = q.QueryRow(ctx, `SELECT label FROM org_roles WHERE org_id = $1 AND key = $2`, orgID, role).Scan(&label)
	return label
}

func (s *Server) UpdateOrgMember(ctx context.Context, req api.UpdateOrgMemberRequestObject) (api.UpdateOrgMemberResponseObject, error) {
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("team", "manage"); err != nil {
			return err
		}
		if err := lockOrg(ctx, tx, c.OrgID); err != nil {
			return err
		}
		m, err := lockMember(ctx, tx, c.OrgID, req.MemberId)
		if err != nil {
			return err
		}
		var changes []change
		role, dept := m.Role, deref(m.Department)
		if p := req.Body.Role; p != nil && *p != m.Role {
			var label string
			if err := tx.QueryRow(ctx, `SELECT label FROM org_roles WHERE org_id = $1 AND key = $2`, c.OrgID, *p).Scan(&label); errors.Is(err, pgx.ErrNoRows) {
				return invalid("Pilih peran", map[string]string{"role": "Pilih peran"})
			} else if err != nil {
				return err
			}
			if last, err := lastOwner(ctx, tx, c.OrgID, m); err != nil {
				return err
			} else if last {
				return &Error{Status: http.StatusConflict, Code: "last_owner", Message: "Organisasi butuh minimal satu Owner", Fields: map[string]string{"role": "Tunjuk Owner lain dulu"}}
			}
			before := roleLabelOf(ctx, tx, c.OrgID, m.Role)
			changes = append(changes, change{Field: "Peran", Before: &before, After: label})
			role = *p
		}
		if p := req.Body.Department; p != nil && strings.TrimSpace(*p) != dept {
			changes = append(changes, change{Field: "Departemen", Before: &dept, After: strings.TrimSpace(*p)})
			dept = strings.TrimSpace(*p)
		}
		if _, err := tx.Exec(ctx, `UPDATE org_members SET role = $2, department = $3 WHERE id = $1`, m.ID, role, dept); err != nil {
			return err
		}
		return c.audit(ctx, tx, "Ubah anggota", "user", m.ID, m.Name, nil, changes...)
	})
	if err != nil {
		return nil, err
	}
	return api.UpdateOrgMember204Response{}, nil
}

func (s *Server) RemoveOrgMember(ctx context.Context, req api.RemoveOrgMemberRequestObject) (api.RemoveOrgMemberResponseObject, error) {
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("team", "manage"); err != nil {
			return err
		}
		if err := lockOrg(ctx, tx, c.OrgID); err != nil {
			return err
		}
		m, err := lockMember(ctx, tx, c.OrgID, req.MemberId)
		if err != nil {
			return err
		}
		if m.UserID != nil && *m.UserID == c.sess.UserID {
			return conflict("self", "Kamu tidak bisa mengeluarkan dirimu sendiri")
		}
		if last, err := lastOwner(ctx, tx, c.OrgID, m); err != nil {
			return err
		} else if last {
			return conflict("last_owner", "Organisasi butuh minimal satu Owner")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM org_members WHERE id = $1`, m.ID); err != nil {
			return err
		}
		return c.audit(ctx, tx, "Keluarkan anggota", "user", m.ID, m.Name, nil)
	})
	if err != nil {
		return nil, err
	}
	return api.RemoveOrgMember204Response{}, nil
}

var customRoleRe = regexp.MustCompile(`^custom-[a-z0-9][a-z0-9-]*$`)

func (s *Server) UpdateOrgTeamSettings(ctx context.Context, req api.UpdateOrgTeamSettingsRequestObject) (api.UpdateOrgTeamSettingsResponseObject, error) {
	in := req.Body
	var out api.OrgSettings
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("team", "manage"); err != nil {
			return err
		}
		if err := lockOrg(ctx, tx, c.OrgID); err != nil {
			return err
		}
		f := map[string]string{}
		ids := []string{}
		for _, r := range in.Roles {
			_, builtIn := orgRoleLabel[r.Id]
			switch {
			case strings.TrimSpace(r.Label) == "":
				f["roles"] = "Nama peran tidak boleh kosong"
			case !builtIn && !customRoleRe.MatchString(r.Id):
				f["roles"] = "Id peran kustom tidak valid: " + r.Id
			case slices.Contains(ids, r.Id):
				f["roles"] = "Peran ganda: " + r.Id
			}
			ids = append(ids, r.Id)
		}
		for _, b := range builtInRoleIDs {
			if !slices.Contains(ids, b) {
				f["roles"] = "Peran bawaan tidak boleh dihapus: " + orgRoleLabel[b]
			}
		}
		perms := map[string][]string{}
		for role, mods := range in.Permissions {
			if !slices.Contains(ids, role) {
				f["permissions"] = "Peran tidak dikenal: " + role
				continue
			}
			for mod, acts := range mods {
				for _, a := range acts {
					if !slices.Contains(orgModules, mod) || !slices.Contains(orgActions, string(a)) {
						f["permissions"] = fmt.Sprintf("Izin tidak dikenal: %s.%s", mod, a)
					} else if p := mod + "." + string(a); !slices.Contains(perms[role], p) {
						perms[role] = append(perms[role], p)
					}
				}
			}
		}
		for i, r := range in.ApprovalRules {
			key := fmt.Sprintf("rule-%d", i)
			switch {
			case strings.TrimSpace(r.Label) == "":
				f[key] = "Beri nama aturan"
			case r.MinAmountIdr < 0:
				f[key] = "Ambang harus angka ≥ 0"
			case len(r.Approvers) == 0:
				f[key] = "Pilih minimal satu approver"
			case len(r.AppliesTo) == 0:
				f[key] = "Pilih procurement dan/atau auction"
			}
			for _, a := range r.Approvers {
				if !slices.Contains(ids, a) {
					f[key] = "Approver bukan peran organisasi ini: " + a
				}
			}
		}

		rows, err := tx.Query(ctx, `
			SELECT DISTINCT r.label FROM org_roles r JOIN org_members m ON m.org_id = r.org_id AND m.role = r.key
			WHERE r.org_id = $1 AND NOT (r.key = ANY($2)) ORDER BY 1`, c.OrgID, ids)
		if err != nil {
			return err
		}
		held, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if len(held) > 0 {
			f["roles"] = "Peran masih dipakai anggota: " + strings.Join(held, ", ")
		}
		if err := invalid("Periksa pengaturan tim", f); err != nil {
			return err
		}

		before, err := loadOrgSettings(ctx, tx, c.OrgID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM org_roles WHERE org_id = $1 AND NOT (key = ANY($2))`, c.OrgID, ids); err != nil {
			return err
		}
		for i, r := range in.Roles {
			p := nonNil(perms[r.Id])
			if r.Id == "owner" {
				p = allPermissions()
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO org_roles (org_id, key, label, position, permissions) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (org_id, key) DO UPDATE SET label = EXCLUDED.label, position = EXCLUDED.position, permissions = EXCLUDED.permissions`,
				c.OrgID, r.Id, strings.TrimSpace(r.Label), i, p); err != nil {
				return err
			}
		}
		depts := []string{}
		for _, d := range in.Departments {
			if d = strings.TrimSpace(d); d != "" && !slices.Contains(depts, d) {
				depts = append(depts, d)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO org_settings (org_id, departments) VALUES ($1, $2) ON CONFLICT (org_id) DO UPDATE SET departments = EXCLUDED.departments`,
			c.OrgID, depts); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM org_approval_rules WHERE org_id = $1`, c.OrgID); err != nil {
			return err
		}
		for i, r := range in.ApprovalRules {
			var approvers, applies []string
			for _, a := range r.Approvers {
				if !slices.Contains(approvers, a) {
					approvers = append(approvers, a)
				}
			}
			for _, a := range r.AppliesTo {
				if !slices.Contains(applies, string(a)) {
					applies = append(applies, string(a))
				}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO org_approval_rules (org_id, position, label, min_amount_idr, approvers, applies_to) VALUES ($1, $2, $3, $4, $5, $6)`,
				c.OrgID, i, strings.TrimSpace(r.Label), r.MinAmountIdr, approvers, applies); err != nil {
				return err
			}
		}
		if out, err = loadOrgSettings(ctx, tx, c.OrgID); err != nil {
			return err
		}
		return c.audit(ctx, tx, "Ubah pengaturan tim", "rule", c.OrgID, out.Profile.Name, nil, settingsChanges(before, out)...)
	})
	if err != nil {
		return nil, err
	}
	return api.UpdateOrgTeamSettings200JSONResponse(out), nil
}

func settingsChanges(a, b api.OrgSettings) []change {
	label := func(s api.OrgSettings, role string) string {
		for _, r := range s.Roles {
			if r.Id == role {
				return r.Label
			}
		}
		return role
	}
	roles := func(s api.OrgSettings) string {
		var l []string
		for _, r := range s.Roles {
			l = append(l, r.Label)
		}
		return strings.Join(l, ", ")
	}
	rules := func(s api.OrgSettings) string {
		var l []string
		for _, r := range s.ApprovalRules {
			var who []string
			for _, a := range r.Approvers {
				who = append(who, label(s, a))
			}
			l = append(l, fmt.Sprintf("%s: %s", r.Label, strings.Join(who, " + ")))
		}
		return strings.Join(l, "; ")
	}
	sameJSON := func(x, y any) bool {
		bx, _ := json.Marshal(x)
		by, _ := json.Marshal(y)
		return string(bx) == string(by)
	}
	stripIDs := func(rs []api.ApprovalRule) []api.ApprovalRule {
		out := slices.Clone(rs)
		for i := range out {
			out[i].Id = ""
		}
		return out
	}
	var out []change
	if !sameJSON(a.Roles, b.Roles) {
		before := roles(a)
		out = append(out, change{Field: "Peran", Before: &before, After: roles(b)})
	}
	if !sameJSON(a.Permissions, b.Permissions) {
		out = append(out, change{Field: "Matriks izin", After: "diperbarui"})
	}
	if !slices.Equal(a.Departments, b.Departments) {
		before := strings.Join(a.Departments, ", ")
		out = append(out, change{Field: "Departemen", Before: &before, After: strings.Join(b.Departments, ", ")})
	}
	if !sameJSON(stripIDs(a.ApprovalRules), stripIDs(b.ApprovalRules)) {
		before := rules(a)
		out = append(out, change{Field: "Aturan approval", Before: &before, After: rules(b)})
	}
	return out
}

func (s *Server) GetOrgInventory(ctx context.Context, req api.GetOrgInventoryRequestObject) (api.GetOrgInventoryResponseObject, error) {
	q := s.DB.Primary()
	c, err := orgAccess(ctx, q, req.OrgId)
	if err != nil {
		return nil, err
	}
	var out api.InventoryData
	rows, err := q.Query(ctx, `
		SELECT id::text, sku, name, coalesce(category_id::text, ''), warehouse, quantity, unit, moq, lead_time_days, quality_spec
		FROM inventory_items WHERE org_id = $1 ORDER BY created_at DESC, id`, c.OrgID)
	if err != nil {
		return nil, err
	}
	if out.Items, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (api.InventoryItem, error) {
		var i api.InventoryItem
		var lead int
		err := r.Scan(&i.Id, &i.Sku, &i.Name, &i.CategoryId, &i.Warehouse, &i.Quantity.Value, &i.Quantity.Unit, &i.Moq, &lead, &i.QualitySpec)
		i.LeadTimeDays = float64(lead)
		return i, err
	}); err != nil {
		return nil, err
	}
	out.Items = nonNil(out.Items)
	if rows, err = q.Query(ctx, `SELECT name, location, capacity_m2 FROM org_warehouses WHERE org_id = $1 ORDER BY created_at, name`, c.OrgID); err != nil {
		return nil, err
	}
	for rows.Next() {
		var w struct {
			CapacityM2 float64 `json:"capacityM2"`
			Location   string  `json:"location"`
			Name       string  `json:"name"`
		}
		if err := rows.Scan(&w.Name, &w.Location, &w.CapacityM2); err != nil {
			return nil, err
		}
		out.Warehouses = append(out.Warehouses, w)
	}
	if rows, err = q.Query(ctx, `SELECT line, output_value, output_unit, utilization FROM org_production_lines WHERE org_id = $1 ORDER BY created_at, line`, c.OrgID); err != nil {
		return nil, err
	}
	for rows.Next() {
		var l struct {
			Line           string       `json:"line"`
			OutputPerMonth api.Quantity `json:"outputPerMonth"`
			Utilization    float64      `json:"utilization"`
		}
		if err := rows.Scan(&l.Line, &l.OutputPerMonth.Value, &l.OutputPerMonth.Unit, &l.Utilization); err != nil {
			return nil, err
		}
		out.Capacity = append(out.Capacity, l)
	}
	if rows, err = q.Query(ctx, `SELECT vehicle_type, count, capacity FROM org_fleet WHERE org_id = $1 ORDER BY created_at, vehicle_type`, c.OrgID); err != nil {
		return nil, err
	}
	for rows.Next() {
		var f struct {
			Capacity string  `json:"capacity"`
			Count    float64 `json:"count"`
			Type     string  `json:"type"`
		}
		var n int
		if err := rows.Scan(&f.Type, &n, &f.Capacity); err != nil {
			return nil, err
		}
		f.Count = float64(n)
		out.Logistics.Fleet = append(out.Logistics.Fleet, f)
	}
	if err := q.QueryRow(ctx, `SELECT coalesce((SELECT service_regions FROM org_settings WHERE org_id = $1), '{}')`, c.OrgID).Scan(&out.Logistics.Regions); err != nil {
		return nil, err
	}
	if rows, err = q.Query(ctx, `
		SELECT id::text, item, quantity, unit, every, counterparty, direction, next_at FROM supply_schedules WHERE org_id = $1 ORDER BY next_at, id`, c.OrgID); err != nil {
		return nil, err
	}
	for rows.Next() {
		var sc struct {
			Counterparty string                              `json:"counterparty"`
			Direction    api.InventoryDataSchedulesDirection `json:"direction"`
			Every        api.InventoryDataSchedulesEvery     `json:"every"`
			Id           string                              `json:"id"`
			Item         string                              `json:"item"`
			NextAt       time.Time                           `json:"nextAt"`
			Quantity     api.Quantity                        `json:"quantity"`
		}
		if err := rows.Scan(&sc.Id, &sc.Item, &sc.Quantity.Value, &sc.Quantity.Unit, &sc.Every, &sc.Counterparty, &sc.Direction, &sc.NextAt); err != nil {
			return nil, err
		}
		out.Schedules = append(out.Schedules, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.Warehouses, out.Capacity, out.Logistics.Fleet, out.Schedules = nonNil(out.Warehouses), nonNil(out.Capacity), nonNil(out.Logistics.Fleet), nonNil(out.Schedules)
	return api.GetOrgInventory200JSONResponse(out), nil
}

type inventoryItem struct {
	SKU, Name, Category, Warehouse, Unit, Spec string
	Qty, MOQ                                   float64
	Lead                                       int
}

func normaliseItem(in api.OrgInventoryItemInput, n int) (inventoryItem, string) {
	it := inventoryItem{SKU: strings.TrimSpace(deref(in.Sku)), Name: strings.TrimSpace(in.Name), Warehouse: strings.TrimSpace(deref(in.Warehouse)),
		Unit: strings.TrimSpace(in.Quantity.Unit), Spec: strings.TrimSpace(deref(in.QualitySpec)), Qty: in.Quantity.Value, Category: string(in.CategoryId)}
	if in.Moq != nil {
		it.MOQ = *in.Moq
	}
	if in.LeadTimeDays != nil {
		it.Lead = int(*in.LeadTimeDays)
	}
	row := fmt.Sprintf("Baris %d: ", n+1)
	switch {
	case it.Name == "":
		return it, row + "nama kosong"
	case !(it.Qty >= 0):
		return it, row + "jumlah bukan angka"
	case categoryLbl[it.Category] == "":
		return it, fmt.Sprintf("%skategori \"%s\" tidak dikenal", row, it.Category)
	case it.MOQ < 0 || it.Lead < 0:
		return it, row + "MOQ dan lead time harus ≥ 0"
	}
	if it.SKU == "" {
		it.SKU = fmt.Sprintf("SKU-%d", n)
	}
	if it.Warehouse == "" {
		it.Warehouse = "Gudang utama"
	}
	if it.Unit == "" {
		it.Unit = "unit"
	}
	return it, ""
}

func insertItem(ctx context.Context, tx pgx.Tx, c *orgCtx, it inventoryItem) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO inventory_items (org_id, sku, name, category_id, warehouse, quantity, unit, moq, lead_time_days, quality_spec, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, clock_timestamp())`,
		c.OrgID, it.SKU, it.Name, it.Category, it.Warehouse, it.Qty, it.Unit, it.MOQ, it.Lead, it.Spec, c.sess.UserID)
	return err
}

func (s *Server) AddOrgInventoryItem(ctx context.Context, req api.AddOrgInventoryItemRequestObject) (api.AddOrgInventoryItemResponseObject, error) {
	in := *req.Body
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("inventory", "create"); err != nil {
			return err
		}
		f := map[string]string{}
		if strings.TrimSpace(in.Name) == "" {
			f["name"] = "Nama item wajib diisi"
		}
		if !(in.Quantity.Value >= 0) || strings.TrimSpace(in.Quantity.Unit) == "" {
			f["quantity"] = "Jumlah harus angka ≥ 0"
		}
		if in.Moq != nil && *in.Moq < 0 {
			f["moq"] = "MOQ harus ≥ 0"
		}
		if in.LeadTimeDays != nil && *in.LeadTimeDays < 0 {
			f["leadTimeDays"] = "Lead time harus ≥ 0"
		}
		if err := invalid("Periksa isian item", f); err != nil {
			return err
		}
		it, _ := normaliseItem(in, 1)
		it.SKU, it.Warehouse = strings.TrimSpace(deref(in.Sku)), strings.TrimSpace(deref(in.Warehouse))
		if err := insertItem(ctx, tx, c, it); err != nil {
			return err
		}
		return c.audit(ctx, tx, "Tambah item inventory", "business", c.OrgID, it.Name, nil)
	})
	if err != nil {
		return nil, err
	}
	return api.AddOrgInventoryItem201Response{}, nil
}

func (s *Server) ImportOrgInventory(ctx context.Context, req api.ImportOrgInventoryRequestObject) (api.ImportOrgInventoryResponseObject, error) {
	in := req.Body
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("inventory", "create"); err != nil {
			return err
		}
		if len(in.Items) > 5000 {
			return &Error{Status: 422, Code: "validation", Message: "Maksimal 5.000 baris per import"}
		}
		items := make([]inventoryItem, len(in.Items))
		for i, raw := range in.Items {
			it, msg := normaliseItem(raw, i+1)
			if msg != "" {
				return &Error{Status: 422, Code: "validation", Message: msg}
			}
			items[i] = it
		}

		for i := len(items) - 1; i >= 0; i-- {
			if err := insertItem(ctx, tx, c, items[i]); err != nil {
				return err
			}
		}
		return c.audit(ctx, tx, fmt.Sprintf("Import %d item inventory", len(items)), "business", c.OrgID, nonEmpty(in.FileName, "-"), nil)
	})
	if err != nil {
		return nil, err
	}
	return api.ImportOrgInventory200JSONResponse{Imported: len(in.Items)}, nil
}

func (s *Server) AddOrgSupplySchedule(ctx context.Context, req api.AddOrgSupplyScheduleRequestObject) (api.AddOrgSupplyScheduleResponseObject, error) {
	in := req.Body
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("inventory", "manage"); err != nil {
			return err
		}
		if strings.TrimSpace(in.Item) == "" {
			return invalid("Isi item", map[string]string{"item": "Item wajib diisi"})
		}
		if !(in.Quantity.Value > 0) || strings.TrimSpace(in.Quantity.Unit) == "" {
			return invalid("Isi jumlah", map[string]string{"quantity": "Jumlah harus > 0"})
		}
		every, direction, next := "monthly", "in", time.Now()
		if in.Every != nil {
			every = string(*in.Every)
		}
		if in.Direction != nil {
			direction = string(*in.Direction)
		}
		if in.NextAt != nil {
			next = *in.NextAt
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO supply_schedules (org_id, item, quantity, unit, every, counterparty, direction, next_at, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			c.OrgID, strings.TrimSpace(in.Item), in.Quantity.Value, strings.TrimSpace(in.Quantity.Unit), every, strings.TrimSpace(deref(in.Counterparty)),
			direction, next, c.sess.UserID); err != nil {
			return err
		}
		return c.audit(ctx, tx, "Tambah jadwal pasokan rutin", "business", c.OrgID, strings.TrimSpace(in.Item), nil)
	})
	if err != nil {
		return nil, err
	}
	return api.AddOrgSupplySchedule201Response{}, nil
}
