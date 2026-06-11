// Package queue implements the Zoho-outage fallback: ticket-creation requests
// that fail are persisted in Redis and retried by a background worker until
// Zoho accepts them. Because the queue lives in Redis it survives restarts.
package queue

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"supportbot/internal/tgfile"
	store "supportbot/redis"
	"supportbot/zoho"
)

const (
	// retryInterval is how often the worker drains the pending queue.
	retryInterval = 30 * time.Second
	// itemTimeout bounds processing of a single queued request (ticket creation
	// plus its attachment uploads) so no one item can stall the drain loop
	// indefinitely — the worker's root context has no deadline (FIX-3).
	itemTimeout = 5 * time.Minute
)

// Worker drains the pending ticket queue and creates the tickets.
type Worker struct {
	store    *store.Client
	desk     *zoho.Desk
	api      *tgbotapi.BotAPI
	uploader *tgfile.Uploader
	logger   *slog.Logger
}

// NewWorker constructs the retry worker.
func NewWorker(st *store.Client, desk *zoho.Desk, api *tgbotapi.BotAPI, logger *slog.Logger) *Worker {
	return &Worker{
		store:    st,
		desk:     desk,
		api:      api,
		uploader: tgfile.NewUploader(api, desk, logger),
		logger:   logger,
	}
}

// Enqueue persists a ticket-creation request for later retry.
func (w *Worker) Enqueue(ctx context.Context, req zoho.CreateTicketRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return w.store.EnqueueTicket(ctx, data)
}

// Run blocks until ctx is cancelled, draining the queue every retryInterval.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()

	w.logger.Info("ticket retry worker started", "interval", retryInterval.String())
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("ticket retry worker stopped")
			return
		case <-ticker.C:
			w.drain(ctx)
		}
	}
}

// drain pops and processes every currently queued request. Requests that still
// fail are re-enqueued for the next tick, and the drain stops so we do not spin
// on a persistent Zoho outage.
func (w *Worker) drain(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		data, err := w.store.DequeueTicket(ctx)
		if err != nil {
			w.logger.Error("dequeue ticket failed", "error", err)
			return
		}
		if data == nil {
			return // queue empty
		}

		var req zoho.CreateTicketRequest
		if err := json.Unmarshal(data, &req); err != nil {
			// Unrecoverable: a malformed entry would block the queue forever.
			w.logger.Error("dropping malformed queued ticket", "error", err)
			continue
		}

		// Bound each item so a stalled attachment download can never freeze the
		// drain loop indefinitely (FIX-3); the worker's root context has no deadline.
		itemCtx, cancel := context.WithTimeout(ctx, itemTimeout)
		ok := w.process(itemCtx, req)
		cancel()
		if !ok {
			// Re-enqueue at the tail and stop draining until the next tick.
			if err := w.Enqueue(ctx, req); err != nil {
				w.logger.Error("re-enqueue ticket failed", "chat_id", req.ChatID, "error", err)
			}
			return
		}
	}
}

// process attempts to create the ticket and finalize routing. It returns true
// on success.
func (w *Worker) process(ctx context.Context, req zoho.CreateTicketRequest) bool {
	created, err := w.desk.CreateTicket(ctx, req)
	if err != nil {
		w.logger.Warn("queued ticket creation still failing", "chat_id", req.ChatID, "error", err)
		return false
	}

	if err := w.store.SetTicket(ctx, req.ChatID, store.TicketRef{ID: created.ID, Number: created.Number}); err != nil {
		w.logger.Error("storing ticket mapping failed", "chat_id", req.ChatID, "ticket_id", created.ID, "error", err)
	}

	w.uploader.Upload(ctx, created.ID, req.Attachments)
	w.flushPendingComments(ctx, req.ChatID, created.ID)

	text := "Ваше обращение зарегистрировано. Специалист свяжется с вами здесь в ближайшее время."
	if created.Number != "" {
		text = "Ваше обращение зарегистрировано, номер №" + created.Number + ". Специалист свяжется с вами здесь в ближайшее время."
	}
	if _, err := w.api.Send(tgbotapi.NewMessage(req.ChatID, text)); err != nil {
		w.logger.Error("notifying client of queued ticket failed", "chat_id", req.ChatID, "error", err)
	}

	w.logger.Info("queued ticket created on retry", "chat_id", req.ChatID, "ticket_id", created.ID, "ticket_number", created.Number)
	return true
}

// flushPendingComments posts any client messages buffered while the ticket did
// not yet exist.
func (w *Worker) flushPendingComments(ctx context.Context, chatID int64, ticketID string) {
	comments, err := w.store.PopAllPendingComments(ctx, chatID)
	if err != nil {
		w.logger.Error("reading pending comments failed", "chat_id", chatID, "error", err)
		return
	}
	for _, content := range comments {
		if err := w.desk.AddComment(ctx, ticketID, "Клиент: "+content); err != nil {
			w.logger.Error("flushing pending comment failed", "chat_id", chatID, "ticket_id", ticketID, "error", err)
		}
	}
}
