// Package mail sends transactional email (verification, password reset).
package mail

import (
	"context"
	"log/slog"
	"sync"
)

type Message struct {
	To      string
	Subject string
	Body    string
	// Link is the call-to-action URL (verification / reset), kept separately so tests and the dev log can read it.
	Link string
}

type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// Log is the development mailer: it writes the message (with its link) to the log instead of sending it.
// ponytail: no SMTP/provider yet; add one implementing Mailer when there is a sending domain.
type Log struct{ Logger *slog.Logger }

func (l Log) Send(_ context.Context, m Message) error {
	l.Logger.Info("email", "to", m.To, "subject", m.Subject, "link", m.Link)
	return nil
}

// Memory keeps sent messages; for tests.
type Memory struct {
	mu   sync.Mutex
	Sent []Message
}

func (m *Memory) Send(_ context.Context, msg Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Sent = append(m.Sent, msg)
	return nil
}

// Last returns the last message sent to `to`.
func (m *Memory) Last(to string) (Message, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.Sent) - 1; i >= 0; i-- {
		if m.Sent[i].To == to {
			return m.Sent[i], true
		}
	}
	return Message{}, false
}
