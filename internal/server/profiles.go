package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

func profileActivitySort(a []api.ProfileActivity) []api.ProfileActivity {
	slices.SortStableFunc(a, func(x, y api.ProfileActivity) int { return y.At.Compare(x.At) })
	return a[:min(len(a), 6)]
}

func (s *Server) GetPersonProfile(ctx context.Context, req api.GetPersonProfileRequestObject) (api.GetPersonProfileResponseObject, error) {
	q := s.DB.Reader()
	var p api.PublicProfile
	var userID string
	var joined time.Time
	err := q.QueryRow(ctx, `
		SELECT u.id::text, u.name, u.username, coalesce(u.location, ''), coalesce(i.bio, ''), u.created_at, u.status
		FROM users u LEFT JOIN identities i ON i.user_id = u.id WHERE u.username = $1`, req.Username).
		Scan(&userID, &p.Name, &p.Username, &p.Location, &p.Bio, &joined, &p.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Profil tidak ditemukan"}
	}
	if err != nil {
		return nil, err
	}
	p.JoinedAt = &joined
	v, err := loadVerification(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	p.Verification.Email, p.Verification.Identity = v.Email, api.PublicProfileVerificationIdentity(v.Identity)
	parties, err := userPartyIDs(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	if p.Reputation, err = partyReputation(ctx, q, parties); err != nil {
		return nil, err
	}

	p.Activity = []api.ProfileActivity{}
	rows, err := q.Query(ctx, `
		SELECT l.id::text, l.item, l.category_id, l.quantity::float8, l.unit, l.price_idr, l.created_at
		FROM listings l JOIN parties pt ON pt.id = l.owner_party_id
		WHERE pt.user_id = $1 AND l.kind = 'supply' AND l.status IN ('available','in_market') ORDER BY l.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var it struct {
			CategoryId api.CategoryId `json:"categoryId"`
			Id         string         `json:"id"`
			Item       string         `json:"item"`
			PriceIdr   int            `json:"priceIdr"`
			Quantity   api.Quantity   `json:"quantity"`
		}
		var at time.Time
		if err := rows.Scan(&it.Id, &it.Item, &it.CategoryId, &it.Quantity.Value, &it.Quantity.Unit, &it.PriceIdr, &at); err != nil {
			rows.Close()
			return nil, err
		}
		p.Supply = append(p.Supply, it)
		p.Activity = append(p.Activity, api.ProfileActivity{Id: it.Id, Title: "Menawarkan " + it.Item, At: at})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if p.Supply == nil {
		_ = widen([]any{}, &p.Supply)
	}

	if err := collect(ctx, q, &p.Markets, `
		SELECT m.id::text, m.code, m.name, m.category_id, m.region FROM watchlist w JOIN markets m ON m.id = w.market_id
		WHERE w.user_id = $1 AND w.joined_at IS NOT NULL AND m.status <> 'draft' ORDER BY w.joined_at DESC`, userID,
		func(r pgx.Rows, m *struct {
			CategoryId api.CategoryId `json:"categoryId"`
			Code       string         `json:"code"`
			Id         string         `json:"id"`
			Name       string         `json:"name"`
			Region     string         `json:"region"`
		}) error {
			return r.Scan(&m.Id, &m.Code, &m.Name, &m.CategoryId, &m.Region)
		}); err != nil {
		return nil, err
	}
	if err := collect(ctx, q, &p.Orgs, `
		SELECT o.name, o.slug::text FROM org_members om JOIN orgs o ON o.id = om.org_id
		WHERE om.user_id = $1 AND om.status = 'active' ORDER BY om.joined_at, o.name`, userID,
		func(r pgx.Rows, o *struct {
			Name string `json:"name"`
			Slug string `json:"slug"`
		}) error {
			return r.Scan(&o.Name, &o.Slug)
		}); err != nil {
		return nil, err
	}
	var done []api.ProfileActivity
	if err := collect(ctx, q, &done, `
		SELECT t.id::text, 'Menyelesaikan transaksi ' || t.title, t.updated_at FROM trades t
		WHERE t.status = 'completed' AND (t.buyer_party_id = ANY($1::uuid[]) OR t.supplier_party_id = ANY($1::uuid[]))
		ORDER BY t.updated_at DESC LIMIT 6`, parties,
		func(r pgx.Rows, a *api.ProfileActivity) error { return r.Scan(&a.Id, &a.Title, &a.At) }); err != nil {
		return nil, err
	}
	p.Activity = profileActivitySort(append(p.Activity, done...))
	return api.GetPersonProfile200JSONResponse(p), nil
}

func collect[T any](ctx context.Context, q dbtx, dst *[]T, sql string, arg any, scan func(pgx.Rows, *T) error) error {
	rows, err := q.Query(ctx, sql, arg)
	if err != nil {
		return err
	}
	defer rows.Close()
	if *dst == nil {
		*dst = []T{}
	}
	for rows.Next() {
		var v T
		if err := scan(rows, &v); err != nil {
			return err
		}
		*dst = append(*dst, v)
	}
	return rows.Err()
}

var errBusinessNotFound = &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Profil bisnis tidak ditemukan"}

func (s *Server) GetBusinessProfile(ctx context.Context, req api.GetBusinessProfileRequestObject) (api.GetBusinessProfileResponseObject, error) {
	q := s.DB.Reader()
	b := api.BusinessProfile{Slug: req.Slug, Documents: []string{}}
	var party *string
	var location string
	err := q.QueryRow(ctx, `
		SELECT o.name, coalesce(pr.region, ''), coalesce(pr.location, ''), coalesce(pr.description, ''),
		       coalesce(pr.verification = 'verified', false), (SELECT id::text FROM parties WHERE org_id = o.id)
		FROM orgs o LEFT JOIN org_profiles pr ON pr.org_id = o.id WHERE o.slug = $1`, req.Slug).
		Scan(&b.Name, &b.Region, &location, &b.Description, &b.Verified, &party)
	if errors.Is(err, pgx.ErrNoRows) {

		rows, err := q.Query(ctx, `
			SELECT DISTINCT p.id::text, p.name, p.verified FROM parties p JOIN markets m ON m.maker_party_id = p.id
			WHERE p.kind = 'external' AND m.status <> 'draft'`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, name string
			var verified bool
			if err := rows.Scan(&id, &name, &verified); err != nil {
				rows.Close()
				return nil, err
			}
			if party == nil && slugify(name) == req.Slug {
				party, b.Name, b.Verified = &id, name, verified
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if party == nil {
			return nil, errBusinessNotFound
		}
	} else if err != nil {
		return nil, err
	}
	if b.Region == "" && location != "" {
		parts := strings.Split(location, ",")
		b.Region = strings.TrimSpace(parts[len(parts)-1])
	}
	if b.Verified {
		b.Documents = []string{"NIB", "NPWP", "Akta pendirian"}
	}
	b.Markets, b.Auctions, b.Activity = []api.Market{}, []api.Auction{}, []api.ProfileActivity{}
	if party != nil {
		if b.Markets, err = loadMarkets(ctx, q, `m.maker_party_id = $1 AND m.status <> 'draft' ORDER BY m.volume_30d_idr DESC, m.id`, *party); err != nil {
			return nil, err
		}
		ids := make([]string, len(b.Markets))
		for i, m := range b.Markets {
			ids[i] = m.Id
		}
		if b.Auctions, err = loadAuctions(ctx, q, `a.market_id = ANY($1::uuid[]) AND a.status <> 'cancelled' ORDER BY a.starts_at DESC, a.id`, ids); err != nil {
			return nil, err
		}
		if b.Reputation, err = partyReputation(ctx, q, []string{*party}); err != nil {
			return nil, err
		}
		var created []time.Time
		if err := collect(ctx, q, &created, `SELECT created_at FROM markets WHERE maker_party_id = $1 AND status <> 'draft' ORDER BY volume_30d_idr DESC, id`,
			*party, func(r pgx.Rows, t *time.Time) error { return r.Scan(t) }); err != nil {
			return nil, err
		}
		for i, m := range b.Markets {
			if i < len(created) {
				b.Activity = append(b.Activity, api.ProfileActivity{Id: m.Id, Title: "Mengoperasikan market " + m.Name, At: created[i]})
			}
		}
		for _, a := range b.Auctions {
			if !a.StartsAt.After(time.Now()) {
				b.Activity = append(b.Activity, api.ProfileActivity{Id: a.Id, Title: "Membuka auction " + a.Title, At: a.StartsAt})
			}
		}
	}
	b.Activity = profileActivitySort(b.Activity)
	if b.Region == "" {
		b.Region = "Indonesia"
		if len(b.Markets) > 0 {
			b.Region = b.Markets[0].Region
		}
	}
	if b.Description == "" {
		b.Description = b.Name + " adalah bisnis di jaringan Ecopurnity."
		if len(b.Markets) > 0 {
			n := 0
			for _, m := range b.Markets {
				n += m.Buyers + m.Suppliers
			}
			b.Description = fmt.Sprintf("%s mengoperasikan %d market di Ecopurnity dan mempertemukan %d peserta.", b.Name, len(b.Markets), n)
		}
	}
	return api.GetBusinessProfile200JSONResponse(b), nil
}
