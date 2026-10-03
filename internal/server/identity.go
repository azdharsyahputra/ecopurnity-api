package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Economic identity (GET/PUT /me/identity): profile, capacity items, availability, preferences.

func loadIdentity(ctx context.Context, q dbtx, userID string) (api.Identity, error) {
	var id api.Identity
	var location *string
	var days []int16
	var categories []string
	var minPrice, maxBudget *int64
	var radius float64
	err := q.QueryRow(ctx, `
		SELECT u.name, u.username, u.location, coalesce(i.bio, ''), coalesce(i.availability_days, '{0,1,2,3,4}'),
		       to_char(coalesce(i.available_from, '08:00'), 'HH24:MI'), to_char(coalesce(i.available_to, '17:00'), 'HH24:MI'),
		       coalesce(i.pref_locations, '{}'), coalesce(i.pref_categories::text[], '{}'), i.min_price_idr, i.max_budget_idr,
		       coalesce(i.delivery_radius_km, 25)
		FROM users u LEFT JOIN identities i ON i.user_id = u.id WHERE u.id = $1`, userID).
		Scan(&id.Profile.Name, &id.Profile.Username, &location, &id.Profile.Bio, &days, &id.Availability.From, &id.Availability.To,
			&id.Preferences.Locations, &categories, &minPrice, &maxBudget, &radius)
	if err != nil {
		return id, err
	}
	if location != nil {
		id.Profile.Location = *location
	}
	id.Availability.Days = make([]int, len(days))
	for i, d := range days {
		id.Availability.Days[i] = int(d)
	}
	id.Preferences.Categories = make([]api.CategoryId, len(categories))
	for i, c := range categories {
		id.Preferences.Categories[i] = api.CategoryId(c)
	}
	id.Preferences.MinPriceIdr = intPtr(minPrice)
	id.Preferences.MaxBudgetIdr = intPtr(maxBudget)
	id.Preferences.DeliveryRadiusKm = radius

	v, err := loadVerification(ctx, q, userID)
	if err != nil {
		return id, err
	}
	id.Profile.Verification.Email, id.Profile.Verification.Phone = v.Email, v.Phone
	id.Profile.Verification.Identity = api.IdentityProfileVerificationIdentity(v.Identity)

	id.Items = []api.CapacityItem{}
	rows, err := q.Query(ctx, `SELECT id, kind, name, detail, category_id FROM capacity_items WHERE user_id = $1 ORDER BY position`, userID)
	if err != nil {
		return id, err
	}
	for rows.Next() {
		var it api.CapacityItem
		var cat *string
		if err := rows.Scan(&it.Id, &it.Kind, &it.Name, &it.Detail, &cat); err != nil {
			return id, err
		}
		if cat != nil {
			c := api.CategoryId(*cat)
			it.CategoryId = &c
		}
		id.Items = append(id.Items, it)
	}
	if err := rows.Err(); err != nil {
		return id, err
	}
	id.Completeness = completeness(id)
	return id, nil
}

// completeness is the share of profile checks that pass (frontend src/mocks/personal.ts completeness()).
func completeness(i api.Identity) float64 {
	has := func(kinds ...api.CapacityKind) bool {
		for _, it := range i.Items {
			for _, k := range kinds {
				if it.Kind == k {
					return true
				}
			}
		}
		return false
	}
	checks := []bool{
		i.Profile.Bio != "", i.Profile.Location != "", i.Profile.Verification.Email, i.Profile.Verification.Phone,
		i.Profile.Verification.Identity == "verified", has("skill", "capacity"), has("asset", "resource"),
		len(i.Preferences.Categories) > 0, len(i.Availability.Days) > 0,
	}
	n := 0
	for _, c := range checks {
		if c {
			n++
		}
	}
	return float64(n) / float64(len(checks))
}

func (s *Server) GetMyIdentity(ctx context.Context, _ api.GetMyIdentityRequestObject) (api.GetMyIdentityResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	id, err := loadIdentity(ctx, s.DB.Primary(), sess.UserID)
	if err != nil {
		return nil, err
	}
	return api.GetMyIdentity200JSONResponse(id), nil
}

var hhmm = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// SaveMyIdentity replaces the identity document. Verification state in the body is ignored (it changes only through
// /me/kyc and email verification); the username is not editable here.
func (s *Server) SaveMyIdentity(ctx context.Context, req api.SaveMyIdentityRequestObject) (api.SaveMyIdentityResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	in := req.Body
	fields := map[string]string{}
	name := strings.TrimSpace(in.Profile.Name)
	if name == "" {
		fields["name"] = "Nama wajib diisi"
	}
	if !hhmm.MatchString(in.Availability.From) || !hhmm.MatchString(in.Availability.To) {
		fields["availability"] = "Jam pakai format HH:mm"
	} else if in.Availability.From >= in.Availability.To {
		fields["availability"] = "Jam selesai harus setelah jam mulai"
	}
	for i, it := range in.Items {
		if strings.TrimSpace(it.Name) == "" {
			fields[fmt.Sprintf("items.%d.name", i)] = "Nama wajib diisi"
		}
	}
	if p := in.Preferences; (p.MinPriceIdr != nil && *p.MinPriceIdr < 0) || (p.MaxBudgetIdr != nil && *p.MaxBudgetIdr < 0) || p.DeliveryRadiusKm < 0 {
		fields["preferences"] = "Nilai tidak boleh negatif"
	}
	if len(fields) > 0 {
		return nil, &Error{Status: 422, Code: "validation", Message: "Periksa kembali isian", Fields: fields}
	}
	categories := make([]string, len(in.Preferences.Categories))
	for i, c := range in.Preferences.Categories {
		categories[i] = string(c)
	}
	days := uniqueDays(in.Availability.Days)
	location := strings.TrimSpace(in.Profile.Location)

	var out api.Identity
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET name = $2, location = nullif($3, '') WHERE id = $1`, sess.UserID, name, location); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE parties SET name = $2 WHERE user_id = $1`, sess.UserID, name); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO identities (user_id, bio, availability_days, available_from, available_to, pref_locations, pref_categories,
			                        min_price_idr, max_budget_idr, delivery_radius_km)
			VALUES ($1, $2, $3, $4::time, $5::time, $6, $7::category_id[], $8, $9, $10)
			ON CONFLICT (user_id) DO UPDATE SET bio = EXCLUDED.bio, availability_days = EXCLUDED.availability_days,
			  available_from = EXCLUDED.available_from, available_to = EXCLUDED.available_to, pref_locations = EXCLUDED.pref_locations,
			  pref_categories = EXCLUDED.pref_categories, min_price_idr = EXCLUDED.min_price_idr, max_budget_idr = EXCLUDED.max_budget_idr,
			  delivery_radius_km = EXCLUDED.delivery_radius_km`,
			sess.UserID, strings.TrimSpace(in.Profile.Bio), days, in.Availability.From, in.Availability.To,
			nonNil(in.Preferences.Locations), categories, in.Preferences.MinPriceIdr, in.Preferences.MaxBudgetIdr, in.Preferences.DeliveryRadiusKm); err != nil {
			return err
		}
		return saveCapacityItems(ctx, tx, sess.UserID, in.Items)
	})
	if err != nil {
		return nil, err
	}
	if out, err = loadIdentity(ctx, s.DB.Primary(), sess.UserID); err != nil {
		return nil, err
	}
	return api.SaveMyIdentity200JSONResponse(out), nil
}

// saveCapacityItems upserts the list in order: items keep their row (and the smart-match state hanging off it) when
// the client sends back their id; new client-side ids become new rows; rows not sent are deleted.
func saveCapacityItems(ctx context.Context, tx pgx.Tx, userID string, items []api.CapacityItem) error {
	// Move existing positions out of the way so the unique (user_id, position) does not trip mid-update.
	if _, err := tx.Exec(ctx, `UPDATE capacity_items SET position = position + 10000 WHERE user_id = $1`, userID); err != nil {
		return err
	}
	keep := []string{}
	for pos, it := range items {
		var cat *string
		if it.CategoryId != nil {
			c := string(*it.CategoryId)
			cat = &c
		}
		var id string
		updated := false
		if uuidPattern.MatchString(it.Id) {
			err := tx.QueryRow(ctx, `
				UPDATE capacity_items SET position = $3, kind = $4, name = $5, detail = $6, category_id = $7
				WHERE id = $1 AND user_id = $2 RETURNING id`, it.Id, userID, pos, it.Kind, strings.TrimSpace(it.Name), strings.TrimSpace(it.Detail), cat).Scan(&id)
			if err == nil {
				updated = true
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if !updated {
			if err := tx.QueryRow(ctx, `
				INSERT INTO capacity_items (user_id, position, kind, name, detail, category_id) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
				userID, pos, it.Kind, strings.TrimSpace(it.Name), strings.TrimSpace(it.Detail), cat).Scan(&id); err != nil {
				return err
			}
		}
		keep = append(keep, id)
	}
	_, err := tx.Exec(ctx, `DELETE FROM capacity_items WHERE user_id = $1 AND NOT (id = ANY($2::uuid[]))`, userID, keep)
	return err
}

func uniqueDays(in []int) []int16 {
	seen := [7]bool{}
	out := []int16{}
	for _, d := range in {
		if d >= 0 && d <= 6 && !seen[d] {
			seen[d] = true
			out = append(out, int16(d))
		}
	}
	return out
}

func intPtr(v *int64) *int {
	if v == nil {
		return nil
	}
	i := int(*v)
	return &i
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
