package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

const minExperience = 30

func mmApplicationErrors(in api.MmApplicationInput) map[string]string {
	f := map[string]string{}
	if utf8.RuneCountInString(strings.TrimSpace(in.Organization)) < 3 {
		f["organization"] = "Isi nama organisasi"
	}
	if len(in.Categories) == 0 {
		f["categories"] = "Pilih minimal satu kategori"
	}
	if utf8.RuneCountInString(strings.TrimSpace(in.Experience)) < minExperience {
		f["experience"] = fmt.Sprintf("Ceritakan minimal %d karakter", minExperience)
	}
	return f
}

func mmApplyBlocked(isMarketMaker bool, latestStatus string) string {
	switch {
	case isMarketMaker:
		return "Akunmu sudah Market Maker"
	case latestStatus == "pending":
		return "Pengajuanmu masih direview"
	}
	return ""
}

var orgTypes = []string{"PT", "CV", "Koperasi", "UMKM", "Asosiasi", "Kelompok tani", "Yayasan"}

func newOrgErrors(in api.NewOrgInput, taken []string) map[string]string {
	f := map[string]string{}
	name := strings.TrimSpace(in.Name)
	if utf8.RuneCountInString(name) < 3 {
		f["name"] = "Minimal 3 karakter"
	} else {
		for _, t := range taken {
			if strings.EqualFold(t, name) {
				f["name"] = "Kamu sudah tergabung di organisasi dengan nama ini"
			}
		}
	}
	if !slices.Contains(orgTypes, string(in.Type)) {
		f["type"] = "Pilih jenis organisasi"
	}
	if d := npwpDigits(in.Npwp); d != "" && len(d) != 15 && len(d) != 16 {
		f["npwp"] = "NPWP 15 atau 16 digit"
	}
	if _, ok := categoryLabels[string(in.CategoryId)]; !ok {
		f["categoryId"] = "Pilih kategori"
	}
	return f
}

func npwpDigits(p *string) string {
	if p == nil {
		return ""
	}
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, *p)
}

var categoryLabels = map[string]string{"agri": "Pertanian", "food": "Pangan", "packaging": "Kemasan", "manufacturing": "Manufaktur",
	"logistics": "Logistik", "it": "Jasa IT", "energy": "Energi"}

const mmApplicationSelect = `
	SELECT a.id, a.user_id, u.name, u.email, a.organization, a.categories::text[], a.experience, a.documents, a.status, a.created_at,
	       a.decided_at, du.name || ' (Admin)', a.decision_reason
	FROM mm_applications a JOIN users u ON u.id = a.user_id LEFT JOIN users du ON du.id = a.decided_by`

func loadMmApplications(ctx context.Context, q dbtx, where string, args ...any) ([]api.MmApplication, error) {
	rows, err := q.Query(ctx, mmApplicationSelect+" WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.MmApplication{}
	for rows.Next() {
		var a api.MmApplication
		var cats []string
		var at *time.Time
		var by *string
		var reason *string
		if err := rows.Scan(&a.Id, &a.UserId, &a.Applicant, &a.Email, &a.Organization, &cats, &a.Experience, &a.Documents, &a.Status,
			&a.SubmittedAt, &at, &by, &reason); err != nil {
			return nil, err
		}
		a.Categories = []api.CategoryId{}
		for _, c := range cats {
			a.Categories = append(a.Categories, api.CategoryId(c))
		}
		if at != nil {
			a.Decision = &struct {
				At     time.Time `json:"at"`
				By     string    `json:"by"`
				Reason *string   `json:"reason,omitempty"`
			}{*at, deref(by), reason}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type mmApplicationResponse struct{ a *api.MmApplication }

func (r mmApplicationResponse) VisitGetMyMmApplicationResponse(w http.ResponseWriter) error {
	if r.a != nil {
		return api.GetMyMmApplication200JSONResponse(*r.a).VisitGetMyMmApplicationResponse(w)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte("null\n"))
	return err
}

func (s *Server) GetMyMmApplication(ctx context.Context, _ api.GetMyMmApplicationRequestObject) (api.GetMyMmApplicationResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	as, err := loadMmApplications(ctx, s.DB.Primary(), `a.user_id = $1 ORDER BY a.created_at DESC LIMIT 1`, sess.UserID)
	if err != nil {
		return nil, err
	}
	if len(as) == 0 {
		return mmApplicationResponse{}, nil
	}
	return mmApplicationResponse{&as[0]}, nil
}

func (s *Server) ApplyMarketMaker(ctx context.Context, req api.ApplyMarketMakerRequestObject) (api.ApplyMarketMakerResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	in := *req.Body
	var out api.MmApplication
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, sess.UserID); err != nil {
			return err
		}
		var isMM bool
		var latest string
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM user_capabilities WHERE user_id = $1 AND capability = 'market_maker'),
			       coalesce((SELECT status FROM mm_applications WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1), '')`,
			sess.UserID).Scan(&isMM, &latest); err != nil {
			return err
		}
		if msg := mmApplyBlocked(isMM, latest); msg != "" {
			return conflict("conflict", msg)
		}
		if f := mmApplicationErrors(in); len(f) > 0 {
			return &Error{Status: 422, Code: "validation", Message: "Periksa lagi isian pengajuan", Fields: f}
		}
		cats := make([]string, len(in.Categories))
		for i, c := range in.Categories {
			cats[i] = string(c)
		}
		org := strings.TrimSpace(in.Organization)
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO mm_applications (user_id, organization, categories, experience, documents) VALUES ($1, $2, $3::category_id[], $4, $5) RETURNING id`,
			sess.UserID, org, cats, strings.TrimSpace(in.Experience), strings.TrimSpace(in.Documents)).Scan(&id); err != nil {
			return err
		}
		if err := writeAudit(ctx, tx, audit{ActorUserID: &sess.UserID, ActorLabel: sess.Name, Action: "Ajukan Market Maker",
			EntityType: "user", EntityID: sess.UserID, EntityLabel: sess.Name}); err != nil {
			return err
		}
		if err := notifyAdmins(ctx, tx, notification{Type: "transaction_update", Title: "Pengajuan Market Maker baru",
			Body: fmt.Sprintf("%s (%s) menunggu review.", sess.Name, org), Href: "/admin/mm-applications"}); err != nil {
			return err
		}
		as, err := loadMmApplications(ctx, tx, `a.id = $1`, id)
		if err != nil {
			return err
		}
		out = as[0]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return api.ApplyMarketMaker201JSONResponse(out), nil
}

func (s *Server) ListMmApplications(ctx context.Context, _ api.ListMmApplicationsRequestObject) (api.ListMmApplicationsResponseObject, error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	out, err := loadMmApplications(ctx, s.DB.Reader(), `true ORDER BY a.status = 'pending' DESC, a.created_at DESC`)
	if err != nil {
		return nil, err
	}
	return api.ListMmApplications200JSONResponse(out), nil
}

func (s *Server) DecideMmApplication(ctx context.Context, req api.DecideMmApplicationRequestObject) (api.DecideMmApplicationResponseObject, error) {
	a, err := s.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	var out api.MmApplication
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var userID, status, name string
		err := tx.QueryRow(ctx, `
			SELECT a.user_id, a.status, u.name FROM mm_applications a JOIN users u ON u.id = a.user_id WHERE a.id::text = $1 FOR UPDATE OF a`, req.Id).
			Scan(&userID, &status, &name)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound("Pengajuan tidak ditemukan")
		}
		if err != nil {
			return err
		}
		if status != "pending" {
			return conflict("conflict", "Pengajuan ini sudah diputuskan")
		}
		approve := req.Body.Action == api.RolesMmDecisionInputActionApprove
		reason := optReason(req.Body.Reason)
		if !approve {
			r, err := needReason(req.Body.Reason)
			if err != nil {
				return err
			}
			reason = &r
		}
		to := "rejected"
		if approve {
			to = "approved"
		}
		if _, err := tx.Exec(ctx, `UPDATE mm_applications SET status = $2, decided_at = now(), decided_by = $3, decision_reason = $4 WHERE id = $1`,
			req.Id, to, a.ID, reason); err != nil {
			return err
		}
		e := audit{Action: "Tolak pengajuan Market Maker", EntityType: "user", EntityID: userID, EntityLabel: name, Reason: reason}
		n := notification{Type: "transaction_update", Title: "Pengajuan Market Maker ditolak", Body: deref(reason), Href: "/app?activate=mm"}
		if approve {
			if _, err := tx.Exec(ctx, `INSERT INTO user_capabilities (user_id, capability, granted_by) VALUES ($1, 'market_maker', $2) ON CONFLICT DO NOTHING`,
				userID, a.ID); err != nil {
				return err
			}
			e.Action, e.Changes = "Setujui pengajuan Market Maker", []change{{Field: "Capability", After: "market_maker"}}
			n = notification{Type: "transaction_update", Title: "Kamu sekarang Market Maker", Body: "Workspace Market Ops sudah aktif di pemilih workspace.", Href: "/mm"}
		}
		if err := a.record(ctx, tx, e); err != nil {
			return err
		}
		if err := notify(ctx, tx, userID, n); err != nil {
			return err
		}
		as, err := loadMmApplications(ctx, tx, `a.id = $1`, req.Id)
		if err != nil {
			return err
		}
		out = as[0]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return api.DecideMmApplication200JSONResponse(out), nil
}

const invitationSelect = `
	SELECT m.id, m.org_id, o.name, m.role, coalesce(r.label, m.role), coalesce(m.department, ''), m.created_at
	FROM org_members m JOIN orgs o ON o.id = m.org_id LEFT JOIN org_roles r ON r.org_id = m.org_id AND r.key = m.role
	JOIN users u ON u.email = m.email
	WHERE m.status = 'invited' AND u.id = $1`

func loadInvitations(ctx context.Context, q dbtx, userID, extra string, args ...any) ([]api.OrgInvitation, error) {
	rows, err := q.Query(ctx, invitationSelect+extra, append([]any{userID}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.OrgInvitation{}
	for rows.Next() {
		var i api.OrgInvitation
		if err := rows.Scan(&i.Id, &i.OrgId, &i.OrgName, &i.Role, &i.RoleLabel, &i.Department, &i.InvitedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Server) ListMyInvitations(ctx context.Context, _ api.ListMyInvitationsRequestObject) (api.ListMyInvitationsResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	out, err := loadInvitations(ctx, s.DB.Primary(), sess.UserID, ` ORDER BY m.created_at DESC`)
	if err != nil {
		return nil, err
	}
	return api.ListMyInvitations200JSONResponse(out), nil
}

func (s *Server) AnswerInvitation(ctx context.Context, req api.AnswerInvitationRequestObject) (api.AnswerInvitationResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	var out api.User
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		invs, err := loadInvitations(ctx, tx, sess.UserID, ` AND m.id::text = $2 FOR UPDATE OF m`, req.Id)
		if err != nil {
			return err
		}
		if len(invs) == 0 {
			return notFound("Undangan tidak ditemukan atau sudah tidak berlaku")
		}
		inv := invs[0]
		accept := req.Body.Action == api.InvitationActionAccept
		e := audit{ActorUserID: &sess.UserID, ActorLabel: sess.Name, Action: "Tolak undangan", EntityType: "user", EntityID: sess.UserID,
			EntityLabel: sess.Name, OrgID: &inv.OrgId}
		title := sess.Name + " menolak undangan"
		body := fmt.Sprintf("Undangan sebagai %s di %s ditutup.", inv.RoleLabel, inv.OrgName)
		if accept {
			_, err := tx.Exec(ctx, `UPDATE org_members SET status = 'active', user_id = $2, name = $3, joined_at = now() WHERE id = $1`,
				inv.Id, sess.UserID, sess.Name)
			if uniqueViolation(err, "org_members_user_key") {
				return conflict("conflict", "Kamu sudah tergabung di organisasi ini")
			}
			if err != nil {
				return err
			}
			e.Action, e.Changes = "Terima undangan", []change{{Field: "Peran", After: inv.RoleLabel}}
			title, body = fmt.Sprintf("%s bergabung ke %s", sess.Name, inv.OrgName), fmt.Sprintf("Sebagai %s, %s.", inv.RoleLabel, inv.Department)
		} else if _, err := tx.Exec(ctx, `DELETE FROM org_members WHERE id = $1`, inv.Id); err != nil {
			return err
		}
		if err := writeAudit(ctx, tx, e); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT user_id FROM org_members WHERE org_id = $1 AND role = 'owner' AND status = 'active' AND user_id <> $2`,
			inv.OrgId, sess.UserID)
		if err != nil {
			return err
		}
		owners, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, o := range owners {
			if err := notify(ctx, tx, o, notification{Type: "transaction_update", Title: title, Body: body, Href: "/org/" + inv.OrgId + "/team"}); err != nil {
				return err
			}
		}
		out, err = loadUser(ctx, tx, sess.UserID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.AnswerInvitation200JSONResponse(out), nil
}

func (s *Server) CreateOrganization(ctx context.Context, req api.CreateOrganizationRequestObject) (api.CreateOrganizationResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	in := *req.Body
	var out api.RolesCreateOrgResult
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT o.name FROM org_members m JOIN orgs o ON o.id = m.org_id WHERE m.user_id = $1 AND m.status = 'active'`, sess.UserID)
		if err != nil {
			return err
		}
		taken, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if f := newOrgErrors(in, taken); len(f) > 0 {
			return &Error{Status: 422, Code: "validation", Message: "Periksa lagi data organisasi", Fields: f}
		}
		name := strings.TrimSpace(in.Name)
		typ := string(in.Type)
		id, err := createOrg(ctx, tx, newOrg{Name: name, Industry: typ + " · " + categoryLabels[string(in.CategoryId)], Type: &typ,
			NPWP: npwpDigits(in.Npwp), Categories: []string{string(in.CategoryId)}, OwnerUserID: sess.UserID})
		if err != nil {
			return err
		}
		if err := writeAudit(ctx, tx, audit{ActorUserID: &sess.UserID, ActorLabel: sess.Name + " (Owner)", Action: "Buat organisasi",
			EntityType: "business", EntityID: id, EntityLabel: name, OrgID: &id}); err != nil {
			return err
		}
		out.OrgId = id
		out.User, err = loadUser(ctx, tx, sess.UserID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.CreateOrganization201JSONResponse(out), nil
}
