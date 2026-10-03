package analytics

import (
	"context"
	"time"
)

// OrgPurchase is one org_purchase_monthly group: what an org bought in a month (category, item, unit, supplier, channel).
type OrgPurchase struct {
	Month                                    time.Time
	Category, Item, Unit, SupplierParty, Via string
	Spend, Budget, Market, Purchases         uint64
	Quantity                                 float64
}

// OrgPurchases returns every month of an org's purchases, oldest first (a few hundred rows per org at most).
func (c *Client) OrgPurchases(ctx context.Context, orgID string) ([]OrgPurchase, error) {
	rows, err := c.conn.Query(ctx, `
		SELECT month, category_id, item, unit, supplier_party_id, via, sum(spend_idr), sum(budget_idr), sum(market_idr), sum(purchases), sum(quantity)
		FROM org_purchase_monthly WHERE buyer_org_id = ?
		GROUP BY month, category_id, item, unit, supplier_party_id, via ORDER BY month`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgPurchase
	for rows.Next() {
		var p OrgPurchase
		if err := rows.Scan(&p.Month, &p.Category, &p.Item, &p.Unit, &p.SupplierParty, &p.Via, &p.Spend, &p.Budget, &p.Market, &p.Purchases, &p.Quantity); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// OrgTrade is one purchase an org struck (trade at agreement), with the auction result when it came from an auction.
type OrgTrade struct {
	Code                                     string
	At                                       time.Time
	Item, Category, SupplierParty, Unit, Via string
	Quantity                                 float64
	UnitPrice, Value, Opening, Budget        uint64
	Bidders                                  uint32
}

// OrgTrades returns an org's latest `limit` purchases, newest first.
func (c *Client) OrgTrades(ctx context.Context, orgID string, limit int) ([]OrgTrade, error) {
	rows, err := c.conn.Query(ctx, `
		SELECT t.code, t.at, t.item, t.category_id, t.supplier_party_id, t.unit, t.via, t.quantity, t.unit_price_idr, t.value_idr,
		       a.opening_idr, t.budget_unit_idr, a.bidders
		FROM trades AS t
		LEFT JOIN (SELECT auction_id, any(opening_idr) AS opening_idr, any(bidders) AS bidders FROM auction_results GROUP BY auction_id) AS a
		  ON a.auction_id = t.auction_id
		WHERE t.buyer_org_id = ? AND t.status = 'agreement'
		ORDER BY t.at DESC LIMIT ?`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgTrade
	for rows.Next() {
		var t OrgTrade
		if err := rows.Scan(&t.Code, &t.At, &t.Item, &t.Category, &t.SupplierParty, &t.Unit, &t.Via, &t.Quantity, &t.UnitPrice, &t.Value,
			&t.Opening, &t.Budget, &t.Bidders); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
