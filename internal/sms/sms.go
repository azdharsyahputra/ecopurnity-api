// Package sms sends one-time codes to phone numbers.
package sms

import (
	"context"
	"log/slog"
	"sync"
)

type Sender interface {
	Send(ctx context.Context, phone, text string) error
}

// Log writes the message to the log instead of sending it.
// ponytail: no SMS/WhatsApp provider yet; add one implementing Sender (and a PHONE_PROVIDER switch) when chosen.
type Log struct{ Logger *slog.Logger }

func (l Log) Send(_ context.Context, phone, text string) error {
	l.Logger.Info("sms", "to", phone, "text", text)
	return nil
}

// Memory keeps sent messages; for tests.
type Memory struct {
	mu   sync.Mutex
	Sent map[string]string // phone -> last text
}

func (m *Memory) Send(_ context.Context, phone, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Sent == nil {
		m.Sent = map[string]string{}
	}
	m.Sent[phone] = text
	return nil
}

func (m *Memory) Last(phone string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Sent[phone]
}
