# ClickHouse analytics

`00001_init.sql` creates database `ecopurnity`. Everything here is derived from the Postgres `outbox` and can be dropped
and replayed. Postgres stays the system of record. Apply it with any client that splits on `;`. Every statement is
`IF NOT EXISTS`, so applying it again is a no-op.

```
outbox (Postgres) --publisher--> events --MV--> bids / trades / listings / round_results / auction_results / activity / audit_log
                                                 bids   --MV--> participants_daily, bid_rate_minute
                                                 trades --MV--> trade_daily, market_price_daily, participants_daily, org_purchase_monthly
                                                 listings --MV--> participants_daily
```

## Tables

| Table | Engine, key | What it answers | Read by |
| --- | --- | --- | --- |
| `events` | ReplacingMergeTree `(topic, occurred_at, outbox_id)`, month partitions | Raw outbox log. Counts per topic over time (opportunities detected, signups). Replay source for every other table. | `/public/stats`, `/explorer/overview` (opportunities), publisher dedupe check |
| `bids` | MergeTree `(auction_id, at)` | Every bid, in time order per auction: distinct bidders, bid runs, price jumps | admin auction findings, `/admin/alerts` engine |
| `trades` | MergeTree `(market_id, at, trade_id)`, bloom on `buyer_org_id` | One row per trade status change (plus `settled`) with the full trade snapshot | `/mm/analytics` growth, `/orgs/{orgId}/analytics` auctions + history |
| `listings` | MergeTree `(kind, category_id, at)` | Supply and demand listings as created, with value | `/explorer/overview` demandSupply, `/explorer/{side}` |
| `round_results` | ReplacingMergeTree(outbox_id) `(market_id, round)` | Latest state of each market round (RoundResult). Read with `FINAL` | `/mm/analytics` priceDiscovery |
| `auction_results` | MergeTree `(market_id, at, auction_id)` | Closed auctions: bidders, opening vs clearing, demand/supply/matched value | `/mm/analytics` efficiency, `/orgs/{orgId}/analytics` auctions |
| `activity` | MergeTree `(at, id)`, TTL 90 days | Public activity feed (ActivityEvent) | `/public/activity`, `/markets/{id}` activity |
| `audit_log` | ReplacingMergeTree `(at, id)`, ngram index on text, bloom on entity/org/market, no TTL | Audit trail mirror: newest first, by entity, by org/market, substring search | `/admin/audit`, `/admin/alerts/{id}` case history |
| `trade_daily` | SummingMergeTree `(category_id, market_id, region, day)` | Completed-trade volume and count per day | `/public/stats`, `/explorer/overview` volume + deltas |
| `market_price_daily` | AggregatingMergeTree `(category_id, market_id, day)` | Deal unit price per market per day: median state, low, high | `/markets/{id}` priceHistory, `/explorer/overview` priceIndex |
| `participants_daily` | AggregatingMergeTree `(category_id, market_id, region, day)` | Active parties (uniq state) per day: bid, listed or struck a deal | `/public/stats`, `/explorer/overview` deltas, `/mm/analytics` growth |
| `org_purchase_monthly` | SummingMergeTree `(buyer_org_id, month, category_id, item, unit, supplier_party_id, via)` | What an org bought per month: spend, budget, market reference, quantity | `/orgs/{orgId}/analytics` spend, savings, unitPrices, priceTrend, demand, suppliers |
| `bid_rate_minute` | SummingMergeTree `(bidder_hash, minute, auction_id)`, TTL 180 days | Bids per bidder per minute across auctions (abnormal bidding rate) | `/admin/alerts` engine |

Not here, on purpose: market buyers/suppliers, liquidity and `byMarket` in `/mm/analytics` are current membership counts
(Postgres `market_participants`). Reputation `trend` replays one party's finished trades with disputes and ratings
(`src/domain/reputation.ts`). That is a few dozen rows per party in Postgres, so it stays there. Fraud alerts themselves
live in Postgres `fraud_alerts`. ClickHouse only supplies the features the detection engine scores.

## Key choices

- **Facts are sorted by their main reader.** Bids by `auction_id` because every fraud check reads one auction in order.
  Trades by `market_id` because market-maker growth and price history are per market. Org reads use the bloom index and
  the org aggregate. Listings by `kind, category_id` because the explorer filters by those.
- **Aggregates are keyed `(category_id, market_id, ...)`.** The explorer filters by category and mm analytics filters by
  market. `category_id` has 7 values, so a market-only filter still prunes through generic exclusion search on the
  second column. `day` comes last so a range read is one contiguous slice per key.
- **Partitions are monthly** on every time-based table. Partitions stay small (the volume is thousands of rows a day), a
  bad month can be dropped and replayed, and TTL drops whole old parts. `round_results` and `org_purchase_monthly` are
  tiny, so they have no partitions.
- **`activity` and `audit_log` are sorted by time** because their hot read is "newest N". `ORDER BY at DESC LIMIT n`
  reads in order from the end. Entity and org lookups on the audit mirror use bloom indexes. Text search uses an
  `ngrambf_v1(3)` index on a materialized lowercase `search` column: actor, action, entity label and reason. Tested
  against 400k rows: searching `frd-1041` reads 1 of 50 granules.
- **Volume and trade counts use `completed`.** It is final and never reverses. Prices, participants and org spend use
  `agreement`, because the price and parties are fixed when the deal is struck (the mock counts org purchases at award).
  To change either, edit the `WHERE status = ...` of one view and replay.
- **Price history comes from deal prices, not bids.** Bids would leak sealed bids through a public median.

## Dedupe (at-least-once outbox)

Each materialized view runs on every insert block. A duplicate outbox row therefore double-counts every fact and
aggregate downstream. Merges cannot undo that. Only `events` heals itself: it is a ReplacingMergeTree keyed on the
outbox row, so a duplicate disappears on merge. That was checked: a duplicate was inserted, `OPTIMIZE` collapsed the
raw table back to 33 rows, and `activity` kept the extra row. The publisher therefore must never insert an outbox id
twice:

1. One publisher at a time (`pg_try_advisory_lock`).
2. Read a batch: `SELECT ... FROM outbox WHERE ch_published_at IS NULL AND topic <> 'rt' ORDER BY id LIMIT 500 FOR
   UPDATE SKIP LOCKED`. (`published_at` is the realtime side's marker; `rt` frames never come here, see
   `migrations/postgres/00008_outbox_replay.sql`.)
3. Drop ids already in ClickHouse: `SELECT outbox_id FROM events WHERE outbox_id IN (...)`. The minmax index on
   `outbox_id` makes this a few-granule read. It covers a crash between the insert and step 5, and an insert that timed
   out but landed.
4. `INSERT INTO events (outbox_id, topic, aggregate_id, occurred_at, payload)` with the rest. `occurred_at` =
   `outbox.created_at`, `payload` = `payload::text`.
5. `UPDATE outbox SET ch_published_at = now() WHERE id = ANY(...)`.

If a view throws mid-insert (a schema bug), the events row exists but some facts may be missing. To fix it, replay that
month from `events`:
`ALTER TABLE x DROP PARTITION 202610`, then `INSERT INTO x SELECT ... FROM events WHERE ...` using the view's SELECT.
The same recipe rebuilds everything after a schema change.

## Payload contract (outbox `payload`, camelCase)

`aggregate_id` is the id of the entity. Missing optional fields read as `''` or `0` (`NULL` where marked nullable).

| Topic | aggregate_id | payload |
| --- | --- | --- |
| `auction.bid` | auction id | `marketId?, categoryId, region, auctionType, priceIdr` (unit), `minStepIdr, bidderPartyId` |
| `trade.status` | trade id | trade snapshot: `code, status, via, item, categoryId, region, marketId?, auctionId?, buyerPartyId, supplierPartyId, buyerOrgId?, supplierOrgId?, quantity, unit, unitPriceIdr, valueIdr, budgetUnitIdr?, marketUnitIdr?` |
| `trade.settled` | trade id | same snapshot (stored as status `settled`) |
| `listing.created` | listing id | `kind` (supply/demand), `categoryId, region, item, quantity, unit, valueIdr` (quantity x indicative price), `partyId` |
| `opportunity.detected` | opportunity id | `categoryId, region` (read from `events` directly) |
| `market.round_result` | market id | RoundResult: `round, auctionId?, title, status, at, openingIdr, currentIdr?, medianIdr?, clearingIdr?`. Re-sent on change, newest wins |
| `auction.closed` | auction id | `code, title, marketId?, orgId?, categoryId, region, bidders, openingIdr, clearingIdr?, demandIdr, supplyIdr, matchedIdr` (values = quantity x reference price) |
| `activity` | activity id | ActivityEvent: `type, title, amountIdr?, marketId?` |
| `audit` | `audit_log.id` | AuditEntry: `actorUserId?, actor, action, entity{type,id,label}, orgId?, marketId?, reason?, changes[]` |
| `user.signup`, `chat.message` | user / conversation id | metadata only, never message bodies. Stays in `events` |

Imported org purchase history is published as `trade.status` events with status `agreement`, so it lands in
`org_purchase_monthly` like live purchases. Party ids are hashed (`cityHash64`) in `bids`, `listings` and
`participants_daily`. That makes them pseudonymous, not anonymous, which is fine because only the server reads these
tables.

## Example reads

Run them as written with `clickhouse-client --database ecopurnity --param_days=30 ...`. The Go client binds the same
`{name:Type}` parameters. An empty `{category}` means all categories. A delta of `NULL` means there was nothing in the
previous period, and the API returns `0`. The Go handler fills missing days and weeks with zeros (or carries the last
index forward) and pivots the long rows into the API shape.

### GET /public/stats (also `ExplorerOverview.stats`)

```sql
SELECT
    (SELECT uniqMerge(parties) FROM participants_daily WHERE day > today() - 30)                      AS activeParticipants,
    (SELECT uniqExact(market_id) FROM participants_daily WHERE day > today() - 30 AND market_id != '') AS activeMarkets,
    (SELECT count() FROM events WHERE topic = 'opportunity.detected')                                 AS opportunitiesDetected,
    (SELECT sum(volume_idr) FROM trade_daily)                                                          AS transactionVolumeIdr
```

These are proposed definitions: participants and markets are those active in the last 30 days, opportunities and volume
are totals since launch. If the product wants the number of markets with `status = 'active'`, take `activeMarkets` from
Postgres instead.

### GET /public/activity (and `/markets/{id}` activity: add `WHERE market_id = {market:String}`)

```sql
SELECT id, type, title, amount_idr AS amountIdr, at FROM activity ORDER BY at DESC LIMIT {n:UInt16}
```

### GET /explorer/overview?range=7d|30d|90d&category=

```sql
-- deltas: this period vs the previous one of the same length
WITH {days:UInt16} AS n, {category:String} AS c
SELECT
    (SELECT uniqMergeIf(parties, day > today() - n) / nullIf(uniqMergeIf(parties, day <= today() - n), 0) - 1
       FROM participants_daily WHERE day > today() - 2 * n AND (c = '' OR category_id = c))                AS participants,
    (SELECT uniqExactIf(market_id, day > today() - n) / nullIf(uniqExactIf(market_id, day <= today() - n), 0) - 1
       FROM participants_daily WHERE day > today() - 2 * n AND market_id != '' AND (c = '' OR category_id = c)) AS markets,
    (SELECT countIf(occurred_at > now() - toIntervalDay(n)) / nullIf(countIf(occurred_at <= now() - toIntervalDay(n)), 0) - 1
       FROM events WHERE topic = 'opportunity.detected' AND occurred_at > now() - toIntervalDay(2 * n)
        AND (c = '' OR JSONExtractString(payload, 'categoryId') = c))                                       AS opportunities,
    (SELECT sumIf(volume_idr, day > today() - n) / nullIf(sumIf(volume_idr, day <= today() - n), 0) - 1
       FROM trade_daily WHERE day > today() - 2 * n AND (c = '' OR category_id = c))                       AS volume;

-- volume: one row per day, gaps filled with 0
SELECT day AS date, sum(volume_idr) AS volumeIdr
FROM trade_daily
WHERE day > today() - {days:UInt16} AND ({category:String} = '' OR category_id = {category:String})
GROUP BY day
ORDER BY day WITH FILL FROM today() - {days:UInt16} + 1 TO today() + 1;

-- priceIndex: each market's daily median relative to its first day in range, averaged per category (start = 100)
SELECT day AS date, category_id, round(100 * avg(median / base), 1) AS idx
FROM
(
    SELECT category_id, market_id, day, median,
           first_value(median) OVER (PARTITION BY market_id ORDER BY day) AS base
    FROM
    (
        SELECT category_id, market_id, day, quantileMerge(0.5)(median_state) AS median
        FROM market_price_daily
        WHERE day > today() - {days:UInt16} AND ({category:String} = '' OR category_id = {category:String})
        GROUP BY category_id, market_id, day
    )
)
GROUP BY date, category_id
ORDER BY date, category_id;

-- demandSupply: listed value in range
SELECT category_id AS categoryId, sumIf(value_idr, kind = 'demand') AS demandIdr, sumIf(value_idr, kind = 'supply') AS supplyIdr
FROM listings
WHERE at > now() - toIntervalDay({days:UInt16}) AND ({category:String} = '' OR category_id = {category:String})
GROUP BY category_id
ORDER BY demandIdr DESC;
```

### GET /explorer/{side} (AggregateRow, `trend` = last 30 days vs the 30 before)

```sql
SELECT category_id AS categoryId, item, region, sum(quantity) AS quantity, unit,
       countIf(at > now() - INTERVAL 30 DAY) AS listings,
       round(listings / nullIf(countIf(at <= now() - INTERVAL 30 DAY), 0) - 1, 2) AS trend
FROM listings
WHERE kind = {side:String} AND at > now() - INTERVAL 60 DAY AND ({category:String} = '' OR category_id = {category:String})
GROUP BY categoryId, item, region, unit
ORDER BY listings DESC
```

### GET /markets/{id}: priceHistory (12 weeks, oldest first)

```sql
SELECT toMonday(day) AS week, round(quantileMerge(0.5)(median_state)) AS medianIdr, min(low_idr) AS lowIdr, max(high_idr) AS highIdr
FROM market_price_daily
WHERE market_id = {market:String} AND day >= toMonday(today()) - 77
GROUP BY week
ORDER BY week
```

### GET /mm/analytics (`{markets}` = the caller's operated market ids, or the one `?market=`)

```sql
-- efficiency: value-weighted matched demand and supply utilization per week (8 weeks)
SELECT toMonday(at) AS week,
       sum(matched_idr) / nullIf(sum(demand_idr), 0) AS matched, 1 - matched AS unmatched,
       sum(matched_idr) / nullIf(sum(supply_idr), 0) AS utilization
FROM auction_results
WHERE market_id IN {markets:Array(String)} AND at >= toMonday(today()) - 49
GROUP BY week
ORDER BY week;

-- growth: active parties, completed trades, distinct buyer-supplier pairs, trades by a pair that traded before
WITH toMonday(today()) - 49 AS since
SELECT week, participants, transactions, connections, repeat
FROM
(
    SELECT toMonday(day) AS week, uniqMerge(parties) AS participants
    FROM participants_daily
    WHERE market_id IN {markets:Array(String)} AND day >= since
    GROUP BY week
) AS p
FULL JOIN
(
    SELECT week, count() AS transactions, uniqExact(buyer_party_id, supplier_party_id) AS connections, countIf(nth > 1) AS repeat
    FROM
    (
        SELECT toMonday(at) AS week, buyer_party_id, supplier_party_id,
               row_number() OVER (PARTITION BY buyer_party_id, supplier_party_id ORDER BY at) AS nth
        FROM trades
        WHERE market_id IN {markets:Array(String)} AND status = 'completed'
    )
    WHERE week >= since
    GROUP BY week
) AS t USING (week)
ORDER BY week;

-- priceDiscovery: rounds per market
SELECT market_id, round, auction_id, title, status, at, opening_idr, current_idr, median_idr, clearing_idr
FROM round_results FINAL
WHERE market_id IN {markets:Array(String)}
ORDER BY market_id, round;
```

### GET /orgs/{orgId}/analytics?months=&category=

Every block starts with the same `since` (the oldest of the latest `months` months that have data):

```sql
WITH (SELECT min(month) FROM (SELECT DISTINCT month FROM org_purchase_monthly
        WHERE buyer_org_id = {org:String} ORDER BY month DESC LIMIT {months:UInt8})) AS since
-- spend (pivot category_id into keys)
SELECT month, category_id, sum(spend_idr) AS spendIdr
FROM org_purchase_monthly
WHERE buyer_org_id = {org:String} AND month >= since AND ({category:String} = '' OR category_id = {category:String})
GROUP BY month, category_id ORDER BY month, category_id;

-- savings:      SELECT month, sum(spend_idr) AS spendIdr, sum(budget_idr) AS budgetIdr, sum(market_idr) AS marketIdr ... GROUP BY month
-- unitPrices:   SELECT item, any(unit) AS unit, round(sum(spend_idr) / sum(quantity)) AS avgIdr,
--                      round(sum(market_idr) / sum(quantity)) AS marketIdr ... GROUP BY item ORDER BY sum(spend_idr) DESC
-- suppliers:    SELECT supplier_party_id, sum(spend_idr) AS spendIdr ... GROUP BY supplier_party_id ORDER BY spendIdr DESC
--               (name, score, onTime come from Postgres suppliers / org_suppliers)

-- priceTrend + demand for the biggest item
WITH
    (SELECT min(month) FROM (SELECT DISTINCT month FROM org_purchase_monthly
       WHERE buyer_org_id = {org:String} ORDER BY month DESC LIMIT {months:UInt8})) AS since,
    (SELECT argMax(item, s) FROM (SELECT item, sum(spend_idr) AS s FROM org_purchase_monthly
       WHERE buyer_org_id = {org:String} AND month >= since AND ({category:String} = '' OR category_id = {category:String})
       GROUP BY item)) AS top
SELECT month, top AS top_item,
       sumIf(spend_idr, item = top) / nullIf(sumIf(quantity, item = top), 0)  AS ours_unit,
       sumIf(market_idr, item = top) / nullIf(sumIf(quantity, item = top), 0) AS market_unit,
       round(100 * ours_unit / first_value(ours_unit) OVER (ORDER BY month), 1)     AS ours,
       round(100 * market_unit / first_value(market_unit) OVER (ORDER BY month), 1) AS market,
       sumIf(quantity, item = top) AS top_quantity,
       sum(purchases)              AS requests
FROM org_purchase_monthly
WHERE buyer_org_id = {org:String} AND month >= since AND ({category:String} = '' OR category_id = {category:String})
GROUP BY month ORDER BY month;

-- auctions (latest 8)
SELECT t.code, concat(t.item, ' · ', formatDateTime(t.at, '%Y-%m')) AS title, a.bidders,
       a.opening_idr AS openingIdr, t.unit_price_idr AS clearingIdr
FROM trades AS t LEFT JOIN auction_results AS a ON a.auction_id = t.auction_id
WHERE t.buyer_org_id = {org:String} AND t.status = 'agreement' AND t.via = 'auction' AND t.at >= since
  AND ({category:String} = '' OR t.category_id = {category:String})
ORDER BY t.at DESC LIMIT 8;

-- history
SELECT code, formatDateTime(at, '%Y-%m') AS month, item, category_id, supplier_party_id, quantity, unit,
       unit_price_idr, value_idr AS totalIdr, via
FROM trades
WHERE buyer_org_id = {org:String} AND status = 'agreement' AND at >= since
  AND ({category:String} = '' OR category_id = {category:String})
ORDER BY at DESC;
```

### GET /admin/audit, /admin/alerts/{id} case history

```sql
SELECT id, actor_label, action, entity_type, entity_id, entity_label, at, reason, changes
FROM audit_log FINAL ORDER BY at DESC LIMIT 500;

-- case history: by entity id, not the mock's substring match on the code
SELECT * FROM audit_log WHERE entity_type = 'alert' AND entity_id = {alert:String} ORDER BY at DESC;

-- free-text search (uses the ngram index)
SELECT * FROM audit_log WHERE search LIKE concat('%', lowerUTF8({q:String}), '%') ORDER BY at DESC LIMIT 100;
```

### Fraud features (admin auction findings, `/admin/alerts` engine)

```sql
-- "Bid beruntun": one bidder with 3 or more bids in a row in an auction
SELECT auction_id, bidder_hash, count() AS run_length, min(at) AS from_at
FROM
(
    SELECT *, sum(new_run) OVER (PARTITION BY auction_id ORDER BY at ROWS UNBOUNDED PRECEDING) AS run_id
    FROM
    (
        SELECT auction_id, at, bidder_hash,
               bidder_hash != lagInFrame(bidder_hash, 1, 0) OVER (PARTITION BY auction_id ORDER BY at
                                                                  ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) AS new_run
        FROM bids WHERE auction_id = {auction:String}
    )
)
GROUP BY auction_id, bidder_hash, run_id
HAVING run_length >= 3;

-- "Lonjakan harga": first jump between consecutive bids larger than 20x the minimum step
SELECT at, prev_idr, price_idr, abs(toInt64(price_idr) - toInt64(prev_idr)) AS diff_idr
FROM
(
    SELECT at, price_idr, min_step_idr,
           lagInFrame(price_idr) OVER (ORDER BY at ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) AS prev_idr,
           row_number() OVER (ORDER BY at) AS nth
    FROM bids WHERE auction_id = {auction:String}
)
WHERE nth > 1 AND diff_idr > 20 * greatest(min_step_idr, 1)
ORDER BY at LIMIT 1;

-- per-auction summary
SELECT count() AS bids, uniqExact(bidder_hash) AS bidders, min(price_idr), max(price_idr) FROM bids WHERE auction_id = {auction:String};

-- abnormal_bidding: peak bids per minute per bidder in the last day, and how many auctions they hit
SELECT bidder_hash, max(per_minute) AS peak_per_minute, sum(per_minute) AS bids, uniqExact(auction_id) AS auctions
FROM (SELECT bidder_hash, minute, auction_id, sum(bids) AS per_minute
      FROM bid_rate_minute WHERE minute > now() - INTERVAL 1 DAY GROUP BY bidder_hash, minute, auction_id)
GROUP BY bidder_hash
HAVING peak_per_minute >= 10
ORDER BY peak_per_minute DESC;
```

The engine maps `bidder_hash` back to a party by hashing candidate party ids. Postgres keeps the real link.
