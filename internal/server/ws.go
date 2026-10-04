package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"golang.org/x/time/rate"
)

const (
	wsMaxFrame     = 16 << 10
	wsQueue        = 256
	wsWriteTimeout = 10 * time.Second
	wsIdle         = 60 * time.Second
	wsSessionCheck = time.Minute
	wsMaxSubs      = 100
	wsReplayMax    = 200
	wsMaxPerUser   = 10
	wsMaxPerIP     = 20

	closeMalformed websocket.StatusCode = 4400
	closeSession   websocket.StatusCode = 4401
	closeSuspended websocket.StatusCode = 4403
	closeIdle      websocket.StatusCode = 4408
	closeAbuse     websocket.StatusCode = 4429
	closeSlow      websocket.StatusCode = 4503
)

type wsConn struct {
	s    *Server
	ws   *websocket.Conn
	user *session
	key  string

	out    chan []byte
	done   chan struct{}
	once   sync.Once
	code   atomic.Int32
	lastIn atomic.Int64

	mu   sync.Mutex
	subs map[string]*wsSub

	frames, chats *rate.Limiter
	typing        map[string]time.Time
	dropFrom      time.Time
	drops         int
}

type wsSub struct {
	ready bool
	last  int64
	buf   []queued
}

type queued struct {
	frame []byte
	seq   int64
}

type clientFrame struct {
	Type           string `json:"type"`
	ID             string `json:"id"`
	Channel        string `json:"channel"`
	SinceSeq       *int64 `json:"sinceSeq"`
	ConversationID string `json:"conversationId"`
	Seq            *int64 `json:"seq"`
	ClientMsgID    string `json:"clientMsgId"`
	Text           string `json:"text"`
}

var frameMessages = map[string]string{
	"bad_frame":          "Frame tidak valid",
	"unauthenticated":    "Belum login",
	"forbidden":          "Tidak boleh mengakses channel ini",
	"not_found":          "Channel tidak ditemukan",
	"rate_limited":       "Terlalu banyak frame, coba lagi sebentar lagi",
	"subscription_limit": "Maksimal 100 channel per koneksi",
	"resync_required":    "Terlalu banyak event terlewat, muat ulang data",
	"internal":           "Terjadi kesalahan di server",
}

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	h := s.rt()
	if !h.live.Load() {

		writeError(w, &Error{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "Realtime sedang tidak tersedia"})
		return
	}
	sess := state(r.Context()).session
	if sess != nil && sess.Status == "suspended" {
		sess = nil
	}
	c := &wsConn{s: s, user: sess, key: "ip:" + clientIP(r), out: make(chan []byte, wsQueue), done: make(chan struct{}),
		subs: map[string]*wsSub{}, frames: rate.NewLimiter(20, 40), chats: rate.NewLimiter(1, 5), typing: map[string]time.Time{}}
	max := wsMaxPerIP
	if sess != nil {
		c.key, max = "u:"+sess.UserID, wsMaxPerUser
	}
	if !h.admit(c.key, max) {
		writeError(w, &Error{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "Terlalu banyak koneksi realtime"})
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.wsOrigins()})
	if err != nil {
		h.release(c.key)
		return
	}
	c.ws = ws
	ws.SetReadLimit(wsMaxFrame)
	c.lastIn.Store(time.Now().UnixNano())
	h.attach(c)
	defer h.leave(c)
	go c.writeLoop()
	c.readLoop()
	c.close(websocket.StatusNormalClosure, "")
}

func (s *Server) wsOrigins() []string {
	if u, err := url.Parse(s.AppURL); err == nil && u.Host != "" {
		return []string{u.Host}
	}
	return nil
}

func (c *wsConn) close(code websocket.StatusCode, reason string) {
	c.once.Do(func() {
		c.code.Store(int32(code))
		close(c.done)
		go c.ws.Close(code, reason)
	})
}

func (c *wsConn) send(b []byte) {
	select {
	case <-c.done:
	case c.out <- b:
	default:
		c.close(closeSlow, "slow consumer")
	}
}

func (c *wsConn) writeLoop() {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	checked := time.Now()
	for {
		select {
		case <-c.done:
			return
		case b := <-c.out:
			ctx, cancel := context.WithTimeout(context.Background(), wsWriteTimeout)
			err := c.ws.Write(ctx, websocket.MessageText, b)
			cancel()
			if errors.Is(err, context.DeadlineExceeded) {
				c.close(closeSlow, "slow consumer")
			}
			if err != nil {
				return
			}
		case now := <-tick.C:
			if now.Sub(time.Unix(0, c.lastIn.Load())) > wsIdle {
				c.close(closeIdle, "idle timeout")
				return
			}
			if c.user != nil && now.Sub(checked) >= wsSessionCheck {
				checked = now
				if code := c.s.sessionEnded(c.user.TokenHash); code != 0 {
					c.close(code, "session ended")
					return
				}
			}
		}
	}
}

func (s *Server) sessionEnded(hash []byte) websocket.StatusCode {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var status string
	err := s.DB.Primary().QueryRow(ctx, `
		SELECT u.status FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = $1 AND s.expires_at > now()`,
		hash).Scan(&status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return closeSession
	case err == nil && status == "suspended":
		return closeSuspended
	}
	return 0
}

func (c *wsConn) readLoop() {
	ctx := context.Background()
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		c.lastIn.Store(time.Now().UnixNano())
		if typ != websocket.MessageText || !json.Valid(data) {
			c.close(closeMalformed, "malformed frame")
			return
		}
		c.handle(ctx, data)
	}
}

func (c *wsConn) handle(ctx context.Context, data []byte) {
	var f clientFrame
	if err := json.Unmarshal(data, &f); err != nil || len(f.ID) > 64 {
		c.answer("", nil, "bad_frame")
		return
	}
	if !c.frames.Allow() {
		c.drop(f.ID)
		return
	}
	if strings.HasPrefix(f.Type, "chat.") {
		c.chat(ctx, f)
		return
	}
	switch f.Type {
	case "ping":
		c.answer(f.ID, nil, "")
	case "subscribe":
		if f.SinceSeq != nil && *f.SinceSeq < 0 {
			c.answer(f.ID, nil, "bad_frame")
			return
		}
		c.subscribe(ctx, f)
	case "unsubscribe":
		c.mu.Lock()
		delete(c.subs, f.Channel)
		c.mu.Unlock()
		c.s.rt().remove(f.Channel, c)
		c.answer(f.ID, nil, "")
	default:
		c.answer(f.ID, nil, "bad_frame")
	}
}

func (c *wsConn) answer(id string, headSeq *int64, code string) {
	var v any
	switch {
	case id != "":
		ack := map[string]any{"type": "ack", "ref": id, "ok": code == ""}
		if headSeq != nil {
			ack["headSeq"] = *headSeq
		}
		if code != "" {
			ack["error"] = map[string]string{"code": code, "message": frameMessages[code]}
		}
		v = ack
	case code != "":
		v = map[string]string{"type": "error", "code": code, "message": frameMessages[code]}
	default:
		return
	}
	b, _ := json.Marshal(v)
	c.send(b)
}

func (c *wsConn) drop(id string) {
	if now := time.Now(); now.Sub(c.dropFrom) > 10*time.Second {
		c.dropFrom, c.drops = now, 0
	}
	if c.drops++; c.drops > 100 {
		c.close(closeAbuse, "rate limits exceeded")
		return
	}
	c.answer(id, nil, "rate_limited")
}

func (c *wsConn) userID() string {
	if c.user == nil {
		return ""
	}
	return c.user.UserID
}

func (c *wsConn) subscribe(ctx context.Context, f clientFrame) {
	ch := f.Channel
	kind, id, _ := strings.Cut(ch, ":")
	c.mu.Lock()
	_, already := c.subs[ch]
	full := len(c.subs) >= wsMaxSubs
	c.mu.Unlock()
	if !already && full {
		c.answer(f.ID, nil, "subscription_limit")
		return
	}
	switch {
	case ch == "public:stats" || ch == "public:activity":
	case kind == "user" && id != "":
		if c.user == nil {
			c.answer(f.ID, nil, "unauthenticated")
			return
		}
		if id != c.user.UserID {
			c.answer(f.ID, nil, "forbidden")
			return
		}
	case kind == "conversation" && c.user == nil:
		c.answer(f.ID, nil, "unauthenticated")
		return
	case (kind == "auction" || kind == "conversation") && uuidPattern.MatchString(id):
		c.subscribeSequenced(ctx, f, kind, id)
		return
	default:
		c.answer(f.ID, nil, "not_found")
		return
	}
	c.listen(ch, true)
	c.answer(f.ID, nil, "")
}

func (c *wsConn) listen(ch string, ready bool) {
	c.mu.Lock()
	c.subs[ch] = &wsSub{ready: ready}
	c.mu.Unlock()
	c.s.rt().add(ch, c)
}

func (c *wsConn) unlisten(ch string) {
	c.mu.Lock()
	delete(c.subs, ch)
	c.mu.Unlock()
	c.s.rt().remove(ch, c)
}

func (c *wsConn) subscribeSequenced(ctx context.Context, f clientFrame, kind, id string) {
	c.listen(f.Channel, false)
	head, frames, code := c.s.replay(ctx, kind, id, c.userID(), f.SinceSeq)
	if code != "" {
		c.unlisten(f.Channel)
		c.answer(f.ID, nil, code)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, fr := range frames {
		c.send(fr)
	}
	c.answer(f.ID, &head, "")
	sub := c.subs[f.Channel]
	sub.ready, sub.last = true, head
	for _, q := range sub.buf {
		if q.seq == 0 || q.seq > head {
			sub.last = max(sub.last, q.seq)
			c.send(q.frame)
		}
	}
	sub.buf = nil
}

func (c *wsConn) push(ch string, frame []byte, seq int64, skipUser string) {
	if skipUser != "" && skipUser == c.userID() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sub := c.subs[ch]
	switch {
	case sub == nil:
	case !sub.ready:
		if len(sub.buf) >= wsQueue {
			c.close(closeSlow, "slow consumer")
			return
		}
		sub.buf = append(sub.buf, queued{frame, seq})
	case seq > 0 && seq <= sub.last:
	default:
		sub.last = max(sub.last, seq)
		c.send(frame)
	}
}

const convHeadSQL = `
	SELECT c.message_seq FROM conversations c WHERE c.id = $1 AND EXISTS (
		SELECT 1 FROM conversation_participants cp LEFT JOIN parties p ON p.id = cp.party_id
		WHERE cp.conversation_id = c.id AND (cp.user_id = $2 OR p.user_id = $2))`

func (s *Server) replay(ctx context.Context, kind, id, userID string, sinceSeq *int64) (int64, [][]byte, string) {
	tx, err := s.DB.Primary().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		s.Log.Error("ws replay", "err", err)
		return 0, nil, "internal"
	}
	defer tx.Rollback(ctx)
	var head int64
	if kind == "auction" {
		err = tx.QueryRow(ctx, `SELECT last_seq FROM auctions WHERE id = $1`, id).Scan(&head)
	} else {
		err = tx.QueryRow(ctx, convHeadSQL, id, userID).Scan(&head)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, "not_found"
	}
	if err != nil {
		s.Log.Error("ws replay", "err", err)
		return 0, nil, "internal"
	}
	from := head
	if sinceSeq != nil {
		from = *sinceSeq
	}
	if from > head || head-from > wsReplayMax {
		return 0, nil, "resync_required"
	}
	if from == head {
		return head, nil, ""
	}
	rows, _ := tx.Query(ctx, `
		SELECT payload::text FROM outbox
		WHERE topic = 'rt' AND aggregate_id = $1 AND (payload->>'seq')::bigint > $2
		ORDER BY id DESC LIMIT $3`, kind+":"+id, from, head-from)
	frames, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		s.Log.Error("ws replay", "err", err)
		return 0, nil, "internal"
	}
	if int64(len(frames)) != head-from {
		return 0, nil, "resync_required"
	}
	slices.Reverse(frames)
	return head, frames, ""
}

func (c *wsConn) chat(ctx context.Context, f clientFrame) {
	if c.user == nil {
		c.answer(f.ID, nil, "unauthenticated")
		return
	}
	conv := f.ConversationID
	switch {
	case f.Type == "chat.send":
		if !c.chats.Allow() {
			c.drop(f.ID)
			return
		}
		c.chatSend(ctx, f)
	case f.Type != "chat.typing" && f.Type != "chat.read":
		c.answer(f.ID, nil, "bad_frame")
	case !uuidPattern.MatchString(conv):
		c.answer(f.ID, nil, "not_found")
	case f.Type == "chat.typing":
		c.answer(f.ID, nil, c.s.typing(ctx, c, conv))
	case f.Seq == nil || *f.Seq < 0:
		c.answer(f.ID, nil, "bad_frame")
	default:
		c.answer(f.ID, nil, c.s.markRead(ctx, c.user.UserID, conv, *f.Seq))
	}
}

func (s *Server) typing(ctx context.Context, c *wsConn, conv string) string {
	now := time.Now()
	if now.Sub(c.typing[conv]) < 3*time.Second {
		return ""
	}
	var head int64
	err := s.DB.Reader().QueryRow(ctx, convHeadSQL, conv, c.user.UserID).Scan(&head)
	if errors.Is(err, pgx.ErrNoRows) {
		return "not_found"
	}
	if err != nil {
		s.Log.Error("ws typing", "err", err)
		return "internal"
	}
	c.typing[conv] = now
	ch := "conversation:" + conv
	frame, err := renderFrame(ch, "typing", nil, map[string]any{"conversationId": conv, "userId": c.user.UserID,
		"by": c.user.Name, "until": now.Add(5 * time.Second).UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return "internal"
	}
	note, _ := json.Marshal(map[string]any{"c": ch, "f": json.RawMessage(frame), "x": c.user.UserID})
	if _, err := s.DB.Primary().Exec(ctx, `SELECT pg_notify('ecp_rt', $1)`, string(note)); err != nil {
		s.Log.Error("ws typing", "err", err)
		return "internal"
	}
	return ""
}

func (s *Server) markRead(ctx context.Context, userID, conv string, seq int64) string {
	code := ""
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var head int64
		err := tx.QueryRow(ctx, convHeadSQL, conv, userID).Scan(&head)
		if errors.Is(err, pgx.ErrNoRows) {
			code = "not_found"
			return nil
		}
		if err != nil {
			return err
		}
		seq = min(seq, head)
		var at time.Time
		err = tx.QueryRow(ctx, `
			UPDATE conversation_participants SET last_read_seq = $3, last_read_at = now()
			WHERE conversation_id = $1 AND user_id = $2 AND last_read_seq < $3 RETURNING last_read_at`, conv, userID, seq).Scan(&at)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return emitFrame(ctx, tx, "conversation:"+conv, "read", nil,
			map[string]any{"conversationId": conv, "userId": userID, "seq": seq, "at": at.UTC().Format(time.RFC3339Nano)})
	})
	if err != nil {
		s.Log.Error("ws chat.read", "err", err)
		return "internal"
	}
	return code
}
