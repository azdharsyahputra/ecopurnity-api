package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
)

type hub struct {
	mu    sync.RWMutex
	subs  map[string]map[*wsConn]struct{}
	conns map[*wsConn]struct{}
	open  map[string]int
	live  atomic.Bool
}

func (s *Server) rt() *hub {
	s.hubOnce.Do(func() {
		s.hub = &hub{subs: map[string]map[*wsConn]struct{}{}, conns: map[*wsConn]struct{}{}, open: map[string]int{}}
	})
	return s.hub
}

func (h *hub) admit(key string, max int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open[key] >= max {
		return false
	}
	h.open[key]++
	return true
}

func (h *hub) attach(c *wsConn) {
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
}

func (h *hub) leave(c *wsConn) {
	c.mu.Lock()
	chans := make([]string, 0, len(c.subs))
	for ch := range c.subs {
		chans = append(chans, ch)
	}
	c.mu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range chans {
		h.removeLocked(ch, c)
	}
	delete(h.conns, c)
	h.releaseLocked(c.key)
}

func (h *hub) release(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.releaseLocked(key)
}

func (h *hub) releaseLocked(key string) {
	if h.open[key]--; h.open[key] <= 0 {
		delete(h.open, key)
	}
}

func (h *hub) add(ch string, c *wsConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[ch] == nil {
		h.subs[ch] = map[*wsConn]struct{}{}
	}
	h.subs[ch][c] = struct{}{}
}

func (h *hub) remove(ch string, c *wsConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removeLocked(ch, c)
}

func (h *hub) removeLocked(ch string, c *wsConn) {
	delete(h.subs[ch], c)
	if len(h.subs[ch]) == 0 {
		delete(h.subs, ch)
	}
}

func (h *hub) has(ch string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs[ch]) > 0
}

func (h *hub) deliver(ch string, frame []byte, seq int64, skipUser string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.subs[ch] {
		c.push(ch, frame, seq, skipUser)
	}
}

func (h *hub) closeAll(code websocket.StatusCode, reason string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.conns {
		c.close(code, reason)
	}
}

func (h *hub) sockets() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

func (s *Server) Listen(ctx context.Context) {
	h := s.rt()
	defer h.closeAll(websocket.StatusGoingAway, "server shutting down")
	wait := time.Second
	for {
		start := time.Now()
		err := s.listen(ctx, h)
		h.live.Store(false)
		if ctx.Err() != nil {
			return
		}

		h.closeAll(websocket.StatusServiceRestart, "fan-out feed lost")
		s.Log.Warn("realtime feed lost", "err", err)
		if time.Since(start) > time.Minute {
			wait = time.Second
		}
		if !sleepCtx(ctx, wait) {
			return
		}
		wait = min(2*wait, 30*time.Second)
	}
}

func (s *Server) listen(ctx context.Context, h *hub) error {
	conn, err := pgx.ConnectConfig(ctx, s.DB.Primary().Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, `LISTEN ecp_rt`); err != nil {
		return err
	}
	h.live.Store(true)
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}

		var m struct {
			C string          `json:"c"`
			O int64           `json:"o"`
			F json.RawMessage `json:"f"`
			X string          `json:"x"`
		}
		if json.Unmarshal([]byte(n.Payload), &m) != nil || !h.has(m.C) {
			continue
		}
		if m.F != nil {
			h.deliver(m.C, m.F, 0, m.X)
			continue
		}
		var frame []byte
		var seq int64

		err = s.DB.Primary().QueryRow(ctx, `SELECT payload::text, coalesce((payload->>'seq')::bigint, 0) FROM outbox WHERE id = $1`, m.O).
			Scan(&frame, &seq)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:

			return fmt.Errorf("read outbox %d: %w", m.O, err)
		default:
			h.deliver(m.C, frame, seq, "")
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
