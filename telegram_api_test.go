package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Telegram answers 400 "object expected as reply markup" to a null
// reply_markup, which silently dropped every message without buttons.
func TestTGClientReplyMarkupOnlyWhenButtons(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, body)
		if m, ok := body["reply_markup"]; ok {
			if _, isObject := m.(map[string]any); !isObject {
				w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: object expected as reply markup"}`))
				return
			}
		}
		w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	defer srv.Close()
	c := newTGClient(srv.URL, "token")
	ctx := context.Background()

	if id, err := c.Send(ctx, 1, "no buttons", nil); err != nil || id != 7 {
		t.Fatalf("send without buttons: %d %v", id, err)
	}
	if _, ok := bodies[0]["reply_markup"]; ok {
		t.Fatal("reply_markup sent for a message without buttons")
	}
	if _, err := c.Send(ctx, 1, "buttons", Keyboard{{{Text: "a", Data: "b"}}}); err != nil {
		t.Fatalf("send with buttons: %v", err)
	}
	// Editing without buttons sends an empty keyboard to clear old ones.
	if err := c.Edit(ctx, 1, 7, "done", nil); err != nil {
		t.Fatalf("edit: %v", err)
	}
	m := bodies[2]["reply_markup"].(map[string]any)
	if kb := m["inline_keyboard"].([]any); len(kb) != 0 {
		t.Fatalf("edit keyboard = %v", kb)
	}
}
