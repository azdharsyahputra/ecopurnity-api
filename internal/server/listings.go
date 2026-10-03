package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Personal listings (supply + demand), spec tag Personal.

type listingRow struct {
	ID, Code, Kind, Status, Item, Category, Unit, Location, Spec, Delivery string
	Quantity                                                               float64
	MarketID, AuctionID                                                    *string
	Price, Budget                                                          *int64
	AvailableFrom, ExpiresAt, Deadline                                     *time.Time
	CreatedAt, UpdatedAt                                                   time.Time
	Attachments                                                            []string
}

const listingSelect = `
	SELECT l.id, l.code, l.kind, l.status, l.item, l.category_id, l.quantity, l.unit, l.location, l.spec, l.delivery,
	       l.market_id, l.auction_id, l.price_idr, l.budget_idr, l.available_from, l.expires_at, l.deadline, l.created_at, l.updated_at,
	       coalesce((SELECT array_agg(a.file_name ORDER BY a.created_at) FROM listing_attachments a WHERE a.listing_id = l.id), '{}')
	FROM listings l JOIN parties p ON p.id = l.owner_party_id`

func scanListing(row pgx.Row) (listingRow, error) {
	var r listingRow
	err := row.Scan(&r.ID, &r.Code, &r.Kind, &r.Status, &r.Item, &r.Category, &r.Quantity, &r.Unit, &r.Location, &r.Spec, &r.Delivery,
		&r.MarketID, &r.AuctionID, &r.Price, &r.Budget, &r.AvailableFrom, &r.ExpiresAt, &r.Deadline, &r.CreatedAt, &r.UpdatedAt, &r.Attachments)
	return r, err
}

// fill sets the shared and kind-specific fields on any union that accepts a SupplyListing or DemandListing.
func (r listingRow) fill(fromSupply func(api.SupplyListing) error, fromDemand func(api.DemandListing) error) error {
	q := api.Quantity{Value: r.Quantity, Unit: r.Unit}
	if r.Kind == "supply" {
		return fromSupply(api.SupplyListing{
			Id: r.ID, Code: r.Code, Kind: "supply", Status: api.SupplyStatus(r.Status), Item: r.Item, CategoryId: api.CategoryId(r.Category),
			Quantity: q, Location: r.Location, Spec: r.Spec, Delivery: api.DeliveryMode(r.Delivery), Attachments: r.Attachments,
			MarketId: r.MarketID, PriceIdr: int(deref64(r.Price)), AvailableFrom: derefTime(r.AvailableFrom), ExpiresAt: r.ExpiresAt,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return fromDemand(api.DemandListing{
		Id: r.ID, Code: r.Code, Kind: "demand", Status: api.DemandStatus(r.Status), Item: r.Item, CategoryId: api.CategoryId(r.Category),
		Quantity: q, Location: r.Location, Spec: r.Spec, Delivery: api.DeliveryMode(r.Delivery), Attachments: r.Attachments,
		MarketId: r.MarketID, AuctionId: r.AuctionID, BudgetIdr: int(deref64(r.Budget)), Deadline: derefTime(r.Deadline),
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	})
}

func (r listingRow) api() (api.Listing, error) {
	var l api.Listing
	err := r.fill(l.FromSupplyListing, l.FromDemandListing)
	return l, err
}

func (r listingRow) final() bool {
	switch r.Status {
	case "sold", "expired", "fulfilled", "cancelled":
		return true
	}
	return false
}

// myListing loads one of the user's listings (optionally locked for update), or 404.
func myListing(ctx context.Context, q dbtx, userID, id string, forUpdate bool) (listingRow, error) {
	sql := listingSelect + ` WHERE l.id::text = $1 AND p.user_id = $2`
	if forUpdate {
		sql += ` FOR UPDATE OF l`
	}
	r, err := scanListing(q.QueryRow(ctx, sql, id, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Listing tidak ditemukan"}
	}
	return r, err
}

func (s *Server) ListMyListings(ctx context.Context, req api.ListMyListingsRequestObject) (api.ListMyListingsResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	where, args := []string{"p.user_id = $1"}, []any{sess.UserID}
	if req.Params.Kind != nil {
		args = append(args, string(*req.Params.Kind))
		where = append(where, fmt.Sprintf("l.kind = $%d", len(args)))
	}
	if req.Params.Status != nil {
		args = append(args, *req.Params.Status)
		where = append(where, fmt.Sprintf("l.status = $%d", len(args)))
	}
	rows, err := s.DB.Reader().Query(ctx, listingSelect+" WHERE "+strings.Join(where, " AND ")+" ORDER BY l.created_at DESC", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := api.ListMyListings200JSONResponse{}
	for rows.Next() {
		r, err := scanListing(rows)
		if err != nil {
			return nil, err
		}
		l, err := r.api()
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// listingDraft is the editable part of a listing, validated the same way on create and update.
type listingDraft struct {
	Kind, Item, Category, Unit, Location, Spec, Delivery string
	Quantity                                             float64
	Price, Budget                                        *int64
	AvailableFrom, ExpiresAt, Deadline                   *time.Time
	Attachments                                          []string
}

func (d *listingDraft) validate(now time.Time, isNew bool) map[string]string {
	f := map[string]string{}
	d.Item, d.Unit, d.Location, d.Spec = strings.TrimSpace(d.Item), strings.TrimSpace(d.Unit), strings.TrimSpace(d.Location), strings.TrimSpace(d.Spec)
	switch {
	case d.Item == "":
		f["item"] = "Item wajib diisi"
	case len([]rune(d.Item)) > 200:
		f["item"] = "Maksimal 200 karakter"
	}
	if !(d.Quantity > 0) || d.Unit == "" {
		f["quantity"] = "Isi jumlah lebih dari 0 dan satuannya"
	}
	if d.Location == "" {
		f["location"] = "Lokasi wajib diisi"
	}
	if len(d.Attachments) > 10 {
		f["attachments"] = "Maksimal 10 lampiran"
	}
	if d.Kind == "supply" {
		if d.Price == nil || *d.Price < 0 {
			f["priceIdr"] = "Isi harga per unit"
		}
		if d.AvailableFrom == nil {
			f["availableFrom"] = "Isi tanggal tersedia"
		}
		if d.ExpiresAt != nil && d.AvailableFrom != nil && !d.ExpiresAt.After(*d.AvailableFrom) {
			f["expiresAt"] = "Harus setelah tanggal tersedia"
		}
	} else {
		if d.Budget == nil || *d.Budget < 0 {
			f["budgetIdr"] = "Isi budget"
		}
		if d.Deadline == nil {
			f["deadline"] = "Isi deadline"
		} else if isNew && d.Deadline.Before(now.Truncate(24*time.Hour)) {
			f["deadline"] = "Deadline tidak boleh di masa lalu"
		}
	}
	return f
}

func validationFields(f map[string]string) error {
	if len(f) == 0 {
		return nil
	}
	return &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: "Periksa kembali isian listing", Fields: f}
}

func (s *Server) CreateMyListing(ctx context.Context, req api.CreateMyListingRequestObject) (api.CreateMyListingResponseObject, error) {
	sess, err := requireActive(ctx)
	if err != nil {
		return nil, err
	}
	kind, err := req.Body.Discriminator()
	if err != nil {
		return nil, err
	}
	d := listingDraft{Kind: kind}
	switch kind {
	case "supply":
		in, err := req.Body.AsSupplyListingInput()
		if err != nil {
			return nil, err
		}
		price := int64(in.PriceIdr)
		af := in.AvailableFrom
		d.Item, d.Category, d.Quantity, d.Unit, d.Location, d.Spec, d.Delivery = in.Item, string(in.CategoryId), in.Quantity.Value, in.Quantity.Unit, in.Location, in.Spec, string(in.Delivery)
		d.Price, d.AvailableFrom, d.ExpiresAt, d.Attachments = &price, &af, in.ExpiresAt, in.Attachments
	case "demand":
		in, err := req.Body.AsDemandListingInput()
		if err != nil {
			return nil, err
		}
		budget := int64(in.BudgetIdr)
		dl := in.Deadline
		d.Item, d.Category, d.Quantity, d.Unit, d.Location, d.Spec, d.Delivery = in.Item, string(in.CategoryId), in.Quantity.Value, in.Quantity.Unit, in.Location, in.Spec, string(in.Delivery)
		d.Budget, d.Deadline, d.Attachments = &budget, &dl, in.Attachments
	default:
		return nil, validationFields(map[string]string{"kind": "Pilih supply atau demand"})
	}
	if err := validationFields(d.validate(time.Now(), true)); err != nil {
		return nil, err
	}

	var out api.Listing
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		party, err := userParty(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		status, prefix := "available", "SUP"
		if kind == "demand" {
			status, prefix = "open", "DEM"
		}
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO listings (code, kind, status, owner_party_id, item, category_id, quantity, unit, location, spec, delivery,
			                      price_idr, available_from, expires_at, budget_idr, deadline)
			VALUES (next_code($1), $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16) RETURNING id`,
			prefix, kind, status, party, d.Item, d.Category, d.Quantity, d.Unit, d.Location, d.Spec, d.Delivery,
			d.Price, d.AvailableFrom, d.ExpiresAt, d.Budget, d.Deadline).Scan(&id); err != nil {
			return err
		}
		if err := replaceAttachments(ctx, tx, id, d.Attachments); err != nil {
			return err
		}
		if err := listingEvent(ctx, tx, id, status, "Dibuat"); err != nil {
			return err
		}
		indicative := d.Price
		if kind == "demand" {
			indicative = d.Budget
		}
		value := deref64(indicative)
		if kind == "supply" {
			value = int64(float64(value) * d.Quantity)
		}
		fact, _ := json.Marshal(map[string]any{"kind": kind, "categoryId": d.Category, "region": d.Location, "item": d.Item,
			"quantity": d.Quantity, "unit": d.Unit, "valueIdr": value, "partyId": party})
		if err := emit(ctx, tx, "listing.created", id, fact); err != nil {
			return err
		}
		r, err := myListing(ctx, tx, sess.UserID, id, false)
		if err != nil {
			return err
		}
		out, err = r.api()
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.CreateMyListing201JSONResponse(out), nil
}

func replaceAttachments(ctx context.Context, tx pgx.Tx, listingID string, names []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM listing_attachments WHERE listing_id = $1`, listingID); err != nil {
		return err
	}
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO listing_attachments (listing_id, file_name) VALUES ($1, $2)`, listingID, n); err != nil {
				return err
			}
		}
	}
	return nil
}

func listingEvent(ctx context.Context, q dbtx, listingID, status, note string) error {
	_, err := q.Exec(ctx, `INSERT INTO listing_events (listing_id, status, note) VALUES ($1, $2, $3)`, listingID, status, note)
	return err
}

func (s *Server) GetMyListing(ctx context.Context, req api.GetMyListingRequestObject) (api.GetMyListingResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Reader()
	r, err := myListing(ctx, q, sess.UserID, req.Id, false)
	if err != nil {
		return nil, err
	}
	var detail api.ListingDetail
	if err := r.fill(detail.FromSupplyListing, detail.FromDemandListing); err != nil {
		return nil, err
	}
	detail.History = make([]struct {
		At     time.Time `json:"at"`
		Note   string    `json:"note"`
		Status string    `json:"status"`
	}, 0)
	rows, err := q.Query(ctx, `SELECT at, status, note FROM listing_events WHERE listing_id = $1 ORDER BY at DESC, id`, r.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var h struct {
			At     time.Time `json:"at"`
			Note   string    `json:"note"`
			Status string    `json:"status"`
		}
		if err := rows.Scan(&h.At, &h.Status, &h.Note); err != nil {
			rows.Close()
			return nil, err
		}
		detail.History = append(detail.History, h)
	}
	rows.Close()
	if detail.Matches, err = loadOpportunities(ctx, q, `o.category_id = $1 AND o.status <> 'dismissed' ORDER BY o.detected_at DESC LIMIT 3`, r.Category); err != nil {
		return nil, err
	}
	if detail.Markets, err = loadMarkets(ctx, q, `m.category_id = $1 AND m.status IN ('active','formation') ORDER BY m.volume_30d_idr DESC`, r.Category); err != nil {
		return nil, err
	}
	return listingDetailResponse{detail}, nil
}

// listingDetailResponse writes a ListingDetail with its own MarshalJSON. The generated GetMyListing200JSONResponse is a
// defined type over ListingDetail and loses that method, which drops the listing's union fields from the JSON
// (oapi-codegen only adds the method for pure unions).
type listingDetailResponse struct{ api.ListingDetail }

func (r listingDetailResponse) VisitGetMyListingResponse(w http.ResponseWriter) error {
	b, err := r.ListingDetail.MarshalJSON()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(append(b, '\n'))
	return err
}

// parseDate accepts an RFC 3339 timestamp or a plain YYYY-MM-DD date; "" means unset.
func parseDate(s string) (*time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t, true
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return &t, true
	}
	return nil, false
}

func (s *Server) UpdateMyListing(ctx context.Context, req api.UpdateMyListingRequestObject) (api.UpdateMyListingResponseObject, error) {
	sess, err := requireActive(ctx)
	if err != nil {
		return nil, err
	}
	in := req.Body
	var out api.Listing
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		r, err := myListing(ctx, tx, sess.UserID, req.Id, true)
		if err != nil {
			return err
		}
		if r.final() {
			return &Error{Status: http.StatusConflict, Code: "not_editable", Message: "Listing yang sudah selesai tidak bisa diubah"}
		}
		f := map[string]string{}
		// The frontend sends the whole draft including kind; another kind is a mistake, not a conversion.
		if k, ok := in.AdditionalProperties["kind"]; ok && k != r.Kind {
			f["kind"] = "Jenis listing tidak bisa diubah"
		}
		d := listingDraft{Kind: r.Kind, Item: r.Item, Category: r.Category, Quantity: r.Quantity, Unit: r.Unit, Location: r.Location,
			Spec: r.Spec, Delivery: r.Delivery, Price: r.Price, Budget: r.Budget, AvailableFrom: r.AvailableFrom, ExpiresAt: r.ExpiresAt,
			Deadline: r.Deadline, Attachments: r.Attachments}
		if in.Item != nil {
			d.Item = *in.Item
		}
		if in.CategoryId != nil {
			d.Category = string(*in.CategoryId)
		}
		if in.Quantity != nil {
			d.Quantity, d.Unit = in.Quantity.Value, in.Quantity.Unit
		}
		if in.Location != nil {
			d.Location = *in.Location
		}
		if in.Spec != nil {
			d.Spec = *in.Spec
		}
		if in.Delivery != nil {
			d.Delivery = string(*in.Delivery)
		}
		if in.Attachments != nil {
			d.Attachments = *in.Attachments
		}
		date := func(field string, v *string, dst **time.Time) {
			if v == nil {
				return
			}
			t, ok := parseDate(*v)
			if !ok {
				f[field] = "Tanggal tidak valid"
				return
			}
			*dst = t
		}
		if r.Kind == "supply" {
			if in.BudgetIdr != nil || in.Deadline != nil {
				f["budgetIdr"] = "Budget dan deadline hanya untuk demand"
			}
			if in.PriceIdr != nil {
				p := int64(*in.PriceIdr)
				d.Price = &p
			}
			date("availableFrom", in.AvailableFrom, &d.AvailableFrom)
			date("expiresAt", in.ExpiresAt, &d.ExpiresAt)
		} else {
			if in.PriceIdr != nil || in.AvailableFrom != nil || in.ExpiresAt != nil {
				f["priceIdr"] = "Harga dan tanggal tersedia hanya untuk supply"
			}
			if in.BudgetIdr != nil {
				b := int64(*in.BudgetIdr)
				d.Budget = &b
			}
			date("deadline", in.Deadline, &d.Deadline)
		}
		for k, v := range d.validate(time.Now(), false) {
			f[k] = v
		}
		if err := validationFields(f); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE listings SET item = $2, category_id = $3, quantity = $4, unit = $5, location = $6, spec = $7, delivery = $8,
			  price_idr = $9, available_from = $10, expires_at = $11, budget_idr = $12, deadline = $13
			WHERE id = $1`, r.ID, d.Item, d.Category, d.Quantity, d.Unit, d.Location, d.Spec, d.Delivery,
			d.Price, d.AvailableFrom, d.ExpiresAt, d.Budget, d.Deadline); err != nil {
			return err
		}
		if in.Attachments != nil {
			if err := replaceAttachments(ctx, tx, r.ID, d.Attachments); err != nil {
				return err
			}
		}
		if err := listingEvent(ctx, tx, r.ID, r.Status, "Diperbarui"); err != nil {
			return err
		}
		updated, err := myListing(ctx, tx, sess.UserID, r.ID, false)
		if err != nil {
			return err
		}
		out, err = updated.api()
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.UpdateMyListing200JSONResponse(out), nil
}

func (s *Server) ArchiveMyListing(ctx context.Context, req api.ArchiveMyListingRequestObject) (api.ArchiveMyListingResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	var out api.Listing
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		r, err := myListing(ctx, tx, sess.UserID, req.Id, true)
		if err != nil {
			return err
		}
		status := "expired"
		if r.Kind == "demand" {
			status = "cancelled"
		}
		if _, err := tx.Exec(ctx, `UPDATE listings SET status = $2 WHERE id = $1`, r.ID, status); err != nil {
			return err
		}
		if err := listingEvent(ctx, tx, r.ID, status, "Diarsipkan"); err != nil {
			return err
		}
		updated, err := myListing(ctx, tx, sess.UserID, r.ID, false)
		if err != nil {
			return err
		}
		out, err = updated.api()
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.ArchiveMyListing200JSONResponse(out), nil
}

// SubmitMyListingToMarket moves a listing into a market and makes the owner a participant there.
func (s *Server) SubmitMyListingToMarket(ctx context.Context, req api.SubmitMyListingToMarketRequestObject) (api.SubmitMyListingToMarketResponseObject, error) {
	sess, err := requireActive(ctx)
	if err != nil {
		return nil, err
	}
	var out api.Listing
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		r, err := myListing(ctx, tx, sess.UserID, req.Id, true)
		if err != nil {
			return err
		}
		if r.final() {
			return &Error{Status: http.StatusConflict, Code: "not_editable", Message: "Listing yang sudah selesai tidak bisa dimasukkan ke market"}
		}
		var marketID, name, category, status string
		var approval *string
		err = tx.QueryRow(ctx, `
			SELECT m.id, m.name, m.category_id, m.status, st.approval FROM markets m LEFT JOIN market_settings st ON st.market_id = m.id
			WHERE m.id::text = $1`, req.Body.MarketId).Scan(&marketID, &name, &category, &status, &approval)
		if errors.Is(err, pgx.ErrNoRows) {
			return &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Market tidak ditemukan"}
		}
		if err != nil {
			return err
		}
		if status != "active" && status != "formation" {
			return &Error{Status: http.StatusConflict, Code: "market_closed", Message: "Market ini sedang tidak menerima listing"}
		}
		if category != r.Category {
			return validationFields(map[string]string{"marketId": "Market ini untuk kategori lain"})
		}
		party, err := userParty(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		role, participant := "supplier", "pending"
		if r.Kind == "demand" {
			role = "buyer"
		}
		if approval != nil && *approval == "auto" {
			participant = "active"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO market_participants (market_id, party_id, role, status) VALUES ($1, $2, $3, $4)
			ON CONFLICT (market_id, party_id) DO NOTHING`, marketID, party, role, participant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO watchlist (user_id, market_id, joined_at) VALUES ($1, $2, now())
			ON CONFLICT (user_id, market_id) DO UPDATE SET joined_at = coalesce(watchlist.joined_at, now())`, sess.UserID, marketID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE listings SET status = 'in_market', market_id = $2 WHERE id = $1`, r.ID, marketID); err != nil {
			return err
		}
		if err := listingEvent(ctx, tx, r.ID, "in_market", "Dimasukkan ke "+name); err != nil {
			return err
		}
		updated, err := myListing(ctx, tx, sess.UserID, r.ID, false)
		if err != nil {
			return err
		}
		out, err = updated.api()
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.SubmitMyListingToMarket200JSONResponse(out), nil
}

func deref64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefTime(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}
