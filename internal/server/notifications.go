package server

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Notifications and their per-type channel preferences (PRD §8.11). Writing a notification is notify() in events.go,
// which skips types the user turned off in-app.

var notificationTypes = []string{"opportunity_detected", "new_market", "auction_invitation", "outbid", "winning_bid", "auction_ending",
	"transaction_update", "payment", "delivery", "reputation_update"}

// emailByDefault: the mock's defaultPrefs (in-app on for every type, email only for these). A type without a
// notification_prefs row uses these defaults.
var emailByDefault = map[string]bool{"outbid": true, "winning_bid": true, "payment": true, "transaction_update": true}

type channelPref struct {
	Email bool `json:"email"`
	InApp bool `json:"inApp"`
}

func (s *Server) ListMyNotifications(ctx context.Context, _ api.ListMyNotificationsRequestObject) (api.ListMyNotificationsResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	// Primary: the bell refetches right after marking read. ponytail: newest 100 (the mock keeps 100); page when needed.
	rows, err := s.DB.Primary().Query(ctx, `
		SELECT id::text, type, title, body, href, read_at IS NOT NULL, created_at FROM notifications
		WHERE user_id = $1 ORDER BY created_at DESC, id LIMIT 100`, sess.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := api.ListMyNotifications200JSONResponse{}
	for rows.Next() {
		var n api.AppNotification
		if err := rows.Scan(&n.Id, &n.Type, &n.Title, &n.Body, &n.Href, &n.Read, &n.At); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Server) MarkNotificationsRead(ctx context.Context, req api.MarkNotificationsReadRequestObject) (api.MarkNotificationsReadResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string // nil = all
	if req.Body != nil && req.Body.Ids != nil {
		ids = append([]string{}, *req.Body.Ids...)
	}
	_, err = s.DB.Primary().Exec(ctx, `
		UPDATE notifications SET read_at = now()
		WHERE user_id = $1 AND read_at IS NULL AND ($2::text[] IS NULL OR id::text = ANY($2::text[]))`, sess.UserID, ids)
	if err != nil {
		return nil, err
	}
	return api.MarkNotificationsRead204Response{}, nil
}

func loadPrefs(ctx context.Context, q dbtx, userID string) (api.NotificationPrefs, error) {
	prefs := map[string]channelPref{}
	for _, t := range notificationTypes {
		prefs[t] = channelPref{InApp: true, Email: emailByDefault[t]}
	}
	rows, err := q.Query(ctx, `SELECT type, in_app, email FROM notification_prefs WHERE user_id = $1`, userID)
	if err != nil {
		return api.NotificationPrefs{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var t string
		var p channelPref
		if err := rows.Scan(&t, &p.InApp, &p.Email); err != nil {
			return api.NotificationPrefs{}, err
		}
		prefs[t] = p
	}
	if err := rows.Err(); err != nil {
		return api.NotificationPrefs{}, err
	}
	var out api.NotificationPrefs
	b, _ := json.Marshal(prefs)
	return out, json.Unmarshal(b, &out)
}

func (s *Server) GetMyNotificationPrefs(ctx context.Context, _ api.GetMyNotificationPrefsRequestObject) (api.GetMyNotificationPrefsResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	p, err := loadPrefs(ctx, s.DB.Primary(), sess.UserID)
	if err != nil {
		return nil, err
	}
	return api.GetMyNotificationPrefs200JSONResponse(p), nil
}

// SaveMyNotificationPrefs replaces every type's row (the validator already requires all ten keys).
func (s *Server) SaveMyNotificationPrefs(ctx context.Context, req api.SaveMyNotificationPrefsRequestObject) (api.SaveMyNotificationPrefsResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(req.Body)
	var in map[string]channelPref
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	var out api.NotificationPrefs
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		for _, t := range notificationTypes {
			p := in[t]
			if _, err := tx.Exec(ctx, `
				INSERT INTO notification_prefs (user_id, type, in_app, email) VALUES ($1, $2, $3, $4)
				ON CONFLICT (user_id, type) DO UPDATE SET in_app = EXCLUDED.in_app, email = EXCLUDED.email`,
				sess.UserID, t, p.InApp, p.Email); err != nil {
				return err
			}
		}
		var err error
		out, err = loadPrefs(ctx, tx, sess.UserID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.SaveMyNotificationPrefs200JSONResponse(out), nil
}
