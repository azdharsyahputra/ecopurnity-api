package server

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// CompleteOnboarding saves the onboarding answers and marks the user onboarded.
//
// Unlike the mock (x-note), it is safe to repeat: preferences and location are overwritten, but the first listing,
// the organization and the market maker application are only created the first time.
func (s *Server) CompleteOnboarding(ctx context.Context, req api.CompleteOnboardingRequestObject) (api.CompleteOnboardingResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	in := req.Body
	location := strings.TrimSpace(in.Location)
	categories := make([]string, 0, len(in.Categories))
	for _, c := range in.Categories {
		categories = append(categories, string(c))
	}
	locations := []string{}
	if location != "" {
		locations = append(locations, location)
	}

	var out api.User
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var first bool
		if err := tx.QueryRow(ctx, `
			UPDATE users SET location = nullif($2, ''), onboarded_at = coalesce(onboarded_at, now())
			WHERE id = $1 RETURNING onboarded_at = now()`, sess.UserID, location).Scan(&first); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO identities (user_id, pref_locations, pref_categories, min_price_idr, max_budget_idr, delivery_radius_km)
			VALUES ($1, $2, $3::category_id[], $4, $5, $6)
			ON CONFLICT (user_id) DO UPDATE SET pref_locations = EXCLUDED.pref_locations, pref_categories = EXCLUDED.pref_categories,
			  min_price_idr = EXCLUDED.min_price_idr, max_budget_idr = EXCLUDED.max_budget_idr, delivery_radius_km = EXCLUDED.delivery_radius_km`,
			sess.UserID, locations, categories, in.MinPriceIdr, in.MaxBudgetIdr, in.RadiusKm); err != nil {
			return err
		}
		if first {
			if err := s.onboardingFirstRun(ctx, tx, sess, in, location, categories); err != nil {
				return err
			}
		}
		out, err = loadUser(ctx, tx, sess.UserID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.CompleteOnboarding200JSONResponse(out), nil
}

func (s *Server) onboardingFirstRun(ctx context.Context, tx pgx.Tx, sess *session, in *api.OnboardingInput, location string, categories []string) error {
	category := "agri"
	if len(categories) > 0 {
		category = categories[0]
	}
	if l := in.FirstListing; l != nil && strings.TrimSpace(l.Item) != "" && l.Quantity.Value > 0 {
		party, err := userParty(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		var listingID, status string
		if l.Kind == "supply" {
			status = "available"
			err = tx.QueryRow(ctx, `
				INSERT INTO listings (code, kind, status, owner_party_id, item, category_id, quantity, unit, location, delivery, price_idr, available_from)
				VALUES (next_code('SUP'), 'supply', $1, $2, $3, $4, $5, $6, $7, 'both', $8, now()) RETURNING id`,
				status, party, strings.TrimSpace(l.Item), category, l.Quantity.Value, l.Quantity.Unit, location, valueOr(in.MinPriceIdr)).Scan(&listingID)
		} else {
			status = "open"
			err = tx.QueryRow(ctx, `
				INSERT INTO listings (code, kind, status, owner_party_id, item, category_id, quantity, unit, location, delivery, budget_idr, deadline)
				VALUES (next_code('DEM'), 'demand', $1, $2, $3, $4, $5, $6, $7, 'both', $8, now() + $9) RETURNING id`,
				status, party, strings.TrimSpace(l.Item), category, l.Quantity.Value, l.Quantity.Unit, location, valueOr(in.MaxBudgetIdr), 30*24*time.Hour).Scan(&listingID)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO listing_events (listing_id, status, note) VALUES ($1, $2, 'Dibuat saat onboarding')`, listingID, status); err != nil {
			return err
		}
	}

	if o := in.Organization; o != nil && strings.TrimSpace(o.Name) != "" {
		if _, err := createOrg(ctx, tx, newOrg{Name: o.Name, Industry: o.Industry, Location: o.Location, OwnerUserID: sess.UserID}); err != nil {
			return err
		}
	}

	if a := in.MarketMakerApplication; a != nil && strings.TrimSpace(a.Organization) != "" {
		_, err := tx.Exec(ctx, `
			INSERT INTO mm_applications (user_id, organization, categories, experience) VALUES ($1, $2, $3::category_id[], $4)`,
			sess.UserID, strings.TrimSpace(a.Organization), categories, strings.TrimSpace(a.Reason))
		if err != nil {
			return err
		}
		if err := writeAudit(ctx, tx, audit{ActorUserID: &sess.UserID, ActorLabel: sess.Name, Action: "Ajukan Market Maker",
			EntityType: "user", EntityID: sess.UserID, EntityLabel: sess.Name}); err != nil {
			return err
		}
		if err := notifyAdmins(ctx, tx, notification{Type: "transaction_update", Title: "Pengajuan Market Maker baru",
			Body: fmt.Sprintf("%s · %s", sess.Name, strings.TrimSpace(a.Organization)), Href: "/admin/mm-applications"}); err != nil {
			return err
		}
	}
	return nil
}

func valueOr(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// ── Organizations ────────────────────────────────────────────────

type newOrg struct {
	Name, Industry, Location string
	Type                     *string
	NPWP                     string
	Categories               []string
	OwnerUserID              string
}

// builtInRoles is the default permission matrix per role (frontend src/domain/org.ts DEFAULT_PERMISSIONS); every new
// org gets these rows so members' roles have a real foreign key and stay editable per org.
var builtInRoles = []struct {
	Key, Label  string
	Permissions []string
}{
	{"owner", "Owner", allPermissions()},
	{"procurement", "Procurement", []string{"procurement.view", "procurement.create", "procurement.manage", "auctions.view", "auctions.create", "auctions.manage",
		"collective.view", "collective.create", "collective.manage", "suppliers.view", "suppliers.create", "suppliers.manage", "inventory.view",
		"transactions.view", "transactions.create", "analytics.view", "team.view", "profile.view"}},
	{"finance", "Finance", []string{"procurement.view", "procurement.approve", "auctions.view", "auctions.approve", "collective.view", "suppliers.view",
		"inventory.view", "transactions.view", "transactions.approve", "transactions.manage", "analytics.view", "team.view", "profile.view"}},
	{"operations", "Operations", []string{"procurement.view", "procurement.create", "auctions.view", "collective.view", "suppliers.view", "inventory.view",
		"inventory.create", "inventory.manage", "transactions.view", "transactions.manage", "analytics.view", "team.view", "profile.view"}},
	{"sales", "Sales", []string{"procurement.view", "auctions.view", "collective.view", "suppliers.view", "inventory.view", "transactions.view",
		"analytics.view", "team.view", "profile.view"}},
}

func allPermissions() []string {
	var out []string
	for _, m := range []string{"procurement", "auctions", "collective", "suppliers", "inventory", "transactions", "analytics", "team", "profile"} {
		for _, a := range []string{"view", "create", "approve", "manage"} {
			out = append(out, m+"."+a)
		}
	}
	return out
}

var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

// createOrg creates an organization with its profile, built-in roles, owner membership and party (used by onboarding
// and POST /orgs). Returns the org id; a duplicate name is reported as 422 on fields.name.
func createOrg(ctx context.Context, tx pgx.Tx, o newOrg) (string, error) {
	name := strings.TrimSpace(o.Name)
	slug := strings.Trim(notSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if slug == "" {
		slug = "org"
	}
	var id string
	for attempt := 0; ; attempt++ {
		candidate := slug
		if attempt > 0 {
			candidate = fmt.Sprintf("%s-%d", slug, attempt+1)
		}
		err := pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
			return sp.QueryRow(ctx, `INSERT INTO orgs (name, slug, created_by) VALUES ($1, $2, $3) RETURNING id`, name, candidate, o.OwnerUserID).Scan(&id)
		})
		switch {
		case err == nil:
		case uniqueViolation(err, "orgs_name_key"):
			return "", &Error{Status: 422, Code: "validation", Message: "Nama organisasi sudah dipakai",
				Fields: map[string]string{"name": "Nama ini sudah terdaftar. Pakai nama lain."}}
		case uniqueViolation(err, "orgs_slug_key") && attempt < 50:
			continue
		default:
			return "", err
		}
		break
	}
	categories := o.Categories
	if categories == nil {
		categories = []string{}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO org_profiles (org_id, org_type, industry, location, npwp, categories) VALUES ($1, $2, $3, $4, $5, $6::category_id[])`,
		id, o.Type, strings.TrimSpace(o.Industry), strings.TrimSpace(o.Location), o.NPWP, categories); err != nil {
		return "", err
	}
	for i, r := range builtInRoles {
		if _, err := tx.Exec(ctx, `INSERT INTO org_roles (org_id, key, label, position, permissions) VALUES ($1, $2, $3, $4, $5)`,
			id, r.Key, r.Label, i, r.Permissions); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO org_members (org_id, user_id, email, name, role, status, joined_at)
		SELECT $1, u.id, u.email, u.name, 'owner', 'active', now() FROM users u WHERE u.id = $2`, id, o.OwnerUserID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO parties (kind, org_id, name, display_kind) VALUES ('org', $1, $2, 'business')`, id, name); err != nil {
		return "", err
	}
	return id, nil
}
