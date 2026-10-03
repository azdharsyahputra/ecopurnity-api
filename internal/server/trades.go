package server

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"
)

// Trade creation, shared by auction award, Dutch accept, direct orders, contracts and (later) RFQ and settlements. One
// row per trade with both parties; the settlement flow (agreement, invoice, shipments, ...) is trade_engine.go.

type newTrade struct {
	Title                     string
	BuyerParty, SupplierParty string
	Quantity                  float64
	Unit                      string
	UnitPriceIdr              int64
	Terms                     string // escrow (default) | net14 | net30
	MakerFeeRate              float64
	MarketID, AuctionID       *string
	SourceListingID           *string // direct order at a posted price
	DeliveryAddress           string
	DueIn                     time.Duration
	Via                       string // analytics channel: auction | dutch | rfq | contract | direct | settlement
	Category, Region          string
	ActorUserID               *string
	// Org sides and price references for org purchase analytics (ClickHouse org_purchase_monthly); optional.
	BuyerOrgID, SupplierOrgID    *string
	BudgetUnitIdr, MarketUnitIdr int64
	Item                         string // the analytics item (groups unit prices and trends); default Title
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
		                    market_id, auction_id, source_listing_id, delivery_address, due_at)
		VALUES ($1, $2, $3, $4::numeric, $5, $6::bigint, round($4::numeric * $6::bigint)::bigint, $7, $8, $9, $10, $11, $12, now() + $13)
		RETURNING id, code`,
		t.Title, t.BuyerParty, t.SupplierParty, t.Quantity, t.Unit, t.UnitPriceIdr, t.Terms, t.MakerFeeRate,
		t.MarketID, t.AuctionID, t.SourceListingID, t.DeliveryAddress, t.DueIn).Scan(&out.ID, &out.Code); err != nil {
		return out, err
	}
	// The purchase order every trade starts with (TransactionDetail.documents, rendered on demand).
	if _, err := q.Exec(ctx, `INSERT INTO trade_documents (trade_id, kind, name, uploaded_by) VALUES ($1, 'order', $2, $3)`,
		out.ID, "PO-"+strings.TrimPrefix(out.Code, "TRX-")+".pdf", t.ActorUserID); err != nil {
		return out, err
	}
	if _, err := q.Exec(ctx, `INSERT INTO trade_events (trade_id, status, note, actor_user_id) VALUES ($1, 'agreement', 'Transaksi dibuat', $2)`,
		out.ID, t.ActorUserID); err != nil {
		return out, err
	}
	payload, _ := json.Marshal(map[string]any{
		"code": out.Code, "status": "agreement", "via": t.Via, "item": nonEmpty(t.Item, t.Title), "categoryId": t.Category, "region": t.Region,
		"marketId": t.MarketID, "auctionId": t.AuctionID, "buyerPartyId": t.BuyerParty, "supplierPartyId": t.SupplierParty,
		"quantity": t.Quantity, "unit": t.Unit, "unitPriceIdr": t.UnitPriceIdr, "valueIdr": total,
		"buyerOrgId": t.BuyerOrgID, "supplierOrgId": t.SupplierOrgID, "budgetUnitIdr": t.BudgetUnitIdr, "marketUnitIdr": t.MarketUnitIdr,
	})
	if err := emit(ctx, q, "trade.status", out.ID, payload); err != nil {
		return out, err
	}
	return out, emitTradeUpdated(ctx, q, out.ID) // every creation path tells both sides' users
}
