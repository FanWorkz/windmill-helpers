// Package telegram is a thin wrapper around go-telegram-bot-api/v5 with
// project-specific defaults: MarkdownV2 escaping and 429-aware retry.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	sdk "github.com/windmill-labs/windmill-go-client"

	"github.com/masarykadam/windmill/helpers/wmill"
)

const (
	defaultMaxRetry  = 3
	defaultRetryWait = 2 * time.Second
	rateLimitCode    = 429
)

// Bot wraps a Telegram bot client.
type Bot struct {
	api      *tgbotapi.BotAPI
	maxRetry int
}

// Option configures a Bot at construction time.
type Option func(*Bot)

// WithMaxRetry overrides the default rate-limit retry budget. n is the
// number of retries *after* the initial send; total attempts = n + 1.
// Values < 0 are clamped to 0 (no retries).
func WithMaxRetry(n int) Option {
	return func(b *Bot) {
		if n < 0 {
			n = 0
		}
		b.maxRetry = n
	}
}

// New creates a Bot using the given bot token (full string from @BotFather).
// Performs a getMe call against the Telegram API to validate the token, so
// returns an error if the token is invalid or the API is unreachable.
func New(token string, opts ...Option) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("init bot api: %w", err)
	}
	b := &Bot{api: api, maxRetry: defaultMaxRetry}
	for _, opt := range opts {
		opt(b)
	}
	return b, nil
}

// resourceShape mirrors the Windmill Telegram resource type, which stores
// the bot token under "token".
type resourceShape struct {
	Token string `json:"token"`
}

// NewFromResource loads the Windmill Telegram resource at path and constructs
// a Bot from its token field. Returns an error if the resource is missing,
// can't be deserialized, has an empty token, or fails the getMe handshake.
func NewFromResource(path string, opts ...Option) (*Bot, error) {
	res, err := wmill.GetResource[resourceShape](path)
	if err != nil {
		return nil, err
	}
	if res.Token == "" {
		return nil, fmt.Errorf("telegram resource %s: empty token", path)
	}
	return New(res.Token, opts...)
}

// NotificationChatID reads the Windmill variable at path and parses it as a
// Telegram chat ID (signed int64; supergroups/channels are negative). Use
// for the shared notifications channel ID stored as a plain-string variable.
// Whitespace is trimmed; a non-numeric value is reported with the raw input
// truncated for log triage.
func NotificationChatID(path string) (int64, error) {
	raw, err := sdk.GetVariable(path)
	if err != nil {
		return 0, fmt.Errorf("get notification chat id variable %s: %w", path, err)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse notification chat id %s = %q: %w", path, raw, err)
	}
	return id, nil
}

// SendMarkdownV2 sends text formatted as MarkdownV2 to chatID. The caller is
// responsible for escaping any literal user-supplied text via EscapeMarkdownV2
// (or EscapeMarkdownV2Code for content inside code spans); formatting
// characters intended as Markdown (e.g. *bold*) must remain unescaped.
//
// Total attempts = maxRetry + 1: one initial send, then up to maxRetry
// retries on rate-limit (HTTP 429), each waiting for the API-supplied
// retry_after value before the next attempt. Non-429 errors return
// immediately.
func (b *Bot) SendMarkdownV2(ctx context.Context, chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeMarkdownV2

	var lastErr error
	for attempt := 0; attempt <= b.maxRetry; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("ctx canceled: %w", err)
		}
		_, err := b.api.Send(msg)
		if err == nil {
			return nil
		}
		lastErr = err
		var apiErr *tgbotapi.Error
		if !errors.As(err, &apiErr) || apiErr.Code != rateLimitCode {
			return fmt.Errorf("send: %w", err)
		}
		wait := time.Duration(apiErr.RetryAfter) * time.Second
		if wait <= 0 {
			wait = defaultRetryWait
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("ctx canceled during rate-limit backoff: %w", ctx.Err())
		case <-time.After(wait):
		}
	}
	return fmt.Errorf("send: exhausted %d retries: %w", b.maxRetry, lastErr)
}

// EscapeMarkdownV2 escapes a literal string for safe inclusion in MarkdownV2
// bodies. Wraps the upstream library helper so callers don't need to import
// tgbotapi directly.
func EscapeMarkdownV2(text string) string {
	return tgbotapi.EscapeText(tgbotapi.ModeMarkdownV2, text)
}

// EscapeMarkdownV2Code escapes the only two characters that remain active
// inside a MarkdownV2 inline code span (`...`): backslash and backtick. Use
// this for content placed between backticks; use EscapeMarkdownV2 for free
// text outside code spans.
func EscapeMarkdownV2Code(text string) string {
	text = strings.ReplaceAll(text, "\\", "\\\\")
	text = strings.ReplaceAll(text, "`", "\\`")
	return text
}
