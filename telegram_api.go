package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Minimal Telegram Bot API client: long polling, HTML messages, inline keyboards.

type tgUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

func (u tgUser) DisplayName() string {
	return strings.TrimSpace(u.FirstName + " " + u.LastName)
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type tgMessage struct {
	MessageID int    `json:"message_id"`
	From      tgUser `json:"from"`
	Chat      tgChat `json:"chat"`
	Text      string `json:"text"`
}

type tgCallback struct {
	ID      string     `json:"id"`
	From    tgUser     `json:"from"`
	Message *tgMessage `json:"message"`
	Data    string     `json:"data"`
}

type tgUpdate struct {
	UpdateID int         `json:"update_id"`
	Message  *tgMessage  `json:"message"`
	Callback *tgCallback `json:"callback_query"`
}

type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data,omitempty"`
	URL  string `json:"url,omitempty"`
}

// Keyboard is rows of inline buttons.
type Keyboard [][]Button

// Messenger is the part of the Bot API the bot logic uses; tests fake it.
type Messenger interface {
	Send(ctx context.Context, chatID int64, html string, kb Keyboard) (int, error)
	Edit(ctx context.Context, chatID int64, messageID int, html string, kb Keyboard) error
	Answer(ctx context.Context, callbackID, text string) error
}

type tgClient struct {
	base string
	http *http.Client
}

func newTGClient(apiURL, token string) *tgClient {
	return &tgClient{base: strings.TrimRight(apiURL, "/") + "/bot" + token + "/", http: &http.Client{Timeout: 70 * time.Second}}
}

type tgError struct {
	Code        int
	Description string
	RetryAfter  int
}

func (e *tgError) Error() string { return fmt.Sprintf("telegram: %d %s", e.Code, e.Description) }

func (c *tgClient) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+method, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("telegram %s: %w", method, err)
		}
		var envelope struct {
			OK          bool            `json:"ok"`
			Result      json.RawMessage `json:"result"`
			ErrorCode   int             `json:"error_code"`
			Description string          `json:"description"`
			Parameters  struct {
				RetryAfter int `json:"retry_after"`
			} `json:"parameters"`
		}
		err = json.NewDecoder(resp.Body).Decode(&envelope)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("telegram %s: %w", method, err)
		}
		if envelope.OK {
			if out != nil {
				return json.Unmarshal(envelope.Result, out)
			}
			return nil
		}
		e := &tgError{Code: envelope.ErrorCode, Description: envelope.Description, RetryAfter: envelope.Parameters.RetryAfter}
		if e.RetryAfter > 0 && attempt < 3 {
			select {
			case <-time.After(time.Duration(e.RetryAfter) * time.Second):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return e
	}
}

func messageParams(chatID int64, html string, kb Keyboard) map[string]any {
	params := map[string]any{
		"chat_id": chatID, "text": html, "parse_mode": "HTML",
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	// Telegram rejects "reply_markup": null; omit it when there are no buttons.
	if kb != nil {
		params["reply_markup"] = map[string]any{"inline_keyboard": kb}
	}
	return params
}

func (c *tgClient) Send(ctx context.Context, chatID int64, html string, kb Keyboard) (int, error) {
	var msg tgMessage
	err := c.call(ctx, "sendMessage", messageParams(chatID, html, kb), &msg)
	return msg.MessageID, err
}

func (c *tgClient) Edit(ctx context.Context, chatID int64, messageID int, html string, kb Keyboard) error {
	if kb == nil {
		kb = Keyboard{} // an empty keyboard removes old buttons
	}
	params := messageParams(chatID, html, kb)
	params["message_id"] = messageID
	err := c.call(ctx, "editMessageText", params, nil)
	if e, ok := err.(*tgError); ok && strings.Contains(e.Description, "message is not modified") {
		return nil
	}
	return err
}

func (c *tgClient) Answer(ctx context.Context, callbackID, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": callbackID, "text": text}, nil)
}

func (c *tgClient) updates(ctx context.Context, offset int) ([]tgUpdate, error) {
	var out []tgUpdate
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": 50, "allowed_updates": []string{"message", "callback_query"},
	}, &out)
	return out, err
}

type botCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

func (c *tgClient) setCommands(ctx context.Context, commands []botCommand, chatID int64) error {
	params := map[string]any{"commands": commands}
	if chatID != 0 {
		params["scope"] = map[string]any{"type": "chat", "chat_id": chatID}
	}
	return c.call(ctx, "setMyCommands", params, nil)
}
