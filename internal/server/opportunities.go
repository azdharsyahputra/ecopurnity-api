package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Opportunities: public list/detail (tag Public) and the caller's personalised view, join/follow/leave (tag Personal).
// Detection lives in detection.go.

var errOpportunityNotFound = &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Opportunity tidak ditemukan"}

func (s *Server) ListOpportunities(ctx context.Context, req api.ListOpportunitiesRequestObject) (api.ListOpportunitiesResponseObject, error) {
	p := req.Params
	page, size := pageParams(p.Page, p.PageSize)
	where := []string{"true"}
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	if p.Q != nil && strings.TrimSpace(*p.Q) != "" {
		ph := arg("%" + likeEscape(strings.TrimSpace(*p.Q)) + "%")
		where = append(where, fmt.Sprintf("(o.title ILIKE %[1]s OR o.code ILIKE %[1]s OR o.description ILIKE %[1]s)", ph))
	}
	if p.Category != nil {
		where = append(where, "o.category_id = "+arg(string(*p.Category)))
	}
	if p.Region != nil && strings.TrimSpace(*p.Region) != "" {
		where = append(where, "lower(o.region) = lower("+arg(strings.TrimSpace(*p.Region))+")")
	}
	if p.Status != nil && strings.TrimSpace(*p.Status) != "" {
		var sts []string
		for _, st := range strings.Split(*p.Status, ",") {
			if st = strings.TrimSpace(st); st != "" {
				sts = append(sts, st)
			}
		}
		where = append(where, "o.status = ANY("+arg(sts)+")")
	}
	cond := strings.Join(where, " AND ")
	q := s.DB.Reader()
	out := api.ListOpportunities200JSONResponse{Meta: api.PageMeta{Page: page, PageSize: size}}
	if err := q.QueryRow(ctx, `SELECT count(*) FROM opportunities o WHERE `+cond, args...).Scan(&out.Meta.Total); err != nil {
		return nil, err
	}
	data, err := loadOpportunities(ctx, q, cond+` ORDER BY o.potential_value_idr DESC, o.id LIMIT `+arg(size)+` OFFSET `+arg((page-1)*size), args...)
	if err != nil {
		return nil, err
	}
	out.Data = data
	return out, nil
}

func (s *Server) GetOpportunity(ctx context.Context, req api.GetOpportunityRequestObject) (api.GetOpportunityResponseObject, error) {
	q := s.DB.Reader()
	os, err := loadOpportunities(ctx, q, `o.id::text = $1`, req.Id)
	if err != nil {
		return nil, err
	}
	if len(os) == 0 {
		return nil, errOpportunityNotFound
	}
	var d api.OpportunityDetail
	if err := widen(os[0], &d); err != nil {
		return nil, err
	}
	if err := q.QueryRow(ctx, `SELECT description, required_contribution, mechanism_reason FROM opportunities WHERE id = $1`, d.Id).
		Scan(&d.Description, &d.RequiredContribution, &d.MechanismReason); err != nil {
		return nil, err
	}
	if d.Markets, err = loadMarkets(ctx, q, `m.opportunity_id = $1 AND m.status <> 'draft' ORDER BY m.created_at`, d.Id); err != nil {
		return nil, err
	}
	if d.History, err = opportunityHistory(ctx, q, d, time.Now()); err != nil {
		return nil, err
	}
	if d.ParticipantsPreview, err = participantsPreview(ctx, q, d, viewerID(ctx) != ""); err != nil {
		return nil, err
	}
	return api.GetOpportunity200JSONResponse(d), nil
}

type previewParty = struct {
	Kind     api.OpportunityDetailParticipantsPreviewKind `json:"kind"`
	Name     string                                       `json:"name"`
	Role     api.OpportunityDetailParticipantsPreviewRole `json:"role"`
	Verified bool                                         `json:"verified"`
}

// participantsPreview: up to 6 parties, joined participants first, then the owners of the open listings the engine
// counts in this opportunity (same category, unit and region). Visitors see initials only (PRD §6.3).
func participantsPreview(ctx context.Context, q dbtx, d api.OpportunityDetail, signedIn bool) ([]previewParty, error) {
	rows, err := q.Query(ctx, `
		SELECT p.id::text, p.name, p.display_kind, p.verified, op.role, ''
		FROM opportunity_participants op JOIN parties p ON p.id = op.party_id WHERE op.opportunity_id = $1
		UNION ALL
		(SELECT p.id::text, p.name, p.display_kind, p.verified, CASE l.kind WHEN 'demand' THEN 'buyer' ELSE 'supplier' END, l.location
		 FROM listings l JOIN parties p ON p.id = l.owner_party_id
		 WHERE l.category_id = $2 AND lower(l.unit) = lower($3) AND l.status IN ('open','matched','available','in_market')
		 ORDER BY l.created_at DESC LIMIT 500)`, d.Id, string(d.CategoryId), d.Demand.Unit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []previewParty{}
	seen := map[string]bool{}
	for rows.Next() {
		var id, location string
		var pp previewParty
		if err := rows.Scan(&id, &pp.Name, &pp.Kind, &pp.Verified, &pp.Role, &location); err != nil {
			return nil, err
		}
		if seen[id] || len(out) == 6 || (location != "" && !strings.EqualFold(regionOf(location), d.Region)) {
			continue
		}
		seen[id] = true
		if !signedIn {
			pp.Name = maskName(pp.Name)
		}
		out = append(out, pp)
	}
	return out, rows.Err()
}

// maskName keeps the first letter of each word: "CV Sumber Pangan" -> "C••• S••• P•••" (mock: name.replace(/\B\w+/g, '•••')).
func maskName(name string) string {
	var b strings.Builder
	run := 0
	for _, r := range name {
		word := r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
		switch {
		case word && run == 0:
			b.WriteRune(r)
			run = 1
		case word:
			if run == 1 {
				b.WriteString("•••")
			}
			run++
		default:
			b.WriteRune(r)
			run = 0
		}
	}
	return b.String()
}

// opportunityHistory: monthly demand vs supply listed in the opportunity's category, unit and region over the last 6
// months (oldest first, empty months 0).
func opportunityHistory(ctx context.Context, q dbtx, d api.OpportunityDetail, now time.Time) ([]struct {
	Demand float64 `json:"demand"`
	Month  string  `json:"month"`
	Supply float64 `json:"supply"`
}, error) {
	now = now.UTC()
	first := time.Date(now.Year(), now.Month()-5, 1, 0, 0, 0, 0, time.UTC)
	out := make([]struct {
		Demand float64 `json:"demand"`
		Month  string  `json:"month"`
		Supply float64 `json:"supply"`
	}, 6)
	idx := map[string]int{}
	for k := range out {
		out[k].Month = first.AddDate(0, k, 0).Format("2006-01")
		idx[out[k].Month] = k
	}
	rows, err := q.Query(ctx, `
		SELECT to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM'), kind, location, quantity::float8 FROM listings
		WHERE category_id = $1 AND lower(unit) = lower($2) AND created_at >= $3`, string(d.CategoryId), d.Demand.Unit, first)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var month, kind, location string
		var qty float64
		if err := rows.Scan(&month, &kind, &location, &qty); err != nil {
			return nil, err
		}
		k, ok := idx[month]
		if !ok || !strings.EqualFold(regionOf(location), d.Region) {
			continue
		}
		if kind == "demand" {
			out[k].Demand += qty
		} else {
			out[k].Supply += qty
		}
	}
	return out, rows.Err()
}

// ── Personal view ────────────────────────────────────────────────

// viewer is what personalisation needs about the signed-in user.
type viewer struct {
	UserID, Name, Home string // Home: account location, else the first preferred location
	PrefLocations      []string
	PrefCategories     map[string]bool
	RadiusKm           float64
	ListingCats        map[string]int // active listings per category
}

func loadViewer(ctx context.Context, q dbtx, userID string) (viewer, error) {
	v := viewer{UserID: userID, PrefCategories: map[string]bool{}, ListingCats: map[string]int{}}
	var location *string
	var cats []string
	err := q.QueryRow(ctx, `
		SELECT u.name, u.location, coalesce(i.pref_locations, '{}'), coalesce(i.pref_categories::text[], '{}'), coalesce(i.delivery_radius_km, 25)::float8
		FROM users u LEFT JOIN identities i ON i.user_id = u.id WHERE u.id = $1`, userID).Scan(&v.Name, &location, &v.PrefLocations, &cats, &v.RadiusKm)
	if err != nil {
		return v, err
	}
	for _, c := range cats {
		v.PrefCategories[c] = true
	}
	switch {
	case location != nil && strings.TrimSpace(*location) != "":
		v.Home = *location
	case len(v.PrefLocations) > 0:
		v.Home = v.PrefLocations[0]
	}
	rows, err := q.Query(ctx, `
		SELECT l.category_id, count(*) FROM listings l JOIN parties p ON p.id = l.owner_party_id
		WHERE p.user_id = $1 AND l.status NOT IN ('sold','expired','fulfilled','cancelled') GROUP BY l.category_id`, userID)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		var n int
		if err := rows.Scan(&c, &n); err != nil {
			return v, err
		}
		v.ListingCats[c] = n
	}
	return v, rows.Err()
}

// near: the opportunity's region is one the user prefers or lives in.
func (v viewer) near(region string) bool {
	if strings.EqualFold(regionOf(v.Home), region) {
		return true
	}
	return slices.ContainsFunc(v.PrefLocations, func(l string) bool { return strings.EqualFold(regionOf(l), region) })
}

// personalOpportunities scores every open opportunity (plus closed ones the user is related to) for the user.
func personalOpportunities(ctx context.Context, q dbtx, userID string) ([]api.PersonalOpportunity, error) {
	v, err := loadViewer(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	opps, err := loadOpportunities(ctx, q, `o.status NOT IN ('dismissed','closed')
		OR o.id IN (SELECT opportunity_id FROM opportunity_follows WHERE user_id = $1)
		OR o.id IN (SELECT op.opportunity_id FROM opportunity_participants op JOIN parties p ON p.id = op.party_id WHERE p.user_id = $1)`, userID)
	if err != nil {
		return nil, err
	}
	type rel struct {
		joined       bool
		role         string
		listingID    *string
		quantity     *float64
		participates bool
	}
	rels := map[string]rel{}
	rows, err := q.Query(ctx, `
		SELECT opportunity_id::text, true, '', NULL::text, NULL::float8 FROM opportunity_follows WHERE user_id = $1
		UNION ALL
		SELECT op.opportunity_id::text, false, op.role, op.listing_id::text, op.quantity::float8
		FROM opportunity_participants op JOIN parties p ON p.id = op.party_id WHERE p.user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var follow bool
		var r rel
		if err := rows.Scan(&id, &follow, &r.role, &r.listingID, &r.quantity); err != nil {
			rows.Close()
			return nil, err
		}
		if follow {
			if _, ok := rels[id]; !ok {
				rels[id] = rel{}
			}
			continue
		}
		r.joined = true
		rels[id] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]api.PersonalOpportunity, 0, len(opps))
	for _, o := range opps {
		var p api.PersonalOpportunity
		if err := widen(o, &p); err != nil {
			return nil, err
		}
		p.DistanceKm = distanceKm(v.Home, o.Region)
		p.Reasons = []api.MatchReason{}
		if v.PrefCategories[string(o.CategoryId)] {
			p.Reasons = append(p.Reasons, api.MatchReason{Label: "Kategori cocok", Detail: "Sesuai kategori preferensimu"})
		}
		if n := v.ListingCats[string(o.CategoryId)]; n > 0 {
			p.Reasons = append(p.Reasons, api.MatchReason{Label: "Kapasitas cocok", Detail: fmt.Sprintf("Kamu punya listing di kategori ini (%d)", n)})
		}
		if v.near(o.Region) {
			p.Reasons = append(p.Reasons, api.MatchReason{Label: "Dekat", Detail: fmt.Sprintf("%s km dari lokasimu", idNumber(p.DistanceKm))})
		}
		if o.Demand.Value > o.Supply.Value {
			p.Reasons = append(p.Reasons, api.MatchReason{Label: "Ada gap",
				Detail: fmt.Sprintf("Supply baru %d%% dari demand", int(jsRound(o.Supply.Value/o.Demand.Value*100)))})
		}
		p.Relation = api.PersonalOpportunityRelationNone
		if r, ok := rels[o.Id]; ok {
			p.Relation = api.PersonalOpportunityRelationFollowing
			if r.joined {
				p.Relation = api.PersonalOpportunityRelationJoined
				kind := api.PersonalOpportunityContributionKind("supply")
				if r.role == "buyer" {
					kind = "demand"
				}
				c := &struct {
					Kind      api.PersonalOpportunityContributionKind `json:"kind"`
					ListingId string                                  `json:"listingId"`
					Quantity  api.Quantity                            `json:"quantity"`
				}{Kind: kind, Quantity: api.Quantity{Unit: o.Demand.Unit}}
				if r.listingID != nil {
					c.ListingId = *r.listingID
				}
				if r.quantity != nil {
					c.Quantity.Value = *r.quantity
				}
				p.Contribution = c
			}
		}
		p.PersonalValueIdr = int(jsRound(float64(o.PotentialValueIdr) / math.Max(float64(o.Participants), 1) * (1 + float64(len(p.Reasons))/2)))
		out = append(out, p)
	}
	slices.SortStableFunc(out, func(a, b api.PersonalOpportunity) int { return cmp.Compare(oppScore(b), oppScore(a)) })
	return out, nil
}

func oppScore(o api.PersonalOpportunity) float64 {
	return float64(len(o.Reasons))*10 - o.DistanceKm/100 + o.Confidence
}

var oppTabs = map[api.ListMyOpportunitiesParamsTab]func(api.PersonalOpportunity) bool{
	"for_you":    func(o api.PersonalOpportunity) bool { return len(o.Reasons) > 0 },
	"nearby":     func(o api.PersonalOpportunity) bool { return o.DistanceKm <= 75 },
	"market_gap": func(o api.PersonalOpportunity) bool { return o.Kind == api.OpportunityKindMarketGap },
	"collective": func(o api.PersonalOpportunity) bool { return o.Kind == api.OpportunityKindCollectiveDemand },
	"supply_gap": func(o api.PersonalOpportunity) bool { return o.Kind == api.OpportunityKindSupplyGap },
	"joined":     func(o api.PersonalOpportunity) bool { return o.Relation == api.PersonalOpportunityRelationJoined },
	"following":  func(o api.PersonalOpportunity) bool { return o.Relation == api.PersonalOpportunityRelationFollowing },
}

func (s *Server) ListMyOpportunities(ctx context.Context, req api.ListMyOpportunitiesRequestObject) (api.ListMyOpportunitiesResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	all, err := personalOpportunities(ctx, s.DB.Primary(), sess.UserID)
	if err != nil {
		return nil, err
	}
	tab := api.ListMyOpportunitiesParamsTab("for_you")
	if req.Params.Tab != nil {
		tab = *req.Params.Tab
	}
	if keep, ok := oppTabs[tab]; ok {
		all = slices.DeleteFunc(all, func(o api.PersonalOpportunity) bool { return !keep(o) })
	}
	return api.ListMyOpportunities200JSONResponse(all), nil
}

func personalOpportunity(ctx context.Context, q dbtx, userID, oppID string) (api.PersonalOpportunity, error) {
	all, err := personalOpportunities(ctx, q, userID)
	if err != nil {
		return api.PersonalOpportunity{}, err
	}
	for _, o := range all {
		if o.Id == oppID {
			return o, nil
		}
	}
	return api.PersonalOpportunity{}, errOpportunityNotFound
}

// lockOpportunity locks the opportunity row and returns its id (404 when missing).
func lockOpportunity(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	var oppID string
	err := tx.QueryRow(ctx, `SELECT id::text FROM opportunities WHERE id::text = $1 FOR UPDATE`, id).Scan(&oppID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errOpportunityNotFound
	}
	return oppID, err
}

// JoinOpportunity records the caller's contribution. Re-joining replaces the previous contribution (the mock adds it
// again), and the listing must be one of the caller's own of the contributed kind.
func (s *Server) JoinOpportunity(ctx context.Context, req api.JoinOpportunityRequestObject) (api.JoinOpportunityResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	in := req.Body
	var out api.PersonalOpportunity
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		oppID, err := lockOpportunity(ctx, tx, req.Id)
		if err != nil {
			return err
		}
		if !(in.Quantity.Value > 0) {
			return &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: "Kuantitas harus > 0",
				Fields: map[string]string{"quantity": "Isi kuantitas kontribusi"}}
		}
		var listingKind string
		err = tx.QueryRow(ctx, `SELECT l.kind FROM listings l JOIN parties p ON p.id = l.owner_party_id WHERE l.id::text = $1 AND p.user_id = $2`,
			in.ListingId, sess.UserID).Scan(&listingKind)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && listingKind != string(in.Kind)) {
			return validationFields(map[string]string{"listingId": "Pilih listing " + string(in.Kind) + " milikmu"})
		}
		if err != nil {
			return err
		}
		party, err := userParty(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		role := "supplier"
		if in.Kind == "demand" {
			role = "buyer"
		}
		var oldRole *string
		var oldQty *float64
		err = tx.QueryRow(ctx, `SELECT role, quantity::float8 FROM opportunity_participants WHERE opportunity_id = $1 AND party_id = $2`,
			oppID, party).Scan(&oldRole, &oldQty)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO opportunity_participants (opportunity_id, party_id, role, listing_id, quantity) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (opportunity_id, party_id) DO UPDATE SET role = EXCLUDED.role, listing_id = EXCLUDED.listing_id, quantity = EXCLUDED.quantity`,
			oppID, party, role, in.ListingId, in.Quantity.Value); err != nil {
			return err
		}
		if err := adjustContribution(ctx, tx, oppID, oldRole, oldQty, -1); err != nil {
			return err
		}
		if err := adjustContribution(ctx, tx, oppID, &role, &in.Quantity.Value, 1); err != nil {
			return err
		}
		if oldRole == nil {
			if _, err := tx.Exec(ctx, `UPDATE opportunities SET participant_count = participant_count + 1 WHERE id = $1`, oppID); err != nil {
				return err
			}
		}
		out, err = personalOpportunity(ctx, tx, sess.UserID, oppID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.JoinOpportunity200JSONResponse(out), nil
}

// adjustContribution adds (sign 1) or removes (sign -1) a contribution from the opportunity's demand or supply total.
func adjustContribution(ctx context.Context, tx pgx.Tx, oppID string, role *string, qty *float64, sign float64) error {
	if role == nil || qty == nil {
		return nil
	}
	col := "supply_value"
	if *role == "buyer" {
		col = "demand_value"
	}
	_, err := tx.Exec(ctx, `UPDATE opportunities SET `+col+` = greatest(0, `+col+` + $2) WHERE id = $1`, oppID, sign**qty)
	return err
}

func (s *Server) FollowOpportunity(ctx context.Context, req api.FollowOpportunityRequestObject) (api.FollowOpportunityResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	tag, err := s.DB.Primary().Exec(ctx, `
		INSERT INTO opportunity_follows (user_id, opportunity_id) SELECT $1, id FROM opportunities WHERE id::text = $2
		ON CONFLICT DO NOTHING`, sess.UserID, req.Id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.DB.Primary().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM opportunities WHERE id::text = $1)`, req.Id).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, errOpportunityNotFound
		}
	}
	return api.FollowOpportunity204Response{}, nil
}

// LeaveOpportunity clears following and joined; a joined contribution is taken back out of the totals.
func (s *Server) LeaveOpportunity(ctx context.Context, req api.LeaveOpportunityRequestObject) (api.LeaveOpportunityResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		oppID, err := lockOpportunity(ctx, tx, req.Id)
		if errors.Is(err, errOpportunityNotFound) {
			return nil // idempotent
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM opportunity_follows WHERE user_id = $1 AND opportunity_id = $2`, sess.UserID, oppID); err != nil {
			return err
		}
		var role *string
		var qty *float64
		err = tx.QueryRow(ctx, `
			DELETE FROM opportunity_participants op USING parties p
			WHERE op.opportunity_id = $1 AND op.party_id = p.id AND p.user_id = $2 RETURNING op.role, op.quantity::float8`, oppID, sess.UserID).
			Scan(&role, &qty)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := adjustContribution(ctx, tx, oppID, role, qty, -1); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE opportunities SET participant_count = greatest(0, participant_count - 1) WHERE id = $1`, oppID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.LeaveOpportunity204Response{}, nil
}
