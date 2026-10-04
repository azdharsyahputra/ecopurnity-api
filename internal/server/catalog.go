package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

func (s *Server) ListPublicListings(ctx context.Context, req api.ListPublicListingsRequestObject) (api.ListPublicListingsResponseObject, error) {
	p := req.Params
	page, size := pageParams(p.Page, p.PageSize)
	where := []string{
		`l.status IN ('available','in_market','open','matched')`,

		`(pt.user_id IS NULL OR u.status = 'active')`,
	}
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	if p.Q != nil && strings.TrimSpace(*p.Q) != "" {
		like := "%" + likeEscape(strings.TrimSpace(*p.Q)) + "%"
		ph := arg(like)
		where = append(where, fmt.Sprintf(`(l.item ILIKE %[1]s OR l.code ILIKE %[1]s OR pt.name ILIKE %[1]s)`, ph))
	}
	if p.Category != nil {
		where = append(where, "l.category_id = "+arg(string(*p.Category)))
	}
	if p.Region != nil && strings.TrimSpace(*p.Region) != "" {
		where = append(where, "l.location ILIKE "+arg("%"+likeEscape(strings.TrimSpace(*p.Region))+"%"))
	}
	if p.Kind != nil {
		where = append(where, "l.kind = "+arg(string(*p.Kind)))
	}
	if p.Market != nil {
		where = append(where, "l.market_id::text = "+arg(*p.Market))
	}
	limit, offset := arg(size), arg((page-1)*size)
	rows, err := s.DB.Reader().Query(ctx, `
		SELECT l.id, l.code, l.kind, l.item, l.category_id, l.quantity, l.unit, l.location, l.spec, l.delivery, l.market_id, l.created_at,
		       CASE WHEN l.kind = 'supply' THEN l.price_idr ELSE round(l.budget_idr / greatest(l.quantity, 1))::bigint END,
		       pt.name, pt.user_id, u.username,
		       CASE pt.kind WHEN 'user' THEN i.identity_verified_at IS NOT NULL
		                    WHEN 'org' THEN coalesce(op.verification = 'verified', false)
		                    ELSE pt.verified END,
		       count(*) OVER (), `+attachmentsJSON+`
		FROM listings l
		JOIN parties pt ON pt.id = l.owner_party_id
		LEFT JOIN users u ON u.id = pt.user_id
		LEFT JOIN identities i ON i.user_id = pt.user_id
		LEFT JOIN org_profiles op ON op.org_id = pt.org_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY l.created_at DESC, l.id
		LIMIT `+limit+` OFFSET `+offset, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := api.ListPublicListings200JSONResponse{Data: []api.PublicListing{}, Meta: api.PageMeta{Page: page, PageSize: size}}
	for rows.Next() {
		var l api.PublicListing
		var qty float64
		var unit string
		var unitPrice int64
		var userID *string
		var total int64
		var atts []attachmentRow
		if err := rows.Scan(&l.Id, &l.Code, &l.Kind, &l.Item, &l.CategoryId, &qty, &unit, &l.Location, &l.Spec, &l.Delivery, &l.MarketId,
			&l.CreatedAt, &unitPrice, &l.Owner.Name, &userID, &l.Owner.Username, &l.Owner.Verified, &total, &atts); err != nil {
			return nil, err
		}

		if l.Attachments, err = s.signAttachments(ctx, atts); err != nil {
			return nil, err
		}
		l.Quantity = api.Quantity{Value: qty, Unit: unit}
		l.UnitPriceIdr = int(unitPrice)
		l.Owner.UserId = userID
		out.Data = append(out.Data, l)
		out.Meta.Total = int(total)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 && page > 1 {

		if err := s.DB.Reader().QueryRow(ctx, `
			SELECT count(*) FROM listings l JOIN parties pt ON pt.id = l.owner_party_id LEFT JOIN users u ON u.id = pt.user_id
			WHERE `+strings.Join(where, " AND "), args[:len(args)-2]...).Scan(&out.Meta.Total); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type nullPriceSuggestion struct{}

func (nullPriceSuggestion) VisitGetPriceSuggestionResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte("null\n"))
	return err
}

func (s *Server) GetPriceSuggestion(ctx context.Context, req api.GetPriceSuggestionRequestObject) (api.GetPriceSuggestionResponseObject, error) {
	p := req.Params
	unit := strings.ToLower(strings.TrimSpace(p.Unit))
	category := string(p.Category)
	exclude := ""
	if p.Exclude != nil {
		exclude = *p.Exclude
	}
	words := []string{}
	if p.Item != nil {
		words = itemWords(*p.Item)
	}
	q := s.DB.Reader()

	type market struct{ ID, Name string }
	var markets []market
	rows, err := q.Query(ctx, `SELECT id::text, name FROM markets WHERE category_id = $1 AND lower(unit) = $2 AND status <> 'closed'`, category, unit)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m market
		if err := rows.Scan(&m.ID, &m.Name); err != nil {
			rows.Close()
			return nil, err
		}
		markets = append(markets, m)
	}
	rows.Close()

	type listing struct {
		Item  string
		Price float64
	}
	var listings []listing
	rows, err = q.Query(ctx, `
		SELECT l.item, CASE WHEN l.kind = 'supply' THEN l.price_idr ELSE l.budget_idr / greatest(l.quantity, 1) END::float8
		FROM listings l JOIN parties pt ON pt.id = l.owner_party_id LEFT JOIN users u ON u.id = pt.user_id
		WHERE l.category_id = $1 AND lower(l.unit) = $2 AND l.id::text <> $3
		  AND l.status IN ('available','in_market','open','matched') AND (pt.user_id IS NULL OR u.status = 'active')`, category, unit, exclude)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l listing
		if err := rows.Scan(&l.Item, &l.Price); err != nil {
			rows.Close()
			return nil, err
		}
		listings = append(listings, l)
	}
	rows.Close()

	pick := func(narrow bool) ([]market, *priceSuggestion, error) {
		var ms []market
		var ids []string
		for _, m := range markets {
			if !narrow || sharesWord(m.Name, words) {
				ms = append(ms, m)
				ids = append(ids, m.ID)
			}
		}
		var samples []float64
		if s.Analytics != nil && len(ids) > 0 {
			ctxCH, cancel := context.WithTimeout(ctx, 2*time.Second)
			medians, err := s.Analytics.WeeklyMedians(ctxCH, ids, 8)
			cancel()
			if err != nil {

				if s.Log != nil {
					s.Log.Warn("price suggestion: clickhouse", "err", err)
				}
			} else {
				samples = append(samples, medians...)
			}
		}
		for _, l := range listings {
			if !narrow || sharesWord(l.Item, words) {
				samples = append(samples, l.Price)
			}
		}
		return ms, suggestPrice(samples), nil
	}
	var used []market
	var sug *priceSuggestion
	if len(words) > 0 {
		if used, sug, err = pick(true); err != nil {
			return nil, err
		}
	}
	if sug == nil {
		if used, sug, err = pick(false); err != nil {
			return nil, err
		}
	}
	if sug == nil {
		return nullPriceSuggestion{}, nil
	}
	out := api.GetPriceSuggestion200JSONResponse{MedianIdr: sug.Median, LowIdr: sug.Low, HighIdr: sug.High, Sample: sug.Sample, Unit: unit}
	out.Markets = make([]struct {
		Id   string `json:"id"`
		Name string `json:"name"`
	}, len(used))
	for i, m := range used {
		out.Markets[i].Id, out.Markets[i].Name = m.ID, m.Name
	}
	return out, nil
}

func pageParams(page, size *int) (int, int) {
	p, n := 1, 12
	if page != nil && *page > 1 {
		p = *page
	}
	if size != nil && *size >= 1 {
		n = min(*size, 50)
	}
	return p, n
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
