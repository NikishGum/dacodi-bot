package zoho

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Priority levels used by the bot before mapping to Zoho's vocabulary.
const (
	PriorityHigh   = "HIGH"
	PriorityNormal = "NORMAL"
)

// Attachment references a Telegram file the client sent as proof. Only the
// file_id and metadata are carried (not the bytes), so a request stays small
// enough to persist in the Redis retry queue; the bytes are fetched from
// Telegram at the moment of upload.
type Attachment struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
}

// CreateTicketRequest is the self-contained payload needed to create a ticket.
// It is JSON-serializable so it can be persisted in the Redis retry queue.
type CreateTicketRequest struct {
	ChatID         int64        `json:"chat_id"`
	AccountID      string       `json:"account_id"`
	Subject        string       `json:"subject"`
	Description    string       `json:"description"`
	Category       string       `json:"category"`
	Priority       string       `json:"priority"`
	ContactName    string       `json:"contact_name"`
	ContactCompany string       `json:"contact_company"`
	Attachments    []Attachment `json:"attachments,omitempty"`
	// IdempotencyKey is a random per-request id written to the configured ticket
	// custom field so a duplicate creation (e.g. a retry after a lost response)
	// can be detected. It is JSON-persisted with the request so it survives the
	// Redis retry queue and restarts (FIX-5).
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// AccountInfo is the subset of a Zoho Desk account the bot cares about.
type AccountInfo struct {
	ID          string
	Name        string
	BotClientID uint32
	ChatID      int64 // 0 when not bound
	Services    []string
	Priority    string
}

// Desk provides typed Zoho Desk operations over the base Client.
type Desk struct {
	client        *Client
	deptID        string
	cfChatID      string
	cfBotClientID string
	cfServices    string
	cfPriority    string
	cfRequestID   string
	contactDomain string
	logger        *slog.Logger
}

// DeskConfig carries the Desk-specific identifiers and custom-field API names.
type DeskConfig struct {
	DeptID        string
	CFChatID      string
	CFBotClientID string
	CFServices    string
	CFPriority    string
	CFRequestID   string
	ContactDomain string
}

// NewDesk builds a Desk API wrapper.
func NewDesk(client *Client, cfg DeskConfig, logger *slog.Logger) *Desk {
	return &Desk{
		client:        client,
		deptID:        cfg.DeptID,
		cfChatID:      cfg.CFChatID,
		cfBotClientID: cfg.CFBotClientID,
		cfServices:    cfg.CFServices,
		cfPriority:    cfg.CFPriority,
		cfRequestID:   cfg.CFRequestID,
		contactDomain: cfg.ContactDomain,
		logger:        logger,
	}
}

type createTicketResponse struct {
	ID           string `json:"id"`
	TicketNumber string `json:"ticketNumber"`
}

// CreatedTicket is the result of a successful ticket creation.
type CreatedTicket struct {
	ID     string
	Number string // human-friendly ticket number shown to the client
}

// CreateTicket creates a Desk ticket and returns its ID and number. The
// Telegram chat ID is written to the configured custom field (for webhook reply
// routing) and to a synthesized contact; when AccountID is set the ticket is
// attached to it.
func (d *Desk) CreateTicket(ctx context.Context, req CreateTicketRequest) (CreatedTicket, error) {
	lastName := req.ContactName
	if lastName == "" {
		lastName = "Telegram " + strconv.FormatInt(req.ChatID, 10)
	}

	payload := map[string]any{
		"subject":      req.Subject,
		"description":  req.Description,
		"departmentId": d.deptID,
		"priority":     zohoPriority(req.Priority),
		"category":     req.Category,
		"channel":      "Telegram",
		"contact": map[string]any{
			"lastName": lastName,
			"email":    d.contactEmail(req.ChatID),
		},
		"cf": map[string]any{
			d.cfChatID: strconv.FormatInt(req.ChatID, 10),
		},
	}
	// Tag the ticket with the idempotency key when a custom field is configured,
	// so a duplicate creation can later be detected by search (FIX-5). Harmless
	// when either side is unset.
	if d.cfRequestID != "" && req.IdempotencyKey != "" {
		payload["cf"].(map[string]any)[d.cfRequestID] = req.IdempotencyKey
	}
	if req.AccountID != "" {
		payload["accountId"] = req.AccountID
	}

	var out createTicketResponse
	if err := d.client.do(ctx, "POST", "/api/v1/tickets", payload, &out); err != nil {
		return CreatedTicket{}, fmt.Errorf("creating ticket: %w", err)
	}
	if out.ID == "" {
		return CreatedTicket{}, fmt.Errorf("ticket created but response contained no id")
	}
	d.logger.Info("zoho ticket created", "ticket_id", out.ID, "ticket_number", out.TicketNumber, "chat_id", req.ChatID, "priority", req.Priority, "category", req.Category)
	return CreatedTicket{ID: out.ID, Number: out.TicketNumber}, nil
}

// TicketStatus is the slim view of a ticket returned by the modified-time list,
// carrying just what the poller needs to detect operator replies and closures.
type TicketStatus struct {
	ID           string
	Number       string
	Status       string
	StatusType   string
	ModifiedTime string
}

// IsClosed reports whether the ticket is in a closed state. statusType is the
// stable signal ("Open"/"Closed"/"On Hold") regardless of any custom status
// label; the raw status is a fallback for older API responses.
func (t TicketStatus) IsClosed() bool {
	return strings.EqualFold(strings.TrimSpace(t.StatusType), "Closed") ||
		strings.EqualFold(strings.TrimSpace(t.Status), "Closed")
}

type ticketStatusRaw struct {
	ID           string `json:"id"`
	TicketNumber string `json:"ticketNumber"`
	Status       string `json:"status"`
	StatusType   string `json:"statusType"`
	ModifiedTime string `json:"modifiedTime"`
}

type ticketStatusList struct {
	Data []ticketStatusRaw `json:"data"`
}

// ListRecentTickets returns one page of the department's tickets sorted by most
// recently modified first. The poller reads as few pages as possible: a single
// page covers every ticket changed since the previous cycle at the expected
// scale. This is the one shared call that detects changes across all active
// tickets, keeping API-credit usage flat regardless of how many are open.
func (d *Desk) ListRecentTickets(ctx context.Context, from, limit int) ([]TicketStatus, error) {
	path := fmt.Sprintf("/api/v1/tickets?departmentId=%s&sortBy=-modifiedTime&from=%d&limit=%d", d.deptID, from, limit)
	var page ticketStatusList
	if err := d.client.do(ctx, "GET", path, nil, &page); err != nil {
		return nil, fmt.Errorf("listing recent tickets: %w", err)
	}
	out := make([]TicketStatus, 0, len(page.Data))
	for _, t := range page.Data {
		out = append(out, TicketStatus{
			ID:           t.ID,
			Number:       t.TicketNumber,
			Status:       t.Status,
			StatusType:   t.StatusType,
			ModifiedTime: t.ModifiedTime,
		})
	}
	return out, nil
}

// GetTicketByID fetches a single ticket's status. found is false when Zoho
// reports the ticket no longer exists (404) — e.g. it was deleted or moved to
// the recycle bin — which the poller treats like a closure so the client is
// reset and can open a new request.
func (d *Desk) GetTicketByID(ctx context.Context, id string) (TicketStatus, bool, error) {
	var t ticketStatusRaw
	if err := d.client.do(ctx, "GET", "/api/v1/tickets/"+id, nil, &t); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return TicketStatus{}, false, nil
		}
		return TicketStatus{}, false, err
	}
	return TicketStatus{
		ID:           t.ID,
		Number:       t.TicketNumber,
		Status:       t.Status,
		StatusType:   t.StatusType,
		ModifiedTime: t.ModifiedTime,
	}, true, nil
}

// CommentEntry is a single ticket comment as seen by the poller.
type CommentEntry struct {
	ID            string
	Content       string
	CommentedTime string
	IsPublic      bool
}

type commentRaw struct {
	ID            string `json:"id"`
	Content       string `json:"content"`
	ContentType   string `json:"contentType"`
	IsPublic      bool   `json:"isPublic"`
	CommentedTime string `json:"commentedTime"`
}

type commentList struct {
	Data []commentRaw `json:"data"`
}

// ListComments returns the most recent comments on a ticket, newest first. The
// poller forwards only the public ones (operator replies) it has not seen yet;
// the bot's own client-relay comments are private and thus skipped.
func (d *Desk) ListComments(ctx context.Context, ticketID string, limit int) ([]CommentEntry, error) {
	path := fmt.Sprintf("/api/v1/tickets/%s/comments?sortBy=-commentedTime&limit=%d", ticketID, limit)
	var page commentList
	if err := d.client.do(ctx, "GET", path, nil, &page); err != nil {
		return nil, fmt.Errorf("listing comments for ticket %s: %w", ticketID, err)
	}
	out := make([]CommentEntry, 0, len(page.Data))
	for _, c := range page.Data {
		content := c.Content
		if strings.EqualFold(c.ContentType, "html") {
			content = stripHTML(content)
		}
		out = append(out, CommentEntry{
			ID:            c.ID,
			Content:       content,
			CommentedTime: c.CommentedTime,
			IsPublic:      c.IsPublic,
		})
	}
	return out, nil
}

var htmlTagRe = regexp.MustCompile(`(?s)<[^>]*>`)

// stripHTML reduces a Zoho rich-text comment to plain text suitable for
// Telegram: block tags become newlines, remaining tags are removed, entities
// are unescaped, and runs of blank lines are collapsed.
func stripHTML(s string) string {
	replacer := strings.NewReplacer(
		"<br>", "\n", "<br/>", "\n", "<br />", "\n",
		"</p>", "\n", "</div>", "\n", "</li>", "\n",
	)
	s = replacer.Replace(s)
	s = htmlTagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	lines := strings.Split(s, "\n")
	trimmed := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t\r")
		if strings.TrimSpace(ln) == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		trimmed = append(trimmed, ln)
	}
	return strings.TrimSpace(strings.Join(trimmed, "\n"))
}

// AddComment appends an internal (non-public) comment to a ticket. It is used to
// relay client messages to the operator. The comment is deliberately NOT public:
// a public comment would itself fire the "public comment" Zoho webhook and be
// echoed straight back to the client, creating a loop. The operator's own
// public replies are what reach the client.
func (d *Desk) AddComment(ctx context.Context, ticketID, content string) error {
	payload := map[string]any{
		"content":     content,
		"contentType": "plainText",
		"isPublic":    false,
	}
	path := "/api/v1/tickets/" + ticketID + "/comments"
	if err := d.client.do(ctx, "POST", path, payload, nil); err != nil {
		return fmt.Errorf("adding comment to ticket %s: %w", ticketID, err)
	}
	d.logger.Info("zoho ticket comment added", "ticket_id", ticketID)
	return nil
}

// AddAttachment uploads a file to an existing ticket as a non-public attachment.
func (d *Desk) AddAttachment(ctx context.Context, ticketID, filename string, data []byte) error {
	if filename == "" {
		filename = "attachment"
	}
	path := "/api/v1/tickets/" + ticketID + "/attachments"
	fields := map[string]string{"isPublic": "false"}
	if err := d.client.doMultipart(ctx, "POST", path, "file", filename, data, fields, nil); err != nil {
		return fmt.Errorf("adding attachment to ticket %s: %w", ticketID, err)
	}
	d.logger.Info("zoho ticket attachment added", "ticket_id", ticketID, "filename", filename, "bytes", len(data))
	return nil
}

type accountRaw struct {
	ID          string         `json:"id"`
	AccountName string         `json:"accountName"`
	CF          map[string]any `json:"cf"`
}

type accountsList struct {
	Data []accountRaw `json:"data"`
}

// AccountByBotClientID finds the account whose cf_bot_client_id matches.
func (d *Desk) AccountByBotClientID(ctx context.Context, botClientID uint32) (*AccountInfo, error) {
	accounts, err := d.listAccounts(ctx)
	if err != nil {
		return nil, err
	}
	d.logger.Debug("resolving account by bot client id",
		"want", botClientID, "cf_field", d.cfBotClientID, "accounts_fetched", len(accounts))
	for i := range accounts {
		full, err := d.accountByID(ctx, accounts[i].ID)
		if err != nil {
			d.logger.Warn("fetching account detail failed", "account_id", accounts[i].ID, "error", err)
			continue
		}
		got := cfUint32(full.CF, d.cfBotClientID)
		d.logger.Debug("account candidate",
			"account_id", full.ID,
			"name", full.AccountName,
			"cf_keys", cfKeys(full.CF),
			"parsed_bot_client_id", got)
		if got == botClientID {
			info := d.toAccountInfo(full)
			return &info, nil
		}
	}
	return nil, nil
}

// cfKeys returns the custom-field keys present on an account, for diagnostics:
// it reveals whether the API actually returns custom fields and under which
// exact API names (e.g. single vs double "cf_" prefix).
func cfKeys(cf map[string]any) []string {
	if len(cf) == 0 {
		return nil
	}
	keys := make([]string, 0, len(cf))
	for k := range cf {
		keys = append(keys, k)
	}
	return keys
}

// AccountByChatID finds the account whose cf_telegram_chat_id matches. Used to
// rebuild the authorization cache after a Redis cold start.
func (d *Desk) AccountByChatID(ctx context.Context, chatID int64) (*AccountInfo, error) {
	accounts, err := d.listAccounts(ctx)
	if err != nil {
		return nil, err
	}
	for i := range accounts {
		full, err := d.accountByID(ctx, accounts[i].ID)
		if err != nil {
			d.logger.Warn("fetching account detail failed", "account_id", accounts[i].ID, "error", err)
			continue
		}
		if cfInt64(full.CF, d.cfChatID) == chatID {
			info := d.toAccountInfo(full)
			return &info, nil
		}
	}
	return nil, nil
}

// accountByID fetches a single account including its custom fields. The account
// LIST endpoint omits the cf object, so the cf_* bindings can only be read via a
// per-account GET.
func (d *Desk) accountByID(ctx context.Context, id string) (accountRaw, error) {
	// Zoho returns the account object directly (not wrapped in a "data" array).
	var a accountRaw
	if err := d.client.do(ctx, "GET", "/api/v1/accounts/"+id, nil, &a); err != nil {
		return accountRaw{}, fmt.Errorf("fetching account %s: %w", id, err)
	}
	return a, nil
}

// AccountByID resolves a single account directly by its Zoho ID, in one API
// call. The registry uses it on a reverse-index hit to skip the full account
// scan. nil is returned (without error) when the account no longer exists (404),
// so a stale index entry can be dropped.
func (d *Desk) AccountByID(ctx context.Context, id string) (*AccountInfo, error) {
	var a accountRaw
	if err := d.client.do(ctx, "GET", "/api/v1/accounts/"+id, nil, &a); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("fetching account %s: %w", id, err)
	}
	info := d.toAccountInfo(a)
	return &info, nil
}

// SetAccountChatID writes the bound Telegram chat ID onto the account. Passing
// chatID 0 clears the field (used on revocation).
func (d *Desk) SetAccountChatID(ctx context.Context, accountID string, chatID int64) error {
	value := ""
	if chatID != 0 {
		value = strconv.FormatInt(chatID, 10)
	}
	payload := map[string]any{
		"cf": map[string]any{
			d.cfChatID: value,
		},
	}
	if err := d.client.do(ctx, "PATCH", "/api/v1/accounts/"+accountID, payload, nil); err != nil {
		return fmt.Errorf("updating account %s chat id: %w", accountID, err)
	}
	d.logger.Info("zoho account chat id updated", "account_id", accountID, "chat_id", chatID)
	return nil
}

// listAccounts pages through all Desk accounts. At the expected scale (tens of
// key clients) this is one page; pagination is handled for safety.
func (d *Desk) listAccounts(ctx context.Context) ([]accountRaw, error) {
	const pageSize = 100
	var all []accountRaw
	for from := 0; ; from += pageSize {
		path := fmt.Sprintf("/api/v1/accounts?from=%d&limit=%d", from, pageSize)
		var page accountsList
		if err := d.client.do(ctx, "GET", path, nil, &page); err != nil {
			return nil, fmt.Errorf("listing accounts: %w", err)
		}
		all = append(all, page.Data...)
		if len(page.Data) < pageSize {
			break
		}
	}
	return all, nil
}

func (d *Desk) toAccountInfo(a accountRaw) AccountInfo {
	return AccountInfo{
		ID:          a.ID,
		Name:        a.AccountName,
		BotClientID: cfUint32(a.CF, d.cfBotClientID),
		ChatID:      cfInt64(a.CF, d.cfChatID),
		Services:    cfStrings(a.CF, d.cfServices),
		Priority:    cfString(a.CF, d.cfPriority),
	}
}

func (d *Desk) contactEmail(chatID int64) string {
	return fmt.Sprintf("tg-%d@%s", chatID, d.contactDomain)
}

// zohoPriority maps the bot's two-level priority to Zoho Desk's vocabulary.
func zohoPriority(p string) string {
	if p == PriorityHigh {
		return "High"
	}
	return "Medium"
}

// ---- custom-field value coercion (Zoho returns strings, numbers, or lists) ----

func cfString(cf map[string]any, key string) string {
	if cf == nil {
		return ""
	}
	switch v := cf[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

func cfUint32(cf map[string]any, key string) uint32 {
	s := cfString(cf, key)
	if s == "" {
		return 0
	}
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}

func cfInt64(cf map[string]any, key string) int64 {
	s := cfString(cf, key)
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func cfStrings(cf map[string]any, key string) []string {
	if cf == nil {
		return nil
	}
	switch v := cf[key].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		var out []string
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		return out
	default:
		return nil
	}
}
