// Package notify sends ops alerts (orphan-suspected, wedged, crash, crash-loop) to Telegram via
// the bot HTTP API — real-time human alerting, distinct from the machine-read status/inference files.
package notify

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Telegram struct {
	Token   string
	ChatID  string
	baseURL string // overridable in tests; defaults to the Telegram bot API
	client  *http.Client
}

func NewTelegram(token, chatID string) *Telegram {
	return &Telegram{Token: token, ChatID: chatID, baseURL: "https://api.telegram.org", client: &http.Client{Timeout: 10 * time.Second}}
}

// FromEnvFile builds a Telegram from a notifier.env (KEY=VALUE lines) — TELEGRAM_BOT_TOKEN and
// TELEGRAM_CHAT_ID. A missing/empty path or absent keys yields a notifier whose Send is a no-op
// (alerting disabled), so callers never need to nil-check. An unreadable file disables alerting.
func FromEnvFile(path string) *Telegram {
	kv := map[string]string{}
	if path != "" {
		if f, err := os.Open(path); err == nil {
			defer f.Close()
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if k, v, ok := strings.Cut(line, "="); ok {
					kv[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
				}
			}
		}
	}
	return NewTelegram(kv["TELEGRAM_BOT_TOKEN"], kv["TELEGRAM_CHAT_ID"])
}

// Enabled reports whether creds are present (alerts will actually be sent).
func (t *Telegram) Enabled() bool { return t.Token != "" && t.ChatID != "" }

// Send posts a message; a no-op when creds are absent (alerting disabled).
func (t *Telegram) Send(text string) error {
	if t.Token == "" || t.ChatID == "" {
		return nil
	}
	api := t.baseURL + "/bot" + t.Token + "/sendMessage"
	resp, err := t.client.PostForm(api, url.Values{"chat_id": {t.ChatID}, "text": {text}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// A bad token (401), wrong chat (400), or rate limit (429) returns a non-2xx with err==nil;
	// surface it so the engine logs the failure instead of silently dropping the alert.
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("telegram %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
