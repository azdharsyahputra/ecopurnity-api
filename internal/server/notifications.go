package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
	"github.com/azdharsyahputra/ecopurnity-api/internal/mail"
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

// ── Notification emails ──────────────────────────────────────────

// Email copies of notifications: notify() only stores the row (in the caller's transaction); this job mails the ones
// whose type has email on for the user (mock defaults when no prefs row), to verified, non-suspended accounts. Only the
// last day is considered, so switching email on later does not mail the backlog. Each notification is claimed with
// FOR UPDATE SKIP LOCKED (one per transaction), so any number of instances can run it. A failed send counts an attempt
// and ends the pass (retried next tick); after mailAttempts the notification is given up and logged.

const mailAttempts = 3

// RunNotificationMailer ticks until ctx ends.
func (s *Server) RunNotificationMailer(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.NotificationMailTick(ctx); err != nil && ctx.Err() == nil && s.Log != nil {
			s.Log.Error("notification mailer", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

var errMailFailed = errors.New("send failed")

// NotificationMailTick mails up to 100 pending notifications (exported for tests).
func (s *Server) NotificationMailTick(ctx context.Context) error {
	if s.Mail == nil {
		return nil
	}
	for range 100 {
		sent, err := s.mailOne(ctx)
		if errors.Is(err, errMailFailed) {
			return nil // logged; retried next tick
		}
		if err != nil || !sent {
			return err
		}
	}
	return nil
}

func (s *Server) mailOne(ctx context.Context) (bool, error) {
	claimed, failed := false, false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var id, typ, title, body, href, email, name string
		err := tx.QueryRow(ctx, `
			SELECT n.id::text, n.type, n.title, n.body, n.href, u.email::text, u.name
			FROM notifications n JOIN users u ON u.id = n.user_id
			LEFT JOIN notification_prefs p ON p.user_id = n.user_id AND p.type = n.type
			WHERE n.emailed_at IS NULL AND n.email_attempts < $1 AND n.created_at > now() - interval '1 day'
			  AND coalesce(p.email, n.type = ANY($2::text[])) AND u.email_verified_at IS NOT NULL AND u.status <> 'suspended'
			ORDER BY n.created_at, n.id LIMIT 1
			FOR UPDATE OF n SKIP LOCKED`, mailAttempts, emailDefaultTypes()).Scan(&id, &typ, &title, &body, &href, &email, &name)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		claimed = true
		m := mail.NotificationEmail(email, name, mail.Notification{Type: typ, Title: title, Body: body, URL: s.AppURL + href}, s.AppURL+"/app/settings")
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if serr := s.Mail.Send(sctx, m); serr != nil {
			var attempts int
			if err := tx.QueryRow(ctx, `UPDATE notifications SET email_attempts = email_attempts + 1 WHERE id = $1 RETURNING email_attempts`, id).
				Scan(&attempts); err != nil {
				return err
			}
			if s.Log != nil {
				msg := "notification email failed"
				if attempts >= mailAttempts {
					msg = "notification email failed; giving up"
				}
				s.Log.Error(msg, "notification", id, "attempts", attempts, "err", serr)
			}
			failed = true
			return nil // commit the attempt count
		}
		_, err = tx.Exec(ctx, `UPDATE notifications SET emailed_at = now() WHERE id = $1`, id)
		return err
	})
	if err == nil && failed {
		err = errMailFailed
	}
	return claimed, err
}

func emailDefaultTypes() []string {
	out := []string{}
	for _, t := range notificationTypes {
		if emailByDefault[t] {
			out = append(out, t)
		}
	}
	return out
}
