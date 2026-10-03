package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The auction clock: starts scheduled auctions, closes the ones whose time is up, and steps Dutch asks down.
// Runs on every API instance; each auction is claimed with FOR UPDATE SKIP LOCKED, so instances never double-process.

const (
	// dutchStepEvery: how often a Dutch ask drops by min_step. ponytail: one global pace; make it a market rule when
	// markets need different paces.
	dutchStepEvery = 30 * time.Second
	// dutchFloorPct: the ask never drops below this share of the opening price (frontend mock: 80%).
	dutchFloorPct = 0.8
)

// RunAuctionClock ticks until ctx ends.
func (s *Server) RunAuctionClock(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.AuctionTick(ctx); err != nil && ctx.Err() == nil && s.Log != nil {
			s.Log.Error("auction clock", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// AuctionTick does one pass (exported for tests).
func (s *Server) AuctionTick(ctx context.Context) error {
	for _, step := range []func(context.Context) (bool, error){s.startOne, s.closeOne, s.dutchOne} {
		// Drain each queue one auction per transaction so a slow one doesn't hold the others' locks.
		for i := 0; i < 100; i++ {
			did, err := step(ctx)
			if err != nil {
				return err
			}
			if !did {
				break
			}
		}
	}
	return nil
}

func (s *Server) claim(ctx context.Context, tx pgx.Tx, where string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM auctions WHERE `+where+` ORDER BY ends_at LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return id, err
}

func (s *Server) startOne(ctx context.Context) (bool, error) {
	did := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		id, err := s.claim(ctx, tx, `status IN ('scheduled','qualification') AND starts_at <= now()`)
		if err != nil || id == "" {
			return err
		}
		did = true
		if _, err := tx.Exec(ctx, `UPDATE auctions SET status = 'live' WHERE id = $1`, id); err != nil {
			return err
		}
		// Market rounds were announced on the public feed when the maker opened them (mmAudit "Round n dibuka").
		var title string
		var market *string
		var round *int32
		var value int64
		if err := tx.QueryRow(ctx, `SELECT title, market_id::text, round_no, round(quantity * opening_price_idr)::bigint FROM auctions WHERE id = $1`, id).
			Scan(&title, &market, &round, &value); err != nil {
			return err
		}
		if round == nil {
			if err := emitActivity(ctx, tx, "auction_started", "Auction dimulai: "+title, &value, market); err != nil {
				return err
			}
		}
		return emitAuctionState(ctx, tx, id)
	})
	return did, err
}

func (s *Server) dutchOne(ctx context.Context) (bool, error) {
	did := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		id, err := s.claim(ctx, tx, fmt.Sprintf(`type = 'dutch' AND status IN ('live','extended') AND updated_at <= now() - interval '%d seconds'
			AND coalesce(current_price_idr, opening_price_idr) > ceil(opening_price_idr * %g)`, int(dutchStepEvery.Seconds()), dutchFloorPct))
		if err != nil || id == "" {
			return err
		}
		did = true
		var price int64
		if err := tx.QueryRow(ctx, fmt.Sprintf(`
			UPDATE auctions SET current_price_idr = greatest(coalesce(current_price_idr, opening_price_idr) - greatest(min_step_idr, 1), ceil(opening_price_idr * %g)::bigint)
			WHERE id = $1 RETURNING current_price_idr`, dutchFloorPct), id).Scan(&price); err != nil {
			return err
		}
		return emitAuction(ctx, tx, id, "auction.price", map[string]any{"kind": "price", "currentPriceIdr": price})
	})
	return did, err
}

// closeOne closes one auction whose time is up.
//   - Buyer auctions (owned): bids stay as they are until the owner awards; the owner is told to evaluate.
//   - Market rounds: the best bidder is marked won and everyone else lost; trades are created by the market maker's
//     collective settlement (market maker area), so bidders are told the result, not handed a trade.
//   - Sealed auctions reveal ranks only now (bid.status frames).
func (s *Server) closeOne(ctx context.Context) (bool, error) {
	did := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		id, err := s.claim(ctx, tx, `status IN ('live','extended') AND ends_at <= now()`)
		if err != nil || id == "" {
			return err
		}
		did = true
		r, err := loadAuction(ctx, tx, id, false)
		if err != nil {
			return err
		}
		os, err := offers(ctx, tx, r)
		if err != nil {
			return err
		}
		var clearing, median *int64
		if len(os) > 0 {
			c := os[0].Price
			clearing = &c
			m := os[len(os)/2].Price
			median = &m
		}
		if _, err := tx.Exec(ctx, `UPDATE auctions SET status = 'closed', clearing_price_idr = $2::bigint, median_price_idr = $3::bigint, current_price_idr = coalesce($2::bigint, current_price_idr) WHERE id = $1`,
			id, clearing, median); err != nil {
			return err
		}
		if err := emitAuction(ctx, tx, id, "auction.closed", map[string]any{"kind": "closed", "status": "closed"}); err != nil {
			return err
		}
		if err := emitActivity(ctx, tx, "auction_closed", "Auction ditutup: "+r.Title, nil, r.MarketID); err != nil {
			return err
		}
		owned := r.OwnerUserID != nil || r.OwnerOrgID != nil
		if !owned && len(os) > 0 {
			if _, err := tx.Exec(ctx, `
				UPDATE bids SET status = CASE WHEN bidder_party_id = $2 THEN 'won' ELSE 'lost' END
				WHERE auction_id = $1 AND status <> 'withdrawn'`, id, os[0].PartyID); err != nil {
				return err
			}
		}
		for i, o := range os {
			if o.UserID == nil {
				continue
			}
			n := notification{Type: "auction_ending", Title: r.Title + " ditutup", Href: "/auctions/" + id}
			switch {
			case owned:
				n.Body = "Penawaranmu sedang dievaluasi pembeli."
			case i == 0:
				n.Type, n.Title = "winning_bid", "Kamu memenangkan "+r.Title
				n.Body = fmt.Sprintf("Harga %s/%s. Transaksi dibuat saat market maker melakukan settlement kolektif.", rupiah(o.Price), r.Unit)
			default:
				n.Body = "Bid kamu tidak menang kali ini."
			}
			if err := notify(ctx, tx, *o.UserID, n); err != nil {
				return err
			}
			r.Status = "closed"
			if err := emitBidStatus(ctx, tx, r, *o.UserID); err != nil {
				return err
			}
		}
		if r.OwnerUserID != nil {
			if err := notify(ctx, tx, *r.OwnerUserID, notification{Type: "auction_ending", Title: r.Title + " sudah ditutup",
				Body: fmt.Sprintf("%d bid masuk. Bandingkan penawaran dan tetapkan pemenang.", r.BidCount), Href: "/app/auctions/" + id + "/evaluate"}); err != nil {
				return err
			}
		}
		var marketID, orgID any
		if r.MarketID != nil {
			marketID = *r.MarketID
		}
		if r.OwnerOrgID != nil {
			orgID = *r.OwnerOrgID
		}
		fact, _ := json.Marshal(map[string]any{"code": r.Code, "title": r.Title, "marketId": marketID, "orgId": orgID, "categoryId": r.Category,
			"bidders": len(os), "openingIdr": r.Opening, "clearingIdr": clearing,
			"demandIdr": int64(r.Quantity * float64(r.Opening)), "supplyIdr": 0, "matchedIdr": 0})
		if err := emit(ctx, tx, "auction.closed", id, fact); err != nil {
			return err
		}
		return emitRoundResult(ctx, tx, id) // market rounds only (mm_markets.go): the round's recorded prices
	})
	return did, err
}
