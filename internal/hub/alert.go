package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Alert events.
const (
	EventOffline      = "offline"
	EventOnline       = "online"
	EventNotReady     = "not_ready"
	EventReady        = "ready"
	EventSinkSelfTest = "test"
)

// Alert is one message to the operators. Text is built from the club id,
// the club name given to the join code, times and fixed reason codes only:
// it never contains a secret.
type Alert struct {
	Event    string    `json:"event"`
	Club     string    `json:"club"`
	ClubName string    `json:"club_name"`
	Text     string    `json:"text"`
	Time     time.Time `json:"time"`
}

// Sink delivers alerts.
type Sink interface {
	Name() string
	Send(ctx context.Context, a Alert) error
}

// transportError strips the request URL from a client error: a Telegram
// URL carries the bot token and a webhook URL usually carries its own.
func transportError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return errors.New("timeout")
		}
		return errors.New("network error")
	}
	return errors.New("request failed")
}

func post(ctx context.Context, client *http.Client, u, contentType string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return errors.New("bad sink address")
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := client.Do(req)
	if err != nil {
		return transportError(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// TelegramSink posts to a chat through the Bot API.
type TelegramSink struct {
	APIURL string
	Token  string
	ChatID string
	Client *http.Client
}

// Name implements Sink.
func (t *TelegramSink) Name() string { return "telegram" }

// Send implements Sink.
func (t *TelegramSink) Send(ctx context.Context, a Alert) error {
	body, _ := json.Marshal(map[string]any{"chat_id": t.ChatID, "text": a.Text, "disable_web_page_preview": true})
	if err := post(ctx, t.Client, t.APIURL+"/bot"+t.Token+"/sendMessage", "application/json", body); err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	return nil
}

// WebhookSink posts the alert as JSON to a URL.
type WebhookSink struct {
	URL    string
	Client *http.Client
}

// Name implements Sink.
func (w *WebhookSink) Name() string { return "webhook" }

// Send implements Sink.
func (w *WebhookSink) Send(ctx context.Context, a Alert) error {
	body, _ := json.Marshal(a)
	if err := post(ctx, w.Client, w.URL, "application/json", body); err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	return nil
}

// SinksFromSettings builds the configured sinks.
func SinksFromSettings(s *Settings, client *http.Client) []Sink {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	var out []Sink
	if s.Telegram != nil {
		out = append(out, &TelegramSink{APIURL: s.Telegram.APIURL, Token: s.Telegram.Token, ChatID: s.Telegram.ChatID, Client: client})
	}
	if s.WebhookURL != "" {
		out = append(out, &WebhookSink{URL: s.WebhookURL, Client: client})
	}
	return out
}
