// Package poller implements the pull-based alternative to Zoho Desk webhooks.
// When inbound webhooks from Zoho are unavailable, this worker periodically
// asks Zoho what changed and reproduces the two effects the webhook handler
// would have: forwarding operator replies to Telegram and resetting a client
// when their ticket is closed.
//
// It is deliberately frugal with Zoho API credits:
//   - When no ticket is open it makes zero Zoho calls.
//   - A single "recently modified tickets" call detects changes across every
//     open ticket at once; closure is read straight from that payload.
//   - A per-ticket modifiedTime marker means comments are fetched only for a
//     ticket that actually changed, never for idle ones.
package poller

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"supportbot/internal/tgtext"
	store "supportbot/redis"
	"supportbot/zoho"
)

const (
	// listLimit is the page size for the recently-modified tickets call.
	listLimit = 100
	// maxListPages bounds the list calls per cycle so a busy department can
	// never make the poll unboundedly expensive. At the expected scale every
	// open ticket changed since the last cycle fits in the first page.
	maxListPages = 3
	// commentsLimit is how many recent comments to inspect per changed ticket.
	commentsLimit = 25
	// existenceCheckInterval throttles the per-ticket "does this still exist?"
	// probe for active tickets absent from the recent-modified list. It bounds
	// the credit cost of detecting a deleted/trashed ticket: an idle open ticket
	// is probed at most once per interval, while an actively-updated one is never
	// probed (it appears in the shared list call instead).
	existenceCheckInterval = 3 * time.Minute
)

// Poller periodically syncs operator replies and closures from Zoho Desk.
type Poller struct {
	store    *store.Client
	desk     *zoho.Desk
	api      *tgbotapi.BotAPI
	interval time.Duration
	logger   *slog.Logger
}

// New constructs a Poller. interval is the time between poll cycles.
func New(st *store.Client, desk *zoho.Desk, api *tgbotapi.BotAPI, interval time.Duration, logger *slog.Logger) *Poller {
	return &Poller{store: st, desk: desk, api: api, interval: interval, logger: logger}
}

// Run blocks until ctx is cancelled, polling Zoho every interval.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	p.logger.Info("zoho poller started", "interval", p.interval.String())
	for {
		select {
		case <-ctx.Done():
			p.logger.Info("zoho poller stopped")
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

// poll runs one cycle: discover changed active tickets and reconcile each.
func (p *Poller) poll(ctx context.Context) {
	active, err := p.store.ActiveTickets(ctx)
	if err != nil {
		p.logger.Error("reading active tickets failed", "error", err)
		return
	}
	if len(active) == 0 {
		return // nothing open: zero Zoho calls this cycle
	}

	// remaining is the set of active tickets we have not yet seen in the list.
	remaining := make(map[string]int64, len(active))
	for id, chat := range active {
		remaining[id] = chat
	}

	for page := 0; page < maxListPages && len(remaining) > 0; page++ {
		tickets, err := p.desk.ListRecentTickets(ctx, page*listLimit, listLimit)
		if err != nil {
			p.logger.Error("listing recent tickets failed", "error", err)
			return
		}
		if len(tickets) == 0 {
			break
		}
		for _, t := range tickets {
			chatID, ok := remaining[t.ID]
			if !ok {
				continue // not one of ours (or already handled)
			}
			delete(remaining, t.ID)
			p.reconcile(ctx, chatID, t)
		}
		if len(tickets) < listLimit {
			break // reached the end of the list
		}
	}

	// Active tickets not present in the recent-modified list may simply be idle,
	// or may have been deleted/trashed in Zoho (which never produces a status
	// change to detect). Probe the stragglers (throttled) and reset the client
	// if a ticket no longer exists.
	p.checkVanished(ctx, remaining)
}

// checkVanished probes active tickets that did not appear in the recent-modified
// list. A ticket that Zoho no longer returns (404) was deleted, so the client is
// reset exactly as on a closure. The per-ticket interval keeps this cheap.
func (p *Poller) checkVanished(ctx context.Context, remaining map[string]int64) {
	if len(remaining) == 0 {
		return
	}
	nowMs := time.Now().UnixMilli()
	for ticketID, chatID := range remaining {
		checked, err := p.store.GetTicketChecked(ctx, ticketID)
		if err != nil {
			p.logger.Error("reading ticket checked marker failed", "ticket_id", ticketID, "error", err)
			continue
		}
		if checked != 0 && nowMs-checked < existenceCheckInterval.Milliseconds() {
			continue // probed recently; skip to bound credit cost
		}

		t, found, err := p.desk.GetTicketByID(ctx, ticketID)
		if err != nil {
			p.logger.Warn("ticket existence check failed", "ticket_id", ticketID, "error", err)
			continue
		}
		if !found {
			p.logger.Info("active ticket no longer exists in zoho (deleted), resetting client",
				"ticket_id", ticketID, "chat_id", chatID)
			p.handleClosure(ctx, chatID, zoho.TicketStatus{ID: ticketID})
			continue
		}
		if t.IsClosed() {
			// A closure missed earlier because the ticket had already fallen out
			// of the recent-modified window.
			p.handleClosure(ctx, chatID, t)
			continue
		}
		if err := p.store.SetTicketChecked(ctx, ticketID, nowMs); err != nil {
			p.logger.Error("updating ticket checked marker failed", "ticket_id", ticketID, "error", err)
		}
	}
}

// reconcile processes a single active ticket: detect closure or forward any new
// operator replies, gated by the stored modifiedTime marker so an unchanged
// ticket costs no further Zoho calls.
func (p *Poller) reconcile(ctx context.Context, chatID int64, t zoho.TicketStatus) {
	modified := parseZohoMillis(t.ModifiedTime)

	lastMod, err := p.store.GetTicketModSeen(ctx, t.ID)
	if err != nil {
		p.logger.Error("reading ticket modseen failed", "ticket_id", t.ID, "error", err)
		return
	}
	if modified != 0 && lastMod != 0 && modified <= lastMod {
		return // no change since we last processed this ticket
	}

	// Forward any new operator replies first — even on the cycle that detects
	// closure — so a final message posted just before the ticket was closed (the
	// operator's "we're closing this" note) still reaches the client before the
	// closure notice. handleClosure then wipes the markers, which is safe because
	// the comment has already been delivered.
	p.forwardNewComments(ctx, chatID, t.ID)

	if t.IsClosed() {
		p.handleClosure(ctx, chatID, t)
		return
	}

	if modified != 0 {
		if err := p.store.SetTicketModSeen(ctx, t.ID, modified); err != nil {
			p.logger.Error("updating ticket modseen failed", "ticket_id", t.ID, "error", err)
		}
	}
}

// forwardNewComments delivers operator public comments newer than the stored
// marker, in chronological order, then advances the marker.
func (p *Poller) forwardNewComments(ctx context.Context, chatID int64, ticketID string) {
	seen, err := p.store.GetTicketCommentSeen(ctx, ticketID)
	if err != nil {
		p.logger.Error("reading comment marker failed", "ticket_id", ticketID, "error", err)
		return
	}

	comments, err := p.desk.ListComments(ctx, ticketID, commentsLimit)
	if err != nil {
		p.logger.Error("listing comments failed", "ticket_id", ticketID, "error", err)
		return
	}

	type pending struct {
		millis  int64
		content string
	}
	var fresh []pending
	maxSeen := seen
	for _, c := range comments {
		ms := parseZohoMillis(c.CommentedTime)
		if seen != 0 && ms != 0 && ms <= seen {
			continue
		}
		if !c.IsPublic { // private = the bot's own client-relay comments
			continue
		}
		if strings.TrimSpace(c.Content) != "" {
			fresh = append(fresh, pending{millis: ms, content: c.Content})
		}
		if ms > maxSeen {
			maxSeen = ms
		}
	}

	// Zoho returns newest-first; deliver oldest-first so the chat reads in order.
	sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].millis < fresh[j].millis })
	for _, f := range fresh {
		// A long Cyrillic reply must be split on rune boundaries into multiple
		// valid messages; byte truncation would corrupt UTF-8 and Telegram would
		// reject it with a 400 (FIX-4). Send parts in order; on failure stop this
		// comment so the marker (advanced per cycle below) does not skip undelivered
		// text on a transient Telegram error mid-comment.
		for _, part := range tgtext.SplitMessage(f.content, tgtext.MaxRunes) {
			if err := p.sendText(chatID, part); err != nil {
				p.logger.Error("forwarding operator reply failed, skipping remaining parts",
					"chat_id", chatID, "ticket_id", ticketID, "error", err)
				break
			}
		}
	}

	if maxSeen != seen {
		if err := p.store.SetTicketCommentSeen(ctx, ticketID, maxSeen); err != nil {
			p.logger.Error("updating comment marker failed", "ticket_id", ticketID, "error", err)
		}
	}
}

// handleClosure mirrors the webhook closure path: clear the active ticket and
// conversation so the client can open a new one, and notify them.
func (p *Poller) handleClosure(ctx context.Context, chatID int64, t zoho.TicketStatus) {
	number := t.Number
	if number == "" {
		if ref, err := p.store.GetTicket(ctx, chatID); err == nil {
			number = ref.Number
		}
	}
	if err := p.store.DelTicket(ctx, chatID); err != nil {
		p.logger.Error("clearing ticket mapping on closure failed", "chat_id", chatID, "error", err)
	}
	if err := p.store.DelFSM(ctx, chatID); err != nil {
		p.logger.Error("clearing conversation on closure failed", "chat_id", chatID, "error", err)
	}
	if err := p.store.DelPendingComments(ctx, chatID); err != nil {
		p.logger.Error("clearing pending comments on closure failed", "chat_id", chatID, "error", err)
	}

	text := "Ваше обращение закрыто. Если вопрос всё ещё актуален, отправьте /start, чтобы создать новое."
	if number != "" {
		text = "Ваше обращение №" + number + " закрыто. Если вопрос всё ещё актуален, отправьте /start, чтобы создать новое."
	}
	p.send(chatID, text, t.ID)
	p.logger.Info("ticket closed via poll, client reset", "chat_id", chatID, "ticket_id", t.ID)
}

func (p *Poller) send(chatID int64, text, ticketID string) {
	if err := p.sendText(chatID, text); err != nil {
		p.logger.Error("forwarding to telegram failed", "chat_id", chatID, "ticket_id", ticketID, "error", err)
	}
}

// sendText sends a single message and returns the delivery error, letting
// callers stop a multi-part sequence on the first failure.
func (p *Poller) sendText(chatID int64, text string) error {
	_, err := p.api.Send(tgbotapi.NewMessage(chatID, text))
	return err
}

// parseZohoMillis parses a Zoho Desk timestamp into Unix milliseconds, trying
// the formats Zoho returns. Returns 0 if it cannot be parsed (treated as "no
// information", never as "older than everything").
func parseZohoMillis(s string) int64 {
	if s == "" {
		return 0
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z0700",
		"2006-01-02T15:04:05Z0700",
	}
	for _, layout := range layouts {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts.UnixMilli()
		}
	}
	return 0
}
