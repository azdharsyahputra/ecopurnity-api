CREATE DATABASE IF NOT EXISTS ecopurnity;

CREATE TABLE IF NOT EXISTS ecopurnity.events
(
    outbox_id    UInt64,
    topic        LowCardinality(String),
    aggregate_id String,
    occurred_at  DateTime64(3, 'UTC'),
    payload      String CODEC(ZSTD(3)),
    INDEX outbox_id_idx outbox_id TYPE minmax GRANULARITY 1
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (topic, occurred_at, outbox_id);

CREATE TABLE IF NOT EXISTS ecopurnity.bids
(
    outbox_id    UInt64,
    at           DateTime64(3, 'UTC'),
    auction_id   String,
    market_id    String,
    category_id  LowCardinality(String),
    region       LowCardinality(String),
    auction_type LowCardinality(String),
    price_idr    UInt64,
    min_step_idr UInt64,
    bidder_hash  UInt64
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(at)
ORDER BY (auction_id, at, outbox_id);

CREATE MATERIALIZED VIEW IF NOT EXISTS ecopurnity.bids_mv TO ecopurnity.bids AS
SELECT
    outbox_id,
    occurred_at                                  AS at,
    aggregate_id                                 AS auction_id,
    JSONExtractString(payload, 'marketId')       AS market_id,
    JSONExtractString(payload, 'categoryId')     AS category_id,
    JSONExtractString(payload, 'region')         AS region,
    JSONExtractString(payload, 'auctionType')    AS auction_type,
    JSONExtractUInt(payload, 'priceIdr')         AS price_idr,
    JSONExtractUInt(payload, 'minStepIdr')       AS min_step_idr,
    cityHash64(JSONExtractString(payload, 'bidderPartyId')) AS bidder_hash
FROM ecopurnity.events
WHERE topic = 'auction.bid';

CREATE TABLE IF NOT EXISTS ecopurnity.trades
(
    outbox_id         UInt64,
    at                DateTime64(3, 'UTC'),
    trade_id          String,
    code              String,
    status            LowCardinality(String),
    via               LowCardinality(String),
    item              String,
    category_id       LowCardinality(String),
    region            LowCardinality(String),
    market_id         String,
    auction_id        String,
    buyer_party_id    String,
    supplier_party_id String,
    buyer_org_id      String,
    supplier_org_id   String,
    quantity          Float64,
    unit              LowCardinality(String),
    unit_price_idr    UInt64,
    value_idr         UInt64,
    budget_unit_idr   UInt64,
    market_unit_idr   UInt64,
    INDEX buyer_org_idx buyer_org_id TYPE bloom_filter GRANULARITY 1
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(at)
ORDER BY (market_id, at, trade_id);

CREATE MATERIALIZED VIEW IF NOT EXISTS ecopurnity.trades_mv TO ecopurnity.trades AS
SELECT
    outbox_id,
    occurred_at                                        AS at,
    aggregate_id                                       AS trade_id,
    JSONExtractString(payload, 'code')                 AS code,
    if(topic = 'trade.settled', 'settled', JSONExtractString(payload, 'status')) AS status,
    JSONExtractString(payload, 'via')                  AS via,
    JSONExtractString(payload, 'item')                 AS item,
    JSONExtractString(payload, 'categoryId')           AS category_id,
    JSONExtractString(payload, 'region')               AS region,
    JSONExtractString(payload, 'marketId')             AS market_id,
    JSONExtractString(payload, 'auctionId')            AS auction_id,
    JSONExtractString(payload, 'buyerPartyId')         AS buyer_party_id,
    JSONExtractString(payload, 'supplierPartyId')      AS supplier_party_id,
    JSONExtractString(payload, 'buyerOrgId')           AS buyer_org_id,
    JSONExtractString(payload, 'supplierOrgId')        AS supplier_org_id,
    JSONExtractFloat(payload, 'quantity')              AS quantity,
    JSONExtractString(payload, 'unit')                 AS unit,
    JSONExtractUInt(payload, 'unitPriceIdr')           AS unit_price_idr,
    JSONExtractUInt(payload, 'valueIdr')               AS value_idr,
    JSONExtractUInt(payload, 'budgetUnitIdr')          AS budget_unit_idr,
    JSONExtractUInt(payload, 'marketUnitIdr')          AS market_unit_idr
FROM ecopurnity.events
WHERE topic IN ('trade.status', 'trade.settled');

CREATE TABLE IF NOT EXISTS ecopurnity.listings
(
    outbox_id   UInt64,
    at          DateTime64(3, 'UTC'),
    listing_id  String,
    kind        LowCardinality(String),
    category_id LowCardinality(String),
    region      LowCardinality(String),
    item        String,
    quantity    Float64,
    unit        LowCardinality(String),
    value_idr   UInt64,
    party_hash  UInt64
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(at)
ORDER BY (kind, category_id, at, listing_id);

CREATE MATERIALIZED VIEW IF NOT EXISTS ecopurnity.listings_mv TO ecopurnity.listings AS
SELECT
    outbox_id,
    occurred_at                                 AS at,
    aggregate_id                                AS listing_id,
    JSONExtractString(payload, 'kind')          AS kind,
    JSONExtractString(payload, 'categoryId')    AS category_id,
    JSONExtractString(payload, 'region')        AS region,
    JSONExtractString(payload, 'item')          AS item,
    JSONExtractFloat(payload, 'quantity')       AS quantity,
    JSONExtractString(payload, 'unit')          AS unit,
    JSONExtractUInt(payload, 'valueIdr')        AS value_idr,
    cityHash64(JSONExtractString(payload, 'partyId')) AS party_hash
FROM ecopurnity.events
WHERE topic = 'listing.created';

CREATE TABLE IF NOT EXISTS ecopurnity.round_results
(
    outbox_id    UInt64,
    market_id    String,
    round        UInt32,
    auction_id   String,
    title        String,
    status       LowCardinality(String),
    at           DateTime64(3, 'UTC'),
    opening_idr  UInt64,
    current_idr  Nullable(UInt64),
    median_idr   Nullable(UInt64),
    clearing_idr Nullable(UInt64)
)
ENGINE = ReplacingMergeTree(outbox_id)
ORDER BY (market_id, round);

CREATE MATERIALIZED VIEW IF NOT EXISTS ecopurnity.round_results_mv TO ecopurnity.round_results AS
SELECT
    outbox_id,
    aggregate_id                                          AS market_id,
    JSONExtractUInt(payload, 'round')                     AS round,
    JSONExtractString(payload, 'auctionId')               AS auction_id,
    JSONExtractString(payload, 'title')                   AS title,
    JSONExtractString(payload, 'status')                  AS status,
    ifNull(parseDateTime64BestEffortOrNull(JSONExtractString(payload, 'at'), 3, 'UTC'), occurred_at) AS at,
    JSONExtractUInt(payload, 'openingIdr')                AS opening_idr,
    JSONExtract(payload, 'currentIdr', 'Nullable(UInt64)')  AS current_idr,
    JSONExtract(payload, 'medianIdr', 'Nullable(UInt64)')   AS median_idr,
    JSONExtract(payload, 'clearingIdr', 'Nullable(UInt64)') AS clearing_idr
FROM ecopurnity.events
WHERE topic = 'market.round_result';

CREATE TABLE IF NOT EXISTS ecopurnity.auction_results
(
    outbox_id    UInt64,
    at           DateTime64(3, 'UTC'),
    auction_id   String,
    code         String,
    title        String,
    market_id    String,
    org_id       String,
    category_id  LowCardinality(String),
    region       LowCardinality(String),
    bidders      UInt32,
    opening_idr  UInt64,
    clearing_idr Nullable(UInt64),
    demand_idr   UInt64,
    supply_idr   UInt64,
    matched_idr  UInt64
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(at)
ORDER BY (market_id, at, auction_id);

CREATE MATERIALIZED VIEW IF NOT EXISTS ecopurnity.auction_results_mv TO ecopurnity.auction_results AS
SELECT
    outbox_id,
    occurred_at                                 AS at,
    aggregate_id                                AS auction_id,
    JSONExtractString(payload, 'code')          AS code,
    JSONExtractString(payload, 'title')         AS title,
    JSONExtractString(payload, 'marketId')      AS market_id,
    JSONExtractString(payload, 'orgId')         AS org_id,
    JSONExtractString(payload, 'categoryId')    AS category_id,
    JSONExtractString(payload, 'region')        AS region,
    JSONExtractUInt(payload, 'bidders')         AS bidders,
    JSONExtractUInt(payload, 'openingIdr')      AS opening_idr,
    JSONExtract(payload, 'clearingIdr', 'Nullable(UInt64)') AS clearing_idr,
    JSONExtractUInt(payload, 'demandIdr')       AS demand_idr,
    JSONExtractUInt(payload, 'supplyIdr')       AS supply_idr,
    JSONExtractUInt(payload, 'matchedIdr')      AS matched_idr
FROM ecopurnity.events
WHERE topic = 'auction.closed';

CREATE TABLE IF NOT EXISTS ecopurnity.activity
(
    at         DateTime64(3, 'UTC'),
    id         String,
    type       LowCardinality(String),
    title      String,
    amount_idr Nullable(UInt64),
    market_id  String
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(at)
ORDER BY (at, id)
TTL toDateTime(at) + INTERVAL 90 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS ecopurnity.activity_mv TO ecopurnity.activity AS
SELECT
    occurred_at                                AS at,
    aggregate_id                               AS id,
    JSONExtractString(payload, 'type')         AS type,
    JSONExtractString(payload, 'title')        AS title,
    JSONExtract(payload, 'amountIdr', 'Nullable(UInt64)') AS amount_idr,
    JSONExtractString(payload, 'marketId')     AS market_id
FROM ecopurnity.events
WHERE topic = 'activity';

CREATE TABLE IF NOT EXISTS ecopurnity.audit_log
(
    id            UInt64,
    at            DateTime64(3, 'UTC'),
    actor_user_id String,
    actor_label   String,
    action        String,
    entity_type   LowCardinality(String),
    entity_id     String,
    entity_label  String,
    org_id        String,
    market_id     String,
    reason        String,
    changes       String,
    search        String MATERIALIZED lowerUTF8(concat(actor_label, ' ', action, ' ', entity_label, ' ', reason)),
    INDEX search_ngram search TYPE ngrambf_v1(3, 8192, 3, 0) GRANULARITY 1,
    INDEX entity_idx entity_id TYPE bloom_filter GRANULARITY 1,
    INDEX org_idx org_id TYPE bloom_filter GRANULARITY 1,
    INDEX market_idx market_id TYPE bloom_filter GRANULARITY 1
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(at)
ORDER BY (at, id);

CREATE MATERIALIZED VIEW IF NOT EXISTS ecopurnity.audit_log_mv TO ecopurnity.audit_log AS
SELECT
    toUInt64OrZero(aggregate_id)                       AS id,
    occurred_at                                        AS at,
    JSONExtractString(payload, 'actorUserId')          AS actor_user_id,
    JSONExtractString(payload, 'actor')                AS actor_label,
    JSONExtractString(payload, 'action')               AS action,
    JSONExtractString(payload, 'entity', 'type')       AS entity_type,
    JSONExtractString(payload, 'entity', 'id')         AS entity_id,
    JSONExtractString(payload, 'entity', 'label')      AS entity_label,
    JSONExtractString(payload, 'orgId')                AS org_id,
    JSONExtractString(payload, 'marketId')             AS market_id,
    JSONExtractString(payload, 'reason')               AS reason,
    if(JSONHas(payload, 'changes'), JSONExtractRaw(payload, 'changes'), '[]') AS changes
FROM ecopurnity.events
WHERE topic = 'audit';

CREATE TABLE IF NOT EXISTS ecopurnity.trade_daily
(
    category_id LowCardinality(String),
    market_id   String,
    region      LowCardinality(String),
    day         Date,
    volume_idr  UInt64,
    trades      UInt64
)
ENGINE = SummingMergeTree
PARTITION BY toYYYYMM(day)
ORDER BY (category_id, market_id, region, day);

CREATE MATERIALIZED VIEW IF NOT EXISTS ecopurnity.trade_daily_mv TO ecopurnity.trade_daily AS
SELECT category_id, market_id, region, toDate(at) AS day, sum(value_idr) AS volume_idr, count() AS trades
FROM ecopurnity.trades
WHERE status = 'completed'
GROUP BY category_id, market_id, region, day;

CREATE TABLE IF NOT EXISTS ecopurnity.market_price_daily
(
    category_id LowCardinality(String),
    market_id   String,
    day         Date,
    median_state AggregateFunction(quantile(0.5), UInt64),
    low_idr     SimpleAggregateFunction(min, UInt64),
    high_idr    SimpleAggregateFunction(max, UInt64),
    deals       SimpleAggregateFunction(sum, UInt64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(day)
ORDER BY (category_id, market_id, day);

CREATE MATERIALIZED VIEW IF NOT EXISTS ecopurnity.market_price_daily_mv TO ecopurnity.market_price_daily AS
SELECT
    category_id, market_id, toDate(at) AS day,
    quantileState(0.5)(unit_price_idr) AS median_state,
    min(unit_price_idr) AS low_idr, max(unit_price_idr) AS high_idr, count() AS deals
FROM ecopurnity.trades
WHERE status = 'agreement' AND market_id != '' AND unit_price_idr > 0
GROUP BY category_id, market_id, day;

CREATE TABLE IF NOT EXISTS ecopurnity.participants_daily
(
    category_id LowCardinality(String),
    market_id   String,
    region      LowCardinality(String),
    day         Date,
    parties     AggregateFunction(uniq, UInt64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(day)
ORDER BY (category_id, market_id, region, day);

CREATE MATERIALIZED VIEW IF NOT EXISTS ecopurnity.participants_from_bids_mv TO ecopurnity.participants_daily AS
SELECT category_id, market_id, region, toDate(at) AS day, uniqState(bidder_hash) AS parties
FROM ecopurnity.bids
GROUP BY category_id, market_id, region, day;

CREATE MATERIALIZED VIEW IF NOT EXISTS ecopurnity.participants_from_listings_mv TO ecopurnity.participants_daily AS
SELECT category_id, '' AS market_id, region, toDate(at) AS day, uniqState(party_hash) AS parties
FROM ecopurnity.listings
GROUP BY category_id, region, day;

CREATE MATERIALIZED VIEW IF NOT EXISTS ecopurnity.participants_from_trades_mv TO ecopurnity.participants_daily AS
SELECT category_id, market_id, region, toDate(at) AS day, uniqState(cityHash64(party)) AS parties
FROM ecopurnity.trades
ARRAY JOIN [buyer_party_id, supplier_party_id] AS party
WHERE status = 'agreement' AND party != ''
GROUP BY category_id, market_id, region, day;

CREATE TABLE IF NOT EXISTS ecopurnity.org_purchase_monthly
(
    buyer_org_id      String,
    month             Date,
    category_id       LowCardinality(String),
    item              String,
    unit              LowCardinality(String),
    supplier_party_id String,
    via               LowCardinality(String),
    spend_idr         UInt64,
    budget_idr        UInt64,
    market_idr        UInt64,
    quantity          Float64,
    purchases         UInt64
)
ENGINE = SummingMergeTree
ORDER BY (buyer_org_id, month, category_id, item, unit, supplier_party_id, via);

CREATE MATERIALIZED VIEW IF NOT EXISTS ecopurnity.org_purchase_monthly_mv TO ecopurnity.org_purchase_monthly AS
SELECT
    buyer_org_id, toStartOfMonth(at) AS month, category_id, item, unit, supplier_party_id, via,
    sum(value_idr) AS spend_idr,
    sum(toUInt64(round(t.budget_unit_idr * t.quantity))) AS budget_idr,
    sum(toUInt64(round(t.market_unit_idr * t.quantity))) AS market_idr,
    sum(t.quantity) AS quantity,
    count() AS purchases
FROM ecopurnity.trades AS t
WHERE status = 'agreement' AND buyer_org_id != ''
GROUP BY buyer_org_id, month, category_id, item, unit, supplier_party_id, via;

CREATE TABLE IF NOT EXISTS ecopurnity.bid_rate_minute
(
    bidder_hash UInt64,
    minute      DateTime('UTC'),
    auction_id  String,
    bids        UInt64
)
ENGINE = SummingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (bidder_hash, minute, auction_id)
TTL minute + INTERVAL 180 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS ecopurnity.bid_rate_minute_mv TO ecopurnity.bid_rate_minute AS
SELECT bidder_hash, toStartOfMinute(toDateTime(at, 'UTC')) AS minute, auction_id, count() AS bids
FROM ecopurnity.bids
GROUP BY bidder_hash, minute, auction_id;
