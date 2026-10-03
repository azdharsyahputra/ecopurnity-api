package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/analytics"
)

// rtEnv is newEnv plus this server's listener and outbox publisher, stopped (and waited for) at cleanup.
func rtEnv(t *testing.T) *testEnv {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); e.server.Listen(ctx) }()
	go func() { defer wg.Done(); e.server.Publish(ctx) }()
	t.Cleanup(func() { cancel(); wg.Wait() })
	waitFor(t, "listener", func() bool { return e.server.rt().live.Load() })
	return e
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// dial opens a socket as the cookie client c (nil: anonymous).
func (e *testEnv) dial(c *http.Client) *websocket.Conn {
	e.t.Helper()
	opts := &websocket.DialOptions{}
	if c != nil {
		opts.HTTPClient = c
	}
	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(e.srv.URL, "http")+BasePath+"/ws", opts)
	if err != nil {
		e.t.Fatal(err)
	}
	ws.SetReadLimit(-1) // the slow-consumer test floods big frames
	e.t.Cleanup(func() { ws.CloseNow() })
	return ws
}

func wsSend(t *testing.T, ws *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func wsRead(t *testing.T, ws *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// request sends a frame with an id and returns the frames that arrive up to and including its ack.
func request(t *testing.T, ws *websocket.Conn, frame map[string]any) (before []map[string]any, ack map[string]any) {
	t.Helper()
	id := fmt.Sprint(time.Now().UnixNano())
	frame["id"] = id
	wsSend(t, ws, frame)
	for {
		m := wsRead(t, ws)
		if m["type"] == "ack" && m["ref"] == id {
			return before, m
		}
		before = append(before, m)
	}
}

func ackCode(ack map[string]any) string {
	e, _ := ack["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func (e *testEnv) userID(c *http.Client) string {
	e.t.Helper()
	r := e.call(c, "GET", "/auth/me", nil)
	id, _ := r.Body["id"].(string)
	if id == "" {
		e.t.Fatalf("me: %d %v", r.Status, r.Body)
	}
	return id
}

func (e *testEnv) seedAuction() string {
	e.t.Helper()
	var id string
	if err := e.db.Primary().QueryRow(t0(), `
		INSERT INTO auctions (title, category_id, type, visibility, lot_item, quantity, unit, opening_price_idr, starts_at, ends_at)
		VALUES ('Beras medium', 'food', 'forward', 'full', 'Beras', 10, 'ton', 11000, now(), now() + interval '1 hour') RETURNING id`).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// emitAuction commits n auction.state frames on auction:{id}, each taking the next seq as the app does.
func (e *testEnv) emitAuction(id string, n int) {
	e.t.Helper()
	err := e.server.inTx(t0(), func(tx pgx.Tx) error {
		for range n {
			var seq int64
			if err := tx.QueryRow(t0(), `UPDATE auctions SET last_seq = last_seq + 1 WHERE id = $1 RETURNING last_seq`, id).Scan(&seq); err != nil {
				return err
			}
			if err := emitFrame(t0(), tx, "auction:"+id, "auction.state", &seq, map[string]any{"kind": "state", "status": "live"}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func TestWSAuctionLiveAndPing(t *testing.T) {
	e := rtEnv(t)
	ws := e.dial(nil)
	auc := e.seedAuction()

	if _, ack := request(t, ws, map[string]any{"type": "ping"}); ack["ok"] != true {
		t.Fatalf("ping: %v", ack)
	}
	if _, ack := request(t, ws, map[string]any{"type": "subscribe", "channel": "auction:" + auc}); ack["ok"] != true || ack["headSeq"] != 0.0 {
		t.Fatalf("subscribe: %v", ack)
	}
	e.emitAuction(auc, 1)
	if m := wsRead(t, ws); m["channel"] != "auction:"+auc || m["type"] != "auction.state" || m["seq"] != 1.0 {
		t.Fatalf("live frame: %v", m)
	}
	// Unknown auction, unknown channel, bad shape.
	for ch, code := range map[string]string{"auction:" + "00000000-0000-0000-0000-000000000000": "not_found", "auction:x": "not_found", "nope": "not_found"} {
		if _, ack := request(t, ws, map[string]any{"type": "subscribe", "channel": ch}); ackCode(ack) != code {
			t.Fatalf("%s: %v", ch, ack)
		}
	}
	if _, ack := request(t, ws, map[string]any{"type": "subscribe", "channel": "public:stats", "sinceSeq": -1}); ackCode(ack) != "bad_frame" {
		t.Fatalf("negative sinceSeq: %v", ack)
	}
	// Frames without an id answer failures with an error frame; chat frames need a session.
	wsSend(t, ws, map[string]any{"type": "chat.read", "conversationId": auc, "seq": 1})
	if m := wsRead(t, ws); m["type"] != "error" || m["code"] != "unauthenticated" {
		t.Fatalf("anonymous chat: %v", m)
	}
	// Not JSON: 4400.
	if err := ws.Write(t0(), websocket.MessageText, []byte("{")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t0(), 5*time.Second)
	defer cancel()
	if _, _, err := ws.Read(ctx); websocket.CloseStatus(err) != closeMalformed {
		t.Fatalf("malformed: %v", err)
	}
}

// A REST bid reaches the room (masked, sequenced) and the bidder's own channel through outbox -> publisher -> LISTEN.
func TestWSBidEndToEnd(t *testing.T) {
	e := rtEnv(t)
	_, auc, _ := e.buyerAuction("full", "reverse")
	bc, bid := e.bidder("Bima Sakti")
	e.qualify(bc, auc)
	ws := e.dial(bc)
	for _, ch := range []string{"auction:" + auc, "user:" + bid} {
		if _, ack := request(t, ws, map[string]any{"type": "subscribe", "channel": ch, "sinceSeq": 0}); ack["ok"] != true {
			t.Fatalf("subscribe %s: %v", ch, ack)
		}
	}
	if r := e.call(bc, "POST", "/auctions/"+auc+"/bids", map[string]any{"priceIdr": 9500}); r.Status != 200 {
		t.Fatalf("bid: %d %v", r.Status, r.Body)
	}
	got := map[string]map[string]any{}
	for got["auction.bid"] == nil || got["bid.status"] == nil {
		m := wsRead(t, ws)
		got[m["type"].(string)] = m
	}
	room, own := got["auction.bid"], got["bid.status"]
	if room["seq"] == nil {
		t.Fatalf("frames: %v", got)
	}
	if b := room["payload"].(map[string]any)["bid"].(map[string]any); b["mine"] != nil || !strings.HasPrefix(b["bidder"].(string), "Supplier ") {
		t.Fatalf("room frame not masked: %v", room)
	}
	if own["payload"].(map[string]any)["bid"].(map[string]any)["mine"] != true {
		t.Fatalf("own frame: %v", own)
	}
}

func TestWSReplayAndResync(t *testing.T) {
	e := rtEnv(t)
	ws := e.dial(nil)
	auc := e.seedAuction()
	ch := "auction:" + auc
	e.emitAuction(auc, 3)

	frames, ack := request(t, ws, map[string]any{"type": "subscribe", "channel": ch, "sinceSeq": 1})
	if ack["ok"] != true || ack["headSeq"] != 3.0 || len(frames) != 2 || frames[0]["seq"] != 2.0 || frames[1]["seq"] != 3.0 {
		t.Fatalf("replay: %v %v", frames, ack)
	}
	// Live continues after the head, and the already replayed frames are not sent again.
	e.emitAuction(auc, 1)
	if m := wsRead(t, ws); m["seq"] != 4.0 {
		t.Fatalf("live after replay: %v", m)
	}
	if _, ack := request(t, ws, map[string]any{"type": "subscribe", "channel": ch, "sinceSeq": 5}); ackCode(ack) != "resync_required" {
		t.Fatalf("ahead of head: %v", ack)
	}

	e.emitAuction(auc, 200) // head 204
	if _, ack := request(t, ws, map[string]any{"type": "subscribe", "channel": ch, "sinceSeq": 3}); ackCode(ack) != "resync_required" {
		t.Fatalf("201 missed: %v", ack)
	}
	frames, ack = request(t, ws, map[string]any{"type": "subscribe", "channel": ch, "sinceSeq": 4})
	if ack["headSeq"] != 204.0 || len(frames) != 200 || frames[0]["seq"] != 5.0 || frames[199]["seq"] != 204.0 {
		t.Fatalf("200 missed: %d frames, %v", len(frames), ack)
	}
	// A frame gone from the outbox (retention) cannot be replayed.
	e.exec(`DELETE FROM outbox WHERE topic = 'rt' AND aggregate_id = $1 AND (payload->>'seq')::bigint = 100`, ch)
	if _, ack := request(t, ws, map[string]any{"type": "subscribe", "channel": ch, "sinceSeq": 50}); ackCode(ack) != "resync_required" {
		t.Fatalf("beyond retention: %v", ack)
	}
}

func TestWSPrivateChannels(t *testing.T) {
	e := rtEnv(t)
	ca, _ := e.signedIn("Ana Putri")
	cb, _ := e.signedIn("Budi Santoso")
	cc, _ := e.signedIn("Citra Lestari")
	a, cID := e.userID(ca), e.userID(cc)
	wa, wb, wc, anon := e.dial(ca), e.dial(cb), e.dial(cc), e.dial(nil)

	// user:{id}
	if _, ack := request(t, wa, map[string]any{"type": "subscribe", "channel": "user:" + a}); ack["ok"] != true {
		t.Fatalf("own user channel: %v", ack)
	}
	if _, ack := request(t, wb, map[string]any{"type": "subscribe", "channel": "user:" + a}); ackCode(ack) != "forbidden" {
		t.Fatalf("other user's channel: %v", ack)
	}
	if _, ack := request(t, anon, map[string]any{"type": "subscribe", "channel": "user:" + a}); ackCode(ack) != "unauthenticated" {
		t.Fatalf("anonymous user channel: %v", ack)
	}

	// conversation:{id} with A and C as participants.
	var conv string
	if err := e.db.Primary().QueryRow(t0(), `INSERT INTO conversations (subject, message_seq) VALUES ('Pengiriman', 5) RETURNING id`).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{a, cID} {
		e.exec(`WITH p AS (INSERT INTO parties (kind, user_id, name) VALUES ('user', $1, 'x') RETURNING id)
			INSERT INTO conversation_participants (conversation_id, party_id, user_id) SELECT $2, id, $1 FROM p`, u, conv)
	}
	ch := "conversation:" + conv
	if _, ack := request(t, wb, map[string]any{"type": "subscribe", "channel": ch}); ackCode(ack) != "not_found" {
		t.Fatalf("non-participant: %v", ack)
	}
	if _, ack := request(t, wb, map[string]any{"type": "chat.read", "conversationId": conv, "seq": 1}); ackCode(ack) != "not_found" {
		t.Fatalf("non-participant read: %v", ack)
	}
	for _, w := range []*websocket.Conn{wa, wc} {
		if _, ack := request(t, w, map[string]any{"type": "subscribe", "channel": ch}); ack["ok"] != true || ack["headSeq"] != 5.0 {
			t.Fatalf("participant: %v", ack)
		}
	}

	// chat.read clamps to the head, persists, and broadcasts `read` (the reader's own sockets included).
	if _, ack := request(t, wa, map[string]any{"type": "chat.read", "conversationId": conv, "seq": 9}); ack["ok"] != true {
		t.Fatalf("read: %v", ack)
	}
	for _, w := range []*websocket.Conn{wa, wc} {
		m := wsRead(t, w)
		p, _ := m["payload"].(map[string]any)
		if m["type"] != "read" || p["userId"] != a || p["seq"] != 5.0 {
			t.Fatalf("read frame: %v", m)
		}
	}
	if n := e.scalar(`SELECT last_read_seq FROM conversation_participants WHERE conversation_id = $1 AND user_id = $2`, conv, a); n != int64(5) {
		t.Fatalf("last_read_seq: %v", n)
	}

	// chat.typing reaches the other participant, not the typer.
	if _, ack := request(t, wa, map[string]any{"type": "chat.typing", "conversationId": conv}); ack["ok"] != true {
		t.Fatalf("typing: %v", ack)
	}
	if m := wsRead(t, wc); m["type"] != "typing" {
		t.Fatalf("typing frame: %v", m)
	}
	// The typer's next frame is its next ack, not its own typing frame.
	if before, ack := request(t, wa, map[string]any{"type": "ping"}); ack["ok"] != true || len(before) != 0 {
		t.Fatalf("after typing: %v %v", before, ack)
	}
}

// chat.send is the REST POST's sendMessage: acked with {messageId, seq}, fanned out as message.created, idempotent.
func TestWSChatSend(t *testing.T) {
	e := rtEnv(t)
	ca, _ := e.signedIn("Ana Socket")
	cb, _ := e.signedIn("Budi Socket")
	cc, _ := e.signedIn("Citra Socket")
	r := e.call(ca, "POST", "/me/conversations", map[string]any{"subject": "Socket", "with": map[string]any{"name": "Budi Socket", "kind": "person", "verified": false, "userId": e.userID(cb)}})
	if r.Status != 201 {
		t.Fatalf("start: %d %v", r.Status, r.Body)
	}
	conv := r.Body["id"].(string)
	wa, wb, wc := e.dial(ca), e.dial(cb), e.dial(cc)
	if _, ack := request(t, wb, map[string]any{"type": "subscribe", "channel": "conversation:" + conv, "sinceSeq": 0}); ack["ok"] != true || ack["headSeq"] != 0.0 {
		t.Fatalf("subscribe: %v", ack)
	}
	key := "0B6F7C1E-5A0E-4C55-9D43-2F2F5D1B7A10" // any case; stored lowercase
	send := func(w *websocket.Conn, frame map[string]any) map[string]any {
		t.Helper()
		_, ack := request(t, w, frame)
		return ack
	}
	ack := send(wa, map[string]any{"type": "chat.send", "conversationId": conv, "clientMsgId": key, "text": "  Halo lewat socket "})
	res, _ := ack["result"].(map[string]any)
	if ack["ok"] != true || res["seq"] != 1.0 || res["messageId"] == nil {
		t.Fatalf("send: %v", ack)
	}
	m := wsRead(t, wb)
	p, _ := m["payload"].(map[string]any)
	if m["type"] != "message.created" || m["seq"] != 1.0 || p["text"] != "Halo lewat socket" || p["clientMsgId"] != strings.ToLower(key) || p["id"] != res["messageId"] {
		t.Fatalf("frame: %v", m)
	}
	// Retry: same message, no new frame (Budi's next frame is his own ack).
	if again := send(wa, map[string]any{"type": "chat.send", "conversationId": conv, "clientMsgId": strings.ToLower(key), "text": "Halo lewat socket"}); again["ok"] != true ||
		again["result"].(map[string]any)["messageId"] != res["messageId"] {
		t.Fatalf("retry: %v", again)
	}
	if before, ack := request(t, wb, map[string]any{"type": "ping"}); ack["ok"] != true || len(before) != 0 {
		t.Fatalf("retry broadcast: %v", before)
	}
	// The REST view agrees.
	if r := e.call(cb, "GET", "/me/conversations/"+conv, nil); r.Status != 200 || r.Body["seq"] != 1.0 || len(r.Body["messages"].([]any)) != 1 {
		t.Fatalf("rest: %d %v", r.Status, r.Body)
	}
	for _, c := range []struct {
		w     *websocket.Conn
		frame map[string]any
		code  string
	}{
		{wc, map[string]any{"type": "chat.send", "conversationId": conv, "clientMsgId": "6c0f0d5e-2a51-4b0e-8f0e-2b7d0c1e9a11", "text": "x"}, "not_found"},
		{wa, map[string]any{"type": "chat.send", "conversationId": conv, "text": "tanpa kunci"}, "validation"},
		{wa, map[string]any{"type": "chat.send", "conversationId": conv, "clientMsgId": "6c0f0d5e-2a51-4b0e-8f0e-2b7d0c1e9a12", "text": "   "}, "validation"},
		{wa, map[string]any{"type": "chat.send", "conversationId": "nope", "clientMsgId": "6c0f0d5e-2a51-4b0e-8f0e-2b7d0c1e9a13", "text": "x"}, "not_found"},
	} {
		if ack := send(c.w, c.frame); ackCode(ack) != c.code {
			t.Fatalf("%v: %v", c.frame, ack)
		}
	}
}

func TestWSSlowConsumer(t *testing.T) {
	e := rtEnv(t)
	ws := e.dial(nil) // never reads until the server gave up on it
	wsSend(t, ws, map[string]any{"type": "subscribe", "channel": "public:stats"})
	h := e.server.rt()
	waitFor(t, "subscription", func() bool { return h.has("public:stats") })
	var c *wsConn
	h.mu.RLock()
	for c = range h.subs["public:stats"] {
	}
	h.mu.RUnlock()

	big := []byte(`{"channel":"public:stats","type":"stats.updated","payload":"` + strings.Repeat("x", 60000) + `","ts":"2026-10-03T00:00:00Z"}`)
	for i := 0; i < 100000; i++ {
		h.deliver("public:stats", big, 0, "")
		select {
		case <-c.done:
			i = 100000
		default:
		}
	}
	if got := c.code.Load(); got != int32(closeSlow) {
		t.Fatalf("close code %d", got)
	}
	// The client drains what was sent, then sees the close.
	ctx, cancel := context.WithTimeout(t0(), 15*time.Second)
	defer cancel()
	for {
		if _, _, err := ws.Read(ctx); err != nil {
			if s := websocket.CloseStatus(err); s != closeSlow {
				t.Fatalf("client saw %v", err)
			}
			break
		}
	}
}

func TestPublisher(t *testing.T) {
	e := newEnv(t)
	ch, raw := chScratch(t)
	if ch != nil {
		e.server.Analytics = ch
	}
	ctx, cancel := context.WithCancel(t0())
	done := make(chan struct{})
	go func() { defer close(done); e.server.Publish(ctx) }()
	defer func() { cancel(); <-done }()

	agg := fmt.Sprintf("pub-test-%d", time.Now().UnixNano())
	err := e.server.inTx(t0(), func(tx pgx.Tx) error {
		if err := emitFrame(t0(), tx, "public:activity", "activity.created", nil, map[string]any{"id": agg}); err != nil {
			return err
		}
		return emit(t0(), tx, "activity", agg, []byte(`{"type":"trade","title":"test"}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	rtID := e.scalar(`SELECT id FROM outbox WHERE topic = 'rt' AND payload->'payload'->>'id' = $1`, agg).(int64)
	evID := e.scalar(`SELECT id FROM outbox WHERE topic = 'activity' AND aggregate_id = $1`, agg).(int64)
	published := func(id int64, col string) bool {
		return e.scalar(`SELECT `+col+` IS NOT NULL FROM outbox WHERE id = $1`, id) == true
	}
	waitFor(t, "published_at", func() bool { return published(rtID, "published_at") && published(evID, "published_at") })
	if !e.server.leader.Load() {
		t.Fatal("publisher is not leader")
	}
	if ch == nil {
		t.Log("clickhouse unreachable: analytics assertions skipped")
		return
	}
	waitFor(t, "ch_published_at", func() bool { return published(evID, "ch_published_at") })
	if published(rtID, "ch_published_at") {
		t.Fatal("rt rows must not go to ClickHouse")
	}
	count := func() uint64 {
		var n uint64
		if err := raw.QueryRow(t0(), `SELECT count() FROM events WHERE aggregate_id = ?`, agg).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 1 {
		t.Fatalf("events rows: %d", n)
	}
	// A row whose mark was lost (crash after the insert) is not inserted twice.
	e.exec(`UPDATE outbox SET ch_published_at = NULL WHERE id = $1`, evID)
	waitFor(t, "re-marked", func() bool { return published(evID, "ch_published_at") })
	if n := count(); n != 1 {
		t.Fatalf("events rows after re-publish: %d", n)
	}
}

// chScratch is a throwaway ClickHouse database holding a copy of `events` (no materialized views, so the dev
// aggregates are untouched), or nil when ClickHouse is not running.
func chScratch(t *testing.T) (*analytics.Client, driver.Conn) {
	addr := os.Getenv("TEST_CLICKHOUSE_ADDR")
	if addr == "" {
		addr = "localhost:9000"
	}
	name := fmt.Sprintf("ecp_test_%d", time.Now().UnixNano())
	conn, err := clickhouse.Open(&clickhouse.Options{Addr: []string{addr}, DialTimeout: 2 * time.Second, Auth: clickhouse.Auth{Database: "default"}})
	if err != nil || conn.Ping(t0()) != nil {
		return nil, nil
	}
	if err := conn.Exec(t0(), "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Exec(t0(), "DROP DATABASE "+name); _ = conn.Close() })
	if err := conn.Exec(t0(), "CREATE TABLE "+name+".events AS ecopurnity.events"); err != nil {
		t.Logf("clickhouse has no ecopurnity.events (make migrate-clickhouse): %v", err)
		return nil, nil
	}
	c, err := analytics.Open(addr, name, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	scratch, err := clickhouse.Open(&clickhouse.Options{Addr: []string{addr}, Auth: clickhouse.Auth{Database: name}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scratch.Close() })
	return c, scratch
}
