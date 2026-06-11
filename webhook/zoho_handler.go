// Package webhook receives inbound Zoho Desk webhooks and forwards operator
// replies to the correct Telegram chat.
package webhook

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"supportbot/internal/tgtext"
	store "supportbot/redis"
)

// sendTimeout bounds Telegram delivery so the operator reply reaches the client
// within the 3-second SLA.
const sendTimeout = 3 * time.Second

// ZohoHandler handles POST /zoho/webhook.
type ZohoHandler struct {
	api          *tgbotapi.BotAPI
	store        *store.Client
	webhookToken string
	logger       *slog.Logger
}

// NewZohoHandler constructs the handler. webhookToken is the shared secret the
// Zoho webhook must present in the X-Webhook-Token header (FIX-6: the query-string
// fallback was removed because it leaks into proxy/access logs).
func NewZohoHandler(api *tgbotapi.BotAPI, st *store.Client, webhookToken string, logger *slog.Logger) *ZohoHandler {
	logger.Info("zoho webhook handler mounted; the Zoho webhook rule must send the secret in the X-Webhook-Token header (query ?token= is not accepted)")
	return &ZohoHandler{api: api, store: st, webhookToken: webhookToken, logger: logger}
}

// payload is the JSON shape the Zoho Desk webhook must be configured to send.
// See README for the field mapping to configure in Zoho Desk. Two event kinds
// are supported: an operator reply (content set) and a ticket closure
// (status = "Closed").
type payload struct {
	TicketID     string `json:"ticketId"`
	TicketNumber string `json:"ticketNumber"`
	ChatID       string `json:"chatId"`
	Content      string `json:"content"`
	Status       string `json:"status"`
}

func (h *ZohoHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.validToken(r) {
		h.logger.Warn("zoho webhook rejected: bad token", "remote", r.RemoteAddr)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		h.logger.Error("reading zoho webhook body failed", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		h.logger.Error("decoding zoho webhook failed", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	chatID, perr := strconv.ParseInt(strings.TrimSpace(p.ChatID), 10, 64)
	if perr != nil {
		h.logger.Info("zoho webhook ignored: missing/invalid chat id", "ticket_id", p.TicketID)
		w.WriteHeader(http.StatusOK)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), sendTimeout)
	defer cancel()

	// Ticket closed: clear the active-ticket binding and conversation so the
	// client can open a new one with the full flow.
	if strings.EqualFold(strings.TrimSpace(p.Status), "Closed") {
		h.handleClosure(ctx, chatID, p)
		w.WriteHeader(http.StatusOK)
		return
	}

	content := strings.TrimSpace(p.Content)
	if content == "" {
		// Nothing actionable (e.g. a non-reply, non-closure event).
		h.logger.Info("zoho webhook ignored: empty content", "ticket_id", p.TicketID)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Keep the chat→ticket mapping fresh (without clobbering a stored number)
	// so subsequent client replies thread to this ticket even after expiry.
	if p.TicketID != "" {
		if ref, gerr := h.store.GetTicket(ctx, chatID); gerr == nil && ref.ID == "" {
			if serr := h.store.SetTicket(ctx, chatID, store.TicketRef{ID: p.TicketID, Number: p.TicketNumber}); serr != nil {
				h.logger.Warn("refreshing ticket mapping failed", "chat_id", chatID, "ticket_id", p.TicketID, "error", serr)
			}
		}
	}

	// Split on rune boundaries so a long Cyrillic reply is delivered as several
	// valid messages rather than rejected by Telegram for severed UTF-8 (FIX-4).
	for _, part := range tgtext.SplitMessage(content, tgtext.MaxRunes) {
		if _, err := h.api.Send(tgbotapi.NewMessage(chatID, part)); err != nil {
			h.logger.Error("forwarding operator reply to telegram failed", "chat_id", chatID, "ticket_id", p.TicketID, "error", err)
			http.Error(w, "delivery failed", http.StatusBadGateway)
			return
		}
	}

	h.logger.Info("operator reply forwarded", "chat_id", chatID, "ticket_id", p.TicketID)
	w.WriteHeader(http.StatusOK)
}

// handleClosure clears the active ticket and conversation state and notifies the
// client that they may open a new request.
func (h *ZohoHandler) handleClosure(ctx context.Context, chatID int64, p payload) {
	number := p.TicketNumber
	if number == "" {
		if ref, err := h.store.GetTicket(ctx, chatID); err == nil {
			number = ref.Number
		}
	}
	if err := h.store.DelTicket(ctx, chatID); err != nil {
		h.logger.Error("clearing ticket mapping on closure failed", "chat_id", chatID, "error", err)
	}
	if err := h.store.DelFSM(ctx, chatID); err != nil {
		h.logger.Error("clearing conversation on closure failed", "chat_id", chatID, "error", err)
	}
	// Drop any comments buffered while a ticket was pending but never created;
	// otherwise they would linger for the full 30d TTL.
	if err := h.store.DelPendingComments(ctx, chatID); err != nil {
		h.logger.Error("clearing pending comments on closure failed", "chat_id", chatID, "error", err)
	}

	text := "Ваше обращение закрыто. Если вопрос всё ещё актуален, отправьте /start, чтобы создать новое."
	if number != "" {
		text = "Ваше обращение №" + number + " закрыто. Если вопрос всё ещё актуален, отправьте /start, чтобы создать новое."
	}
	if _, err := h.api.Send(tgbotapi.NewMessage(chatID, text)); err != nil {
		h.logger.Error("notifying client of closure failed", "chat_id", chatID, "error", err)
	}
	h.logger.Info("ticket closed, client reset", "chat_id", chatID, "ticket_id", p.TicketID)
}

// validToken compares the presented token in constant time. Only the
// X-Webhook-Token header is accepted: a query-string token would persist in
// nginx/FastPanel/BitNinja access logs and intermediate proxies (FIX-6).
func (h *ZohoHandler) validToken(r *http.Request) bool {
	presented := r.Header.Get("X-Webhook-Token")
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(h.webhookToken)) == 1
}
