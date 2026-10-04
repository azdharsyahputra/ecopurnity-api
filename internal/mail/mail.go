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
	HTML    string

	Link string
	Code string
}

type Mailer interface {
	Send(ctx context.Context, m Message) error
}

type Log struct{ Logger *slog.Logger }

func (l Log) Send(_ context.Context, m Message) error {
	l.Logger.Info("email", "to", m.To, "subject", m.Subject, "link", m.Link, "code", m.Code)
	return nil
}

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

func (m *Memory) Count(to string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, msg := range m.Sent {
		if msg.To == to {
			n++
		}
	}
	return n
}

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
