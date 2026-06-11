// Package registry resolves "which key client is behind this chat". The source
// of truth is Zoho Desk (accounts with custom fields); Redis is a hot cache and
// clients.json provides an optional bootstrap allowlist for test/service chats.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"supportbot/config"
	store "supportbot/redis"
	"supportbot/zoho"
)

var (
	// ErrUnknownClient means no account matches the bot client ID.
	ErrUnknownClient = errors.New("unknown client")
	// ErrChatBound means the chat is already bound to a different client.
	ErrChatBound = errors.New("chat already bound to another client")
	// ErrScanThrottled means a cold-miss full account scan was denied by the
	// process-wide rate limiter (FIX-1). Callers treat it as transient.
	ErrScanThrottled = errors.New("zoho account scan rate-limited")
)

// scanLimiter is a process-wide token bucket of size one: it permits at most one
// full Zoho account scan per interval, protecting the API-credit budget from
// unbound strangers messaging the bot. An interval of 0 disables the limit.
type scanLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
}

func (l *scanLimiter) allow() bool {
	if l.interval <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.last.IsZero() || now.Sub(l.last) >= l.interval {
		l.last = now
		return true
	}
	return false
}

// Profile is the resolved client identity used by the bot.
type Profile struct {
	BotClientID   uint32   `json:"bot_client_id"`
	ZohoAccountID string   `json:"zoho_account_id"`
	Company       string   `json:"company"`
	Services      []string `json:"services"`
	Priority      string   `json:"priority"`
	Bootstrap     bool     `json:"bootstrap"`
}

// Registry implements authorization and binding.
type Registry struct {
	store  *store.Client
	desk   *zoho.Desk
	cfg    *config.Config
	logger *slog.Logger
	scan   *scanLimiter
}

// New constructs a Registry.
func New(st *store.Client, desk *zoho.Desk, cfg *config.Config, logger *slog.Logger) *Registry {
	return &Registry{
		store:  st,
		desk:   desk,
		cfg:    cfg,
		logger: logger,
		scan:   &scanLimiter{interval: cfg.ZohoScanMinInterval},
	}
}

// Authorize resolves the profile for a chat. It returns ok=false (without
// error) when the chat is not bound to any client.
func (r *Registry) Authorize(ctx context.Context, chatID int64) (Profile, bool, error) {
	if c, ok := r.cfg.ClientByChatID(chatID); ok {
		return bootstrapProfile(c), true, nil
	}

	v, err := r.store.GetAuth(ctx, chatID)
	if err != nil {
		return Profile{}, false, err
	}

	switch {
	case v == "":
		// Cold cache: look the chat up in Zoho.
		acc, err := r.resolveByChatID(ctx, chatID)
		if err != nil {
			if errors.Is(err, ErrScanThrottled) {
				// The cold-miss limiter denied a scan: do not call Zoho. Cache the
				// chat as unknown briefly so a legitimate client recovers quickly.
				r.logger.Debug("cold-miss account scan throttled", "chat_id", chatID)
				if e := r.store.SetAuthUnknownThrottled(ctx, chatID); e != nil {
					r.logger.Warn("caching throttled negative auth failed", "chat_id", chatID, "error", e)
				}
				return Profile{}, false, nil
			}
			return Profile{}, false, err
		}
		if acc == nil {
			if e := r.store.SetAuthUnknown(ctx, chatID); e != nil {
				r.logger.Warn("caching negative auth failed", "chat_id", chatID, "error", e)
			}
			return Profile{}, false, nil
		}
		p := profileFromAccount(acc)
		r.cache(ctx, chatID, p)
		return p, true, nil

	case v == store.AuthUnknown:
		return Profile{}, false, nil

	default:
		id, perr := strconv.ParseUint(v, 10, 32)
		if perr != nil {
			r.logger.Warn("corrupt auth cache value", "chat_id", chatID, "value", v)
			return Profile{}, false, nil
		}
		p, found, err := r.profileByID(ctx, uint32(id))
		if err != nil {
			return Profile{}, false, err
		}
		if !found {
			// Account disappeared; drop the stale binding.
			_ = r.store.DelAuth(ctx, chatID)
			return Profile{}, false, nil
		}
		return p, true, nil
	}
}

// Bind attaches a chat to the account identified by botClientID (token
// redemption). It enforces isolation and handles re-binding to a new chat.
func (r *Registry) Bind(ctx context.Context, chatID int64, botClientID uint32) (Profile, error) {
	acc, err := r.resolveByBotID(ctx, botClientID)
	if err != nil {
		return Profile{}, err
	}
	if acc == nil {
		return Profile{}, ErrUnknownClient
	}

	// Isolation via cache.
	if v, err := r.store.GetAuth(ctx, chatID); err == nil && v != "" && v != store.AuthUnknown {
		if id, perr := strconv.ParseUint(v, 10, 32); perr == nil && uint32(id) != botClientID {
			return Profile{}, ErrChatBound
		}
	}
	// Isolation via Zoho: is this chat already on another account?
	if other, err := r.resolveByChatID(ctx, chatID); err != nil {
		return Profile{}, err
	} else if other != nil && other.BotClientID != botClientID {
		return Profile{}, ErrChatBound
	}

	// Re-binding: clear the account's previous chat binding.
	if acc.ChatID != 0 && acc.ChatID != chatID {
		if err := r.store.DelAuth(ctx, acc.ChatID); err != nil {
			r.logger.Warn("clearing old auth on rebind failed", "old_chat_id", acc.ChatID, "error", err)
		}
		if err := r.store.DelAccountIDByChat(ctx, acc.ChatID); err != nil {
			r.logger.Warn("clearing old chat index on rebind failed", "old_chat_id", acc.ChatID, "error", err)
		}
	}

	if err := r.desk.SetAccountChatID(ctx, acc.ID, chatID); err != nil {
		return Profile{}, err
	}
	acc.ChatID = chatID

	p := profileFromAccount(acc)
	r.cache(ctx, chatID, p)
	return p, nil
}

// Revoke removes a chat's binding both in Zoho and in the cache.
func (r *Registry) Revoke(ctx context.Context, chatID int64) error {
	acc, err := r.resolveByChatID(ctx, chatID)
	if err != nil {
		return err
	}
	if acc != nil {
		if err := r.desk.SetAccountChatID(ctx, acc.ID, 0); err != nil {
			return err
		}
	}
	if err := r.store.DelAccountIDByChat(ctx, chatID); err != nil {
		r.logger.Warn("clearing chat index on revoke failed", "chat_id", chatID, "error", err)
	}
	return r.store.DelAuth(ctx, chatID)
}

// resolveByBotID returns the account for a bot client ID. It first tries the
// Redis reverse index for a single-call GET /accounts/{id}; on a miss (or a
// stale entry) it falls back to the full account scan and refreshes the index.
func (r *Registry) resolveByBotID(ctx context.Context, botClientID uint32) (*zoho.AccountInfo, error) {
	if id, err := r.store.GetAccountIDByBot(ctx, botClientID); err == nil && id != "" {
		acc, aerr := r.desk.AccountByID(ctx, id)
		if aerr != nil {
			return nil, aerr
		}
		if acc != nil && acc.BotClientID == botClientID {
			return acc, nil
		}
		// Stale index (account deleted or its cf_bot_client_id changed): rescan.
	}
	if !r.scan.allow() {
		return nil, ErrScanThrottled
	}
	acc, err := r.desk.AccountByBotClientID(ctx, botClientID)
	if err != nil {
		return nil, err
	}
	if acc != nil {
		r.indexAccount(ctx, acc)
	}
	return acc, nil
}

// resolveByChatID returns the account bound to a chat, using the reverse index
// first and falling back to the scan, mirroring resolveByBotID.
func (r *Registry) resolveByChatID(ctx context.Context, chatID int64) (*zoho.AccountInfo, error) {
	if id, err := r.store.GetAccountIDByChat(ctx, chatID); err == nil && id != "" {
		acc, aerr := r.desk.AccountByID(ctx, id)
		if aerr != nil {
			return nil, aerr
		}
		if acc != nil && acc.ChatID == chatID {
			return acc, nil
		}
		// Stale: the chat moved off this account (or it was deleted). Drop it.
		if derr := r.store.DelAccountIDByChat(ctx, chatID); derr != nil {
			r.logger.Warn("dropping stale chat index failed", "chat_id", chatID, "error", derr)
		}
	}
	if !r.scan.allow() {
		return nil, ErrScanThrottled
	}
	acc, err := r.desk.AccountByChatID(ctx, chatID)
	if err != nil {
		return nil, err
	}
	if acc != nil {
		r.indexAccount(ctx, acc)
	}
	return acc, nil
}

// indexAccount records both reverse-index entries known for an account.
func (r *Registry) indexAccount(ctx context.Context, acc *zoho.AccountInfo) {
	if acc.BotClientID != 0 {
		if err := r.store.SetAccountIDByBot(ctx, acc.BotClientID, acc.ID); err != nil {
			r.logger.Warn("indexing account by bot id failed", "bot_client_id", acc.BotClientID, "error", err)
		}
	}
	if acc.ChatID != 0 {
		if err := r.store.SetAccountIDByChat(ctx, acc.ChatID, acc.ID); err != nil {
			r.logger.Warn("indexing account by chat id failed", "chat_id", acc.ChatID, "error", err)
		}
	}
}

func (r *Registry) profileByID(ctx context.Context, botClientID uint32) (Profile, bool, error) {
	if data, err := r.store.GetProfile(ctx, botClientID); err != nil {
		r.logger.Warn("reading profile cache failed", "bot_client_id", botClientID, "error", err)
	} else if data != nil {
		var p Profile
		if uerr := json.Unmarshal(data, &p); uerr == nil {
			return p, true, nil
		}
	}

	acc, err := r.resolveByBotID(ctx, botClientID)
	if err != nil {
		return Profile{}, false, err
	}
	if acc == nil {
		return Profile{}, false, nil
	}
	p := profileFromAccount(acc)
	if data, merr := json.Marshal(p); merr == nil {
		if serr := r.store.SetProfile(ctx, botClientID, data); serr != nil {
			r.logger.Warn("caching profile failed", "bot_client_id", botClientID, "error", serr)
		}
	}
	return p, true, nil
}

func (r *Registry) cache(ctx context.Context, chatID int64, p Profile) {
	if err := r.store.SetAuth(ctx, chatID, p.BotClientID); err != nil {
		r.logger.Warn("caching auth failed", "chat_id", chatID, "error", err)
	}
	if data, err := json.Marshal(p); err == nil {
		if serr := r.store.SetProfile(ctx, p.BotClientID, data); serr != nil {
			r.logger.Warn("caching profile failed", "bot_client_id", p.BotClientID, "error", serr)
		}
	}
	// Index the resolved account so future lookups are a single GET by ID.
	if p.ZohoAccountID != "" {
		if p.BotClientID != 0 {
			if err := r.store.SetAccountIDByBot(ctx, p.BotClientID, p.ZohoAccountID); err != nil {
				r.logger.Warn("indexing account by bot id failed", "bot_client_id", p.BotClientID, "error", err)
			}
		}
		if err := r.store.SetAccountIDByChat(ctx, chatID, p.ZohoAccountID); err != nil {
			r.logger.Warn("indexing account by chat id failed", "chat_id", chatID, "error", err)
		}
	}
}

func profileFromAccount(a *zoho.AccountInfo) Profile {
	return Profile{
		BotClientID:   a.BotClientID,
		ZohoAccountID: a.ID,
		Company:       a.Name,
		Services:      a.Services,
		Priority:      a.Priority,
	}
}

func bootstrapProfile(c config.Client) Profile {
	return Profile{
		Company:   c.Company,
		Services:  c.Services,
		Priority:  c.Priority,
		Bootstrap: true,
	}
}
