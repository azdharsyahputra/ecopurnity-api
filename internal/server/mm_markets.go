package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

type storedRuleVersion struct {
	ruleVersion
	CreatedAt time.Time
	Author    string
	Reason    *string
}

func loadRuleVersions(ctx context.Context, q dbtx, marketID string) ([]storedRuleVersion, error) {
	rows, err := q.Query(ctx, `
		SELECT id, version, rules, effective_from_round, created_at, author, reason FROM market_rule_versions
		WHERE market_id = $1 ORDER BY version`, marketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedRuleVersion
	for rows.Next() {
		var v storedRuleVersion
		var raw []byte
		if err := rows.Scan(&v.ID, &v.Version, &raw, &v.EffectiveFromRound, &v.CreatedAt, &v.Author, &v.Reason); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &v.Rules); err != nil {
			return nil, fmt.Errorf("market %s rules v%d: %w", marketID, v.Version, err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func ensureRuleVersions(ctx context.Context, q dbtx, m mmMarket) ([]storedRuleVersion, error) {
	vs, err := loadRuleVersions(ctx, q, m.ID)
	if err != nil || len(vs) > 0 {
		return vs, err
	}
	rules, _ := json.Marshal(defaultRules(m.Demand, m.Supply, m.Region, m.Mechanism, time.Now()))
	if _, err := q.Exec(ctx, `INSERT INTO market_rule_versions (market_id, version, rules, effective_from_round, author)
		VALUES ($1, 1, $2, 1, 'Sistem (migrasi aturan awal)')`, m.ID, rules); err != nil {
		return nil, err
	}
	return loadRuleVersions(ctx, q, m.ID)
}

func (v storedRuleVersion) api() api.RuleVersion {
	date := func(s string) openapi_types.Date {
		t, _ := time.Parse(time.DateOnly, s)
		return openapi_types.Date{Time: t}
	}
	r := v.Rules
	return api.RuleVersion{Version: v.Version, EffectiveFromRound: v.EffectiveFromRound, CreatedAt: v.CreatedAt, Author: v.Author, Reason: v.Reason,
		Rules: api.MarketRules{Award: api.AwardRule(r.Award), Eligibility: api.Eligibility(r.Eligibility), MaxQuantity: r.MaxQuantity,
			MinQuantity: r.MinQuantity, MinStepPct: r.MinStepPct, RadiusKm: r.RadiusKm, Region: r.Region, Visibility: api.BidVisibility(r.Visibility),
			WindowStart: date(r.WindowStart), WindowEnd: date(r.WindowEnd)}}
}

func rulesFromAPI(r api.MarketRules) marketRules {
	return marketRules{Eligibility: string(r.Eligibility), Visibility: string(r.Visibility), MinStepPct: r.MinStepPct, MinQuantity: r.MinQuantity,
		MaxQuantity: r.MaxQuantity, WindowStart: r.WindowStart.Format(time.DateOnly), WindowEnd: r.WindowEnd.Format(time.DateOnly),
		Region: strings.TrimSpace(r.Region), RadiusKm: r.RadiusKm, Award: string(r.Award)}
}

func roundsOpened(ctx context.Context, q dbtx, marketID string) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT coalesce(max(round_no), 0) FROM auctions WHERE market_id = $1`, marketID).Scan(&n)
	return n, err
}

func roundResults(ctx context.Context, q dbtx, where string, args ...any) ([]api.RoundResult, error) {
	rows, err := q.Query(ctx, `
		SELECT a.id::text, a.round_no, a.title, a.status, a.type, a.starts_at, a.opening_price_idr, a.current_price_idr,
		       a.clearing_price_idr, a.median_price_idr,
		       (SELECT (array_agg(b.price_idr ORDER BY b.price_idr))[count(*) / 2 + 1] FROM bids b WHERE b.auction_id = a.id AND b.status <> 'withdrawn')
		FROM auctions a
		WHERE a.round_no IS NOT NULL AND a.status NOT IN ('scheduled','qualification','cancelled') AND `+where+` ORDER BY a.round_no`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.RoundResult{}
	toInt := func(p *int64) *int {
		if p == nil {
			return nil
		}
		return ptr(int(*p))
	}
	for rows.Next() {
		var id, title, status, typ string
		var round int
		var at time.Time
		var opening int64
		var best, clearing, median, bidsMedian *int64
		if err := rows.Scan(&id, &round, &title, &status, &typ, &at, &opening, &best, &clearing, &median, &bidsMedian); err != nil {
			return nil, err
		}
		live := status == "live" || status == "extended"
		r := api.RoundResult{Round: round, AuctionId: &id, Title: title, Status: "closed", At: at, OpeningIdr: int(opening)}
		if live {
			r.Status = "live"
			if typ == "sealed" {
				out = append(out, r)
				continue
			}
			r.CurrentIdr = toInt(best)
		} else {
			if median == nil {
				median = bidsMedian
			}
			if clearing == nil {
				clearing = best
			}
			r.ClearingIdr = toInt(clearing)
		}
		if median == nil {
			median = bidsMedian
		}
		if median == nil && best != nil {
			median = ptr(int64(math.Round(float64(opening+*best) / 2)))
		}
		r.MedianIdr = toInt(median)
		out = append(out, r)
	}
	return out, rows.Err()
}

func emitRoundResult(ctx context.Context, q dbtx, auctionID string) error {
	var marketID *string
	if err := q.QueryRow(ctx, `SELECT market_id::text FROM auctions WHERE id = $1 AND round_no IS NOT NULL`, auctionID).Scan(&marketID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	rs, err := roundResults(ctx, q, `a.id = $1`, auctionID)
	if err != nil || len(rs) == 0 || marketID == nil {
		return err
	}
	b, _ := json.Marshal(rs[0])
	return emit(ctx, q, "market.round_result", *marketID, b)
}

type marketForm struct {
	OpportunityID                              *string
	Name, Objective, Mechanism, Category, Unit string
	Demand, Supply                             float64
	RefPrice                                   int64
	Rules                                      marketRules
	AutoInvite                                 bool
	Approval, Verification                     string
	Invite                                     []string
}

func formMarket(ctx context.Context, tx pgx.Tx, sess *session, f marketForm) (mmMarket, error) {
	var oppID, oppCode, oppTitle string
	if f.OpportunityID != nil {
		err := tx.QueryRow(ctx, `SELECT id, code, title FROM opportunities WHERE id::text = $1 FOR UPDATE`, *f.OpportunityID).Scan(&oppID, &oppCode, &oppTitle)
		if errors.Is(err, pgx.ErrNoRows) {
			return mmMarket{}, &Error{Status: 422, Code: "validation", Message: "Periksa kembali isian market",
				Fields: map[string]string{"opportunityId": "Opportunity tidak ditemukan"}}
		}
		if err != nil {
			return mmMarket{}, err
		}
	}
	var makerParty, makerName string
	err := tx.QueryRow(ctx, `
		SELECT p.id, p.name FROM org_members om JOIN parties p ON p.org_id = om.org_id
		WHERE om.user_id = $1 AND om.status = 'active' ORDER BY om.joined_at NULLS LAST, om.created_at LIMIT 1`, sess.UserID).Scan(&makerParty, &makerName)
	if errors.Is(err, pgx.ErrNoRows) {
		makerName = sess.Name
		makerParty, err = userParty(ctx, tx, sess.UserID)
	}
	if err != nil {
		return mmMarket{}, err
	}
	name, unit := strings.TrimSpace(f.Name), strings.TrimSpace(f.Unit)
	desc := name + " dibentuk oleh " + makerName
	if oppID != "" {
		desc += " dari opportunity " + oppCode
	}
	desc += fmt.Sprintf(". Objective %s dengan mekanisme %s.", strings.ToLower(objectiveLabel[f.Objective]), strings.ToLower(mechanismLabel[f.Mechanism]))
	var opp *string
	if oppID != "" {
		opp = &oppID
	}
	m := mmMarket{Name: name, Status: "active", Unit: unit, Category: f.Category, Region: f.Rules.Region, Mechanism: f.Mechanism,
		Demand: f.Demand, Supply: math.Max(0, f.Supply), PriceMax: int64(math.Round(float64(f.RefPrice) * 1.05))}
	if err := tx.QueryRow(ctx, `
		INSERT INTO markets (name, category_id, region, objective, mechanism, status, maker_party_id, opportunity_id, description, unit,
		                     demand_value, supply_value, price_min_idr, price_max_idr, created_by)
		VALUES ($1, $2, $3, $4, $5, 'active', $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING id, code`,
		name, f.Category, m.Region, f.Objective, f.Mechanism, makerParty, opp, desc, unit, m.Demand, m.Supply,
		int64(math.Round(float64(f.RefPrice)*0.95)), m.PriceMax, sess.UserID).Scan(&m.ID, &m.Code); err != nil {
		return m, err
	}
	rules, _ := json.Marshal(f.Rules)
	if _, err := tx.Exec(ctx, `
		WITH op AS (INSERT INTO market_operators (market_id, user_id, added_by) VALUES ($1, $2, $2)),
		     st AS (INSERT INTO market_settings (market_id, approval, supplier_verification, updated_by) VALUES ($1, $3, $4, $2))
		INSERT INTO market_rule_versions (market_id, version, rules, effective_from_round, author, created_by) VALUES ($1, 1, $5, 1, $6, $2)`,
		m.ID, sess.UserID, f.Approval, f.Verification, rules, mmActor(sess)); err != nil {
		return m, err
	}
	status := "pending"
	if f.Approval == "auto" {
		status = "active"
	}
	if f.AutoInvite && oppID != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO market_participants (market_id, party_id, role, status)
			SELECT DISTINCT ON (party_id) $1::uuid, party_id, role, $2 FROM (
			  SELECT party_id, role, 0 AS src FROM opportunity_participants WHERE opportunity_id = $3
			  UNION ALL
			  SELECT party_id, role, 1 FROM opportunity_listings WHERE opportunity_id = $3) x
			ORDER BY party_id, src
			ON CONFLICT DO NOTHING`, m.ID, status, oppID); err != nil {
			return m, err
		}
	}
	if len(f.Invite) > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO market_participants (market_id, party_id, role, status)
			SELECT $1, unnest($2::uuid[]), 'buyer', $3 ON CONFLICT DO NOTHING`, m.ID, f.Invite, status); err != nil {
			return m, err
		}
	}
	var changes []change
	changes = append(changes, change{Field: "Status", After: marketStatusLabel["active"]}, change{Field: "Mekanisme", After: mechanismLabel[f.Mechanism]})
	if oppID != "" {
		if err := carryOverOpportunity(ctx, tx, m, oppID, oppCode, oppTitle); err != nil {
			return m, err
		}
		changes = append(changes, change{Field: "Opportunity", Before: &oppCode, After: "Market Live"})
	}
	return m, mmAudit(ctx, tx, sess, audit{Action: "Publish market", EntityType: "market", EntityID: m.ID, EntityLabel: m.Name, MarketID: &m.ID,
		Changes: changes}, "Market terbentuk: "+m.Name, nil)
}

func carryOverOpportunity(ctx context.Context, tx pgx.Tx, m mmMarket, oppID, oppCode, oppTitle string) error {
	if _, err := tx.Exec(ctx, `UPDATE opportunities SET status = 'market_live' WHERE id = $1`, oppID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO mm_pipeline (opportunity_id, stage, market_id) VALUES ($1, 'market_live', $2)
		ON CONFLICT (opportunity_id) DO UPDATE SET stage = 'market_live', market_id = EXCLUDED.market_id, dismiss_reason = NULL`, oppID, m.ID); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		UPDATE listings l SET market_id = $1, status = 'in_market'
		FROM parties p
		WHERE p.id = l.owner_party_id AND l.status NOT IN ('sold','expired','fulfilled','cancelled')
		  AND l.id IN (SELECT listing_id FROM opportunity_participants WHERE opportunity_id = $2 AND listing_id IS NOT NULL
		               UNION SELECT listing_id FROM opportunity_listings WHERE opportunity_id = $2)
		RETURNING l.id::text, l.item, p.user_id::text`, m.ID, oppID)
	if err != nil {
		return err
	}
	type moved struct {
		id, item string
		user     *string
	}
	var ls []moved
	for rows.Next() {
		var l moved
		if err := rows.Scan(&l.id, &l.item, &l.user); err != nil {
			rows.Close()
			return err
		}
		ls = append(ls, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	items := map[string]string{}
	for _, l := range ls {
		if err := listingEvent(ctx, tx, l.id, "in_market", fmt.Sprintf("Otomatis masuk %s dari opportunity %s", m.Name, oppCode)); err != nil {
			return err
		}
		if l.user != nil {
			items[*l.user] = l.item
			if _, err := tx.Exec(ctx, `
				INSERT INTO watchlist (user_id, market_id, joined_at) VALUES ($1, $2, now())
				ON CONFLICT (user_id, market_id) DO UPDATE SET joined_at = coalesce(watchlist.joined_at, now())`, *l.user, m.ID); err != nil {
				return err
			}
		}
	}
	urows, err := tx.Query(ctx, `
		SELECT p.user_id::text FROM opportunity_participants op JOIN parties p ON p.id = op.party_id
		WHERE op.opportunity_id = $1 AND p.user_id IS NOT NULL
		UNION SELECT p.user_id::text FROM opportunity_listings ol JOIN parties p ON p.id = ol.party_id
		WHERE ol.opportunity_id = $1 AND p.user_id IS NOT NULL
		UNION SELECT user_id::text FROM opportunity_follows WHERE opportunity_id = $1`, oppID)
	if err != nil {
		return err
	}
	users, err := pgx.CollectRows(urows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, u := range users {
		body := m.Name + " sudah live. Gabung untuk ikut round pertama."
		if item, ok := items[u]; ok {
			body = fmt.Sprintf("%s sudah live; kontribusimu (%s) otomatis masuk lot round pertama.", m.Name, item)
		}
		if err := notify(ctx, tx, u, notification{Type: "new_market", Title: "Market baru dari " + oppTitle, Body: body, Href: "/markets/" + m.ID}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) CreateMmMarket(ctx context.Context, req api.CreateMmMarketRequestObject) (api.CreateMmMarketResponseObject, error) {
	sess, err := s.requireMaker(ctx)
	if err != nil {
		return nil, err
	}
	in := req.Body
	rules := rulesFromAPI(in.Rules)
	f := rules.validate()
	if strings.TrimSpace(in.Name) == "" {
		f["name"] = "Nama market wajib diisi"
	}
	if strings.TrimSpace(in.Unit) == "" {
		f["unit"] = "Isi satuan"
	}
	if !(in.ReferencePriceIdr > 0) {
		f["referencePriceIdr"] = "Isi harga acuan per unit"
	}
	if !(in.Demand > 0) {
		f["demand"] = "Isi perkiraan demand"
	}
	if len(f) > 0 {
		return nil, &Error{Status: 422, Code: "validation", Message: "Periksa kembali isian market", Fields: f}
	}
	var m mmMarket
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		m, err = formMarket(ctx, tx, sess, marketForm{OpportunityID: in.OpportunityId, Name: in.Name, Objective: string(in.Objective),
			Mechanism: string(in.Mechanism), Category: string(in.CategoryId), Unit: in.Unit, Demand: in.Demand, Supply: in.Supply,
			RefPrice: int64(in.ReferencePriceIdr), Rules: rules, AutoInvite: in.AutoInvite, Approval: string(in.Approval),
			Verification: string(in.SupplierVerification)})
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.CreateMmMarket201JSONResponse{Id: m.ID}, nil
}

type roundForm struct {
	Title           string
	Quantity        float64
	OpeningIdr      int64
	DurationMinutes int
	StartsAt        *time.Time
	Spec            *string
}

type openedRound struct {
	ID, Code string
	Round    int
}

func openRound(ctx context.Context, tx pgx.Tx, sess *session, m mmMarket, in roundForm) (openedRound, error) {
	var out openedRound
	n, err := roundsOpened(ctx, tx, m.ID)
	if err != nil {
		return out, err
	}
	out.Round = n + 1
	versions, err := ensureRuleVersions(ctx, tx, m)
	if err != nil {
		return out, err
	}
	v := activeVersion(ruleVersionsOf(versions), out.Round)
	typ := roundType[m.Mechanism]
	visibility := v.Rules.Visibility
	if typ == "sealed" {
		visibility = "sealed"
	}
	step := int64(math.Max(1, math.Round(float64(in.OpeningIdr)*v.Rules.MinStepPct/100)))
	var current *int64
	if visibility == "full" || typ == "dutch" {
		current = &in.OpeningIdr
	}
	start, status := time.Now(), "live"
	if in.StartsAt != nil && in.StartsAt.After(start) {
		start, status = *in.StartsAt, "scheduled"
		if v.Rules.Eligibility != "open" {
			status = "qualification"
		}
	}
	spec := fmt.Sprintf("Round %d, aturan market v%d", out.Round, v.Version)
	if in.Spec != nil {
		spec = *in.Spec
	}
	stepLabel := "Langkah minimum"
	if typ == "dutch" {
		stepLabel = "Penurunan harga"
	}
	labels := []api.LabeledValue{
		{Label: "Round", Value: fmt.Sprintf("%d di %s", out.Round, m.Name)},
		{Label: "Visibilitas bid", Value: ruleVisibility[visibility]},
		{Label: stepLabel, Value: fmt.Sprintf("%s per %s", rupiah(step), m.Unit)},
		{Label: "Perpanjangan otomatis", Value: "+5 menit jika ada bid di 2 menit terakhir"},
	}
	for _, l := range v.Rules.labeled(m.Unit) {
		if l.Label == "Eligibility" || l.Label == "Penetapan pemenang" || l.Label == "Wilayah" {
			labels = append(labels, l)
		}
	}
	rules, _ := json.Marshal(labels)
	if err := tx.QueryRow(ctx, `
		INSERT INTO auctions (title, market_id, round_no, rule_version_id, category_id, type, status, visibility, lot_item, lot_spec, quantity, unit,
		                      opening_price_idr, current_price_idr, min_step_idr, starts_at, ends_at, rules, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $16::timestamptz + make_interval(mins => $17), $18, $19)
		RETURNING id, code`,
		strings.TrimSpace(in.Title), m.ID, out.Round, v.ID, m.Category, typ, status, visibility, m.Name, spec, in.Quantity, m.Unit,
		in.OpeningIdr, current, step, start, in.DurationMinutes, rules, sess.UserID).Scan(&out.ID, &out.Code); err != nil {
		return out, err
	}
	changes := []change{{Field: "Round", After: fmt.Sprintf("%d (%s)", out.Round, out.Code)}, {Field: "Harga pembuka", After: rupiah(in.OpeningIdr)}}
	if m.Status == "formation" {
		if _, err := tx.Exec(ctx, `UPDATE markets SET status = 'active' WHERE id = $1`, m.ID); err != nil {
			return out, err
		}
		before := marketStatusLabel["formation"]
		changes = append(changes, change{Field: "Status", Before: &before, After: marketStatusLabel["active"]})
	}
	title := strings.TrimSpace(in.Title)
	amount := int64(math.Round(float64(in.OpeningIdr) * in.Quantity))
	if err := mmAudit(ctx, tx, sess, audit{Action: fmt.Sprintf("Buka round %d: %s", out.Round, title), EntityType: "market", EntityID: m.ID,
		EntityLabel: m.Name, MarketID: &m.ID, Changes: changes}, fmt.Sprintf("Round %d dibuka: %s", out.Round, title), &amount); err != nil {
		return out, err
	}
	if err := notifyMarket(ctx, tx, m.ID, notification{Type: "auction_invitation", Title: fmt.Sprintf("Round %d dibuka di %s", out.Round, m.Name),
		Body: fmt.Sprintf("%s: %s %s, harga pembuka %s.", title, idNumber(in.Quantity), m.Unit, rupiah(in.OpeningIdr)), Href: "/auctions/" + out.ID}); err != nil {
		return out, err
	}
	if status != "live" {
		return out, nil
	}
	return out, emitRoundResult(ctx, tx, out.ID)
}

func ruleVersionsOf(vs []storedRuleVersion) []ruleVersion {
	out := make([]ruleVersion, len(vs))
	for i, v := range vs {
		out[i] = v.ruleVersion
	}
	return out
}

func errMarketInactive(status string) error {
	return &Error{Status: http.StatusConflict, Code: "market_inactive", Message: fmt.Sprintf("Market berstatus %s; round baru tidak bisa dibuka", marketStatusLabel[status])}
}

func (s *Server) OpenMmRound(ctx context.Context, req api.OpenMmRoundRequestObject) (api.OpenMmRoundResponseObject, error) {
	sess, err := s.requireMaker(ctx)
	if err != nil {
		return nil, err
	}
	in := req.Body
	var out openedRound
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		m, err := operatedMarket(ctx, tx, sess.UserID, req.Id, true)
		if err != nil {
			return err
		}
		if m.Status != "active" && m.Status != "formation" {
			return errMarketInactive(m.Status)
		}
		f := map[string]string{}
		if strings.TrimSpace(in.Title) == "" {
			f["title"] = "Isi judul round"
		}
		if !(in.Quantity > 0) {
			f["quantity"] = "Kuantitas harus lebih dari 0"
		}
		if !(in.OpeningPriceIdr > 0) {
			f["openingPriceIdr"] = "Isi harga pembuka"
		}
		if !(in.DurationMinutes > 0) {
			f["durationMinutes"] = "Pilih durasi"
		}
		if len(f) > 0 {
			return &Error{Status: 422, Code: "validation", Message: "Periksa isian round", Fields: f}
		}
		out, err = openRound(ctx, tx, sess, m, roundForm{Title: in.Title, Quantity: in.Quantity, OpeningIdr: int64(in.OpeningPriceIdr),
			DurationMinutes: in.DurationMinutes, StartsAt: in.StartsAt})
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.OpenMmRound201JSONResponse{Id: out.ID}, nil
}

func loadParticipants(ctx context.Context, q dbtx, where string, args ...any) ([]api.MmParticipant, error) {
	rows, err := q.Query(ctx, `
		SELECT mp.id::text, CASE WHEN pm.opt_in = false THEN 'Bisnis lain #' || pm.n ELSE p.name END, p.display_kind, mp.verified,
		       mp.role, mp.status, mp.joined_at, p.user_id::text, mp.note, mp.party_id::text
		FROM market_participants mp JOIN parties p ON p.id = mp.party_id
		LEFT JOIN LATERAL (
			SELECT x.opt_in, x.n FROM (
				SELECT pm.org_id, pm.opt_in, row_number() OVER (ORDER BY pm.created_at, pm.id) AS n
				FROM collective_pools cp JOIN pool_members pm ON pm.pool_id = cp.id WHERE cp.market_id = mp.market_id) x
			WHERE x.org_id = p.org_id LIMIT 1) pm ON true
		WHERE `+where+` ORDER BY mp.joined_at, mp.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.MmParticipant{}
	var parties []string
	for rows.Next() {
		var p api.MmParticipant
		var party string
		if err := rows.Scan(&p.Id, &p.Name, &p.Kind, &p.Verified, &p.Role, &p.Status, &p.JoinedAt, &p.UserId, &p.Note, &party); err != nil {
			return nil, err
		}
		out = append(out, p)
		parties = append(parties, party)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i, party := range parties {
		score, _, err := reputationOf(ctx, q, party)
		if err != nil {
			return nil, err
		}
		out[i].Reputation = float64(score)
	}
	return out, nil
}

func loadMmDisputes(ctx context.Context, q dbtx, where string, args ...any) ([]api.MmDispute, error) {
	rows, err := q.Query(ctx, `
		SELECT md.id::text, md.title, md.parties, coalesce(d.status, md.status), md.opened_at, md.resolution, md.escalated_to::text
		FROM market_disputes md LEFT JOIN disputes d ON d.id = md.escalated_to
		WHERE `+where+` ORDER BY md.opened_at DESC, md.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.MmDispute{}
	for rows.Next() {
		var d api.MmDispute
		if err := rows.Scan(&d.Id, &d.Title, &d.Parties, &d.Status, &d.OpenedAt, &d.Resolution, &d.EscalatedTo); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Server) GetMmMarket(ctx context.Context, req api.GetMmMarketRequestObject) (api.GetMmMarketResponseObject, error) {
	sess, err := s.requireMaker(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Primary()
	m, err := operatedMarket(ctx, q, sess.UserID, req.Id, false)
	if errors.Is(err, errMarketNotFound) {
		return nil, &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Market tidak ditemukan atau bukan operasimu"}
	}
	if err != nil {
		return nil, err
	}
	out := api.GetMmMarket200JSONResponse{RuleVersions: []api.RuleVersion{}}
	if out.Market, err = s.marketDetail(ctx, q, m.ID); err != nil {
		return nil, err
	}
	out.Rounds = out.Market.Auctions
	if out.Participants, err = loadParticipants(ctx, q, `mp.market_id = $1`, m.ID); err != nil {
		return nil, err
	}
	if out.Disputes, err = loadMmDisputes(ctx, q, `md.market_id = $1`, m.ID); err != nil {
		return nil, err
	}
	vs, err := loadRuleVersions(ctx, q, m.ID)
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		out.RuleVersions = append(out.RuleVersions, v.api())
	}
	if out.CurrentRound, err = roundsOpened(ctx, q, m.ID); err != nil {
		return nil, err
	}
	if out.Results, err = roundResults(ctx, q, `a.market_id = $1`, m.ID); err != nil {
		return nil, err
	}
	var approval, verification string
	if err := q.QueryRow(ctx, `SELECT coalesce(max(approval), 'manual'), coalesce(max(supplier_verification), 'documents') FROM market_settings WHERE market_id = $1`,
		m.ID).Scan(&approval, &verification); err != nil {
		return nil, err
	}
	out.Settings.Approval, out.Settings.SupplierVerification = api.MmMarketOpsSettingsApproval(approval), api.SupplierVerification(verification)
	qs, err := queues(ctx, q, []string{m.ID})
	if err != nil {
		return nil, err
	}
	var summary api.Market
	if err := widen(out.Market, &summary); err != nil {
		return nil, err
	}
	out.Alerts = mmAlerts(summary, qs[m.ID])
	return out, nil
}

func (s *Server) ListMmMarketAudit(ctx context.Context, req api.ListMmMarketAuditRequestObject) (api.ListMmMarketAuditResponseObject, error) {
	sess, err := s.requireMaker(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Primary()
	m, err := operatedMarket(ctx, q, sess.UserID, req.Id, false)
	if err != nil {
		return nil, err
	}

	rows, err := q.Query(ctx, `
		SELECT id, actor_label, action, entity_type, entity_id, entity_label, at, reason, changes
		FROM audit_log WHERE entity_type = 'market' AND entity_id = $1 ORDER BY at DESC, id DESC`, m.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := api.ListMmMarketAudit200JSONResponse{}
	for rows.Next() {
		var a api.AuditEntry
		var id int64
		var changes []byte
		if err := rows.Scan(&id, &a.Actor, &a.Action, &a.Entity.Type, &a.Entity.Id, &a.Entity.Label, &a.At, &a.Reason, &changes); err != nil {
			return nil, err
		}
		a.Id = fmt.Sprint(id)
		if err := json.Unmarshal(changes, &a.Changes); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

var participantVerb = map[string]string{"approve": "Approve", "reject": "Tolak", "verify": "Verifikasi supplier", "suspend": "Suspend"}

func (s *Server) ActOnMmParticipant(ctx context.Context, req api.ActOnMmParticipantRequestObject) (api.ActOnMmParticipantResponseObject, error) {
	sess, err := s.requireMaker(ctx)
	if err != nil {
		return nil, err
	}
	action, reason := string(req.Body.Action), strings.TrimSpace(deref(req.Body.Reason))
	var out api.MmParticipant
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		m, err := operatedMarket(ctx, tx, sess.UserID, req.Id, false)
		if errors.Is(err, errMarketNotFound) {
			return &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Participant tidak ditemukan"}
		}
		if err != nil {
			return err
		}
		var id string
		if err := tx.QueryRow(ctx, `SELECT id FROM market_participants WHERE id::text = $1 AND market_id = $2 FOR UPDATE`, req.Pid, m.ID).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
			return &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Participant tidak ditemukan"}
		} else if err != nil {
			return err
		}
		ps, err := loadParticipants(ctx, tx, `mp.id = $1`, id)
		if err != nil {
			return err
		}
		p := ps[0]
		allowed := map[string]bool{"approve": p.Status == "pending", "reject": p.Status == "pending",
			"verify": p.Role == "supplier" && !p.Verified && p.Status != "rejected", "suspend": p.Status == "active"}
		if !allowed[action] {
			return &Error{Status: http.StatusConflict, Code: "invalid_transition", Message: "Aksi ini tidak tersedia untuk status participant sekarang"}
		}
		if (action == "reject" || action == "suspend") && reason == "" {
			return &Error{Status: 422, Code: "validation", Message: "Alasan wajib diisi", Fields: map[string]string{"reason": "Alasan wajib diisi dan akan tercatat di audit log"}}
		}
		before := string(p.Status)
		next := map[string]string{"approve": "active", "reject": "rejected", "suspend": "suspended", "verify": before}[action]
		var note *string
		if reason != "" {
			note = &reason
		}
		if _, err := tx.Exec(ctx, `UPDATE market_participants SET status = $2, verified = verified OR $3, note = coalesce($4, note) WHERE id = $1`,
			id, next, action == "verify", note); err != nil {
			return err
		}
		changes := []change{{Field: "Status", Before: &before, After: next}}
		if action == "verify" {
			b := "Belum"
			changes = []change{{Field: "Verifikasi", Before: &b, After: "Terverifikasi"}}
		}
		verb := participantVerb[action]
		if err := mmAudit(ctx, tx, sess, audit{Action: verb + " participant " + p.Name, EntityType: "market", EntityID: m.ID, EntityLabel: m.Name,
			MarketID: &m.ID, Reason: note, Changes: changes}, "", nil); err != nil {
			return err
		}
		if p.UserId != nil {
			body := map[string]string{"approve": "Kamu sekarang peserta aktif " + m.Name + ".", "reject": "Pendaftaranmu ditolak: " + reason,
				"suspend": "Kamu disuspend dari " + m.Name + ": " + reason, "verify": "Status supplier kamu di " + m.Name + " terverifikasi."}[action]
			if err := notify(ctx, tx, *p.UserId, notification{Type: "new_market", Title: m.Name + ": " + strings.ToLower(verb), Body: body, Href: "/markets/" + m.ID}); err != nil {
				return err
			}
		}
		ps, err = loadParticipants(ctx, tx, `mp.id = $1`, id)
		if err != nil {
			return err
		}
		out = ps[0]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return api.ActOnMmParticipant200JSONResponse(out), nil
}

func (s *Server) UpdateMmMarketRules(ctx context.Context, req api.UpdateMmMarketRulesRequestObject) (api.UpdateMmMarketRulesResponseObject, error) {
	sess, err := s.requireMaker(ctx)
	if err != nil {
		return nil, err
	}
	rules := rulesFromAPI(req.Body.Rules)
	reason := strings.TrimSpace(deref(req.Body.Reason))
	var why *string
	if reason != "" {
		why = &reason
	}
	out := api.UpdateMmMarketRules200JSONResponse{}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		m, err := operatedMarket(ctx, tx, sess.UserID, req.Id, true)
		if err != nil {
			return err
		}
		if m.Status == "closed" {
			return &Error{Status: http.StatusConflict, Code: "market_closed", Message: "Market sudah ditutup"}
		}
		if f := rules.validate(); len(f) > 0 {
			return &Error{Status: 422, Code: "validation", Message: "Aturan belum valid", Fields: f}
		}
		vs, err := ensureRuleVersions(ctx, tx, m)
		if err != nil {
			return err
		}
		changes := diffRules(vs[len(vs)-1].Rules, rules, m.Unit)
		if len(changes) == 0 {
			return &Error{Status: 422, Code: "no_change", Message: "Tidak ada perubahan dibanding versi terakhir"}
		}
		round, err := roundsOpened(ctx, tx, m.ID)
		if err != nil {
			return err
		}
		version := len(vs) + 1
		raw, _ := json.Marshal(rules)
		if _, err := tx.Exec(ctx, `INSERT INTO market_rule_versions (market_id, version, rules, effective_from_round, author, created_by, reason)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, m.ID, version, raw, round+1, mmActor(sess), sess.UserID, why); err != nil {
			return err
		}
		if err := mmAudit(ctx, tx, sess, audit{Action: fmt.Sprintf("Aturan v%d dibuat (berlaku mulai round %d)", version, round+1), EntityType: "market",
			EntityID: m.ID, EntityLabel: m.Name, MarketID: &m.ID, Reason: why, Changes: changes}, "", nil); err != nil {
			return err
		}
		if err := notifyMarket(ctx, tx, m.ID, notification{Type: "new_market", Title: "Aturan " + m.Name + " berubah",
			Body: fmt.Sprintf("%d perubahan berlaku mulai round %d. Round yang sudah berjalan tidak terpengaruh.", len(changes), round+1), Href: "/markets/" + m.ID}); err != nil {
			return err
		}
		if vs, err = loadRuleVersions(ctx, tx, m.ID); err != nil {
			return err
		}
		for _, v := range vs {
			out = append(out, v.api())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

var marketTransitions = map[string]struct {
	from       []string
	to, verb   string
	publicFeed bool
}{
	"pause":  {[]string{"active", "formation"}, "paused", "Pause market", false},
	"resume": {[]string{"paused"}, "active", "Lanjutkan market", false},
	"close":  {[]string{"active", "formation", "paused"}, "closed", "Tutup market", true},
}

func (s *Server) SetMmMarketStatus(ctx context.Context, req api.SetMmMarketStatusRequestObject) (api.SetMmMarketStatusResponseObject, error) {
	sess, err := s.requireMaker(ctx)
	if err != nil {
		return nil, err
	}
	t := marketTransitions[string(req.Body.Action)]
	reason := strings.TrimSpace(req.Body.Reason)
	var out api.Market
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		m, err := operatedMarket(ctx, tx, sess.UserID, req.Id, true)
		if err != nil {
			return err
		}
		allowed := false
		for _, f := range t.from {
			allowed = allowed || f == m.Status
		}
		if !allowed {
			return &Error{Status: http.StatusConflict, Code: "invalid_transition", Message: "Market berstatus " + marketStatusLabel[m.Status]}
		}
		if reason == "" {
			return &Error{Status: 422, Code: "validation", Message: "Alasan wajib diisi", Fields: map[string]string{"reason": "Alasan wajib diisi dan dikirim ke participant"}}
		}
		if _, err := tx.Exec(ctx, `UPDATE markets SET status = $2 WHERE id = $1`, m.ID, t.to); err != nil {
			return err
		}
		before, after := marketStatusLabel[m.Status], marketStatusLabel[t.to]
		public := ""
		if t.publicFeed {
			public = m.Name + " → " + after
		}
		if err := mmAudit(ctx, tx, sess, audit{Action: t.verb, EntityType: "market", EntityID: m.ID, EntityLabel: m.Name, MarketID: &m.ID,
			Reason: &reason, Changes: []change{{Field: "Status", Before: &before, After: after}}}, public, nil); err != nil {
			return err
		}
		if err := notifyMarket(ctx, tx, m.ID, notification{Type: "new_market", Title: m.Name + ": " + after, Body: reason, Href: "/markets/" + m.ID}); err != nil {
			return err
		}
		ms, err := loadMarkets(ctx, tx, `m.id = $1`, m.ID)
		if err != nil {
			return err
		}
		out = ms[0]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return api.SetMmMarketStatus200JSONResponse(out), nil
}

func (s *Server) ActOnMmDispute(ctx context.Context, req api.ActOnMmDisputeRequestObject) (api.ActOnMmDisputeResponseObject, error) {
	sess, err := s.requireMaker(ctx)
	if err != nil {
		return nil, err
	}
	action, note := string(req.Body.Action), strings.TrimSpace(deref(req.Body.Note))
	notFound := &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Dispute tidak ditemukan"}
	var out api.MmDispute
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		m, err := operatedMarket(ctx, tx, sess.UserID, req.Id, false)
		if errors.Is(err, errMarketNotFound) {
			return notFound
		}
		if err != nil {
			return err
		}
		var id, title, parties, status string
		var openedAt time.Time
		var escalated *string
		err = tx.QueryRow(ctx, `SELECT id, title, parties, status, opened_at, escalated_to::text FROM market_disputes WHERE id::text = $1 AND market_id = $2 FOR UPDATE`,
			req.Did, m.ID).Scan(&id, &title, &parties, &status, &openedAt, &escalated)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound
		}
		if err != nil {
			return err
		}
		if escalated != nil {
			return &Error{Status: http.StatusConflict, Code: "escalated", Message: "Dispute ini sudah ditangani admin"}
		}
		if status == "resolved" || (action == "review" && status == "review") {
			return &Error{Status: http.StatusConflict, Code: "invalid_transition", Message: "Dispute sudah di tahap ini"}
		}
		switch action {
		case "escalate":
			if note == "" {
				return &Error{Status: 422, Code: "validation", Message: "Tulis alasan eskalasi", Fields: map[string]string{"note": "Jelaskan kenapa perlu admin"}}
			}
			if err := escalateDispute(ctx, tx, sess, m, id, title, parties, openedAt, note); err != nil {
				return err
			}
		case "review", "resolve":
			if action == "resolve" && note == "" {
				return &Error{Status: 422, Code: "validation", Message: "Isi keputusan", Fields: map[string]string{"note": "Tulis keputusan moderasi; dikirim ke kedua pihak"}}
			}
			next := map[string]string{"review": "review", "resolve": "resolved"}[action]
			var resolution, why *string
			if action == "resolve" {
				resolution = &note
			}
			if note != "" {
				why = &note
			}
			if _, err := tx.Exec(ctx, `UPDATE market_disputes SET status = $2, resolution = coalesce($3, resolution) WHERE id = $1`, id, next, resolution); err != nil {
				return err
			}
			if err := mmAudit(ctx, tx, sess, audit{Action: "Moderasi dispute: " + title, EntityType: "market", EntityID: m.ID, EntityLabel: m.Name,
				MarketID: &m.ID, Reason: why, Changes: []change{{Field: "Status dispute", Before: &status, After: next}}}, "", nil); err != nil {
				return err
			}
		}
		ds, err := loadMmDisputes(ctx, tx, `md.id = $1`, id)
		if err != nil {
			return err
		}
		out = ds[0]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return api.ActOnMmDispute200JSONResponse(out), nil
}

func escalateDispute(ctx context.Context, tx pgx.Tx, sess *session, m mmMarket, id, title, parties string, openedAt time.Time, note string) error {
	buyer, supplier, ok := strings.Cut(parties, " vs ")
	if !ok || strings.TrimSpace(buyer) == "" {
		buyer = "Pembeli"
	}
	if strings.TrimSpace(supplier) == "" {
		supplier = "Supplier"
	}
	var buyerParty, supplierParty, tradeID, caseID string
	for _, p := range []struct {
		name string
		dst  *string
	}{{buyer, &buyerParty}, {supplier, &supplierParty}} {
		if err := tx.QueryRow(ctx, `INSERT INTO parties (kind, name, display_kind, verified) VALUES ('external', $1, 'business', true) RETURNING id`,
			strings.TrimSpace(p.name)).Scan(p.dst); err != nil {
			return err
		}
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO trades (title, buyer_party_id, supplier_party_id, quantity, unit, unit_price_idr, total_idr, status, market_id, delivery_address, due_at)
		VALUES ($1, $2, $3, 1, 'lot', $4, $4, 'disputed', $5, '-', now() + interval '7 days') RETURNING id`,
		title+" · "+m.Name, buyerParty, supplierParty, m.PriceMax, m.ID).Scan(&tradeID); err != nil {
		return err
	}
	actor := mmActor(sess)
	if _, err := tx.Exec(ctx, `INSERT INTO trade_events (trade_id, status, note, actor_user_id) VALUES ($1, 'disputed', 'Dieskalasi dari market maker', $2)`,
		tradeID, sess.UserID); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO disputes (trade_id, status, reason, opened_by, opened_at, market_id) VALUES ($1, 'open', $2, $3, $4, $5) RETURNING id`,
		tradeID, title, sess.UserID, openedAt, m.ID).Scan(&caseID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		WITH ev AS (INSERT INTO dispute_evidence (dispute_id, side, author_user_id, author_name, text) VALUES ($1, 'buyer', $2, $3, $4))
		INSERT INTO dispute_events (dispute_id, actor_user_id, actor_label, label) VALUES ($1, $2, $3, 'Dieskalasi dari market maker')`,
		caseID, sess.UserID, actor, "Catatan market maker: "+note); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE market_disputes SET escalated_to = $2, escalated_by = $3, escalated_at = now() WHERE id = $1`, id, caseID, sess.UserID); err != nil {
		return err
	}
	before := "Market maker"
	if err := mmAudit(ctx, tx, sess, audit{Action: "Eskalasi dispute ke admin: " + title, EntityType: "market", EntityID: m.ID, EntityLabel: m.Name,
		MarketID: &m.ID, Reason: &note, Changes: []change{{Field: "Penanganan", Before: &before, After: "Admin governance"}}}, "", nil); err != nil {
		return err
	}
	return notifyAdmins(ctx, tx, notification{Type: "transaction_update", Title: "Eskalasi dispute dari " + m.Name, Body: title, Href: "/admin/disputes/" + caseID})
}

func (s *Server) ListMmPools(ctx context.Context, _ api.ListMmPoolsRequestObject) (api.ListMmPoolsResponseObject, error) {
	sess, err := s.requireMaker(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Primary()
	rows, err := q.Query(ctx, `
		SELECT cp.id::text, cp.title, cp.category_id, cp.spec, cp.region, cp.deadline, cp.unit, cp.base_unit_price_idr, cp.ref_qty, cp.threshold_qty,
		       cp.status, cp.market_requested_at, cp.market_id::text, cp.auction_id::text, a.status, a.ends_at,
		       st.settled_at, u.name, w.name, st.price_idr
		FROM collective_pools cp
		LEFT JOIN auctions a ON a.id = cp.auction_id
		LEFT JOIN settlements st ON st.id = cp.settlement_id
		LEFT JOIN users u ON u.id = st.settled_by
		LEFT JOIN parties w ON w.id = st.winner_party_id
		WHERE cp.status = 'market_requested' OR cp.formed_by = $1
		ORDER BY cp.market_requested_at DESC NULLS LAST, cp.created_at DESC, cp.id`, sess.UserID)
	if err != nil {
		return nil, err
	}
	out := api.ListMmPools200JSONResponse{}
	idx := map[string]int{}
	for rows.Next() {
		var p api.CollectivePool
		var price int64
		var refQty, threshold float64
		var roundStatus *string
		var endsAt, settledAt *time.Time
		var by, winner *string
		var settledPrice *int64
		if err := rows.Scan(&p.Id, &p.Title, &p.CategoryId, &p.Spec, &p.Region, &p.Deadline, &p.Unit, &price, &refQty, &threshold, &p.Status,
			&p.MarketRequestedAt, &p.MarketId, &p.AuctionId, &roundStatus, &endsAt, &settledAt, &by, &winner, &settledPrice); err != nil {
			rows.Close()
			return nil, err
		}
		p.BaseUnitPriceIdr, p.RefQty, p.ThresholdQty, p.Members = int(price), refQty, threshold, []api.PoolMember{}
		if roundStatus != nil {
			p.Round = &struct {
				EndsAt time.Time         `json:"endsAt"`
				Status api.AuctionStatus `json:"status"`
			}{*endsAt, api.AuctionStatus(*roundStatus)}
		}
		if settledAt != nil {
			p.Settlement = &api.PoolSettlement{At: *settledAt, By: deref(by), Winner: deref(winner), PriceIdr: int(deref64(settledPrice))}
			p.Settlement.Lines = []struct {
				AmountIdr     int     `json:"amountIdr"`
				Mine          *bool   `json:"mine,omitempty"`
				Name          string  `json:"name"`
				OptIn         bool    `json:"optIn"`
				Quantity      float64 `json:"quantity"`
				Share         float64 `json:"share"`
				TransactionId *string `json:"transactionId,omitempty"`
			}{}
		}
		idx[p.Id] = len(out)
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out))
	for _, p := range out {
		ids = append(ids, p.Id)
	}
	mrows, err := q.Query(ctx, `
		SELECT pm.pool_id::text, coalesce(o.name, pm.name), pm.quantity, pm.opt_in, sl.quantity, sl.share, sl.amount_idr
		FROM pool_members pm LEFT JOIN orgs o ON o.id = pm.org_id LEFT JOIN settlement_lines sl ON sl.id = pm.settlement_line_id
		WHERE pm.pool_id::text = ANY($1) ORDER BY pm.created_at, pm.id`, ids)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var poolID, name string
		var qty float64
		var optIn bool
		var lineQty, lineShare *float64
		var lineAmount *int64
		if err := mrows.Scan(&poolID, &name, &qty, &optIn, &lineQty, &lineShare, &lineAmount); err != nil {
			return nil, err
		}
		p := &out[idx[poolID]]
		if !optIn {
			name = fmt.Sprintf("Bisnis lain #%d", len(p.Members)+1)
		}
		p.Members = append(p.Members, api.PoolMember{Name: name, Quantity: qty, OptIn: optIn})
		if p.Settlement != nil && lineQty != nil {
			p.Settlement.Lines = append(p.Settlement.Lines, struct {
				AmountIdr     int     `json:"amountIdr"`
				Mine          *bool   `json:"mine,omitempty"`
				Name          string  `json:"name"`
				OptIn         bool    `json:"optIn"`
				Quantity      float64 `json:"quantity"`
				Share         float64 `json:"share"`
				TransactionId *string `json:"transactionId,omitempty"`
			}{AmountIdr: int(*lineAmount), Name: name, OptIn: optIn, Quantity: *lineQty, Share: *lineShare})
		}
	}
	return out, mrows.Err()
}

func (s *Server) FormMmPoolMarket(ctx context.Context, req api.FormMmPoolMarketRequestObject) (api.FormMmPoolMarketResponseObject, error) {
	sess, err := s.requireMaker(ctx)
	if err != nil {
		return nil, err
	}
	var out api.FormMmPoolMarket201JSONResponse
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var id, title, category, spec, region, unit, status string
		var marketID *string
		var price int64
		err := tx.QueryRow(ctx, `
			SELECT id, title, category_id, spec, region, unit, base_unit_price_idr, status, market_id::text
			FROM collective_pools WHERE id::text = $1 FOR UPDATE`, req.Id).Scan(&id, &title, &category, &spec, &region, &unit, &price, &status, &marketID)
		if errors.Is(err, pgx.ErrNoRows) {
			return &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Pool tidak ditemukan"}
		}
		if err != nil {
			return err
		}
		if status != "market_requested" {
			msg := "Pool ini belum meminta market"
			if marketID != nil {
				msg = "Market untuk pool ini sudah dibentuk"
			}
			return &Error{Status: http.StatusConflict, Code: "invalid_transition", Message: msg}
		}
		minutes := int(math.Round(req.Body.DurationMinutes))
		if minutes <= 0 {
			return &Error{Status: 422, Code: "validation", Message: "Pilih durasi", Fields: map[string]string{"durationMinutes": "Pilih durasi"}}
		}
		var total float64
		var orgs, parties []string
		rows, err := tx.Query(ctx, `
			SELECT pm.quantity, pm.org_id::text, p.id::text FROM pool_members pm LEFT JOIN parties p ON p.org_id = pm.org_id
			WHERE pm.pool_id = $1 ORDER BY pm.created_at, pm.id`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var qty float64
			var org, party *string
			if err := rows.Scan(&qty, &org, &party); err != nil {
				rows.Close()
				return err
			}
			total += qty
			if org != nil && party != nil {
				orgs, parties = append(orgs, *org), append(parties, *party)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		m, err := formMarket(ctx, tx, sess, marketForm{Name: fmt.Sprintf("Kolektif %s · %s", title, region), Objective: "procurement",
			Mechanism: "collective_procurement", Category: category, Unit: unit, Demand: total, RefPrice: price, Approval: "auto",
			Verification: "documents", Rules: defaultRules(total, total, region, "collective_procurement", time.Now()), Invite: parties})
		if err != nil {
			return err
		}
		r, err := openRound(ctx, tx, sess, m, roundForm{Title: fmt.Sprintf("%s · %s %s", title, idNumber(total), unit), Quantity: total,
			OpeningIdr: price, DurationMinutes: minutes, Spec: &spec})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE collective_pools SET status = 'market_live', market_id = $2, auction_id = $3, formed_by = $4, formed_at = now() WHERE id = $1`,
			id, m.ID, r.ID, sess.UserID); err != nil {
			return err
		}
		before := "Menunggu market"
		for _, org := range orgs {
			if err := writeAudit(ctx, tx, audit{ActorUserID: &sess.UserID, ActorLabel: mmActor(sess), Action: "Market terbentuk dari pool collective",
				EntityType: "market", EntityID: m.ID, EntityLabel: title, OrgID: &org,
				Changes: []change{{Field: "Pool", Before: &before, After: fmt.Sprintf("Market live (%s)", r.Code)}}}); err != nil {
				return err
			}
			if err := notifyOrg(ctx, tx, org, notification{Type: "new_market", Title: "Market terbentuk: " + title,
				Body: fmt.Sprintf("%s membuka round %s untuk %s %s. Supplier mulai bersaing.", m.Name, r.Code, idNumber(total), unit),
				Href: fmt.Sprintf("/org/%s/collective?pool=%s", org, id)}); err != nil {
				return err
			}
		}
		out = api.FormMmPoolMarket201JSONResponse{MarketId: m.ID, AuctionId: r.ID}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func notifyOrg(ctx context.Context, q dbtx, orgID string, n notification) error {
	rows, err := q.Query(ctx, `SELECT user_id::text FROM org_members WHERE org_id = $1 AND status = 'active' AND user_id IS NOT NULL`, orgID)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := notify(ctx, q, id, n); err != nil {
			return err
		}
	}
	return nil
}
