// Package bot implements the Telegram side of the support bot: the update
// router, the per-chat conversation FSM, and ticket creation/forwarding.
package bot

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"supportbot/auth"
	"supportbot/config"
	"supportbot/internal/tgfile"
	store "supportbot/redis"
	"supportbot/registry"
	"supportbot/zoho"
)

// telegramSecretHeader is the header Telegram sends with each webhook call when
// a secret token is configured.
const telegramSecretHeader = "X-Telegram-Bot-Api-Secret-Token"

// updateTimeout bounds the processing of a single update (including a possible
// synchronous Zoho call).
const updateTimeout = 30 * time.Second

// ticketEnqueuer is the subset of the retry worker the bot depends on, kept as
// an interface to avoid an import cycle and to ease testing.
type ticketEnqueuer interface {
	Enqueue(ctx context.Context, req zoho.CreateTicketRequest) error
}

// Bot wires Telegram, configuration, Redis, Zoho Desk, the retry queue, the
// client registry, and the binding-token manager.
type Bot struct {
	api      *tgbotapi.BotAPI
	cfg      *config.Config
	store    *store.Client
	desk     *zoho.Desk
	queue    ticketEnqueuer
	registry *registry.Registry
	auth     *auth.Manager
	uploader *tgfile.Uploader
	logger   *slog.Logger

	// locks serializes FSM updates per chat. A fixed shard array bounds memory
	// (the previous map grew one entry per chat ever seen, including unauthorized
	// strangers, and never evicted — FIX-12). A rare cross-chat hash collision
	// only momentarily serializes two unrelated chats, which is harmless here.
	locks [chatLockShards]sync.Mutex
}

// chatLockShards is the number of mutex shards. Bounded, fixed memory.
const chatLockShards = 1024

// New constructs a Bot.
func New(api *tgbotapi.BotAPI, cfg *config.Config, st *store.Client, desk *zoho.Desk, q ticketEnqueuer, reg *registry.Registry, authMgr *auth.Manager, logger *slog.Logger) *Bot {
	return &Bot{
		api:      api,
		cfg:      cfg,
		store:    st,
		desk:     desk,
		queue:    q,
		registry: reg,
		auth:     authMgr,
		uploader: tgfile.NewUploader(api, desk, logger),
		logger:   logger,
	}
}

// chatLock returns the mutex shard for a chat so concurrent updates for the same
// chat are serialized while different chats proceed in parallel.
func (b *Bot) chatLock(chatID int64) *sync.Mutex {
	return &b.locks[uint64(chatID)%chatLockShards]
}

// WebhookHandler returns the HTTP handler for POST /telegram/webhook. It
// validates the secret token, then processes the update synchronously before
// returning 200 so Telegram preserves delivery order.
func (b *Bot) WebhookHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Defense in depth: with an empty configured secret the constant-time
		// compare below would accept an absent header, so refuse outright rather
		// than authenticate every caller (FIX-2). main.go only mounts this handler
		// in webhook mode, where the secret is required, so this should never fire
		// in practice.
		if b.cfg.TelegramWebhookSecret == "" {
			b.logger.Error("telegram webhook handler reached with no secret configured; rejecting")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(telegramSecretHeader)), []byte(b.cfg.TelegramWebhookSecret)) != 1 {
			b.logger.Warn("telegram webhook rejected: bad secret token", "remote", r.RemoteAddr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if err != nil {
			b.logger.Error("reading telegram webhook body failed", "error", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var update tgbotapi.Update
		if err := json.Unmarshal(body, &update); err != nil {
			b.logger.Error("decoding telegram update failed", "error", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
		defer cancel()
		b.handleUpdate(ctx, update)

		w.WriteHeader(http.StatusOK)
	}
}

// StartPolling runs long-polling until ctx is cancelled. Used when
// TELEGRAM_MODE=polling (local development without a public URL).
func (b *Bot) StartPolling(ctx context.Context) {
	// Ensure no webhook is registered, otherwise getUpdates is rejected.
	if _, err := b.api.Request(tgbotapi.DeleteWebhookConfig{DropPendingUpdates: false}); err != nil {
		b.logger.Warn("deleting webhook before polling failed", "error", err)
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	u.AllowedUpdates = []string{"message", "callback_query"}
	updates := b.api.GetUpdatesChan(u)

	b.logger.Info("telegram long-polling started")
	for {
		select {
		case <-ctx.Done():
			b.api.StopReceivingUpdates()
			b.logger.Info("telegram long-polling stopped")
			return
		case update := <-updates:
			procCtx, cancel := context.WithTimeout(ctx, updateTimeout)
			b.handleUpdate(procCtx, update)
			cancel()
		}
	}
}

// ConfigureWebhook registers the webhook URL and secret token with Telegram.
// Called at startup when TELEGRAM_MODE=webhook.
func (b *Bot) ConfigureWebhook(ctx context.Context) error {
	params := tgbotapi.Params{}
	params["url"] = b.cfg.PublicBaseURL + "/telegram/webhook"
	params["secret_token"] = b.cfg.TelegramWebhookSecret
	if err := params.AddInterface("allowed_updates", []string{"message", "callback_query"}); err != nil {
		return err
	}
	resp, err := b.api.MakeRequest("setWebhook", params)
	if err != nil {
		return err
	}
	if !resp.Ok {
		// A silently unregistered webhook means the bot receives nothing in
		// production, so fail startup loudly rather than appear healthy.
		return fmt.Errorf("setWebhook rejected by telegram: %s", resp.Description)
	}
	b.logger.Info("telegram webhook configured", "url", params["url"])
	return nil
}

// ---- send helpers ----

func (b *Bot) send(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.api.Send(msg); err != nil {
		b.logger.Error("sending message failed", "chat_id", chatID, "error", err)
	}
}

func (b *Bot) sendWithKeyboard(chatID int64, text string, kb tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = kb
	if _, err := b.api.Send(msg); err != nil {
		b.logger.Error("sending keyboard message failed", "chat_id", chatID, "error", err)
	}
}

func (b *Bot) answerCallback(id string) {
	if _, err := b.api.Request(tgbotapi.NewCallback(id, "")); err != nil {
		b.logger.Warn("answering callback failed", "error", err)
	}
}

// ---- small utilities shared within the package ----

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
