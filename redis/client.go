// Package store wraps the Redis client and exposes the typed operations used
// across the bot. It is the single place that knows Redis key formats and
// TTLs, so no raw key strings leak into the rest of the codebase.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	fsmTTL    = 24 * time.Hour
	ticketTTL = 30 * 24 * time.Hour

	authTTL = 30 * 24 * time.Hour
	// authUnknownTTL caps how long an unbound chat is remembered as "unknown" so
	// it does not re-trigger a Zoho lookup on every message. Raised from 5m to 30m
	// (FIX-1) to shrink the cold-miss scan surface. Trade-off: a chat bound
	// manually in Zoho — outside the token flow — is not recognized until this
	// TTL expires. The normal token-binding flow is unaffected: it writes the
	// positive cache directly (registry.cache → SetAuth), overwriting this key.
	authUnknownTTL = 30 * time.Minute
	// authUnknownThrottledTTL is the shorter negative-cache TTL used when a scan
	// was skipped because the cold-miss limiter denied it. A legitimate client hit
	// by the limiter recovers within this window rather than waiting authUnknownTTL.
	authUnknownThrottledTTL = time.Minute
	profileTTL              = time.Hour
	bindAttemptTTL          = time.Hour

	// AuthUnknown is the sentinel stored in the negative cache for chats that
	// were looked up and found not to be bound to any client.
	AuthUnknown = "unknown"

	keyQueue = "queue:tickets"

	// keyActiveTickets is a hash of ticket_id -> chat_id for every ticket the
	// Zoho poller must watch. It lets the poller skip all Zoho calls when no
	// ticket is open and reverse-map a changed ticket back to its chat.
	keyActiveTickets = "active_tickets"
)

// Client is a thin typed wrapper over a go-redis client.
type Client struct {
	rdb *redis.Client
}

// New connects to Redis and verifies connectivity with a PING.
func New(ctx context.Context, addr, password string, db int) (*Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &Client{rdb: rdb}, nil
}

// Ping verifies connectivity, used by the health check endpoint.
func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Close releases the underlying connection pool.
func (c *Client) Close() error {
	return c.rdb.Close()
}

// ---- FSM conversation state (raw JSON, marshaled by the bot package) ----

func fsmKey(chatID int64) string { return "fsm:" + strconv.FormatInt(chatID, 10) }

// SetFSM stores the serialized conversation with a 24h TTL.
func (c *Client) SetFSM(ctx context.Context, chatID int64, data []byte) error {
	return c.rdb.Set(ctx, fsmKey(chatID), data, fsmTTL).Err()
}

// GetFSM returns the serialized conversation, or (nil, nil) when absent.
func (c *Client) GetFSM(ctx context.Context, chatID int64) ([]byte, error) {
	data, err := c.rdb.Get(ctx, fsmKey(chatID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// DelFSM removes the conversation state.
func (c *Client) DelFSM(ctx context.Context, chatID int64) error {
	return c.rdb.Del(ctx, fsmKey(chatID)).Err()
}

// ---- Telegram chat -> Zoho ticket mapping ----

func ticketKey(chatID int64) string { return "ticket:" + strconv.FormatInt(chatID, 10) }

// TicketRef is the active ticket bound to a chat: its internal ID (for API
// calls) and its human-friendly number (for display to the client).
type TicketRef struct {
	ID     string `json:"id"`
	Number string `json:"number"`
}

func ticketModSeenKey(ticketID string) string { return "ticket_modseen:" + ticketID }
func ticketCmtSeenKey(ticketID string) string { return "ticket_cmtseen:" + ticketID }
func ticketCheckedKey(ticketID string) string { return "ticket_checked:" + ticketID }

// SetTicket maps a chat to its active Zoho ticket with a 30d TTL and registers
// the ticket in the poller's watch set. On the first registration of a ticket
// it baselines the poll markers to "now" (millis) so the ticket's own creation
// and any pre-existing activity are never re-forwarded; subsequent calls (e.g.
// refreshing the stored number) leave the markers untouched via SetNX.
func (c *Client) SetTicket(ctx context.Context, chatID int64, ref TicketRef) error {
	data, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	if err := c.rdb.Set(ctx, ticketKey(chatID), data, ticketTTL).Err(); err != nil {
		return err
	}
	if ref.ID != "" {
		if err := c.rdb.HSet(ctx, keyActiveTickets, ref.ID, chatID).Err(); err != nil {
			return err
		}
		now := strconv.FormatInt(time.Now().UnixMilli(), 10)
		if err := c.rdb.SetNX(ctx, ticketModSeenKey(ref.ID), now, ticketTTL).Err(); err != nil {
			return err
		}
		if err := c.rdb.SetNX(ctx, ticketCmtSeenKey(ref.ID), now, ticketTTL).Err(); err != nil {
			return err
		}
		// Baseline the existence-probe marker so a freshly created ticket is not
		// probed for deletion until a full check interval has passed.
		if err := c.rdb.SetNX(ctx, ticketCheckedKey(ref.ID), now, ticketTTL).Err(); err != nil {
			return err
		}
	}
	return nil
}

// GetTicket returns the active ticket reference; ref.ID is "" when none exists.
func (c *Client) GetTicket(ctx context.Context, chatID int64) (TicketRef, error) {
	data, err := c.rdb.Get(ctx, ticketKey(chatID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return TicketRef{}, nil
	}
	if err != nil {
		return TicketRef{}, err
	}
	var ref TicketRef
	if uerr := json.Unmarshal(data, &ref); uerr != nil {
		return TicketRef{}, uerr
	}
	return ref, nil
}

// DelTicket clears the active-ticket mapping (e.g. when the ticket is closed)
// and removes the ticket from the poller's watch set along with its markers.
func (c *Client) DelTicket(ctx context.Context, chatID int64) error {
	if ref, err := c.GetTicket(ctx, chatID); err == nil && ref.ID != "" {
		if err := c.rdb.HDel(ctx, keyActiveTickets, ref.ID).Err(); err != nil {
			return err
		}
		if err := c.rdb.Del(ctx, ticketModSeenKey(ref.ID), ticketCmtSeenKey(ref.ID), ticketCheckedKey(ref.ID)).Err(); err != nil {
			return err
		}
	}
	return c.rdb.Del(ctx, ticketKey(chatID)).Err()
}

// ---- Zoho poller watch set + per-ticket sync markers ----

// ActiveTickets returns the ticket_id -> chat_id map of every ticket the poller
// must watch. An empty map means the poller can skip Zoho entirely this cycle.
func (c *Client) ActiveTickets(ctx context.Context) (map[string]int64, error) {
	raw, err := c.rdb.HGetAll(ctx, keyActiveTickets).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(raw))
	for ticketID, chatStr := range raw {
		chatID, perr := strconv.ParseInt(chatStr, 10, 64)
		if perr != nil {
			continue
		}
		out[ticketID] = chatID
	}
	return out, nil
}

// GetTicketModSeen returns the last processed modifiedTime marker (Unix millis)
// for a ticket, or 0 when absent.
func (c *Client) GetTicketModSeen(ctx context.Context, ticketID string) (int64, error) {
	return c.getMillis(ctx, ticketModSeenKey(ticketID))
}

// SetTicketModSeen records the modifiedTime (Unix millis) the poller has now
// processed up to for a ticket.
func (c *Client) SetTicketModSeen(ctx context.Context, ticketID string, millis int64) error {
	return c.rdb.Set(ctx, ticketModSeenKey(ticketID), strconv.FormatInt(millis, 10), ticketTTL).Err()
}

// GetTicketCommentSeen returns the commentedTime (Unix millis) of the newest
// operator comment already forwarded for a ticket, or 0 when absent.
func (c *Client) GetTicketCommentSeen(ctx context.Context, ticketID string) (int64, error) {
	return c.getMillis(ctx, ticketCmtSeenKey(ticketID))
}

// SetTicketCommentSeen records the commentedTime (Unix millis) up to which
// operator comments have been forwarded for a ticket.
func (c *Client) SetTicketCommentSeen(ctx context.Context, ticketID string, millis int64) error {
	return c.rdb.Set(ctx, ticketCmtSeenKey(ticketID), strconv.FormatInt(millis, 10), ticketTTL).Err()
}

// GetTicketChecked returns the Unix-millis time the poller last confirmed a
// ticket still exists in Zoho, or 0 when absent.
func (c *Client) GetTicketChecked(ctx context.Context, ticketID string) (int64, error) {
	return c.getMillis(ctx, ticketCheckedKey(ticketID))
}

// SetTicketChecked records that the ticket was confirmed to exist at millis.
func (c *Client) SetTicketChecked(ctx context.Context, ticketID string, millis int64) error {
	return c.rdb.Set(ctx, ticketCheckedKey(ticketID), strconv.FormatInt(millis, 10), ticketTTL).Err()
}

func (c *Client) getMillis(ctx context.Context, key string) (int64, error) {
	v, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil {
		return 0, nil
	}
	return n, nil
}

// ---- Zoho access token cache ----

const tokenKey = "zoho:access_token"

// SetToken caches the access token with the given TTL.
func (c *Client) SetToken(ctx context.Context, token string, ttl time.Duration) error {
	return c.rdb.Set(ctx, tokenKey, token, ttl).Err()
}

// GetToken returns the cached token and its remaining TTL, or ("", 0) when absent.
func (c *Client) GetToken(ctx context.Context) (string, time.Duration, error) {
	token, err := c.rdb.Get(ctx, tokenKey).Result()
	if errors.Is(err, redis.Nil) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	ttl, err := c.rdb.TTL(ctx, tokenKey).Result()
	if err != nil {
		return token, 0, err
	}
	if ttl < 0 {
		ttl = 0
	}
	return token, ttl, nil
}

// DelToken evicts the cached token, forcing a refresh on next use.
func (c *Client) DelToken(ctx context.Context) error {
	return c.rdb.Del(ctx, tokenKey).Err()
}

// ---- Pending ticket-creation queue (Zoho outage fallback) ----

// EnqueueTicket appends a serialized ticket-creation request to the queue.
func (c *Client) EnqueueTicket(ctx context.Context, data []byte) error {
	return c.rdb.RPush(ctx, keyQueue, data).Err()
}

// DequeueTicket pops the oldest queued request, or (nil, nil) when empty.
func (c *Client) DequeueTicket(ctx context.Context) ([]byte, error) {
	data, err := c.rdb.LPop(ctx, keyQueue).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// ---- Pending client comments (buffered until a ticket exists) ----

func pendingKey(chatID int64) string {
	return "pending_comments:" + strconv.FormatInt(chatID, 10)
}

// PushPendingComment buffers a client message to be posted once the ticket is
// created. It carries the same 30d TTL as the ticket mapping.
func (c *Client) PushPendingComment(ctx context.Context, chatID int64, text string) error {
	key := pendingKey(chatID)
	if err := c.rdb.RPush(ctx, key, text).Err(); err != nil {
		return err
	}
	return c.rdb.Expire(ctx, key, ticketTTL).Err()
}

// DelPendingComments discards any buffered comments (e.g. when a ticket is
// closed before it was ever created and its buffer would otherwise linger).
func (c *Client) DelPendingComments(ctx context.Context, chatID int64) error {
	return c.rdb.Del(ctx, pendingKey(chatID)).Err()
}

// PopAllPendingComments returns and clears every buffered comment in order.
func (c *Client) PopAllPendingComments(ctx context.Context, chatID int64) ([]string, error) {
	key := pendingKey(chatID)
	items, err := c.rdb.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	if err := c.rdb.Del(ctx, key).Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// ---- Authorization cache (chat_id -> bot_client_id, or "unknown") ----

func authKey(chatID int64) string { return "auth:" + strconv.FormatInt(chatID, 10) }

// SetAuth caches a positive binding with a 30d TTL.
func (c *Client) SetAuth(ctx context.Context, chatID int64, botClientID uint32) error {
	return c.rdb.Set(ctx, authKey(chatID), strconv.FormatUint(uint64(botClientID), 10), authTTL).Err()
}

// SetAuthUnknown caches a negative result so unbound chats do not hammer Zoho on
// every message.
func (c *Client) SetAuthUnknown(ctx context.Context, chatID int64) error {
	return c.rdb.Set(ctx, authKey(chatID), AuthUnknown, authUnknownTTL).Err()
}

// SetAuthUnknownThrottled caches a negative result with a short TTL, used when a
// cold-miss Zoho scan was skipped by the rate limiter rather than actually
// resolved. The short TTL lets a legitimate but limiter-denied client recover
// quickly on a later message (FIX-1).
func (c *Client) SetAuthUnknownThrottled(ctx context.Context, chatID int64) error {
	return c.rdb.Set(ctx, authKey(chatID), AuthUnknown, authUnknownThrottledTTL).Err()
}

// GetAuth returns the cached value ("" on miss, "unknown", or a numeric id).
func (c *Client) GetAuth(ctx context.Context, chatID int64) (string, error) {
	v, err := c.rdb.Get(ctx, authKey(chatID)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// DelAuth removes the cached binding (used on revocation / re-binding).
func (c *Client) DelAuth(ctx context.Context, chatID int64) error {
	return c.rdb.Del(ctx, authKey(chatID)).Err()
}

// ---- Zoho account-ID reverse index (avoids the list+N account scan) ----

func accIDByBotKey(botClientID uint32) string {
	return "acc_by_botid:" + strconv.FormatUint(uint64(botClientID), 10)
}
func accIDByChatKey(chatID int64) string {
	return "acc_by_chat:" + strconv.FormatInt(chatID, 10)
}

// SetAccountIDByBot records which Zoho account a bot client ID maps to so the
// next resolve is a single GET /accounts/{id} instead of a full scan.
func (c *Client) SetAccountIDByBot(ctx context.Context, botClientID uint32, accountID string) error {
	return c.rdb.Set(ctx, accIDByBotKey(botClientID), accountID, authTTL).Err()
}

// GetAccountIDByBot returns the cached account ID for a bot client ID, or "".
func (c *Client) GetAccountIDByBot(ctx context.Context, botClientID uint32) (string, error) {
	return c.getString(ctx, accIDByBotKey(botClientID))
}

// SetAccountIDByChat records which Zoho account a chat is bound to.
func (c *Client) SetAccountIDByChat(ctx context.Context, chatID int64, accountID string) error {
	return c.rdb.Set(ctx, accIDByChatKey(chatID), accountID, authTTL).Err()
}

// GetAccountIDByChat returns the cached account ID for a chat, or "".
func (c *Client) GetAccountIDByChat(ctx context.Context, chatID int64) (string, error) {
	return c.getString(ctx, accIDByChatKey(chatID))
}

// DelAccountIDByChat drops a chat's account-ID index (revocation / rebind).
func (c *Client) DelAccountIDByChat(ctx context.Context, chatID int64) error {
	return c.rdb.Del(ctx, accIDByChatKey(chatID)).Err()
}

func (c *Client) getString(ctx context.Context, key string) (string, error) {
	v, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// ---- Client profile cache (bot_client_id -> JSON) ----

func profileKey(botClientID uint32) string {
	return "client:" + strconv.FormatUint(uint64(botClientID), 10)
}

// SetProfile caches the serialized client profile with a 1h TTL.
func (c *Client) SetProfile(ctx context.Context, botClientID uint32, data []byte) error {
	return c.rdb.Set(ctx, profileKey(botClientID), data, profileTTL).Err()
}

// GetProfile returns the cached profile, or (nil, nil) when absent.
func (c *Client) GetProfile(ctx context.Context, botClientID uint32) ([]byte, error) {
	data, err := c.rdb.Get(ctx, profileKey(botClientID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// ---- Telegram update_id deduplication ----

// updateDedupTTL is how long a processed update_id is remembered. Telegram only
// redelivers an update for a short window after a slow/failed 200, so 10 minutes
// is ample.
const updateDedupTTL = 10 * time.Minute

// MarkUpdateProcessed records an update_id as processed and reports whether this
// call was the first to see it (true = process; false = duplicate, skip). It is
// atomic via SETNX so a redelivered update is handled exactly once (FIX-11).
func (c *Client) MarkUpdateProcessed(ctx context.Context, updateID int) (bool, error) {
	key := "tg_update:" + strconv.Itoa(updateID)
	return c.rdb.SetNX(ctx, key, "1", updateDedupTTL).Result()
}

// ---- Binding token one-time-use guard ----

// ClaimToken atomically marks a token jti as used. It returns true if the token
// was previously unused (claim succeeded), false if already redeemed.
func (c *Client) ClaimToken(ctx context.Context, jti string, ttl time.Duration) (bool, error) {
	return c.rdb.SetNX(ctx, "binding_token:"+jti, "used", ttl).Result()
}

// ReleaseToken undoes a claim so the token can be retried (used when binding
// fails for a transient reason after the claim succeeded).
func (c *Client) ReleaseToken(ctx context.Context, jti string) error {
	return c.rdb.Del(ctx, "binding_token:"+jti).Err()
}

// ---- Binding attempt rate limiting ----

// IncrBindAttempts increments and returns the per-chat binding attempt counter,
// setting a 1h expiry on first use.
func (c *Client) IncrBindAttempts(ctx context.Context, chatID int64) (int64, error) {
	key := "bind_attempts:" + strconv.FormatInt(chatID, 10)
	n, err := c.rdb.Incr(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if n == 1 {
		if err := c.rdb.Expire(ctx, key, bindAttemptTTL).Err(); err != nil {
			return n, err
		}
	}
	return n, nil
}

// RefundBindAttempt cancels one increment of the per-chat attempt counter. It is
// used when a binding fails for a transient infrastructure reason (Redis/Zoho
// outage) rather than a wrong token, so an outage does not consume the client's
// brute-force budget. The TTL set at first increment is left untouched.
func (c *Client) RefundBindAttempt(ctx context.Context, chatID int64) error {
	return c.rdb.Decr(ctx, "bind_attempts:"+strconv.FormatInt(chatID, 10)).Err()
}
