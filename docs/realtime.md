# Realtime (WebSocket)

The machine-readable contract is [`api/asyncapi.yaml`](../api/asyncapi.yaml) (AsyncAPI 3.0). Payload schemas that
already exist in the REST contract are referenced from [`api/openapi.yaml`](../api/openapi.yaml). This page explains
how the pieces fit together.

Ground rules:

- **One socket per tab**: `GET /api/v1/ws`, same origin, same `ecp_session` cookie as REST. Logical channels are
  multiplexed over it.
- **Writes stay REST.** Placing a bid, accepting a Dutch price, withdrawing and every other state change go through
  REST, where validation, KYC limits and idempotency already live. The socket only fans out. Chat is the one exception:
  `chat.send` is accepted on the socket, with `POST /me/conversations/{id}/messages` as the fallback.
- **The database is the source of truth, the socket is a hint stream.** Every event can be recovered from REST,
  and the sequenced channels can also be replayed from the Postgres outbox. Delivery is at-least-once, so clients dedupe.

## Connection lifecycle

1. **Connect.** The browser opens `wss://<host>/api/v1/ws`. The server checks `Origin` against the allowed origins
   (HTTP 403 otherwise, protection against cross-site WebSocket hijacking) and reads the session cookie:
   - valid session: the connection belongs to that user;
   - no or expired session: anonymous, public channels only;
   - suspended account: treated as anonymous (public channels only; every private channel and chat frame answers
     `unauthenticated`, as REST answers 403 for them).
   The identity is fixed for the life of the socket. **After sign-in or sign-out the client reconnects.**
   The upgrade is refused with HTTP 429 `rate_limited` past the connection limits, and with HTTP 503 `unavailable`
   while the instance has lost its fan-out feed (see Lost feed); back off and retry.
2. **Subscribe** to what the current screen needs (`subscribe {channel, sinceSeq?}`), unsubscribe when it unmounts.
   Subscriptions are reference counted on the client; the server only sees one subscribe per channel.
3. **Heartbeat.** The client sends `{type:"ping", id}` every 25 s; the server acks. No ack within 10 s means the
   connection is dead: close and reconnect. The server closes with 4408 after 60 s without any client frame.
4. **Reconnect** with exponential backoff (1 s, 2 s, 4 s ... capped at 30 s, plus up to 1 s jitter), then
   re-subscribe every channel. Sequenced channels pass their `lastSeq` as `sinceSeq`; unsequenced ones refetch over
   REST (see [Resume](#ordering-and-resume)).
5. **Session ends mid-connection** (logout elsewhere, expiry, revoke): the server closes with 4401. Suspension: 4403.
   The client refreshes `GET /auth/me` and reconnects (anonymously if signed out). The server re-reads the session of
   each signed-in socket once a minute, so this takes up to 60 s.

### Frames

Client frames are `{ type, id?, ... }`. `id` is any string the client picks; when present the server answers with an
`ack` whose `ref` is that id. A frame without `id` that fails produces an `error` frame instead.

| Client frame | Fields | Ack on success |
| --- | --- | --- |
| `subscribe` | `channel`, `sinceSeq?` | `{ok: true, headSeq?}` after any replayed frames |
| `unsubscribe` | `channel` | `{ok: true}` (also when not subscribed) |
| `ping` | | `{ok: true}` |
| `chat.send` | `conversationId`, `clientMsgId` (uuid), `text` (1..4000 chars, trimmed) | `{ok: true, result: {messageId, seq}}`. **Not built yet:** answers `not_implemented` until the conversations REST exists |
| `chat.typing` | `conversationId` | `{ok: true}` (also when throttled and dropped) |
| `chat.read` | `conversationId`, `seq` | `{ok: true}` |

Server frames:

- **Events**: `{ channel, type, seq?, payload, ts }` (the REST `RealtimeMessage` plus `seq`).
- **`ack`**: `{ type: "ack", ref, ok, headSeq?, result?, error?: {code, message, fields?} }`.
- **`error`**: `{ type: "error", code, message }`.

Error codes (`ack.error.code` / `error.code`): `bad_frame`, `unauthenticated`, `forbidden`, `not_found`,
`validation`, `rate_limited`, `subscription_limit`, `resync_required`, `internal`, and for now `not_implemented`
(`chat.send` only).

## Channels

| Channel | Who may subscribe | Event types | `seq` |
| --- | --- | --- | --- |
| `public:stats` | anyone, anonymous included | `stats.updated` (`PublicStats`) | no |
| `public:activity` | anyone | `activity.created` (`ActivityEvent`) | no |
| `auction:{id}` | anyone who may `GET /auctions/{id}` | `auction.bid`, `auction.extended`, `auction.price`, `auction.closed`, `auction.state` | yes (`bids.seq`) |
| `user:{id}` | that user only (`forbidden` for others, `unauthenticated` when anonymous) | `notification.created`, `bid.status`, `trade.updated`, `mm.activity` | no |
| `conversation:{id}` | participants only (`not_found` otherwise, as in REST) | `message.created`, `message.updated`, `message.deleted` (sequenced); `typing`, `read` (not sequenced) | messages only (`messages.seq`) |

`user:{id}` replaces the old `user:{id}:notifications` and `mm:{id}:events` channels; consumers switch on `type`.

### What each viewer sees on an auction

The `auction:{id}` channel is the same for every viewer (owner, bidder, guest). Masking is decided by the auction's
`visibility` and matches `GET /auctions/{id}`:

| Frame | `full` | `rank_only` | `sealed` |
| --- | --- | --- | --- |
| `auction.bid` `bid.bidder` | masked label | masked label | masked label |
| `auction.bid` `bid.priceIdr` | real price | `0` | `0` |
| `auction.bid` `currentPriceIdr` | new best price | absent | absent |
| `auction.bid` `bidCount`, `participants` | yes | yes | yes |
| `auction.bid` `bid.mine` | never | never | never |
| `auction.extended`, `auction.closed`, `auction.state` | as is (`state.currentPriceIdr` masked like `Auction.currentPriceIdr`) | same | same |
| `auction.price` (Dutch ask) | yes | n/a | n/a |

The masked label is stable per (auction, user): `Supplier N` for reverse and sealed auctions, `Bidder N` otherwise,
N in 11..90 derived from a hash of auction id and user id (the mock's `bidderLabel`). The REST bid list
(`AuctionDetail.bids`) uses the same labels, so live and fetched rows line up.

Each bidder additionally gets **`bid.status` on their own `user:{id}`** (`BidStatusEvent`): their current bid with the
real price and `mine: true`, `status` (`submitted`, `leading`, `outbid`, `won`, `lost`, `withdrawn`), `rank` and
`canWithdraw`. `bid.id` equals the id of the public `auction.bid` frame, which is how the room marks "Kamu".

| Trigger | `full` | `rank_only` | `sealed` |
| --- | --- | --- | --- |
| Own bid accepted (REST 200) | `leading` + rank 1 | `leading` + rank 1 | `submitted`, no rank |
| Someone else's bid changes my status or rank | `outbid` / new rank | `outbid` / new rank | nothing |
| My withdrawal, or a withdrawal that moves me up | yes | yes | own withdrawal only |
| Close (economy rounds) / award (buyer auctions) / Dutch accept | `won` / `lost` + final rank | same | same, rank now present |

Outbid and win/lose also produce the usual `notification.created` (`outbid`, `winning_bid`, `auction_ending`) when the
user's preference has `inApp` on; `bid.status` is the structured state, the notification is the toast.

Nobody gets a privileged live view: owners, market makers and admins read hidden prices through REST
(`/auctions/{id}/evaluation`, the admin bid view).

### Auction events

| `type` | When | Payload |
| --- | --- | --- |
| `auction.bid` | a bid is accepted (`POST /auctions/{id}/bids`) | `AuctionEventBid` |
| `auction.extended` | the bid landed inside the anti-sniping window and the extension cap is not reached; sent right after that bid | `AuctionEventExtended` (status becomes `extended`) |
| `auction.price` | a Dutch auction's ask steps down | `AuctionEventPrice` |
| `auction.closed` | `endsAt` passed (`closed`) or a Dutch price was accepted (`awarded`) | `AuctionEventClosed` |
| `auction.state` | other status changes: start, freeze, unfreeze, award, cancel | `AuctionEventState` (`kind: "state"`: status, endsAt, masked currentPriceIdr, bidCount, participants) |

Closing, extensions, Dutch steps and scheduled starts are decided by the server. Postgres `now()` is the clock: the
bid transaction rejects a bid when `now() >= ends_at` under the auction row lock, so a bid and the closer cannot both win.

### User events

| `type` | Payload |
| --- | --- |
| `notification.created` | `AppNotification`, exactly what `GET /me/notifications` will list |
| `bid.status` | `BidStatusEvent` (above) |
| `trade.updated` | `{transactionId, status, updatedAt}`: a trade of the user changed; refetch it |
| `mm.activity` | `ActivityEvent` for the market maker operations feed |

### Conversation events

| `type` | Payload | Notes |
| --- | --- | --- |
| `message.created` | `ConversationMessage` (`id, conversationId, seq, by, userId?, text, at, clientMsgId?`) | from `chat.send`, REST POST, or a fictional participant's reply |
| `message.updated` / `message.deleted` | `ConversationMessage` with `editedAt` / `deletedAt` (text emptied) | server side only (moderation). No edit frame or endpoint exists yet |
| `typing` | `{conversationId, userId, by, until}` | not sent to the typer; show until `until` (receipt + 5 s) |
| `read` | `{conversationId, userId, seq, at}` | the participant's read position advanced |

`chat.send` follows the REST POST exactly: trimmed non-blank text, participant check, `updatedAt` bump, a
`transaction_update` notification to every other platform participant, a fictional participant's reply after about
5 s. It is idempotent by (conversation, sender, `clientMsgId`): a retry, on the socket or through the REST fallback
with the same `clientMsgId`, returns the stored message and broadcasts nothing new. The sender gets its own
`message.created` (with `clientMsgId`) and swaps its optimistic copy.

`chat.read {seq}` stores `max(last_read_seq, min(seq, head))` and broadcasts `read` only when it advanced.

## Ordering and resume

`auction:{id}` and `conversation:{id}` (message events) are **sequenced**: every frame carries `seq`, a per-channel
counter that increases by exactly one per event. Within a channel the server delivers in `seq` order.

Client bookkeeping per sequenced channel: `lastSeq`.

- Load the REST snapshot first; it carries the channel head (`AuctionDetail.seq`, `Conversation.seq`). Subscribe with
  `sinceSeq` = that seq. No client-side buffering is needed.
- `seq <= lastSeq`: duplicate, drop.
- `seq == lastSeq + 1`: apply, `lastSeq = seq`.
- `seq > lastSeq + 1`: gap. Send `subscribe {channel, sinceSeq: lastSeq}` again (re-subscribing is allowed and means
  "replay"), ignore live frames until its ack.

Server side of `subscribe {sinceSeq}`:

1. Register the subscription in the local hub and buffer live frames for it.
2. Read the head and the missed events from the **primary** (the replica may lag behind the event that woke us).
3. If `head - sinceSeq > 200` or `sinceSeq > head`: ack `{ok: false, error: {code: "resync_required"}}`, drop the
   subscription. The client refetches REST and subscribes again with the new seq.
4. Otherwise send the replay, then `ack {ok: true, headSeq: head}`, then flush buffered live frames with `seq > head`.
   The replay is the stored frames themselves: the `outbox` rows of the channel (`topic = 'rt'`, `aggregate_id` =
   channel) with `seq > sinceSeq`, in order, byte for byte what was sent live (so an auction frame carries the
   `currentPriceIdr` / `bidCount` of its time, not today's). Every seq in `(sinceSeq, head]` must still be there;
   if one is missing the answer is `resync_required` too.
5. Without `sinceSeq` the server still reads the head and acks it (`headSeq`), with nothing to replay.

Replayed frames are gap-free; `headSeq` in the ack is the new `lastSeq`.

**Retention.** The publisher deletes outbox rows 2 days after they were published, so a client can resume after at
most 2 days away; older gaps (and seq numbers that never had a frame, such as seeded bids) answer `resync_required`.
The horizon is `outboxRetention` in `internal/server/publisher.go` (see `migrations/postgres/00008_outbox_replay.sql`).

Unsequenced channels (`public:*`, `user:{id}`, `typing`, `read`) are never replayed. On reconnect the client refetches
what they feed: public stats and activity, `/me/notifications` (and everything under `me`), and the participant state
of open auctions (`/auctions/{id}/me`). Typing indicators simply expire.

## Close codes

| Code | Meaning | Client action |
| --- | --- | --- |
| 1000 | normal close | none |
| 1001 | instance shutting down (deploy) | reconnect, resume |
| 1008 | origin not allowed | do not reconnect |
| 1009 | frame larger than 16 KiB | fix the client; reconnect with backoff |
| 1011 | internal error | reconnect with backoff |
| 1012 | instance lost its fan-out feed (LISTEN connection dropped) | reconnect, resume |
| 1013 | server overloaded | back off, at least 30 s |
| 4400 | malformed frame (not JSON) | bug; reconnect with backoff |
| 4401 | session ended | refresh `/auth/me`, reconnect |
| 4403 | account suspended | refresh `/auth/me` (suspended screen), reconnect anonymously |
| 4408 | idle timeout, no frame for 60 s | reconnect; send pings |
| 4429 | rate limits exceeded repeatedly | back off, at least 30 s |
| 4503 | slow consumer: outbound buffer full | reconnect, resume |

## Limits

Per connection unless stated otherwise. Exceeding a limit drops the frame and answers `rate_limited` (ack if the
frame had an `id`, `error` frame otherwise). More than 100 dropped frames within 10 s closes with 4429.

| Limit | Value |
| --- | --- |
| Inbound frame size | 16 KiB (close 1009) |
| Inbound frames, all types | 20/s, burst 40 |
| `chat.send` | 1/s, burst 5 |
| `chat.typing` | 1 per 3 s per conversation (extra frames are acked `ok` and dropped, nothing is broadcast; they do not count as drops) |
| Subscribed channels | 100 (`subscription_limit`) |
| Connections | 10 per user, 20 anonymous per IP (upgrade refused with HTTP 429) |
| Outbound buffer | 256 frames; a write blocked for 10 s or a full buffer closes with 4503 |
| Replay | 200 events, otherwise `resync_required` |

The REST fallback for messages keeps its own HTTP rate limit; the socket limits are not shared across connections.

## How the Go server fans out

```
 REST handler / scheduler                 outbox publisher (leader)              every API instance
 ─────────────────────────                ─────────────────────────              ──────────────────
 BEGIN                                    LISTEN outbox_new + poll 250 ms         LISTEN ecp_rt
   UPDATE auctions SET last_seq+1   ─┐    SELECT ... WHERE published_at IS NULL    on notify {c, o}:
   INSERT bids (seq = last_seq)      │      ORDER BY id FOR UPDATE SKIP LOCKED       local subscribers of c?
   INSERT outbox (topic, channel,    │    pg_notify('ecp_rt', {c, o})                  no  -> ignore
          payload = rendered frame)  │    UPDATE outbox SET published_at              yes -> fetch outbox o (primary,
   pg_notify('outbox_new')           │                                                        batched), write frame
 COMMIT  ────────────────────────────┘                                                        to each connection
```

- **Write path.** The transaction that changes state also allocates the channel seq (the `bids` / `messages` insert triggers do
  it; other auction events run `UPDATE auctions SET last_seq = last_seq + 1 RETURNING last_seq`) and inserts one `outbox` row per target channel with
  the frame already rendered: the masked public frame for `auction:{id}`, a `bid.status` per affected bidder for each
  `user:{id}`, a `notification.created` per notified user. Visibility masking happens here, in one function shared
  with replay, never per connection. The row lock taken for the seq serializes writers of one channel, so outbox rows
  of a channel are committed in seq order.
- **Publisher** (`internal/server/publisher.go`). One instance holds a Postgres advisory lock (on a dedicated
  connection, which also runs the realtime loop: if it dies the lock and the in-flight batch go with it). For each
  batch of unpublished rows, in id order, it sends `pg_notify('ecp_rt', '{"c": channel, "o": outboxId}')` for the `rt`
  rows and sets `published_at`, in one transaction (the NOTIFYs go out at commit). It wakes on `outbox_new` (an
  `AFTER INSERT` statement trigger on `outbox`, delivered on commit) and polls every 250 ms as a fallback.
- **ClickHouse never holds up realtime.** The same leader feeds ClickHouse `events` from a second loop with its own
  marker, `ch_published_at`: rows of every topic except `rt`, with the dedupe steps of
  `migrations/clickhouse/README.md`. While ClickHouse is down that loop backs off (up to 1 min) and the rows wait; bids
  keep flowing. `rt` rows are not copied to ClickHouse: they are rendered copies of facts that have their own topics,
  and they carry chat text and notification bodies. Rows both loops are done with are deleted after 2 days.
- **Instances.** Every API instance keeps one dedicated `LISTEN ecp_rt` connection and an in-memory hub
  (`channel -> connections`). On a notification it fetches the frame only if it has local subscribers for that channel
  (one `WHERE id = $1` read per notification, from the primary, in order; batching is the upgrade when one instance
  follows most of a busy feed) and writes it to each connection's outbound buffer. A frame it cannot read counts as a
  lost feed (below).
- **Ephemeral events.** `typing` is the only event that skips the outbox: the receiving instance sends
  `pg_notify('ecp_rt', '{"c": channel, "f": frame}')` with the frame inline (always under the 8000-byte NOTIFY limit).
  `read` is persisted (`conversation_participants.last_read_seq`) and goes through the outbox like everything else.
- **Periodic events.** The leader also runs the scheduler: auction start/close, Dutch price steps, and `stats.updated`
  every 5 s when the numbers changed.
- **No sticky sessions.** Any instance can serve any socket. Per-connection state is just the subscription set;
  everything needed to resume is in Postgres (seq) or behind REST. The load balancer only needs WebSocket upgrade
  support and an idle timeout above 60 s.
- **Lost feed.** If an instance's LISTEN connection drops, NOTIFYs sent meanwhile are gone for that instance. It closes
  all its sockets with 1012 and refuses upgrades (HTTP 503) until it listens again; clients reconnect (anywhere) and
  resume by seq or refetch. `GET /readyz` shows `realtime: {feed, publisher, sockets}` (informational: REST stays in
  rotation).
- **Upgrade path.** LISTEN/NOTIFY goes through one Postgres connection per instance and a single notification queue
  per database (a few thousand notifications per second is comfortable). When that is the bottleneck, or the outbox
  lag grows, the publisher publishes to Redis pub/sub or NATS (subject = channel) instead of `pg_notify`, and the
  instances subscribe there. Frames, seq, replay and the client contract do not change.

## Postgres columns this relies on

| Column | Purpose |
| --- | --- |
| `auctions.last_seq bigint NOT NULL DEFAULT 0` | channel head; the `bids` insert trigger takes the next value for each bid, and the app bumps it for `auction.extended/price/closed/state` |
| `bids.seq bigint NOT NULL`, `UNIQUE (auction_id, seq)` | gapless bid order (the replay itself reads the stored frames) |
| `auctions.extension_count int NOT NULL DEFAULT 0` | anti-sniping cap (the mock caps at 3 extensions) |
| `conversations.message_seq bigint NOT NULL DEFAULT 0` | channel head; the `messages` insert trigger takes the next value |
| `messages.seq bigint NOT NULL`, `UNIQUE (conversation_id, seq)` | gapless message order |
| `outbox (aggregate_id, id) WHERE topic = 'rt'` | replay: a channel's stored frames, newest first (00008) |
| `outbox.ch_published_at` | ClickHouse side of the publisher, independent of `published_at` (00008) |
| `messages.client_msg_id uuid`, `UNIQUE (author_user_id, client_msg_id)` | `chat.send` idempotency (catch 23505 and return the existing message; never `ON CONFLICT DO NOTHING`, which would burn a seq) |
| `conversation_participants.last_read_seq bigint NOT NULL DEFAULT 0`, `last_read_at timestamptz` | read receipts |
| `messages.edited_at`, `deleted_at`, `edit_seq` | only once message edit/delete exists |
