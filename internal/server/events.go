package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type audit struct {
	ActorUserID *string
	ActorLabel  string
	Action      string
	EntityType  string
	EntityID    string
	EntityLabel string
	OrgID       *string
	MarketID    *string
	Reason      *string
	Changes     []change
}

type change struct {
	Field  string  `json:"field"`
	Before *string `json:"before,omitempty"`
	After  string  `json:"after"`
}

func writeAudit(ctx context.Context, q dbtx, a audit) error {
	if a.Changes == nil {
		a.Changes = []change{}
	}
	changes, _ := json.Marshal(a.Changes)
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO audit_log (actor_user_id, actor_label, action, entity_type, entity_id, entity_label, org_id, market_id, reason, changes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		a.ActorUserID, a.ActorLabel, a.Action, a.EntityType, a.EntityID, a.EntityLabel, a.OrgID, a.MarketID, a.Reason, changes).Scan(&id)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{
		"id": id, "actorLabel": a.ActorLabel, "action": a.Action, "entityType": a.EntityType, "entityId": a.EntityID,
		"entityLabel": a.EntityLabel, "orgId": a.OrgID, "marketId": a.MarketID, "reason": a.Reason, "changes": a.Changes,
	})
	return emit(ctx, q, "audit", a.EntityID, payload)
}

func emit(ctx context.Context, q dbtx, topic, aggregateID string, payload []byte) error {
	_, err := q.Exec(ctx, `INSERT INTO outbox (topic, aggregate_id, payload) VALUES ($1, $2, $3)`, topic, aggregateID, payload)
	return err
}

type notification struct {
	Type, Title, Body, Href string
}

func notify(ctx context.Context, q dbtx, userID string, n notification) error {
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO notifications (user_id, type, title, body, href) SELECT $1, $2, $3, $4, $5
		WHERE NOT EXISTS (SELECT 1 FROM notification_prefs WHERE user_id = $1 AND type = $2 AND NOT in_app)
		RETURNING id`,
		userID, n.Type, n.Title, n.Body, n.Href).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return emitFrame(ctx, q, "user:"+userID, "notification.created", nil,
		map[string]any{"id": id, "type": n.Type, "title": n.Title, "body": n.Body, "href": n.Href, "read": false})
}

func emitFrame(ctx context.Context, q dbtx, channel, typ string, seq *int64, payload any) error {
	b, err := renderFrame(channel, typ, seq, payload)
	if err != nil {
		return err
	}
	return emit(ctx, q, "rt", channel, b)
}

func renderFrame(channel, typ string, seq *int64, payload any) ([]byte, error) {
	frame := map[string]any{"channel": channel, "type": typ, "payload": payload, "ts": time.Now().UTC().Format(time.RFC3339Nano)}
	if seq != nil {
		frame["seq"] = *seq
	}
	return json.Marshal(frame)
}

func notifyAdmins(ctx context.Context, q dbtx, n notification) error {
	rows, err := q.Query(ctx, `SELECT user_id FROM user_capabilities WHERE capability = 'admin'`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := notify(ctx, q, id, n); err != nil {
			return err
		}
	}
	return nil
}
