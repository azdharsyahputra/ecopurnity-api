package server

import (
	"fmt"
	"testing"
	"time"
)

func newUUID(e *testEnv) string { return e.scalar(`SELECT gen_random_uuid()::text`).(string) }

func TestConversations(t *testing.T) {
	e := newEnv(t)
	ana, _ := e.bidder("Ana Chat")
	budi, _ := e.bidder("Budi Chat")
	luar, _ := e.bidder("Luar Chat")
	anaID, budiID := e.userID(ana), e.userID(budi)

	for _, bad := range []map[string]any{
		{"subject": " ", "with": map[string]any{"name": "Budi", "kind": "person", "verified": true}},
		{"subject": "Halo", "with": map[string]any{"name": "Budi", "kind": "person", "verified": true, "userId": newUUID(e)}},
		{"subject": "Halo", "with": map[string]any{"name": "Budi", "kind": "person", "verified": true}, "link": map[string]any{"type": "rfq", "id": "x", "href": "/"}},
	} {
		if r := e.call(ana, "POST", "/me/conversations", bad); r.Status != 422 || r.code() != "validation" {
			t.Fatalf("start %v: %d %v", bad, r.Status, r.Body)
		}
	}

	trade := newUUID(e)
	start := map[string]any{"subject": "Pengiriman cabai", "with": map[string]any{"name": "Budi Chat", "kind": "person", "verified": true, "userId": budiID},
		"link": map[string]any{"type": "transaction", "id": trade, "href": "/ignored"}, "text": "  Halo Budi  "}
	r := e.call(ana, "POST", "/me/conversations", start)
	if r.Status != 201 || r.Body["seq"] != 1.0 || r.Body["unread"] != 0.0 || len(r.Body["participants"].([]any)) != 2 {
		t.Fatalf("start: %d %v", r.Status, r.Body)
	}
	conv := r.Body["id"].(string)
	if l := r.Body["link"].(map[string]any); l["href"] != "/app/transactions/"+trade {
		t.Fatalf("link: %v", l)
	}
	if m := r.Body["messages"].([]any)[0].(map[string]any); m["text"] != "Halo Budi" || m["seq"] != 1.0 || m["userId"] != anaID || m["by"] != "Ana Chat" {
		t.Fatalf("opening message: %v", m)
	}
	if r := e.call(ana, "POST", "/me/conversations", start); r.Status != 200 || r.Body["id"] != conv {
		t.Fatalf("reuse by link: %d %v", r.Status, r.Body)
	}
	frames := e.frames("conversation:" + conv)
	if len(frames) != 1 || frames[0]["type"] != "message.created" || frames[0]["seq"] != 1.0 || frames[0]["payload"].(map[string]any)["conversationId"] != conv {
		t.Fatalf("frames: %v", frames)
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND title = 'Pesan dari Ana Chat' AND href = $2`, budiID, "/app/messages/"+conv); n != int64(1) {
		t.Fatalf("notification: %v", n)
	}

	var inbox map[string]any
	for _, c := range e.callArray(budi, "/me/conversations") {
		if c["id"] == conv {
			inbox = c
		}
	}
	if inbox == nil || inbox["unread"] != 1.0 || inbox["lastReadSeq"] != 0.0 {
		t.Fatalf("inbox: %v", inbox)
	}
	if r := e.call(budi, "GET", "/me/conversations/"+conv, nil); r.Status != 200 || r.Body["unread"] != 0.0 || r.Body["lastReadSeq"] != 1.0 {
		t.Fatalf("open: %d %v", r.Status, r.Body)
	}
	if f := e.frames("conversation:" + conv); len(f) != 2 || f[1]["type"] != "read" {
		t.Fatalf("read frame: %v", f)
	}

	key := newUUID(e)
	for i := range 2 {
		r = e.call(budi, "POST", "/me/conversations/"+conv+"/messages", map[string]any{"text": "Siap, besok dikirim", "clientMsgId": key})
		msgs := r.Body["messages"].([]any)
		if last := msgs[len(msgs)-1].(map[string]any); r.Status != 201 || len(msgs) != 2 || last["seq"] != 2.0 || last["clientMsgId"] != key {
			t.Fatalf("send #%d: %d %v", i, r.Status, r.Body)
		}
	}
	if f := e.frames("conversation:" + conv); len(f) != 3 || f[2]["seq"] != 2.0 {
		t.Fatalf("frames after retry: %v", f)
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND href = $2`, anaID, "/app/messages/"+conv); n != int64(1) {
		t.Fatalf("notifications after retry: %v", n)
	}

	r = e.call(ana, "POST", "/me/conversations", map[string]any{"subject": "Lain", "with": map[string]any{"name": fmt.Sprintf("Toko Luar %d", time.Now().UnixNano()), "kind": "business", "verified": false}})
	if r.Status != 201 || r.Body["participants"].([]any)[1].(map[string]any)["userId"] != nil {
		t.Fatalf("external conversation: %d %v", r.Status, r.Body)
	}
	other := r.Body["id"].(string)
	if r := e.call(budi, "POST", "/me/conversations/"+other+"/messages", map[string]any{"text": "x"}); r.Status != 404 {
		t.Fatalf("non-participant send: %d", r.Status)
	}
	if r := e.call(ana, "POST", "/me/conversations/"+other+"/messages", map[string]any{"text": "x", "clientMsgId": key}); r.Status != 201 {
		t.Fatalf("keys are per author: %d %v", r.Status, r.Body)
	}
	if r := e.call(budi, "POST", "/me/conversations/"+other+"/messages", map[string]any{"text": "x", "clientMsgId": key}); r.Status != 404 {
		t.Fatalf("participant check first: %d", r.Status)
	}
	k2 := newUUID(e)
	if r := e.call(ana, "POST", "/me/conversations/"+other+"/messages", map[string]any{"text": "a", "clientMsgId": k2}); r.Status != 201 {
		t.Fatalf("k2: %d", r.Status)
	}
	if r := e.call(ana, "POST", "/me/conversations/"+conv+"/messages", map[string]any{"text": "a", "clientMsgId": k2}); r.Status != 422 || r.field("clientMsgId") == "" {
		t.Fatalf("key reused in another conversation: %d %v", r.Status, r.Body)
	}
	if r := e.call(ana, "POST", "/me/conversations/"+conv+"/messages", map[string]any{"text": "   "}); r.Status != 422 || r.field("text") == "" {
		t.Fatalf("blank: %d %v", r.Status, r.Body)
	}
	if r := e.call(luar, "GET", "/me/conversations/"+conv, nil); r.Status != 404 {
		t.Fatalf("outsider get: %d", r.Status)
	}

	if n := e.scalar(`SELECT count(*) FROM messages WHERE conversation_id = $1 AND author_user_id IS NULL`, other); n != int64(0) {
		t.Fatalf("external replied by itself: %v", n)
	}
}
