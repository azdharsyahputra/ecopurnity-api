package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

var errConvNotFound = &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Percakapan tidak ditemukan"}

type convParty struct {
	PartyID string
	UserID  *string
}

func createConversation(ctx context.Context, q dbtx, subject string, createdBy *string, linkType, linkID string, parties []convParty) (string, error) {
	var lt, lid *string
	if linkType != "" {
		lt, lid = &linkType, &linkID
	}
	var id string
	if err := q.QueryRow(ctx, `INSERT INTO conversations (subject, link_type, link_id, created_by) VALUES ($1, $2, $3, $4) RETURNING id::text`,
		subject, lt, lid, createdBy).Scan(&id); err != nil {
		return "", err
	}
	for _, p := range parties {
		if err := addParticipant(ctx, q, id, p); err != nil {
			return "", err
		}
	}
	return id, nil
}

func addParticipant(ctx context.Context, q dbtx, convID string, p convParty) error {

	_, err := q.Exec(ctx, `
		INSERT INTO conversation_participants (conversation_id, party_id, user_id, joined_at) VALUES ($1, $2, $3, clock_timestamp())
		ON CONFLICT DO NOTHING`,
		convID, p.PartyID, p.UserID)
	return err
}

func externalParty(ctx context.Context, q dbtx, name, kind string, verified bool) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id::text FROM parties WHERE kind = 'external' AND name = $1 ORDER BY created_at, id LIMIT 1`, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = q.QueryRow(ctx, `INSERT INTO parties (kind, name, display_kind, verified) VALUES ('external', $1, $2, $3) RETURNING id::text`,
			name, kind, verified).Scan(&id)
	}
	return id, err
}

func (s *Server) sendMessage(ctx context.Context, tx pgx.Tx, conversationID, authorUserID, clientMsgID, text string) (api.ConversationMessage, error) {
	if !uuidPattern.MatchString(conversationID) {
		return api.ConversationMessage{}, errConvNotFound
	}
	var party, name string
	err := tx.QueryRow(ctx, `
		SELECT cp.party_id::text, p.name FROM conversation_participants cp JOIN parties p ON p.id = cp.party_id
		WHERE cp.conversation_id = $1 AND cp.user_id = $2 ORDER BY cp.joined_at LIMIT 1`, conversationID, authorUserID).Scan(&party, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ConversationMessage{}, errConvNotFound
	}
	if err != nil {
		return api.ConversationMessage{}, err
	}
	f := map[string]string{}
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		f["text"] = "Tulis pesan"
	case utf8.RuneCountInString(text) > 4000:
		f["text"] = "Pesan maksimal 4000 karakter"
	}
	var key *string
	if clientMsgID != "" {
		if k := strings.ToLower(clientMsgID); uuidPattern.MatchString(k) {
			key = &k
		} else {
			f["clientMsgId"] = "clientMsgId harus berupa uuid"
		}
	}
	if len(f) > 0 {
		return api.ConversationMessage{}, &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: "Tulis pesan", Fields: f}
	}
	return postMessage(ctx, tx, conversationID, convParty{PartyID: party, UserID: &authorUserID}, name, text, key)
}

func postMessage(ctx context.Context, tx pgx.Tx, conv string, by convParty, name, text string, clientMsgID *string) (api.ConversationMessage, error) {
	m := api.ConversationMessage{By: name, Text: text, UserId: by.UserID, ClientMsgId: clientMsgID}
	var seq int
	err := pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
		return sp.QueryRow(ctx, `
			INSERT INTO messages (conversation_id, author_party_id, author_user_id, body, client_msg_id)
			VALUES ($1, $2, $3, $4, $5) RETURNING id::text, seq, created_at`, conv, by.PartyID, by.UserID, text, clientMsgID).
			Scan(&m.Id, &seq, &m.At)
	})
	if uniqueViolation(err, "messages_author_user_id_client_msg_id_key") {

		var other string
		err = tx.QueryRow(ctx, `SELECT conversation_id::text, id::text, seq, body, created_at FROM messages WHERE author_user_id = $1 AND client_msg_id = $2`,
			by.UserID, clientMsgID).Scan(&other, &m.Id, &seq, &m.Text, &m.At)
		if err == nil && other != conv {
			err = &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: "Tulis pesan",
				Fields: map[string]string{"clientMsgId": "clientMsgId sudah dipakai untuk pesan lain"}}
		}
		m.Seq = &seq
		return m, err
	}
	if err != nil {
		return m, err
	}
	m.Seq, m.At = &seq, m.At.UTC()
	if by.UserID != nil {
		if _, err := tx.Exec(ctx, `
			UPDATE conversation_participants SET last_read_seq = greatest(last_read_seq, $3), last_read_at = now()
			WHERE conversation_id = $1 AND user_id = $2`, conv, *by.UserID, seq); err != nil {
			return m, err
		}
	}
	seq64 := int64(seq)
	frame := struct {
		api.ConversationMessage
		ConversationID string `json:"conversationId"`
	}{m, conv}
	if err := emitFrame(ctx, tx, "conversation:"+conv, "message.created", &seq64, frame); err != nil {
		return m, err
	}
	rows, _ := tx.Query(ctx, `
		SELECT DISTINCT user_id::text FROM conversation_participants
		WHERE conversation_id = $1 AND user_id IS NOT NULL AND user_id IS DISTINCT FROM $2`, conv, by.UserID)
	others, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return m, err
	}
	body := text
	if r := []rune(body); len(r) > 120 {
		body = string(r[:120])
	}
	for _, u := range others {
		if err := notify(ctx, tx, u, notification{Type: "transaction_update", Title: "Pesan dari " + name, Body: body, Href: "/app/messages/" + conv}); err != nil {
			return m, err
		}
	}
	return m, nil
}

func loadConversations(ctx context.Context, q dbtx, userID string, ids []string, lastOnly bool) ([]api.Conversation, error) {
	rows, err := q.Query(ctx, `
		SELECT c.id::text, c.subject, c.link_type, c.link_id::text,
		       CASE c.link_type WHEN 'rfq' THEN '/app/rfq/' || c.link_id
		                        WHEN 'transaction' THEN '/app/transactions/' || c.link_id
		                        WHEN 'match' THEN (SELECT '/opportunities/' || m.opportunity_id FROM matches m WHERE m.id = c.link_id) END,
		       c.message_seq, c.last_message_at, max(cp.last_read_seq)
		FROM conversations c JOIN conversation_participants cp ON cp.conversation_id = c.id AND cp.user_id = $1
		WHERE $2::uuid[] IS NULL OR c.id = ANY($2::uuid[])
		GROUP BY c.id ORDER BY c.last_message_at DESC, c.id`, userID, ids)
	if err != nil {
		return nil, err
	}
	out := []api.Conversation{}
	idx := map[string]int{}
	var found []string
	for rows.Next() {
		var c api.Conversation
		var lt, lid, href *string
		var seq, read int
		if err := rows.Scan(&c.Id, &c.Subject, &lt, &lid, &href, &seq, &c.UpdatedAt, &read); err != nil {
			rows.Close()
			return nil, err
		}
		if lt != nil {
			c.Link = &struct {
				Href string                   `json:"href"`
				Id   string                   `json:"id"`
				Type api.ConversationLinkType `json:"type"`
			}{deref(href), *lid, api.ConversationLinkType(*lt)}
		}
		unread := max(seq-read, 0)
		c.Seq, c.LastReadSeq, c.Unread = &seq, &read, &unread
		c.Participants, c.Messages = []api.TradeParty{}, []api.ConversationMessage{}
		idx[c.Id] = len(out)
		found = append(found, c.Id)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}

	rows, err = q.Query(ctx, `
		SELECT cp.conversation_id::text, `+partyCols+` FROM conversation_participants cp JOIN parties p ON p.id = cp.party_id
		WHERE cp.conversation_id = ANY($1::uuid[]) ORDER BY cp.joined_at, cp.id`, found)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var p api.TradeParty
		if err := rows.Scan(append([]any{&id}, scanParty(&p)...)...); err != nil {
			rows.Close()
			return nil, err
		}
		out[idx[id]].Participants = append(out[idx[id]].Participants, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `
		SELECT m.conversation_id::text, m.id::text, m.seq, coalesce(p.name, ''), m.author_user_id::text,
		       CASE WHEN m.deleted_at IS NULL THEN m.body ELSE '' END, m.created_at, m.client_msg_id::text
		FROM messages m JOIN conversations c ON c.id = m.conversation_id LEFT JOIN parties p ON p.id = m.author_party_id
		WHERE m.conversation_id = ANY($1::uuid[]) AND (NOT $2 OR m.seq = c.message_seq)
		ORDER BY m.conversation_id, m.seq`, found, lastOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var m api.ConversationMessage
		var seq int
		if err := rows.Scan(&id, &m.Id, &seq, &m.By, &m.UserId, &m.Text, &m.At, &m.ClientMsgId); err != nil {
			return nil, err
		}
		m.Seq = &seq
		out[idx[id]].Messages = append(out[idx[id]].Messages, m)
	}
	return out, rows.Err()
}

func loadConversation(ctx context.Context, q dbtx, userID, id string) (api.Conversation, error) {
	if !uuidPattern.MatchString(id) {
		return api.Conversation{}, errConvNotFound
	}
	list, err := loadConversations(ctx, q, userID, []string{id}, false)
	if err != nil {
		return api.Conversation{}, err
	}
	if len(list) == 0 {
		return api.Conversation{}, errConvNotFound
	}
	return list[0], nil
}

func (s *Server) ListConversations(ctx context.Context, _ api.ListConversationsRequestObject) (api.ListConversationsResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	list, err := loadConversations(ctx, s.DB.Reader(), sess.UserID, nil, true)
	if err != nil {
		return nil, err
	}
	return api.ListConversations200JSONResponse(list), nil
}

func (s *Server) GetConversation(ctx context.Context, req api.GetConversationRequestObject) (api.GetConversationResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if !uuidPattern.MatchString(req.Id) {
		return nil, errConvNotFound
	}

	switch s.markRead(ctx, sess.UserID, req.Id, 1<<62) {
	case "not_found":
		return nil, errConvNotFound
	case "internal":
		return nil, errors.New("mark read failed")
	}
	c, err := loadConversation(ctx, s.DB.Primary(), sess.UserID, req.Id)
	if err != nil {
		return nil, err
	}
	return api.GetConversation200JSONResponse(c), nil
}

func (s *Server) StartConversation(ctx context.Context, req api.StartConversationRequestObject) (api.StartConversationResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	in := req.Body
	subject, withName := strings.TrimSpace(in.Subject), strings.TrimSpace(in.With.Name)
	incomplete := &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: "Percakapan tidak lengkap"}
	if subject == "" || withName == "" || (in.Link != nil && !uuidPattern.MatchString(in.Link.Id)) {
		return nil, incomplete
	}
	var out api.Conversation
	created := false
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if in.Link != nil {
			var id string
			err := tx.QueryRow(ctx, `
				SELECT c.id::text FROM conversations c JOIN conversation_participants cp ON cp.conversation_id = c.id AND cp.user_id = $1
				WHERE c.link_type = $2 AND c.link_id = $3 ORDER BY c.created_at LIMIT 1`, sess.UserID, string(in.Link.Type), in.Link.Id).Scan(&id)
			if err == nil {
				out, err = loadConversation(ctx, tx, sess.UserID, id)
				return err
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		me, err := userParty(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		other := convParty{}
		if u := in.With.UserId; u != nil {
			var exists bool
			if uuidPattern.MatchString(*u) {
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, *u).Scan(&exists); err != nil {
					return err
				}
			}
			if !exists {
				return incomplete
			}
			if other.PartyID, err = userParty(ctx, tx, *u); err != nil {
				return err
			}
			other.UserID = u
		} else if other.PartyID, err = externalParty(ctx, tx, withName, string(in.With.Kind), in.With.Verified); err != nil {
			return err
		}
		var lt, lid string
		if in.Link != nil {
			lt, lid = string(in.Link.Type), in.Link.Id
		}
		mine := convParty{PartyID: me, UserID: &sess.UserID}
		id, err := createConversation(ctx, tx, subject, &sess.UserID, lt, lid, []convParty{mine, other})
		if err != nil {
			return err
		}
		if text := strings.TrimSpace(deref(in.Text)); text != "" {
			if _, err := s.sendMessage(ctx, tx, id, sess.UserID, "", text); err != nil {
				return err
			}
		}
		created = true
		out, err = loadConversation(ctx, tx, sess.UserID, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	if created {
		return api.StartConversation201JSONResponse(out), nil
	}
	return api.StartConversation200JSONResponse(out), nil
}

func (s *Server) SendMessage(ctx context.Context, req api.SendMessageRequestObject) (api.SendMessageResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	key := ""
	if req.Body.ClientMsgId != nil {
		key = req.Body.ClientMsgId.String()
	}
	var out api.Conversation
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.sendMessage(ctx, tx, req.Id, sess.UserID, key, req.Body.Text); err != nil {
			return err
		}
		out, err = loadConversation(ctx, tx, sess.UserID, req.Id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.SendMessage201JSONResponse(out), nil
}

func (c *wsConn) chatSend(ctx context.Context, f clientFrame) {
	var m api.ConversationMessage
	var err error = &Error{Code: "validation", Message: "clientMsgId wajib diisi", Fields: map[string]string{"clientMsgId": "clientMsgId wajib diisi"}}
	if f.ClientMsgID != "" {
		err = c.s.inTx(ctx, func(tx pgx.Tx) error {
			var err error
			m, err = c.s.sendMessage(ctx, tx, f.ConversationID, c.user.UserID, f.ClientMsgID, f.Text)
			return err
		})
	}
	ack := map[string]any{"type": "ack", "ref": f.ID, "ok": err == nil}
	var apiErr *Error
	switch {
	case err == nil:
		ack["result"] = map[string]any{"messageId": m.Id, "seq": *m.Seq}
	case errors.As(err, &apiErr):
		e := map[string]any{"code": apiErr.Code, "message": apiErr.Message}
		if len(apiErr.Fields) > 0 {
			e["fields"] = apiErr.Fields
		}
		ack["error"] = e
	default:
		c.s.Log.Error("ws chat.send", "err", err)
		ack["error"] = map[string]any{"code": "internal", "message": frameMessages["internal"]}
	}
	if f.ID == "" {
		if err == nil {
			return
		}
		e := ack["error"].(map[string]any)
		ack = map[string]any{"type": "error", "code": e["code"], "message": e["message"]}
	}
	b, _ := json.Marshal(ack)
	c.send(b)
}
