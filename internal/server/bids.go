package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Bidding, Dutch accept, buyer-created auctions and awards. Every write locks the auction row first, so bids on one
// auction are serialised and the realtime seq is gapless (see docs/database.md).

const maxExtensions = 3

var errAuctionClosed = &Error{Status: http.StatusConflict, Code: "auction_closed", Message: "Auction tidak sedang berjalan"}

// bumpSeq takes the next realtime seq of an auction channel for an event that is not a bid (bids take theirs in the
// insert trigger).
func bumpSeq(ctx context.Context, q dbtx, auctionID string) (int64, error) {
	var seq int64
	err := q.QueryRow(ctx, `UPDATE auctions SET last_seq = last_seq + 1 WHERE id = $1 RETURNING last_seq`, auctionID).Scan(&seq)
	return seq, err
}

func emitAuction(ctx context.Context, q dbtx, auctionID, typ string, payload any) error {
	seq, err := bumpSeq(ctx, q, auctionID)
	if err != nil {
		return err
	}
	return emitFrame(ctx, q, "auction:"+auctionID, typ, &seq, payload)
}

// emitAuctionState sends the auction.state snapshot (start, freeze, award, cancel...).
func emitAuctionState(ctx context.Context, q dbtx, auctionID string) error {
	r, err := loadAuction(ctx, q, auctionID, false)
	if err != nil {
		return err
	}
	a := r.api()
	p := map[string]any{"kind": "state", "status": a.Status, "endsAt": a.EndsAt, "bidCount": a.BidCount, "participants": a.Participants}
	if a.CurrentPriceIdr != nil {
		p["currentPriceIdr"] = *a.CurrentPriceIdr
	}
	return emitAuction(ctx, q, auctionID, "auction.state", p)
}

// emitBidStatus tells a bidder (on user:{id}) where their bid stands now.
func emitBidStatus(ctx context.Context, q dbtx, r auctionRow, userID string) error {
	b, err := myBid(ctx, q, r, userID)
	if err != nil || b == nil {
		return err
	}
	var id string
	var no int32
	var at time.Time
	if err := q.QueryRow(ctx, `SELECT id, bidder_no, created_at FROM bids WHERE auction_id = $1 AND bidder_user_id = $2 ORDER BY (status = 'withdrawn'), seq DESC LIMIT 1`,
		r.ID, userID).Scan(&id, &no, &at); err != nil {
		return err
	}
	p := map[string]any{"auctionId": r.ID, "status": b.Status, "canWithdraw": b.CanWithdraw, "updatedAt": b.UpdatedAt,
		"bid": map[string]any{"id": id, "bidder": r.bidderLabel(no), "priceIdr": b.PriceIdr, "at": at, "mine": true}}
	if b.Rank != nil {
		p["rank"] = *b.Rank
	}
	return emitFrame(ctx, q, "user:"+userID, "bid.status", nil, p)
}

// bidLimit is the price a new bid must reach (inclusive): the opening price, or the best price minus/plus the step.
func bidLimit(r auctionRow) int64 {
	if r.Type == "sealed" || r.Current == nil {
		return r.Opening
	}
	if r.Type == "forward" {
		return *r.Current + r.MinStep
	}
	return *r.Current - r.MinStep
}

func (s *Server) PlaceBid(ctx context.Context, req api.PlaceBidRequestObject) (api.PlaceBidResponseObject, error) {
	sess, err := requireActive(ctx)
	if err != nil {
		return nil, err
	}
	price := int64(req.Body.PriceIdr)
	var out *api.MyBid
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		r, err := loadAuction(ctx, tx, req.Id, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return errAuctionNotFound
		}
		if err != nil {
			return err
		}
		if r.Status != "live" && r.Status != "extended" {
			return errAuctionClosed
		}
		if r.Type == "dutch" {
			return &Error{Status: http.StatusConflict, Code: "auction_closed", Message: "Auction Dutch tidak menerima bid; terima harga yang berjalan"}
		}
		qual, err := qualificationOf(ctx, tx, r.ID, sess.UserID)
		if err != nil {
			return err
		}
		if qual.Status != "qualified" {
			return &Error{Status: http.StatusForbidden, Code: "not_qualified", Message: "Selesaikan kualifikasi dulu"}
		}
		if owner, err := isOwner(ctx, tx, r, sess.UserID); err != nil {
			return err
		} else if owner {
			return &Error{Status: http.StatusForbidden, Code: "owner", Message: "Pembuat auction tidak bisa ikut bid"}
		}
		if err := s.commitGuard(ctx, tx, sess.UserID, int64(math.Round(float64(price)*r.Quantity))); err != nil {
			return err
		}
		limit := bidLimit(r)
		if r.Type == "forward" && price < limit {
			return &Error{Status: 422, Code: "invalid_bid", Message: "Bid harus ≥ " + rupiah(limit), Fields: map[string]string{"price": "Bid harus ≥ " + rupiah(limit)}}
		}
		if r.Type != "forward" && price > limit {
			return &Error{Status: 422, Code: "invalid_bid", Message: "Bid harus ≤ " + rupiah(limit), Fields: map[string]string{"price": "Bid harus ≤ " + rupiah(limit)}}
		}
		party, err := userParty(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}

		sealed := r.Type == "sealed"
		status := "leading"
		if sealed {
			status = "submitted"
		}
		// The bidder's earlier bids are superseded; the previous leader (someone else) is outbid.
		var outbid []string
		if !sealed {
			rows, err := tx.Query(ctx, `
				UPDATE bids SET status = 'outbid' WHERE auction_id = $1 AND status = 'leading' AND bidder_party_id <> $2
				RETURNING bidder_user_id::text`, r.ID, party)
			if err != nil {
				return err
			}
			for rows.Next() {
				var u *string
				if err := rows.Scan(&u); err != nil {
					rows.Close()
					return err
				}
				if u != nil {
					outbid = append(outbid, *u)
				}
			}
			rows.Close()
		}
		if _, err := tx.Exec(ctx, `UPDATE bids SET status = 'outbid' WHERE auction_id = $1 AND bidder_party_id = $2 AND status IN ('leading','submitted')`, r.ID, party); err != nil {
			return err
		}
		var bidID string
		var seq int64
		var no int32
		var at time.Time
		// seq and bidder_no come from the insert trigger (it also bumps bid_count / participant_count).
		if err := tx.QueryRow(ctx, `
			INSERT INTO bids (auction_id, bidder_party_id, bidder_user_id, price_idr, status, seq, bidder_no) VALUES ($1, $2, $3, $4, $5, 0, 0)
			RETURNING id, seq, bidder_no, created_at`, r.ID, party, sess.UserID, price, status).Scan(&bidID, &seq, &no, &at); err != nil {
			return err
		}
		best := price
		if sealed {
			// Keep the hidden best (lowest) for the record; never published while live.
			if r.Current != nil && *r.Current < best {
				best = *r.Current
			}
		}
		// Anti-sniping: a bid inside the window pushes the close out, at most maxExtensions times.
		extended := false
		if time.Until(r.EndsAt) < time.Duration(r.ExtWindow)*time.Minute && r.Extensions < maxExtensions {
			extended = true
		}
		var endsAt time.Time
		var bidCount, participants int32
		if err := tx.QueryRow(ctx, `
			UPDATE auctions SET current_price_idr = $2,
			  ends_at = CASE WHEN $3 THEN ends_at + make_interval(mins => ext_minutes) ELSE ends_at END,
			  extension_count = extension_count + CASE WHEN $3 THEN 1 ELSE 0 END,
			  status = CASE WHEN $3 THEN 'extended' ELSE status END
			WHERE id = $1 RETURNING ends_at, bid_count, participant_count`, r.ID, best, extended).Scan(&endsAt, &bidCount, &participants); err != nil {
			return err
		}
		r.Current = &best
		// Public frame, masked for the audience: real price only with visibility full.
		pubPrice := int64(0)
		if r.Visibility == "full" {
			pubPrice = price
		}
		frame := map[string]any{"kind": "bid", "bidCount": bidCount, "participants": participants,
			"bid": map[string]any{"id": bidID, "bidder": r.bidderLabel(no), "priceIdr": pubPrice, "at": at}}
		if r.Visibility == "full" {
			frame["currentPriceIdr"] = best
		}
		if err := emitFrame(ctx, tx, "auction:"+r.ID, "auction.bid", &seq, frame); err != nil {
			return err
		}
		if extended {
			if err := emitAuction(ctx, tx, r.ID, "auction.extended", map[string]any{"kind": "extended", "endsAt": endsAt}); err != nil {
				return err
			}
		}
		fact, _ := json.Marshal(map[string]any{"marketId": r.MarketID, "categoryId": r.Category, "region": "", "auctionType": r.Type,
			"priceIdr": price, "minStepIdr": r.MinStep, "bidderPartyId": party})
		if err := emit(ctx, tx, "auction.bid", r.ID, fact); err != nil {
			return err
		}
		var bidValue *int64 // public feed: the lot value at the bid price, only where prices are public
		if r.Visibility == "full" {
			bidValue = ptr(round(float64(price) * r.Quantity))
		}
		if err := emitActivity(ctx, tx, "bid_placed", "Bid baru di auction "+r.Title, bidValue, r.MarketID); err != nil {
			return err
		}
		r.EndsAt = endsAt
		if r2, err := loadAuction(ctx, tx, r.ID, false); err == nil {
			r = r2
		} else {
			return err
		}
		for _, u := range outbid {
			if u == sess.UserID {
				continue
			}
			if err := emitBidStatus(ctx, tx, r, u); err != nil {
				return err
			}
			body := "Ada bid yang lebih baik."
			if r.Visibility == "full" {
				body = fmt.Sprintf("Harga terbaik sekarang %s/%s.", rupiah(best), r.Unit)
			}
			if err := notify(ctx, tx, u, notification{Type: "outbid", Title: "Kamu tersalip di " + r.Title, Body: body, Href: "/auctions/" + r.ID}); err != nil {
				return err
			}
		}
		if err := emitBidStatus(ctx, tx, r, sess.UserID); err != nil {
			return err
		}
		out, err = myBid(ctx, tx, r, sess.UserID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.PlaceBid200JSONResponse(*out), nil
}

func (s *Server) WithdrawMyBid(ctx context.Context, req api.WithdrawMyBidRequestObject) (api.WithdrawMyBidResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	var out *api.MyBid
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		r, err := loadAuction(ctx, tx, req.Id, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return errAuctionNotFound
		}
		if err != nil {
			return err
		}
		b, err := myBid(ctx, tx, r, sess.UserID)
		if err != nil {
			return err
		}
		if b == nil {
			return &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Belum ada bid"}
		}
		if !b.CanWithdraw {
			return &Error{Status: http.StatusConflict, Code: "cannot_withdraw", Message: "Bid terdepan atau 30 menit terakhir tidak bisa ditarik"}
		}
		if _, err := tx.Exec(ctx, `UPDATE bids SET status = 'withdrawn' WHERE auction_id = $1 AND bidder_user_id = $2 AND status <> 'withdrawn'`, r.ID, sess.UserID); err != nil {
			return err
		}
		if err := emitBidStatus(ctx, tx, r, sess.UserID); err != nil {
			return err
		}
		out, err = myBid(ctx, tx, r, sess.UserID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.WithdrawMyBid200JSONResponse(*out), nil
}

// AcceptDutchPrice buys the whole lot at the current ask; the counterparty is the market's maker.
func (s *Server) AcceptDutchPrice(ctx context.Context, req api.AcceptDutchPriceRequestObject) (api.AcceptDutchPriceResponseObject, error) {
	sess, err := requireActive(ctx)
	if err != nil {
		return nil, err
	}
	var txID string
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		r, err := loadAuction(ctx, tx, req.Id, true)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (r.Type != "dutch" || (r.Status != "live" && r.Status != "extended"))) {
			return &Error{Status: http.StatusConflict, Code: "auction_closed", Message: "Harga tidak bisa diterima sekarang"}
		}
		if err != nil {
			return err
		}
		qual, err := qualificationOf(ctx, tx, r.ID, sess.UserID)
		if err != nil {
			return err
		}
		if qual.Status != "qualified" {
			return &Error{Status: http.StatusForbidden, Code: "not_qualified", Message: "Selesaikan kualifikasi dulu"}
		}
		price := r.Opening
		if r.Current != nil {
			price = *r.Current
		}
		if err := s.commitGuard(ctx, tx, sess.UserID, int64(math.Round(float64(price)*r.Quantity))); err != nil {
			return err
		}
		if r.MarketID == nil {
			return &Error{Status: http.StatusConflict, Code: "auction_closed", Message: "Auction ini tidak punya penjual"}
		}
		var maker, region string
		if err := tx.QueryRow(ctx, `SELECT maker_party_id, region FROM markets WHERE id::text = $1`, *r.MarketID).Scan(&maker, &region); err != nil {
			return err
		}
		party, err := userParty(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO bids (auction_id, bidder_party_id, bidder_user_id, price_idr, status, seq, bidder_no) VALUES ($1, $2, $3, $4, 'won', 0, 0)`,
			r.ID, party, sess.UserID, price); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE auctions SET status = 'awarded', clearing_price_idr = $2, current_price_idr = $2 WHERE id = $1`, r.ID, price); err != nil {
			return err
		}
		t, err := createTrade(ctx, tx, newTrade{Title: fmt.Sprintf("%s %s %s", r.Item, qtyLabel(r.Quantity), r.Unit), BuyerParty: party, SupplierParty: maker,
			Quantity: r.Quantity, Unit: r.Unit, UnitPriceIdr: price, MakerFeeRate: 0.005, MarketID: r.MarketID, AuctionID: &r.ID,
			DeliveryAddress: region, Via: "dutch", Category: r.Category, Region: region, ActorUserID: &sess.UserID})
		if err != nil {
			return err
		}
		txID = t.ID
		if err := emitAuctionState(ctx, tx, r.ID); err != nil {
			return err
		}
		return notify(ctx, tx, sess.UserID, notification{Type: "winning_bid", Title: "Kamu memenangkan " + r.Title,
			Body: fmt.Sprintf("Harga %s/%s. Transaksi dibuat.", rupiah(price), r.Unit), Href: "/app/transactions/" + t.ID})
	})
	if err != nil {
		return nil, err
	}
	return api.AcceptDutchPrice200JSONResponse{TransactionId: txID}, nil
}

func qtyLabel(v float64) string {
	if v == math.Trunc(v) {
		return strings.TrimPrefix(rupiah(int64(v)), "Rp ")
	}
	return fmt.Sprintf("%g", v)
}

// ── Buyer auctions ───────────────────────────────────────────────

func (s *Server) CreateBuyerAuction(ctx context.Context, req api.CreateBuyerAuctionRequestObject) (api.CreateBuyerAuctionResponseObject, error) {
	sess, err := requireActive(ctx)
	if err != nil {
		return nil, err
	}
	in := req.Body
	var out api.AuctionDetail
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		d, err := myListing(ctx, tx, sess.UserID, in.DemandId, true)
		if err != nil || d.Kind != "demand" {
			return &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Demand tidak ditemukan"}
		}
		if d.AuctionID != nil {
			return &Error{Status: http.StatusConflict, Code: "already_in_auction", Message: "Demand ini sudah punya auction"}
		}
		if d.Status != "open" && d.Status != "matched" {
			return &Error{Status: http.StatusConflict, Code: "not_open", Message: "Demand ini sudah tidak terbuka"}
		}
		f := map[string]string{}
		if in.OpeningPriceIdr <= 0 {
			f["openingPriceIdr"] = "Isi harga pembuka"
		}
		if in.MinStepIdr < 0 || (in.OpeningPriceIdr > 0 && in.MinStepIdr >= in.OpeningPriceIdr) {
			f["minStepIdr"] = "Langkah minimum harus lebih kecil dari harga pembuka"
		}
		if in.DurationMinutes < 5 || in.DurationMinutes > 14*24*60 {
			f["durationMinutes"] = "Durasi 5 menit sampai 14 hari"
		}
		if in.Type != "reverse" && in.Type != "sealed" {
			f["type"] = "Pilih reverse atau sealed"
		}
		if err := validationFields(f); err != nil {
			return err
		}
		if err := s.commitGuard(ctx, tx, sess.UserID, int64(math.Round(float64(in.OpeningPriceIdr)*d.Quantity))); err != nil {
			return err
		}
		visibility := string(in.Visibility)
		if in.Type == "sealed" {
			visibility = "sealed"
		}
		typeLabel := map[string]string{"reverse": "Reverse auction", "sealed": "Sealed bid"}[string(in.Type)]
		invites := "Terbuka untuk supplier terkualifikasi"
		if len(in.Invite) > 0 {
			invites = strings.Join(in.Invite, ", ")
		}
		rules, _ := json.Marshal([]api.LabeledValue{
			{Label: "Tipe", Value: typeLabel},
			{Label: "Harga pembuka", Value: fmt.Sprintf("%s per %s", rupiah(int64(in.OpeningPriceIdr)), d.Unit)},
			{Label: "Penurunan minimum", Value: rupiah(int64(in.MinStepIdr))},
			{Label: "Undangan", Value: invites},
			{Label: "Perpanjangan", Value: fmt.Sprintf("+5 menit jika ada bid di 2 menit terakhir (maks. %d kali)", maxExtensions)},
		})
		var id, code string
		if err := tx.QueryRow(ctx, `
			INSERT INTO auctions (title, market_id, owner_user_id, category_id, type, status, visibility, lot_item, lot_spec, quantity, unit,
			                      opening_price_idr, min_step_idr, starts_at, ends_at, rules, invitees, created_by)
			VALUES ($1, $2, $3, $4, $5, 'live', $6, $7, $8, $9, $10, $11, $12, now(), now() + make_interval(mins => $13), $14, $15, $3)
			RETURNING id, code`,
			fmt.Sprintf("%s %s %s", d.Item, qtyLabel(d.Quantity), d.Unit), d.MarketID, sess.UserID, d.Category, in.Type, visibility, d.Item,
			nonEmpty(d.Spec, "Sesuai deskripsi demand"), d.Quantity, d.Unit, in.OpeningPriceIdr, in.MinStepIdr, in.DurationMinutes, rules,
			nonNil(in.Invite), // invitees are free-text names, as the UI sends them
		).Scan(&id, &code); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE listings SET auction_id = $2, status = 'in_market' WHERE id = $1`, d.ID, id); err != nil {
			return err
		}
		if err := listingEvent(ctx, tx, d.ID, "in_market", "Auction "+code+" dibuat"); err != nil {
			return err
		}
		if err := writeAudit(ctx, tx, audit{ActorUserID: &sess.UserID, ActorLabel: sess.Name, Action: "Buka auction",
			EntityType: "auction", EntityID: id, EntityLabel: code + " · " + d.Item}); err != nil {
			return err
		}
		r, err := loadAuction(ctx, tx, id, false)
		if err != nil {
			return err
		}
		out, err = auctionDetail(ctx, tx, r, sess.UserID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.CreateBuyerAuction201JSONResponse(out), nil
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func (s *Server) ListMyAuctions(ctx context.Context, _ api.ListMyAuctionsRequestObject) (api.ListMyAuctionsResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Primary()
	out := api.ListMyAuctions200JSONResponse{}
	// Eligible: open auctions the caller has not bid on and does not own; forward auctions sell to buyers, so they
	// are not offered here (they show in public lists).
	rows, err := q.Query(ctx, auctionSelect+`
		WHERE a.status IN ('live','extended','qualification','scheduled') AND a.type <> 'forward'
		  AND a.owner_user_id IS DISTINCT FROM $1
		  AND NOT EXISTS (SELECT 1 FROM bids b WHERE b.auction_id = a.id AND b.bidder_user_id = $1)
		ORDER BY a.ends_at LIMIT 50`, sess.UserID)
	if err != nil {
		return nil, err
	}
	var eligible []auctionRow
	for rows.Next() {
		r, err := scanAuction(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		eligible = append(eligible, r)
	}
	rows.Close()
	for _, r := range eligible {
		qual, err := qualificationOf(ctx, q, r.ID, sess.UserID)
		if err != nil {
			return nil, err
		}
		// The generated element type flattens allOf(Auction, {qualification}); fill it through JSON.
		var item = struct {
			api.Auction
			Qualification api.QualificationStatus `json:"qualification"`
		}{r.api(), qual.Status}
		if err := convertJSON(item, &out.Eligible); err != nil {
			return nil, err
		}
	}
	rows, err = q.Query(ctx, auctionSelect+` WHERE EXISTS (SELECT 1 FROM bids b WHERE b.auction_id = a.id AND b.bidder_user_id = $1) ORDER BY a.ends_at DESC LIMIT 100`, sess.UserID)
	if err != nil {
		return nil, err
	}
	var mine []auctionRow
	for rows.Next() {
		r, err := scanAuction(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		mine = append(mine, r)
	}
	rows.Close()
	for _, r := range mine {
		b, err := myBid(ctx, q, r, sess.UserID)
		if err != nil {
			return nil, err
		}
		if b != nil {
			out.Bids = append(out.Bids, *b)
		}
	}
	if out.Owned, err = loadAuctions(ctx, q, `a.owner_user_id = $1 ORDER BY a.created_at DESC`, sess.UserID); err != nil {
		return nil, err
	}
	if out.Eligible == nil {
		_ = json.Unmarshal([]byte("[]"), &out.Eligible) // empty list, not null (the element type is anonymous)
	}
	if out.Bids == nil {
		out.Bids = []api.MyBid{}
	}
	return out, nil
}

// convertJSON appends src (marshalled) to the slice dst points at, or decodes it into dst when dst is not a slice.
func convertJSON[T any](src any, dst *[]T) error {
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*dst = append(*dst, v)
	return nil
}

// ── Evaluation and award (owner) ─────────────────────────────────

type offer struct {
	BidID, PartyID, Name, Kind string
	UserID                     *string
	Price                      int64
	At                         time.Time
	Verified                   bool
}

// offers: each bidder's best active bid.
func offers(ctx context.Context, q dbtx, r auctionRow) ([]offer, error) {
	order := "price_idr ASC"
	if r.Type == "forward" {
		order = "price_idr DESC"
	}
	rows, err := q.Query(ctx, `
		SELECT DISTINCT ON (b.bidder_party_id) b.id, b.bidder_party_id, p.name, p.display_kind, b.bidder_user_id::text, b.price_idr, b.created_at,
		       CASE p.kind WHEN 'user' THEN coalesce(i.identity_verified_at IS NOT NULL, false) WHEN 'org' THEN coalesce(op.verification = 'verified', false) ELSE p.verified END
		FROM bids b JOIN parties p ON p.id = b.bidder_party_id
		LEFT JOIN identities i ON i.user_id = p.user_id LEFT JOIN org_profiles op ON op.org_id = p.org_id
		WHERE b.auction_id = $1 AND b.status <> 'withdrawn'
		ORDER BY b.bidder_party_id, b.`+order+`, b.seq`, r.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []offer
	for rows.Next() {
		var o offer
		if err := rows.Scan(&o.BidID, &o.PartyID, &o.Name, &o.Kind, &o.UserID, &o.Price, &o.At, &o.Verified); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if r.Type == "forward" {
			return out[i].Price > out[j].Price
		}
		return out[i].Price < out[j].Price
	})
	return out, rows.Err()
}

func ownedAuction(ctx context.Context, q dbtx, s *session, id string, forUpdate bool) (auctionRow, error) {
	r, err := loadAuction(ctx, q, id, forUpdate)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, errAuctionNotFound
	}
	if err != nil {
		return r, err
	}
	if owner, err := isOwner(ctx, q, r, s.UserID); err != nil {
		return r, err
	} else if !owner {
		return r, errAuctionNotFound
	}
	return r, nil
}

func (s *Server) GetAuctionEvaluation(ctx context.Context, req api.GetAuctionEvaluationRequestObject) (api.GetAuctionEvaluationResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Primary()
	r, err := ownedAuction(ctx, q, sess, req.Id, false)
	if err != nil {
		return nil, err
	}
	detail, err := auctionDetail(ctx, q, r, sess.UserID)
	if err != nil {
		return nil, err
	}
	os, err := offers(ctx, q, r)
	if err != nil {
		return nil, err
	}
	out := api.GetAuctionEvaluation200JSONResponse{Auction: detail, Offers: []api.Offer{}}
	left := r.Quantity
	var total float64
	for _, o := range os {
		var off api.Offer
		off.Id, off.PriceIdr, off.SubmittedAt = o.BidID, int(o.Price), o.At
		off.Supplier.Name, off.Supplier.Kind, off.Supplier.Verified = o.Name, api.OfferSupplierKind(o.Kind), o.Verified
		// ponytail: reputation and capacity come with the reputation / supplier areas; until then every bidder offers
		// the whole lot at the 80 baseline, so the suggestion is simply the best price.
		off.Supplier.Reputation = 80
		off.Capacity = api.Quantity{Value: r.Quantity, Unit: r.Unit}
		out.Offers = append(out.Offers, off)
		if left > 0 {
			take := math.Min(left, r.Quantity)
			out.Suggestion.Lines = append(out.Suggestion.Lines, api.AllocationLine{OfferId: o.BidID, Supplier: o.Name, Quantity: take, PriceIdr: int(o.Price)})
			total += take * float64(o.Price)
			left -= take
		}
	}
	if out.Suggestion.Lines == nil {
		out.Suggestion.Lines = []api.AllocationLine{}
	}
	covered := r.Quantity - math.Max(left, 0)
	out.Suggestion.TotalIdr = int(math.Round(total))
	out.Suggestion.SavingsIdr = int(math.Round(float64(r.Opening)*covered - total))
	out.Suggestion.Reason = "Harga terbaik lebih dulu, tiap penawaran sampai kapasitasnya, sampai lot terpenuhi."
	if len(os) == 0 {
		out.Suggestion.Reason = "Belum ada penawaran."
	}
	return out, nil
}

func (s *Server) AwardAuction(ctx context.Context, req api.AwardAuctionRequestObject) (api.AwardAuctionResponseObject, error) {
	sess, err := requireActive(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		r, err := ownedAuction(ctx, tx, sess, req.Id, true)
		if err != nil {
			return err
		}
		if r.Status != "closed" {
			return &Error{Status: http.StatusConflict, Code: "not_closed", Message: "Pemenang bisa ditetapkan setelah auction ditutup"}
		}
		os, err := offers(ctx, tx, r)
		if err != nil {
			return err
		}
		byID := map[string]offer{}
		for _, o := range os {
			byID[o.BidID] = o
		}
		f := map[string]string{}
		var sum float64
		seen := map[string]bool{}
		for i, l := range req.Body.Lines {
			o, ok := byID[l.OfferId]
			switch {
			case !ok:
				f[fmt.Sprintf("lines.%d.offerId", i)] = "Penawaran tidak ditemukan"
			case seen[l.OfferId]:
				f[fmt.Sprintf("lines.%d.offerId", i)] = "Penawaran dipilih dua kali"
			case !(l.Quantity > 0):
				f[fmt.Sprintf("lines.%d.quantity", i)] = "Jumlah harus lebih dari 0"
			default:
				_ = o
			}
			seen[l.OfferId] = true
			sum += l.Quantity
		}
		if len(req.Body.Lines) == 0 {
			f["lines"] = "Pilih minimal satu penawaran"
		}
		if sum > r.Quantity+1e-9 {
			f["lines"] = "Total alokasi melebihi kuantitas lot"
		}
		if err := validationFields(f); err != nil {
			return err
		}
		// The price is the offer's, never the client's.
		var total float64
		for _, l := range req.Body.Lines {
			total += l.Quantity * float64(byID[l.OfferId].Price)
		}
		if err := s.commitGuard(ctx, tx, sess.UserID, int64(math.Round(total))); err != nil {
			return err
		}
		buyer, err := userParty(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		var demandID, location string
		_ = tx.QueryRow(ctx, `SELECT id::text, location FROM listings WHERE auction_id = $1`, r.ID).Scan(&demandID, &location)
		var awardID string
		if err := tx.QueryRow(ctx, `INSERT INTO auction_awards (auction_id, awarded_by) VALUES ($1, $2) RETURNING id`, r.ID, sess.UserID).Scan(&awardID); err != nil {
			return err
		}
		var changes []change
		winners := map[string]bool{}
		for _, l := range req.Body.Lines {
			o := byID[l.OfferId]
			winners[o.PartyID] = true
			if _, err := tx.Exec(ctx, `INSERT INTO auction_award_lines (award_id, bid_id, quantity, price_idr) VALUES ($1, $2, $3, $4)`, awardID, o.BidID, l.Quantity, o.Price); err != nil {
				return err
			}
			t, err := createTrade(ctx, tx, newTrade{Title: fmt.Sprintf("%s %s %s", r.Item, qtyLabel(l.Quantity), r.Unit), BuyerParty: buyer, SupplierParty: o.PartyID,
				Quantity: l.Quantity, Unit: r.Unit, UnitPriceIdr: o.Price, MakerFeeRate: makerFee(r), MarketID: r.MarketID, AuctionID: &r.ID,
				DeliveryAddress: nonEmpty(location, "-"), Via: "auction", Category: r.Category, ActorUserID: &sess.UserID})
			if err != nil {
				return err
			}
			ids = append(ids, t.ID)
			changes = append(changes, change{Field: o.Name, After: fmt.Sprintf("%s %s × %s", qtyLabel(l.Quantity), r.Unit, rupiah(o.Price))})
			if o.UserID != nil {
				if err := notify(ctx, tx, *o.UserID, notification{Type: "winning_bid", Title: "Penawaranmu dipilih: " + r.Title,
					Body: fmt.Sprintf("%s %s × %s. Setujui agreement-nya.", qtyLabel(l.Quantity), r.Unit, rupiah(o.Price)), Href: "/app/transactions"}); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE bids SET status = CASE WHEN bidder_party_id = ANY($2::uuid[]) THEN 'won' ELSE 'lost' END
			WHERE auction_id = $1 AND status <> 'withdrawn'`, r.ID, keys(winners)); err != nil {
			return err
		}
		for _, o := range os {
			if o.UserID != nil && !winners[o.PartyID] {
				if err := notify(ctx, tx, *o.UserID, notification{Type: "auction_ending", Title: r.Title + " selesai", Body: "Penawaranmu belum terpilih kali ini.", Href: "/auctions/" + r.ID}); err != nil {
					return err
				}
			}
			if o.UserID != nil {
				if err := emitBidStatus(ctx, tx, r, *o.UserID); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE auctions SET status = 'awarded' WHERE id = $1`, r.ID); err != nil {
			return err
		}
		if demandID != "" {
			if _, err := tx.Exec(ctx, `UPDATE listings SET status = 'matched' WHERE id::text = $1`, demandID); err != nil {
				return err
			}
			if err := listingEvent(ctx, tx, demandID, "matched", fmt.Sprintf("Award ke %d supplier", len(winners))); err != nil {
				return err
			}
		}
		if err := writeAudit(ctx, tx, audit{ActorUserID: &sess.UserID, ActorLabel: sess.Name, Action: "Tetapkan pemenang",
			EntityType: "auction", EntityID: r.ID, EntityLabel: r.Code + " · " + r.Title, Changes: changes}); err != nil {
			return err
		}
		return emitAuctionState(ctx, tx, r.ID)
	})
	if err != nil {
		return nil, err
	}
	return api.AwardAuction200JSONResponse{TransactionIds: ids}, nil
}

// makerFee: 0.5% when a market maker runs the market, none for direct procurement.
func makerFee(r auctionRow) float64 {
	if r.MarketID != nil {
		return 0.005
	}
	return 0
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
