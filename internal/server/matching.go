package server

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Smart Matching (PRD §8.6): what a user has (supply listings, identity items) against what open opportunities need.
// The score is a port of the frontend's src/domain/matching.ts.

var matchWeights = struct{ Category, Distance, Coverage, Confidence float64 }{35, 25, 25, 15}

type matchInput struct {
	CategoryMatch bool
	DistanceKm    float64
	RadiusKm      float64  // the user's delivery radius
	Coverage      *float64 // have / gap in the same unit; nil when units differ
	Confidence    float64  // engine confidence 0-1
}

func clamp01(n float64) float64 { return math.Min(1, math.Max(0, n)) }

// scoreMatch is 0-100. Distance is full inside the radius and fades to 0 at 3x the radius; coverage is capped at 1
// and counts half when units differ.
func scoreMatch(m matchInput) (float64, api.MatchParts) {
	radius := math.Max(1, m.RadiusKm)
	distance := 1.0
	if m.DistanceKm > radius {
		distance = clamp01(1 - (m.DistanceKm-radius)/(2*radius))
	}
	coverage := 0.5
	if m.Coverage != nil {
		coverage = clamp01(*m.Coverage)
	}
	p := api.MatchParts{
		Distance:   jsRound(matchWeights.Distance * distance),
		Coverage:   jsRound(matchWeights.Coverage * coverage),
		Confidence: jsRound(matchWeights.Confidence * clamp01(m.Confidence)),
	}
	if m.CategoryMatch {
		p.Category = matchWeights.Category
	}
	return p.Category + p.Distance + p.Coverage + p.Confidence, p
}

// ponytail: keyword guess only for identity items saved before they carried a category.
var categoryHints = []struct {
	re  *regexp.Regexp
	cat string
}{
	{regexp.MustCompile(`(?i)truk|angkut|kirim|trip|logistik|pengiriman|gudang`), "logistics"},
	{regexp.MustCompile(`(?i)kemasan|pouch|karton|box|karung`), "packaging"},
	{regexp.MustCompile(`(?i)kopi|bean|pupuk|tani|panen`), "agri"},
	{regexp.MustCompile(`(?i)developer|backend|frontend|software|aplikasi`), "it"},
	{regexp.MustCompile(`(?i)surya|listrik|energi|jelantah`), "energy"},
	{regexp.MustCompile(`(?i)makan|bubuk|gula|katering|beras`), "food"},
}

// itemCategory is the category an identity item is matched in: its own when set, else a keyword guess ("" = unmatched).
func itemCategory(name, detail string, category *string) string {
	if category != nil && *category != "" {
		return *category
	}
	for _, h := range categoryHints {
		if h.re.MatchString(name + " " + detail) {
			return h.cat
		}
	}
	return ""
}

// estimateMatchValue is the value of the part of the gap this user can fill: min(have, gap) x unit price.
func estimateMatchValue(have, gap float64, unitPriceIdr int64) int64 {
	return int64(jsRound(math.Max(0, math.Min(have, gap)) * float64(unitPriceIdr)))
}

// ── Places ───────────────────────────────────────────────────────

// ponytail: a fixed gazetteer of the regions the product covers (province -> capital) and the cities that show up in
// listings. Locations it does not know count as far away (unknownDistanceKm). Replace with geocoded coordinates on
// listings and identities when the product leaves these regions.
type place struct {
	name     string // lower case, matched as a substring of a free-text location
	lat, lon float64
	region   string // province, as opportunities.region / markets.region
}

var places = []place{ // cities first: "Bandung, Jawa Barat" resolves to Bandung, not the province capital
	{"bandung", -6.9175, 107.6191, "Jawa Barat"}, {"garut", -7.2279, 107.9087, "Jawa Barat"},
	{"bogor", -6.5950, 106.8166, "Jawa Barat"}, {"bekasi", -6.2383, 106.9756, "Jawa Barat"},
	{"depok", -6.4025, 106.7942, "Jawa Barat"}, {"karawang", -6.3227, 107.3376, "Jawa Barat"},
	{"cirebon", -6.7320, 108.5523, "Jawa Barat"}, {"tasikmalaya", -7.3274, 108.2207, "Jawa Barat"},
	{"tangerang", -6.1783, 106.6300, "Banten"}, {"serang", -6.1200, 106.1503, "Banten"},
	{"jakarta", -6.2088, 106.8456, "DKI Jakarta"}, {"semarang", -6.9667, 110.4167, "Jawa Tengah"},
	{"surakarta", -7.5755, 110.8243, "Jawa Tengah"}, {"solo", -7.5755, 110.8243, "Jawa Tengah"},
	{"magelang", -7.4797, 110.2177, "Jawa Tengah"}, {"yogyakarta", -7.7956, 110.3695, "DI Yogyakarta"},
	{"jogja", -7.7956, 110.3695, "DI Yogyakarta"}, {"sleman", -7.7160, 110.3550, "DI Yogyakarta"},
	{"surabaya", -7.2575, 112.7521, "Jawa Timur"}, {"malang", -7.9666, 112.6326, "Jawa Timur"},
	{"sidoarjo", -7.4478, 112.7183, "Jawa Timur"}, {"denpasar", -8.6705, 115.2126, "Bali"},
	{"medan", 3.5952, 98.6722, "Sumatera Utara"}, {"toba", 2.6845, 98.8756, "Sumatera Utara"},
	// provinces (capital coordinates)
	{"dki jakarta", -6.2088, 106.8456, "DKI Jakarta"}, {"jawa barat", -6.9175, 107.6191, "Jawa Barat"},
	{"jawa tengah", -6.9667, 110.4167, "Jawa Tengah"}, {"di yogyakarta", -7.7956, 110.3695, "DI Yogyakarta"},
	{"jawa timur", -7.2575, 112.7521, "Jawa Timur"}, {"bali", -8.6705, 115.2126, "Bali"},
	{"sumatera utara", 3.5952, 98.6722, "Sumatera Utara"}, {"banten", -6.1200, 106.1503, "Banten"},
}

const unknownDistanceKm = 500

func resolvePlace(location string) (place, bool) {
	l := strings.ToLower(location)
	for _, p := range places {
		if strings.Contains(l, p.name) {
			return p, true
		}
	}
	return place{}, false
}

// regionOf maps a free-text location to its province; unknown locations keep their own trimmed text.
func regionOf(location string) string {
	if p, ok := resolvePlace(location); ok {
		return p.region
	}
	return strings.TrimSpace(location)
}

// distanceKm between two free-text locations (great-circle, whole km).
func distanceKm(a, b string) float64 {
	pa, okA := resolvePlace(a)
	pb, okB := resolvePlace(b)
	if !okA || !okB {
		if strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) && strings.TrimSpace(a) != "" {
			return 0
		}
		return unknownDistanceKm
	}
	rad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat, dLon := rad(pb.lat-pa.lat), rad(pb.lon-pa.lon)
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(rad(pa.lat))*math.Cos(rad(pb.lat))*math.Sin(dLon/2)*math.Sin(dLon/2)
	return math.Round(6371 * 2 * math.Asin(math.Sqrt(h)))
}

// ── Matches ──────────────────────────────────────────────────────

type have struct {
	source, id, label, detail, category, location string
	listing                                       bool
	qty                                           float64
	unit                                          string
	price                                         int64
}

type matchOpp struct {
	id, title, region, category, unit, contribution string
	demand, supply, confidence                      float64
	potential                                       int64
	participants                                    int
}

type storedMatch struct {
	state          string
	conversationID *string
}

type computedMatch struct {
	api.Match
	have have
	opp  matchOpp
}

var errMatchNotFound = &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Match tidak ditemukan"}

// matchesFor computes the user's current matches, best first (spec: GET /me/matches).
func matchesFor(ctx context.Context, q dbtx, userID string) ([]computedMatch, error) {
	v, err := loadViewer(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	var haves []have
	rows, err := q.Query(ctx, `
		SELECT l.id::text, l.item, l.category_id, l.quantity::float8, l.unit, l.price_idr, l.location
		FROM listings l JOIN parties p ON p.id = l.owner_party_id
		WHERE p.user_id = $1 AND l.kind = 'supply' AND l.status IN ('available','in_market','reserved')
		ORDER BY l.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		h := have{source: "supply", listing: true}
		if err := rows.Scan(&h.id, &h.label, &h.category, &h.qty, &h.unit, &h.price, &h.location); err != nil {
			rows.Close()
			return nil, err
		}
		h.detail = fmt.Sprintf("%s %s · Rp %s/%s", idNumber(h.qty), h.unit, idNumber(float64(h.price)), h.unit)
		haves = append(haves, h)
	}
	rows.Close()
	rows, err = q.Query(ctx, `SELECT id::text, name, detail, category_id FROM capacity_items WHERE user_id = $1 ORDER BY position`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		h := have{source: "identity", location: v.Home}
		var cat *string
		if err := rows.Scan(&h.id, &h.label, &h.detail, &cat); err != nil {
			rows.Close()
			return nil, err
		}
		h.category = itemCategory(h.label, h.detail, cat)
		haves = append(haves, h)
	}
	rows.Close()

	stored := map[string]storedMatch{}
	rows, err = q.Query(ctx, `
		SELECT coalesce(have_listing_id, have_capacity_item_id)::text || '--' || opportunity_id::text, state, conversation_id::text
		FROM matches WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var m storedMatch
		if err := rows.Scan(&id, &m.state, &m.conversationID); err != nil {
			rows.Close()
			return nil, err
		}
		stored[id] = m
	}
	rows.Close()

	byCat := map[string][]matchOpp{}
	rows, err = q.Query(ctx, `
		SELECT id::text, title, region, category_id, unit, required_contribution, demand_value::float8, supply_value::float8,
		       confidence::float8, potential_value_idr, participant_count
		FROM opportunities
		WHERE (status NOT IN ('dismissed','closed') AND demand_value > supply_value)
		   OR id IN (SELECT opportunity_id FROM matches WHERE user_id = $1)`, userID)
	if err != nil {
		return nil, err
	}
	open := map[string]bool{}
	for rows.Next() {
		var o matchOpp
		if err := rows.Scan(&o.id, &o.title, &o.region, &o.category, &o.unit, &o.contribution, &o.demand, &o.supply,
			&o.confidence, &o.potential, &o.participants); err != nil {
			rows.Close()
			return nil, err
		}
		open[o.id] = o.demand > o.supply
		byCat[o.category] = append(byCat[o.category], o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []computedMatch
	for _, h := range haves {
		if h.category == "" {
			continue
		}
		var cands []computedMatch
		for _, o := range byCat[h.category] {
			id := h.id + "--" + o.id
			st, acted := stored[id]
			if !open[o.id] && !acted {
				continue
			}
			gap := o.demand - o.supply
			sameUnit := h.listing && strings.EqualFold(h.unit, o.unit)
			in := matchInput{CategoryMatch: true, DistanceKm: distanceKm(h.location, o.region), RadiusKm: v.RadiusKm, Confidence: o.confidence}
			value := int64(jsRound(float64(o.potential) / math.Max(1, float64(o.participants))))
			if sameUnit {
				c := h.qty / gap
				in.Coverage = &c
				value = estimateMatchValue(h.qty, gap, h.price)
			}
			m := computedMatch{have: h, opp: o}
			m.Id, m.DistanceKm, m.EstimatedValueIdr, m.State = id, in.DistanceKm, int(value), api.MatchStateNew
			m.Score, m.Parts = scoreMatch(in)
			if acted {
				m.State, m.ConversationId = api.MatchState(st.state), st.conversationID
			}
			m.Have.Source, m.Have.Id, m.Have.Label, m.Have.Detail = api.MatchHaveSource(h.source), h.id, h.label, h.detail
			m.Need.OpportunityId, m.Need.Title, m.Need.Region, m.Need.CategoryId = o.id, o.title, o.region, api.CategoryId(o.category)
			m.Need.Gap, m.Need.Detail = api.Quantity{Value: gap, Unit: o.unit}, o.contribution
			cands = append(cands, m)
		}
		slices.SortStableFunc(cands, func(a, b computedMatch) int { return cmp.Compare(b.Score, a.Score) })
		for i, c := range cands {
			// Two best per have; matches the user acted on are always kept.
			if i < 2 || c.State != api.MatchStateNew {
				out = append(out, c)
			}
		}
	}
	slices.SortStableFunc(out, func(a, b computedMatch) int {
		return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.DistanceKm, b.DistanceKm))
	})
	// Keep the inbox varied: at most two new matches per opportunity.
	perOpp := map[string]int{}
	kept := out[:0]
	for _, m := range out {
		perOpp[m.opp.id]++
		if perOpp[m.opp.id] <= 2 || m.State != api.MatchStateNew {
			kept = append(kept, m)
		}
	}
	return kept, nil
}

func (s *Server) ListMyMatches(ctx context.Context, _ api.ListMyMatchesRequestObject) (api.ListMyMatchesResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	ms, err := matchesFor(ctx, s.DB.Primary(), sess.UserID)
	if err != nil {
		return nil, err
	}
	out := api.ListMyMatches200JSONResponse{}
	for _, m := range ms {
		out = append(out, m.Match)
	}
	return out, nil
}

var matchNextState = map[api.AuthPublicMatchActionRequestAction]api.MatchState{
	"connect": api.MatchStateConnected, "save": api.MatchStateSaved, "dismiss": api.MatchStateDismissed, "reset": api.MatchStateNew,
}

func (s *Server) ActOnMatch(ctx context.Context, req api.ActOnMatchRequestObject) (api.ActOnMatchResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	var out api.Match
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		// Serialise per user: two connects at once would open two conversations.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, sess.UserID); err != nil {
			return err
		}
		ms, err := matchesFor(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(ms, func(m computedMatch) bool { return m.Id == req.Id })
		if i < 0 {
			return errMatchNotFound
		}
		m := ms[i]
		next, ok := matchNextState[req.Body.Action]
		if !ok {
			return validationFields(map[string]string{"action": "Aksi tidak dikenal"})
		}
		var reason *string
		if req.Body.Reason != nil && strings.TrimSpace(*req.Body.Reason) != "" {
			reason = ptr(strings.TrimSpace(*req.Body.Reason))
		}
		var listingID, itemID *string
		if m.have.listing {
			listingID = &m.have.id
		} else {
			itemID = &m.have.id
		}
		var rowID string
		var conv *string
		if err := tx.QueryRow(ctx, `
			INSERT INTO matches (user_id, opportunity_id, have_listing_id, have_capacity_item_id, state, reason, score,
			                     part_category, part_distance, part_coverage, part_confidence, distance_km, estimated_value_idr)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (user_id, opportunity_id, have_listing_id, have_capacity_item_id) DO UPDATE
			SET state = EXCLUDED.state, reason = EXCLUDED.reason, score = EXCLUDED.score, part_category = EXCLUDED.part_category,
			    part_distance = EXCLUDED.part_distance, part_coverage = EXCLUDED.part_coverage, part_confidence = EXCLUDED.part_confidence,
			    distance_km = EXCLUDED.distance_km, estimated_value_idr = EXCLUDED.estimated_value_idr
			RETURNING id::text, conversation_id::text`,
			sess.UserID, m.opp.id, listingID, itemID, string(next), reason, m.Score, m.Parts.Category, m.Parts.Distance,
			m.Parts.Coverage, m.Parts.Confidence, m.DistanceKm, m.EstimatedValueIdr).Scan(&rowID, &conv); err != nil {
			return err
		}
		if req.Body.Action == "connect" {
			// Connecting follows the opportunity (a joined user stays joined: joined wins over following).
			if _, err := tx.Exec(ctx, `INSERT INTO opportunity_follows (user_id, opportunity_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
				sess.UserID, m.opp.id); err != nil {
				return err
			}
			if conv == nil {
				id, err := openMatchConversation(ctx, tx, sess, rowID, m)
				if err != nil {
					return err
				}
				conv = &id
				if _, err := tx.Exec(ctx, `UPDATE matches SET conversation_id = $2 WHERE id = $1`, rowID, id); err != nil {
					return err
				}
			}
		}
		out = m.Match
		out.State, out.ConversationId = next, conv
		return nil
	})
	if err != nil {
		return nil, err
	}
	return api.ActOnMatch200JSONResponse(out), nil
}

// openMatchConversation opens the "Match: <title>" conversation with whoever coordinates the opportunity: the maker of
// its first market (its operators, the maker org's market makers, or the maker user), else an external "Tim <code>"
// placeholder (answered by the demo bots in counterparties.go when they run), and posts the opening message.
// The message is inserted directly rather than through postMessage: that would also send every maker a generic
// "Pesan dari ..." notification next to the spec's "<name> ingin terhubung" one. Nobody is subscribed to a brand-new
// conversation, so no message.created frame is needed.
func openMatchConversation(ctx context.Context, tx pgx.Tx, sess *session, matchRowID string, m computedMatch) (string, error) {
	me, err := userParty(ctx, tx, sess.UserID)
	if err != nil {
		return "", err
	}
	var makerParty, marketID *string
	var code string
	if err := tx.QueryRow(ctx, `
		SELECT o.code, (SELECT mk.maker_party_id::text FROM markets mk WHERE mk.opportunity_id = o.id ORDER BY mk.created_at LIMIT 1),
		       (SELECT mk.id::text FROM markets mk WHERE mk.opportunity_id = o.id ORDER BY mk.created_at LIMIT 1)
		FROM opportunities o WHERE o.id = $1`, m.opp.id).Scan(&code, &makerParty, &marketID); err != nil {
		return "", err
	}
	var makerUsers []string
	if makerParty == nil {
		id, err := externalParty(ctx, tx, "Tim "+code, "business", true)
		if err != nil {
			return "", err
		}
		makerParty = &id
	} else {
		rows, err := tx.Query(ctx, `
			SELECT user_id::text FROM market_operators WHERE market_id = $1
			UNION
			SELECT om.user_id::text FROM parties p JOIN org_members om ON om.org_id = p.org_id AND om.status = 'active'
			JOIN user_capabilities c ON c.user_id = om.user_id AND c.capability = 'market_maker'
			WHERE p.id = $2
			UNION
			SELECT user_id::text FROM parties WHERE id = $2 AND user_id IS NOT NULL`, marketID, *makerParty)
		if err != nil {
			return "", err
		}
		if makerUsers, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return "", err
		}
		makerUsers = slices.DeleteFunc(makerUsers, func(u string) bool { return u == sess.UserID })
		slices.Sort(makerUsers)
	}
	parties := []convParty{{PartyID: me, UserID: &sess.UserID}}
	if len(makerUsers) == 0 {
		parties = append(parties, convParty{PartyID: *makerParty})
	}
	for _, u := range makerUsers {
		parties = append(parties, convParty{PartyID: *makerParty, UserID: &u})
	}
	conv, err := createConversation(ctx, tx, "Match: "+m.opp.title, &sess.UserID, "match", matchRowID, parties)
	if err != nil {
		return "", err
	}
	body := fmt.Sprintf("Halo, saya punya %s (%s) dan tertarik dengan %s. Bisa diskusi kebutuhannya?", m.have.label, m.have.detail, m.opp.title)
	var seq int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO messages (conversation_id, kind, author_party_id, author_user_id, body) VALUES ($1, 'text', $2, $3, $4) RETURNING seq`,
		conv, me, sess.UserID, body).Scan(&seq); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE conversation_participants SET last_read_seq = $3, last_read_at = now() WHERE conversation_id = $1 AND user_id = $2`,
		conv, sess.UserID, seq); err != nil {
		return "", err
	}
	for _, u := range makerUsers {
		if err := notify(ctx, tx, u, notification{Type: "opportunity_detected", Title: sess.Name + " ingin terhubung", Body: m.opp.title,
			Href: "/app/messages/" + conv}); err != nil {
			return "", err
		}
	}
	return conv, nil
}
