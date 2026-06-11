package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"supportbot/auth"
	store "supportbot/redis"
	"supportbot/registry"
	"supportbot/zoho"
)

// maxBindAttempts caps token-redemption attempts per chat within the rate-limit
// window.
const maxBindAttempts = 5

// bindingClaimTTL is how long a redeemed token's jti is remembered to prevent
// replay. It must strictly exceed the token's own validity, otherwise a token
// whose TTL was raised above the claim window could be replayed after the claim
// expired (FIX-8). Derived as BindingTokenTTL + 1h with a 24h floor.
func (b *Bot) bindingClaimTTL() time.Duration {
	ttl := b.cfg.BindingTokenTTL + time.Hour
	if ttl < 24*time.Hour {
		ttl = 24 * time.Hour
	}
	return ttl
}

// handleUpdate is the single entry point for both webhook and polling. Binding
// via "/start <token>" is handled before authorization; everything else
// requires an authorized (bound) chat.
func (b *Bot) handleUpdate(ctx context.Context, update tgbotapi.Update) {
	chatID, ok := chatIDFromUpdate(update)
	if !ok {
		return
	}

	// Skip a redelivered update so a free-text answer is not recorded twice.
	// Telegram redelivers when the 200 is slow — under a Zoho outage the handler
	// can take up to the full update timeout (FIX-11). On a Redis error treat the
	// update as fresh (fail open): dropping updates is worse than losing dedup.
	if update.UpdateID != 0 {
		if first, err := b.store.MarkUpdateProcessed(ctx, update.UpdateID); err != nil {
			b.logger.Warn("update dedup check failed, processing anyway", "update_id", update.UpdateID, "error", err)
		} else if !first {
			b.logger.Debug("skipping duplicate update", "update_id", update.UpdateID, "chat_id", chatID)
			return
		}
	}

	// Deep-link binding (backward compatibility): /start carrying a token still
	// binds immediately, so existing t.me/Bot?start=<token> links keep working.
	if msg := update.Message; msg != nil && msg.IsCommand() && msg.Command() == "start" {
		if arg := strings.TrimSpace(msg.CommandArguments()); arg != "" {
			lock := b.chatLock(chatID)
			lock.Lock()
			defer lock.Unlock()
			b.handleBinding(ctx, chatID, arg)
			return
		}
	}

	profile, authorized, err := b.registry.Authorize(ctx, chatID)
	if err != nil {
		b.logger.Error("authorization failed", "chat_id", chatID, "error", err)
		return
	}
	if !authorized {
		// Not bound yet: ask for the access key and accept it as a plain message.
		b.handleUnboundPrompt(ctx, chatID, update)
		return
	}

	lock := b.chatLock(chatID)
	lock.Lock()
	defer lock.Unlock()

	conv, err := b.loadConversation(ctx, chatID)
	if err != nil {
		b.logger.Error("loading conversation failed", "chat_id", chatID, "error", err)
		return
	}

	switch {
	case update.CallbackQuery != nil:
		b.handleCallback(ctx, conv, profile, update.CallbackQuery)
	case update.Message != nil:
		b.handleMessage(ctx, conv, profile, update.Message)
	}
}

// handleUnboundPrompt drives the message-based binding flow for a chat that is
// not yet bound: "/start" greets and asks for the access key, and any other
// text message is treated as the key and redeemed. Callbacks (stale buttons)
// are acknowledged and ignored. The brute-force guard in handleBinding caps how
// many keys a single chat may try.
func (b *Bot) handleUnboundPrompt(ctx context.Context, chatID int64, update tgbotapi.Update) {
	msg := update.Message
	if msg == nil {
		if update.CallbackQuery != nil {
			b.answerCallback(update.CallbackQuery.ID)
		}
		return
	}

	lock := b.chatLock(chatID)
	lock.Lock()
	defer lock.Unlock()

	const askKey = "Здравствуйте! Для доступа к поддержке отправьте ключ доступа, который выдал ваш менеджер."

	if msg.IsCommand() && msg.Command() == "start" {
		// A token passed as a deep-link argument is handled earlier; here /start
		// has no argument, so simply request the key.
		b.send(chatID, askKey)
		return
	}

	token := strings.TrimSpace(msg.Text)
	if token == "" {
		b.send(chatID, askKey)
		return
	}
	b.handleBinding(ctx, chatID, token)
}

// handleBinding redeems a binding token and, on success, starts the support
// flow for the now-bound chat.
func (b *Bot) handleBinding(ctx context.Context, chatID int64, token string) {
	// Fail closed: if the attempt counter cannot be read, refuse rather than let
	// binding proceed without a brute-force guard (FIX-7). This is consistent —
	// ClaimToken below also requires Redis, so binding cannot succeed without it.
	n, err := b.store.IncrBindAttempts(ctx, chatID)
	if err != nil {
		b.logger.Error("bind attempt counter failed, refusing", "chat_id", chatID, "error", err)
		b.send(chatID, "Временная ошибка. Пожалуйста, попробуйте позже.")
		return
	}
	if n > maxBindAttempts {
		b.logger.Warn("binding rate limit exceeded", "chat_id", chatID, "attempts", n)
		b.send(chatID, "Слишком много попыток. Пожалуйста, попробуйте позже.")
		return
	}

	token = strings.TrimSpace(token)
	b.logger.Debug("verifying binding token", "chat_id", chatID, "token_len", len(token))

	botClientID, jti, err := b.auth.Verify(token)
	if err != nil {
		b.logger.Warn("binding token rejected", "chat_id", chatID, "token_len", len(token), "error", err)
		if errors.Is(err, auth.ErrExpired) {
			b.send(chatID, "Срок действия ключа истёк. Запросите новый у вашего менеджера.")
		} else {
			b.send(chatID, "Ключ недействителен. Проверьте его и отправьте ещё раз или запросите новый у менеджера.")
		}
		return
	}

	claimed, err := b.store.ClaimToken(ctx, jti, b.bindingClaimTTL())
	if err != nil {
		b.logger.Error("claiming token failed", "chat_id", chatID, "error", err)
		b.send(chatID, "Временная ошибка. Пожалуйста, попробуйте позже.")
		return
	}
	if !claimed {
		b.send(chatID, "Этот ключ уже использован.")
		return
	}

	profile, err := b.registry.Bind(ctx, chatID, botClientID)
	if err != nil {
		switch {
		case errors.Is(err, registry.ErrUnknownClient):
			b.send(chatID, "Ключ недействителен.")
		case errors.Is(err, registry.ErrChatBound):
			b.send(chatID, "Этот чат уже привязан к другому клиенту.")
		default:
			// Transient failure: release the claim so the client can retry.
			b.logger.Error("binding failed", "chat_id", chatID, "bot_client_id", botClientID, "error", err)
			if rerr := b.store.ReleaseToken(ctx, jti); rerr != nil {
				b.logger.Error("releasing token claim failed", "chat_id", chatID, "error", rerr)
			}
			b.send(chatID, "Временная ошибка при привязке. Пожалуйста, попробуйте позже.")
		}
		return
	}

	b.logger.Info("chat bound to client", "chat_id", chatID, "bot_client_id", botClientID)
	conv := &Conversation{ChatID: chatID, State: StateIdle}
	b.startFlow(ctx, conv, profile)
}

func (b *Bot) handleMessage(ctx context.Context, conv *Conversation, profile registry.Profile, msg *tgbotapi.Message) {
	if msg.IsCommand() && msg.Command() == "start" {
		// A client may have only one active ticket at a time.
		if conv.State == StateTicketOpen {
			b.notifyActiveTicket(ctx, conv.ChatID)
			return
		}
		b.startFlow(ctx, conv, profile)
		return
	}

	switch conv.State {
	case StateAskingQuestions:
		// Capture any attached media (proof/logs) so it can be uploaded to the
		// ticket once it exists; the recorded answer keeps a textual note.
		conv.Attachments = append(conv.Attachments, attachmentsFromMessage(msg)...)
		b.recordTextAnswer(ctx, conv, messageContent(msg))

	case StateTicketOpen:
		b.appendClientComment(ctx, conv.ChatID, messageContent(msg))

	case StateAwaitingService, StateConfirming:
		b.repromptCurrentStep(conv, profile)

	default: // StateIdle or unknown
		b.startFlow(ctx, conv, profile)
	}
}

func (b *Bot) handleCallback(ctx context.Context, conv *Conversation, profile registry.Profile, cq *tgbotapi.CallbackQuery) {
	b.answerCallback(cq.ID)
	data := cq.Data

	switch {
	case strings.HasPrefix(data, cbService):
		b.handleServiceSelection(ctx, conv, profile, strings.TrimPrefix(data, cbService))
	case strings.HasPrefix(data, cbAnswer):
		b.handleOptionAnswer(ctx, conv, strings.TrimPrefix(data, cbAnswer))
	case strings.HasPrefix(data, cbConfirm):
		b.handleConfirm(ctx, conv, profile, strings.TrimPrefix(data, cbConfirm))
	default:
		b.logger.Warn("unknown callback data", "chat_id", conv.ChatID, "data", data)
	}
}

// startFlow greets the client and presents the service selection keyboard.
func (b *Bot) startFlow(ctx context.Context, conv *Conversation, profile registry.Profile) {
	conv.State = StateAwaitingService
	conv.Service = ""
	conv.Group = ""
	conv.Category = ""
	conv.QuestionIndex = 0
	conv.Answers = nil
	conv.Attachments = nil

	if err := b.saveConversation(ctx, conv); err != nil {
		b.logger.Error("saving conversation failed", "chat_id", conv.ChatID, "error", err)
	}

	greeting := "Здравствуйте! По какому сервису возник вопрос?"
	b.sendWithKeyboard(conv.ChatID, greeting, serviceKeyboard(profile.Services))
}

func (b *Bot) handleServiceSelection(ctx context.Context, conv *Conversation, profile registry.Profile, service string) {
	if conv.State != StateAwaitingService {
		return // stale button
	}
	if !serviceAllowed(profile.Services, service) {
		b.logger.Warn("client selected a service not assigned to them", "chat_id", conv.ChatID, "service", service)
		b.sendWithKeyboard(conv.ChatID, "Пожалуйста, выберите один из доступных сервисов.", serviceKeyboard(profile.Services))
		return
	}

	conv.Service = service
	conv.Group = groupForService(service)
	conv.Category = categoryForGroup(conv.Group)
	b.beginQuestions(ctx, conv)
}

func (b *Bot) beginQuestions(ctx context.Context, conv *Conversation) {
	conv.State = StateAskingQuestions
	conv.QuestionIndex = 0
	conv.Answers = nil
	conv.Attachments = nil
	if err := b.saveConversation(ctx, conv); err != nil {
		b.logger.Error("saving conversation failed", "chat_id", conv.ChatID, "error", err)
	}
	b.askCurrentQuestion(conv)
}

func (b *Bot) askCurrentQuestion(conv *Conversation) {
	q, ok := conv.currentQuestion()
	if !ok {
		return
	}
	if len(q.Options) > 0 {
		b.sendWithKeyboard(conv.ChatID, q.Prompt, optionsKeyboard(q.Options))
		return
	}
	b.send(conv.ChatID, q.Prompt)
}

func (b *Bot) recordTextAnswer(ctx context.Context, conv *Conversation, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		b.askCurrentQuestion(conv)
		return
	}
	more := conv.recordAnswer(value)
	b.advanceAfterAnswer(ctx, conv, more)
}

func (b *Bot) handleOptionAnswer(ctx context.Context, conv *Conversation, idxStr string) {
	if conv.State != StateAskingQuestions {
		return
	}
	q, ok := conv.currentQuestion()
	if !ok || len(q.Options) == 0 {
		return
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || idx >= len(q.Options) {
		b.logger.Warn("invalid option index", "chat_id", conv.ChatID, "value", idxStr)
		return
	}
	more := conv.recordAnswer(q.Options[idx])
	b.advanceAfterAnswer(ctx, conv, more)
}

func (b *Bot) advanceAfterAnswer(ctx context.Context, conv *Conversation, more bool) {
	if err := b.saveConversation(ctx, conv); err != nil {
		b.logger.Error("saving conversation failed", "chat_id", conv.ChatID, "error", err)
	}
	if more {
		b.askCurrentQuestion(conv)
		return
	}
	conv.State = StateConfirming
	if err := b.saveConversation(ctx, conv); err != nil {
		b.logger.Error("saving conversation failed", "chat_id", conv.ChatID, "error", err)
	}
	b.sendWithKeyboard(conv.ChatID, conv.confirmationSummary(), confirmKeyboard())
}

func (b *Bot) handleConfirm(ctx context.Context, conv *Conversation, profile registry.Profile, action string) {
	if conv.State != StateConfirming {
		return
	}
	switch action {
	case "restart":
		b.startFlow(ctx, conv, profile)
	case "yes":
		b.finalizeTicket(ctx, conv, profile)
	}
}

// finalizeTicket creates the Zoho ticket (or enqueues it on failure), stores the
// chat→ticket mapping, and switches the conversation to silent forwarding mode.
func (b *Bot) finalizeTicket(ctx context.Context, conv *Conversation, profile registry.Profile) {
	priority := conv.detectPriority()
	// The account's configured default acts as a floor: a client flagged HIGH in
	// Zoho always gets a High ticket regardless of the per-request heuristic (FIX-14).
	if isPriorityFloorHigh(profile.Priority) {
		priority = zoho.PriorityHigh
	}

	req := zoho.CreateTicketRequest{
		ChatID:         conv.ChatID,
		AccountID:      profile.ZohoAccountID,
		Subject:        conv.subject(),
		Description:    conv.description(profile.Company, priority),
		Category:       conv.Category,
		Priority:       priority,
		ContactName:    profile.Company,
		ContactCompany: profile.Company,
		Attachments:    conv.Attachments,
		// Stable per-request id for idempotency. Generated once here so it is the
		// same on the first attempt and on every queue retry of this request (the
		// request is JSON-persisted in Redis), enabling duplicate detection (FIX-5).
		IdempotencyKey: newIdempotencyKey(),
	}

	// Switch to silent mode first so any client message arriving during ticket
	// creation is buffered as a pending comment rather than dropped.
	conv.State = StateTicketOpen
	if err := b.saveConversation(ctx, conv); err != nil {
		b.logger.Error("saving conversation failed", "chat_id", conv.ChatID, "error", err)
	}

	created, err := b.desk.CreateTicket(ctx, req)
	if err != nil {
		b.logger.Error("ticket creation failed, enqueueing for retry", "chat_id", conv.ChatID, "error", err)
		if qerr := b.queue.Enqueue(ctx, req); qerr != nil {
			// Could not even persist the request: do not strand the client in
			// StateTicketOpen with no ticket and no retry. Revert to the
			// confirmation step so they can try again.
			b.logger.Error("enqueueing ticket failed, reverting to confirmation", "chat_id", conv.ChatID, "error", qerr)
			conv.State = StateConfirming
			if serr := b.saveConversation(ctx, conv); serr != nil {
				b.logger.Error("saving conversation failed", "chat_id", conv.ChatID, "error", serr)
			}
			b.sendWithKeyboard(conv.ChatID, "Не удалось зарегистрировать обращение. Пожалуйста, попробуйте ещё раз.", confirmKeyboard())
			return
		}
		b.send(conv.ChatID, "Ваше обращение принято и регистрируется. Специалист свяжется с вами здесь в ближайшее время.")
		return
	}

	if err := b.store.SetTicket(ctx, conv.ChatID, store.TicketRef{ID: created.ID, Number: created.Number}); err != nil {
		b.logger.Error("storing ticket mapping failed", "chat_id", conv.ChatID, "ticket_id", created.ID, "error", err)
	}
	b.uploader.Upload(ctx, created.ID, req.Attachments)
	b.flushPendingComments(ctx, conv.ChatID, created.ID)
	b.send(conv.ChatID, ticketCreatedMessage(created.Number))
}

// notifyActiveTicket reminds the client they already have an open ticket.
func (b *Bot) notifyActiveTicket(ctx context.Context, chatID int64) {
	ref, err := b.store.GetTicket(ctx, chatID)
	if err != nil {
		b.logger.Error("reading ticket mapping failed", "chat_id", chatID, "error", err)
	}
	msg := "У вас уже есть активное обращение"
	if ref.Number != "" {
		msg += " №" + ref.Number
	}
	msg += ". Дождитесь его закрытия, прежде чем создавать новое. Сообщения в этом чате передаются оператору."
	b.send(chatID, msg)
}

func ticketCreatedMessage(number string) string {
	if number != "" {
		return "Спасибо! Ваше обращение зарегистрировано, номер №" + number + ". Специалист свяжется с вами здесь в ближайшее время."
	}
	return "Спасибо! Ваше обращение зарегистрировано. Специалист свяжется с вами здесь в ближайшее время."
}

// appendClientComment forwards a client message to the open ticket as a comment.
// If the ticket does not exist yet (still queued), the message is buffered.
func (b *Bot) appendClientComment(ctx context.Context, chatID int64, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	ref, err := b.store.GetTicket(ctx, chatID)
	if err != nil {
		b.logger.Error("reading ticket mapping failed", "chat_id", chatID, "error", err)
	}
	if ref.ID == "" {
		if err := b.store.PushPendingComment(ctx, chatID, content); err != nil {
			b.logger.Error("buffering pending comment failed", "chat_id", chatID, "error", err)
		}
		return
	}
	if err := b.desk.AddComment(ctx, ref.ID, "Клиент: "+content); err != nil {
		b.logger.Error("forwarding client comment failed, buffering", "chat_id", chatID, "ticket_id", ref.ID, "error", err)
		if perr := b.store.PushPendingComment(ctx, chatID, content); perr != nil {
			b.logger.Error("buffering pending comment failed", "chat_id", chatID, "error", perr)
		}
	}
}

func (b *Bot) flushPendingComments(ctx context.Context, chatID int64, ticketID string) {
	comments, err := b.store.PopAllPendingComments(ctx, chatID)
	if err != nil {
		b.logger.Error("reading pending comments failed", "chat_id", chatID, "error", err)
		return
	}
	for _, c := range comments {
		if err := b.desk.AddComment(ctx, ticketID, "Клиент: "+c); err != nil {
			b.logger.Error("flushing pending comment failed", "chat_id", chatID, "ticket_id", ticketID, "error", err)
		}
	}
}

// repromptCurrentStep re-sends the prompt/keyboard appropriate to the state when
// the client types instead of pressing a button.
func (b *Bot) repromptCurrentStep(conv *Conversation, profile registry.Profile) {
	switch conv.State {
	case StateAwaitingService:
		b.sendWithKeyboard(conv.ChatID, "Пожалуйста, выберите сервис.", serviceKeyboard(profile.Services))
	case StateConfirming:
		b.sendWithKeyboard(conv.ChatID, conv.confirmationSummary(), confirmKeyboard())
	}
}

// ---- helpers ----

func chatIDFromUpdate(update tgbotapi.Update) (int64, bool) {
	switch {
	case update.Message != nil && update.Message.Chat != nil:
		return update.Message.Chat.ID, true
	case update.CallbackQuery != nil && update.CallbackQuery.Message != nil && update.CallbackQuery.Message.Chat != nil:
		return update.CallbackQuery.Message.Chat.ID, true
	default:
		return 0, false
	}
}

// messageContent extracts a textual representation of a message, with a short
// note for attached media. The actual file is forwarded separately as a ticket
// attachment (see attachmentsFromMessage), so the note no longer carries the
// raw Telegram file_id, which is useless to an operator.
func messageContent(msg *tgbotapi.Message) string {
	if msg.Text != "" {
		return msg.Text
	}
	if len(msg.Photo) > 0 {
		note := "[приложен скриншот]"
		if msg.Caption != "" {
			note = msg.Caption + " " + note
		}
		return note
	}
	if msg.Document != nil {
		note := "[приложен файл: " + msg.Document.FileName + "]"
		if msg.Caption != "" {
			note = msg.Caption + " " + note
		}
		return note
	}
	if msg.Caption != "" {
		return msg.Caption
	}
	return "[неподдерживаемый тип сообщения]"
}

// attachmentsFromMessage extracts uploadable files (photos, documents) from a
// message. Photos have no Telegram filename, so a stable one is synthesized
// from the file_unique_id.
func attachmentsFromMessage(msg *tgbotapi.Message) []zoho.Attachment {
	var out []zoho.Attachment
	if len(msg.Photo) > 0 {
		largest := msg.Photo[len(msg.Photo)-1]
		out = append(out, zoho.Attachment{
			FileID:   largest.FileID,
			FileName: "screenshot-" + largest.FileUniqueID + ".jpg",
		})
	}
	if msg.Document != nil {
		name := msg.Document.FileName
		if name == "" {
			name = "document-" + msg.Document.FileUniqueID
		}
		out = append(out, zoho.Attachment{FileID: msg.Document.FileID, FileName: name})
	}
	return out
}

// newIdempotencyKey returns a random 16-byte hex token used to tag a ticket
// creation request for duplicate detection (FIX-5). crypto/rand.Read never fails
// on the platforms we target; on the impossible error we return "" and the key
// is simply omitted (degrades to today's behavior).
// isPriorityFloorHigh reports whether an account's configured cf_bot_priority
// value should force a High ticket. It accepts the canonical "HIGH" and the
// legacy "key" label (case-insensitive, trimmed) used in bootstrap configs.
func isPriorityFloorHigh(p string) bool {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "high", "key":
		return true
	default:
		return false
	}
}

func newIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

func serviceAllowed(services []string, service string) bool {
	if service == "Billing" {
		return true // account-level support is always available
	}
	return contains(services, service)
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
