package server

import (
	"context"
	"encoding/json"
	"math"
	"time"
)

// Trade creation, shared by auction award, Dutch accept and (later) RFQ, contracts and settlements. One row per trade
// with both parties; the full settlement flow (agreement, invoice, shipments, ...) lives with the transactions area.

type newTrade struct {
	Title                     string
	BuyerParty, SupplierParty string
	Quantity                  float64
	Unit                      string
	UnitPriceIdr              int64
	Terms                     string // escrow (default) | net14 | net30
	MakerFeeRate              float64
	MarketID, AuctionID       *string
	DeliveryAddress           string
	DueIn                     time.Duration
	Via                       string // analytics channel: auction | dutch | rfq | contract | direct | settlement
	Category, Region          string
	ActorUserID               *string
	// Org sides and price references for org purchase analytics (ClickHouse org_purchase_monthly); optional.
	BuyerOrgID, SupplierOrgID    *string
	BudgetUnitIdr, MarketUnitIdr int64
}

type createdTrade struct{ ID, Code string }

func createTrade(ctx context.Context, q dbtx, t newTrade) (createdTrade, error) {
	if t.Terms == "" {
		t.Terms = "escrow"
	}
	if t.DueIn == 0 {
		t.DueIn = 14 * 24 * time.Hour
	}
	total := int64(math.Round(t.Quantity * float64(t.UnitPriceIdr)))
	var out createdTrade
	if err := q.QueryRow(ctx, `
		INSERT INTO trades (title, buyer_party_id, supplier_party_id, quantity, unit, unit_price_idr, total_idr, terms, maker_fee_rate,
		                    market_id, auction_id, delivery_address, due_at)
		VALUES ($1, $2, $3, $4::numeric, $5, $6::bigint, round($4::numeric * $6::bigint)::bigint, $7, $8, $9, $10, $11, now() + $12)
		RETURNING id, code`,
		t.Title, t.BuyerParty, t.SupplierParty, t.Quantity, t.Unit, t.UnitPriceIdr, t.Terms, t.MakerFeeRate,
		t.MarketID, t.AuctionID, t.DeliveryAddress, t.DueIn).Scan(&out.ID, &out.Code); err != nil {
		return out, err
	}
	if _, err := q.Exec(ctx, `INSERT INTO trade_events (trade_id, status, note, actor_user_id) VALUES ($1, 'agreement', 'Transaksi dibuat', $2)`,
		out.ID, t.ActorUserID); err != nil {
		return out, err
	}
	payload, _ := json.Marshal(map[string]any{
		"code": out.Code, "status": "agreement", "via": t.Via, "item": t.Title, "categoryId": t.Category, "region": t.Region,
		"marketId": t.MarketID, "auctionId": t.AuctionID, "buyerPartyId": t.BuyerParty, "supplierPartyId": t.SupplierParty,
		"quantity": t.Quantity, "unit": t.Unit, "unitPriceIdr": t.UnitPriceIdr, "valueIdr": total,
		"buyerOrgId": t.BuyerOrgID, "supplierOrgId": t.SupplierOrgID, "budgetUnitIdr": t.BudgetUnitIdr, "marketUnitIdr": t.MarketUnitIdr,
	})
	return out, emit(ctx, q, "trade.status", out.ID, payload)
}
