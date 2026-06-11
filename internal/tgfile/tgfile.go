// Package tgfile centralizes downloading a Telegram file and uploading it to a
// Zoho Desk ticket as an attachment. It is shared by the bot (synchronous ticket
// path) and the queue worker (retry path) so the bounded-download behavior lives
// in exactly one place (FIX-3).
package tgfile

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"supportbot/zoho"
)

// maxAttachmentBytes caps a downloaded Telegram file. The Bot API only serves
// files up to 20 MiB anyway; this bounds memory if that ever changes.
const maxAttachmentBytes = 20 << 20

// downloadTimeout bounds a single file download. The caller's context may carry
// a deadline too; this is an independent ceiling so one stalled download can
// never block the queue worker forever (the worker's context has no deadline).
// 2 minutes leaves headroom for a 20 MiB file over a slow link.
const downloadTimeout = 2 * time.Minute

// Uploader fetches Telegram files and uploads them to Zoho Desk tickets.
type Uploader struct {
	api    *tgbotapi.BotAPI
	desk   *zoho.Desk
	http   *http.Client
	logger *slog.Logger
}

// NewUploader builds an Uploader with a dedicated HTTP client (separate from
// http.DefaultClient, which has no timeout).
func NewUploader(api *tgbotapi.BotAPI, desk *zoho.Desk, logger *slog.Logger) *Uploader {
	return &Uploader{
		api:    api,
		desk:   desk,
		http:   &http.Client{Timeout: downloadTimeout},
		logger: logger,
	}
}

// Upload fetches each referenced Telegram file and uploads it to the ticket.
// Failures are logged but never block the ticket flow: the structured
// description and notes already reached the operator.
func (u *Uploader) Upload(ctx context.Context, ticketID string, atts []zoho.Attachment) {
	for _, a := range atts {
		data, err := u.download(ctx, a.FileID)
		if err != nil {
			u.logger.Error("downloading telegram attachment failed", "ticket_id", ticketID, "file_id", a.FileID, "error", err)
			continue
		}
		if err := u.desk.AddAttachment(ctx, ticketID, a.FileName, data); err != nil {
			u.logger.Error("uploading attachment to zoho failed", "ticket_id", ticketID, "filename", a.FileName, "error", err)
		}
	}
}

// download resolves a file_id to its direct URL and downloads the bytes (capped
// at maxAttachmentBytes), bounded by downloadTimeout.
func (u *Uploader) download(ctx context.Context, fileID string) ([]byte, error) {
	directURL, err := u.api.GetFileDirectURL(fileID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, directURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram file download status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxAttachmentBytes))
}
