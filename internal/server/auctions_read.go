package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

type auctionRow struct {
	ID, Code, Title, Category, Type, Status, Visibility, Item, Spec, Unit string
	MarketID, MarketName, OwnerUserID, OwnerOrgID                         *string
	OrgAuctionID, WithdrawRule                                            *string
	RoundNo                                                               *int32
	Quantity                                                              float64
	Opening, MinStep                                                      int64
	Current                                                               *int64
	StartsAt, EndsAt                                                      time.Time
	ExtWindow, ExtMinutes, Extensions, BidCount, Participants             int32
	LastSeq                                                               int64
	Rules                                                                 []byte
}

const auctionSelect = `
	SELECT a.id, a.code, a.title, a.category_id, a.type, a.status, a.visibility, a.lot_item, a.lot_spec, a.unit,
	       a.market_id::text, m.name, a.owner_user_id::text, a.owner_org_id::text, a.round_no, a.quantity, a.opening_price_idr,
	       a.min_step_idr, a.current_price_idr, a.starts_at, a.ends_at, a.ext_window_minutes, a.ext_minutes, a.extension_count,
	       a.bid_count, a.participant_count, a.last_seq, a.rules, ol.org_auction_id::text, oa.withdraw_rule
	FROM auctions a LEFT JOIN markets m ON m.id = a.market_id
	LEFT JOIN org_auction_lots ol ON ol.auction_id = a.id LEFT JOIN org_auctions oa ON oa.id = ol.org_auction_id`

func scanAuction(row interface{ Scan(...any) error }) (auctionRow, error) {
	var r auctionRow
	err := row.Scan(&r.ID, &r.Code, &r.Title, &r.Category, &r.Type, &r.Status, &r.Visibility, &r.Item, &r.Spec, &r.Unit,
		&r.MarketID, &r.MarketName, &r.OwnerUserID, &r.OwnerOrgID, &r.RoundNo, &r.Quantity, &r.Opening,
		&r.MinStep, &r.Current, &r.StartsAt, &r.EndsAt, &r.ExtWindow, &r.ExtMinutes, &r.Extensions,
		&r.BidCount, &r.Participants, &r.LastSeq, &r.Rules, &r.OrgAuctionID, &r.WithdrawRule)
	return r, err
}

func (r auctionRow) lowerWins() bool { return r.Type == "reverse" || r.Type == "sealed" }

func (r auctionRow) pricePublic() bool { return r.Visibility == "full" || r.Type == "dutch" }

func (r auctionRow) bidderLabel(no int32) string {
	if r.lowerWins() {
		return fmt.Sprintf("Supplier %d", no)
	}
	return fmt.Sprintf("Bidder %d", no)
}

func (r auctionRow) api() api.Auction {
	a := api.Auction{
		Id: r.ID, Code: r.Code, Title: r.Title, CategoryId: api.CategoryId(r.Category), Type: api.AuctionType(r.Type),
		Status: api.AuctionStatus(r.Status), Visibility: api.BidVisibility(r.Visibility), StartsAt: r.StartsAt, EndsAt: r.EndsAt,
		Participants: int(r.Participants), BidCount: int(r.BidCount), OpeningPriceIdr: int(r.Opening),
		MarketId: "", MarketName: "Pengadaan langsung",
	}
	if r.MarketID != nil {
		a.MarketId = *r.MarketID
	}
	if r.MarketName != nil {
		a.MarketName = *r.MarketName
	}
	a.Lot.Item, a.Lot.Spec, a.Lot.Quantity = r.Item, r.Spec, api.Quantity{Value: r.Quantity, Unit: r.Unit}
	if r.Current != nil && r.pricePublic() {
		c := int(*r.Current)
		a.CurrentPriceIdr = &c
	}
	return a
}

func loadAuctions(ctx context.Context, q dbtx, where string, args ...any) ([]api.Auction, error) {
	rows, err := q.Query(ctx, auctionSelect+" WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Auction{}
	for rows.Next() {
		r, err := scanAuction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r.api())
	}
	return out, rows.Err()
}

func loadAuction(ctx context.Context, q dbtx, id string, forUpdate bool) (auctionRow, error) {
	sql := auctionSelect + ` WHERE a.id::text = $1`
	if forUpdate {
		sql += ` FOR UPDATE OF a`
	}
	return scanAuction(q.QueryRow(ctx, sql, id))
}

func auctionDetail(ctx context.Context, q dbtx, r auctionRow, viewerUserID string) (api.AuctionDetail, error) {
	d := api.AuctionDetail{}
	base := r.api()

	b, _ := json.Marshal(base)
	if err := json.Unmarshal(b, &d); err != nil {
		return d, err
	}
	d.MinStepIdr = int(r.MinStep)
	d.Extension.WindowMinutes, d.Extension.ExtendMinutes = int(r.ExtWindow), int(r.ExtMinutes)
	d.Rules = []api.LabeledValue{}
	if len(r.Rules) > 0 {
		if err := json.Unmarshal(r.Rules, &d.Rules); err != nil {
			return d, err
		}
	}
	d.Bids = []api.PublicBid{}
	if r.Visibility != "full" {
		return d, nil
	}
	rows, err := q.Query(ctx, `
		SELECT id, bidder_no, price_idr, created_at, coalesce(bidder_user_id::text = $2, false)
		FROM bids WHERE auction_id = $1 AND status <> 'withdrawn' ORDER BY seq DESC LIMIT 50`, r.ID, viewerUserID)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var bid api.PublicBid
		var no int32
		var price int64
		var mine bool
		if err := rows.Scan(&bid.Id, &no, &price, &bid.At, &mine); err != nil {
			return d, err
		}
		bid.Bidder, bid.PriceIdr = r.bidderLabel(no), int(price)
		if mine {
			bid.Mine = ptr(true)
		}
		d.Bids = append(d.Bids, bid)
	}
	return d, rows.Err()
}
