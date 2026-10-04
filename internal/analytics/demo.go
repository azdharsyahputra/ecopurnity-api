package analytics

import (
	"context"
	"fmt"
	"strings"
)

type DemoIDs struct {
	Parties, Orgs, Markets, Auctions, Trades, Listings, Other, Titles []string
}

func orNone(xs []string) []string {
	if len(xs) == 0 {
		return []string{"-"}
	}
	return xs
}

const demoTagged = `(SELECT aggregate_id FROM events WHERE JSONExtractBool(payload, 'demo'))`

func (c *Client) PurgeDemo(ctx context.Context, ids DemoIDs) error {
	p, o, m, a, t, l := orNone(ids.Parties), orNone(ids.Orgs), orNone(ids.Markets), orNone(ids.Auctions), orNone(ids.Trades), orNone(ids.Listings)
	all := orNone(append(append(append(append(append([]string{}, ids.Other...), ids.Markets...), ids.Auctions...), ids.Trades...), ids.Listings...))
	titles := orNone(ids.Titles)
	steps := []struct {
		sql  string
		args []any
	}{
		{`ALTER TABLE bids DELETE WHERE auction_id IN ` + demoTagged + ` OR auction_id IN ? OR bidder_hash IN (SELECT cityHash64(arrayJoin(?)))`, []any{a, p}},
		{`ALTER TABLE bid_rate_minute DELETE WHERE auction_id IN ` + demoTagged + ` OR auction_id IN ?`, []any{a}},
		{`ALTER TABLE trades DELETE WHERE trade_id IN ` + demoTagged + ` OR trade_id IN ? OR buyer_party_id IN ? OR supplier_party_id IN ? OR market_id IN ?`, []any{t, p, p, m}},
		{`ALTER TABLE listings DELETE WHERE listing_id IN ` + demoTagged + ` OR listing_id IN ? OR party_hash IN (SELECT cityHash64(arrayJoin(?)))`, []any{l, p}},
		{`ALTER TABLE round_results DELETE WHERE market_id IN ` + demoTagged + ` OR market_id IN ?`, []any{m}},
		{`ALTER TABLE auction_results DELETE WHERE auction_id IN ` + demoTagged + ` OR auction_id IN ? OR market_id IN ? OR org_id IN ?`, []any{a, m, o}},
		{`ALTER TABLE activity DELETE WHERE id IN ` + demoTagged + ` OR market_id IN ? OR title IN ?`, []any{m, titles}},
		{`ALTER TABLE audit_log DELETE WHERE entity_id IN ? OR org_id IN ? OR market_id IN ? OR actor_user_id IN ?`, []any{all, o, m, all}},
		{`ALTER TABLE events DELETE WHERE JSONExtractBool(payload, 'demo') OR aggregate_id IN ?
		  OR (topic = 'activity' AND (JSONExtractString(payload, 'title') IN ? OR JSONExtractString(payload, 'marketId') IN ?))`, []any{all, titles, m}},
		{`TRUNCATE TABLE trade_daily`, nil},
		{`INSERT INTO trade_daily SELECT category_id, market_id, region, toDate(at) AS day, sum(value_idr), count() FROM trades
		  WHERE status = 'completed' GROUP BY category_id, market_id, region, day`, nil},
		{`TRUNCATE TABLE market_price_daily`, nil},
		{`INSERT INTO market_price_daily SELECT category_id, market_id, toDate(at) AS day, quantileState(0.5)(unit_price_idr), min(unit_price_idr),
		  max(unit_price_idr), count() FROM trades WHERE status = 'agreement' AND market_id != '' AND unit_price_idr > 0 GROUP BY category_id, market_id, day`, nil},
		{`TRUNCATE TABLE participants_daily`, nil},
		{`INSERT INTO participants_daily SELECT category_id, market_id, region, toDate(at) AS day, uniqState(bidder_hash) FROM bids
		  GROUP BY category_id, market_id, region, day`, nil},
		{`INSERT INTO participants_daily SELECT category_id, '', region, toDate(at) AS day, uniqState(party_hash) FROM listings GROUP BY category_id, region, day`, nil},
		{`INSERT INTO participants_daily SELECT category_id, market_id, region, toDate(at) AS day, uniqState(cityHash64(party)) FROM trades
		  ARRAY JOIN [buyer_party_id, supplier_party_id] AS party WHERE status = 'agreement' AND party != '' GROUP BY category_id, market_id, region, day`, nil},
		{`TRUNCATE TABLE org_purchase_monthly`, nil},
		{`INSERT INTO org_purchase_monthly SELECT buyer_org_id, toStartOfMonth(at) AS month, category_id, item, unit, supplier_party_id, via,
		  sum(value_idr), sum(toUInt64(round(t.budget_unit_idr * t.quantity))), sum(toUInt64(round(t.market_unit_idr * t.quantity))), sum(t.quantity), count()
		  FROM trades AS t WHERE status = 'agreement' AND buyer_org_id != '' GROUP BY buyer_org_id, month, category_id, item, unit, supplier_party_id, via`, nil},
	}
	for _, s := range steps {
		q := s.sql
		if strings.HasPrefix(q, "ALTER") {
			q += ` SETTINGS mutations_sync = 2`
		}
		if err := c.conn.Exec(ctx, q, s.args...); err != nil {
			return fmt.Errorf("%s: %w", strings.Fields(s.sql)[2], err)
		}
	}
	return nil
}

func (c *Client) DemoFacts(ctx context.Context) (uint64, error) {
	var n uint64
	err := c.conn.QueryRow(ctx, `SELECT count() FROM events WHERE JSONExtractBool(payload, 'demo')`).Scan(&n)
	return n, err
}
