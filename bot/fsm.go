package bot

import (
	"context"
	"encoding/json"
	"html"
	"strings"

	"supportbot/zoho"
)

// State enumerates the conversation stages.
type State string

const (
	StateIdle            State = ""
	StateAwaitingService State = "awaiting_service"
	StateAskingQuestions State = "asking_questions"
	StateConfirming      State = "confirming"
	StateTicketOpen      State = "ticket_open"
)

// Answer is a single collected question/value pair, kept ordered for the
// operator-facing description.
type Answer struct {
	Key    string `json:"key"`
	Prompt string `json:"prompt"`
	Value  string `json:"value"`
}

// Conversation is the persisted per-chat state machine.
type Conversation struct {
	ChatID        int64             `json:"chat_id"`
	State         State             `json:"state"`
	Service       string            `json:"service"`
	Group         string            `json:"group"`
	Category      string            `json:"category"`
	QuestionIndex int               `json:"question_index"`
	Answers       []Answer          `json:"answers"`
	Attachments   []zoho.Attachment `json:"attachments,omitempty"`
}

// Question describes one prompt. When Options is non-empty it is presented as
// an inline keyboard; otherwise free text (or media) is accepted.
type Question struct {
	Key     string
	Prompt  string
	Options []string
}

// questionSets maps a service group to its ordered question script.
var questionSets = map[string][]Question{
	"cdn": {
		{Key: "description", Prompt: "Опишите, пожалуйста, проблему."},
		{Key: "domain_ip", Prompt: "Какой домен или IP затронут?"},
		{Key: "time_started", Prompt: "Когда началась проблема? Укажите, пожалуйста, время по UTC."},
		{Key: "origin_accessible", Prompt: "Доступен ли origin напрямую?", Options: []string{"Да", "Нет", "Не знаю"}},
		{Key: "proof", Prompt: "Приложите, пожалуйста, скриншоты или логи (отправьте сейчас или напишите «нет»)."},
		{Key: "regions", Prompt: "Какие регионы затронуты, если известно? (напишите «н/д», если неизвестно)"},
	},
	"security": {
		{Key: "description", Prompt: "Опишите, пожалуйста, проблему."},
		{Key: "domain_ip", Prompt: "Какой домен или IP затронут?"},
		{Key: "time_started", Prompt: "Когда началась проблема? Укажите, пожалуйста, время по UTC."},
		{Key: "origin_accessible", Prompt: "Доступен ли origin напрямую?", Options: []string{"Да", "Нет", "Не знаю"}},
		{Key: "proof", Prompt: "Приложите, пожалуйста, скриншоты или логи (отправьте сейчас или напишите «нет»)."},
		{Key: "regions", Prompt: "Какие регионы затронуты, если известно? (напишите «н/д», если неизвестно)"},
	},
	"ai": {
		{Key: "description", Prompt: "Опишите, пожалуйста, проблему."},
		{Key: "endpoint", Prompt: "Какой API-эндпоинт затронут?"},
		{Key: "classification", Prompt: "Это ложное срабатывание или пропущенная атака?", Options: []string{"Ложное срабатывание", "Пропущенная атака", "Затрудняюсь ответить"}},
		{Key: "terraform_state", Prompt: "Есть ли релевантный Terraform state? (напишите «н/д», если нет)"},
		{Key: "time_started", Prompt: "Когда началась проблема? Укажите, пожалуйста, время по UTC."},
		{Key: "logs", Prompt: "Пришлите, пожалуйста, логи или текст ошибки (или напишите «нет»)."},
	},
	"server": {
		{Key: "description", Prompt: "Опишите, пожалуйста, проблему."},
		{Key: "server", Prompt: "Укажите IP или имя хоста сервера."},
		{Key: "severity", Prompt: "Сервер полностью недоступен или работает с деградацией?", Options: []string{"Полностью недоступен", "Деградация производительности"}},
		{Key: "last_working", Prompt: "Когда сервер в последний раз точно работал? (UTC)"},
		{Key: "ipmi", Prompt: "Нужен ли доступ IPMI/KVM?", Options: []string{"Да", "Нет"}},
		{Key: "time_started", Prompt: "Когда началась проблема? Укажите, пожалуйста, время по UTC."},
	},
	"dns": {
		{Key: "description", Prompt: "Опишите, пожалуйста, проблему."},
		{Key: "domain", Prompt: "Какой домен затронут?"},
		{Key: "resolver", Prompt: "Какой DNS-резолвер вы используете?"},
		{Key: "time_started", Prompt: "Когда началась проблема? Укажите, пожалуйста, время по UTC."},
	},
	"billing": {
		{Key: "contract", Prompt: "Укажите номер договора или идентификатор аккаунта."},
		{Key: "nature", Prompt: "В чём заключается ваш вопрос по биллингу или аккаунту?"},
	},
	"generic": {
		{Key: "description", Prompt: "Опишите, пожалуйста, проблему."},
		{Key: "component", Prompt: "Какой эндпоинт, ресурс или компонент затронут?"},
		{Key: "time_started", Prompt: "Когда началась проблема? Укажите, пожалуйста, время по UTC."},
		{Key: "logs", Prompt: "Пришлите, пожалуйста, логи или текст ошибки (или напишите «нет»)."},
	},
}

// serviceGroups maps the exact service labels (as configured per client and in
// the service keyboard) to a question-set group.
var serviceGroups = map[string]string{
	"CDN":                            "cdn",
	"SecurityCDN":                    "security",
	"AI Full-Stack Env Protection":   "ai",
	"Dedicated Servers":              "server",
	"VPS":                            "server",
	"Protected DNS":                  "dns",
	"API services":                   "generic",
	"Terraform provider integration": "generic",
	"Billing":                        "billing",
}

// categoryByGroup maps a question group to the Zoho Desk category.
var categoryByGroup = map[string]string{
	"cdn":      "CDN",
	"security": "Security",
	"ai":       "AI Protection",
	"server":   "Infrastructure",
	"dns":      "DNS",
	"billing":  "Billing",
	"generic":  "Infrastructure",
}

func groupForService(service string) string {
	if g, ok := serviceGroups[service]; ok {
		return g
	}
	return "generic"
}

func categoryForGroup(group string) string {
	if c, ok := categoryByGroup[group]; ok {
		return c
	}
	return "Infrastructure"
}

func questionsForGroup(group string) []Question {
	if qs, ok := questionSets[group]; ok {
		return qs
	}
	return questionSets["generic"]
}

// currentQuestion returns the question the conversation is waiting on, and
// whether one exists.
func (c *Conversation) currentQuestion() (Question, bool) {
	qs := questionsForGroup(c.Group)
	if c.QuestionIndex < 0 || c.QuestionIndex >= len(qs) {
		return Question{}, false
	}
	return qs[c.QuestionIndex], true
}

// recordAnswer appends the value for the current question and advances the
// cursor. It returns true when there are more questions remaining.
func (c *Conversation) recordAnswer(value string) bool {
	q, ok := c.currentQuestion()
	if !ok {
		return false
	}
	c.Answers = append(c.Answers, Answer{Key: q.Key, Prompt: q.Prompt, Value: value})
	c.QuestionIndex++
	_, more := c.currentQuestion()
	return more
}

// answerMap returns answers keyed for priority/subject heuristics.
func (c *Conversation) answerMap() map[string]string {
	m := make(map[string]string, len(c.Answers))
	for _, a := range c.Answers {
		m[a.Key] = a.Value
	}
	return m
}

// detectPriority implements the HIGH/NORMAL classification from the spec:
// full server outage, active unfiltered DDoS, or complete CDN failure are HIGH.
func (c *Conversation) detectPriority() string {
	m := c.answerMap()
	if c.Group == "server" && strings.EqualFold(strings.TrimSpace(m["severity"]), "Полностью недоступен") {
		return zoho.PriorityHigh
	}

	var b strings.Builder
	for _, a := range c.Answers {
		b.WriteString(strings.ToLower(a.Value))
		b.WriteByte('\n')
	}
	text := b.String()

	switch c.Group {
	case "cdn", "security":
		highSignals := []string{"полност", "полный отказ", "не работает", "лежит", "недоступ", "простой", "отключ", "ddos", "ддос", "не фильтр", "атака", "под атакой"}
		if containsAny(text, highSignals) {
			return zoho.PriorityHigh
		}
	}
	return zoho.PriorityNormal
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// subject builds the ticket subject: "[Service] short summary".
func (c *Conversation) subject() string {
	m := c.answerMap()
	summary := firstNonEmpty(m["description"], m["nature"], m["component"])
	summary = strings.TrimSpace(summary)
	if summary == "" {
		summary = "Support request"
	}
	// Truncate by runes, not bytes: a byte slice of Cyrillic UTF-8 text would
	// split a multi-byte character and corrupt the subject.
	if r := []rune(summary); len(r) > 60 {
		summary = strings.TrimSpace(string(r[:60])) + "…"
	}
	return "[" + c.Service + "] " + summary
}

// description builds the full structured body the operator reads.
//
// Client-controlled values (the company name and every free-text answer) are
// HTML-escaped defensively (FIX-13): if Zoho Desk renders the description as
// HTML, a client could otherwise inject markup/scripts into the operator's view.
// This is pending a manual verification of how Zoho actually renders the field;
// escaping is harmless if it turns out to be plain text (operators would at most
// see a literal entity in the rare case of a raw '<' in their text). The static
// labels, service, category and numeric chat id are trusted and left as-is.
func (c *Conversation) description(company, priority string) string {
	var b strings.Builder
	b.WriteString("ID чата Telegram: ")
	b.WriteString(itoa(c.ChatID))
	b.WriteByte('\n')
	if company != "" {
		b.WriteString("Клиент: ")
		b.WriteString(html.EscapeString(company))
		b.WriteByte('\n')
	}
	b.WriteString("Сервис: ")
	b.WriteString(c.Service)
	b.WriteByte('\n')
	b.WriteString("Категория: ")
	b.WriteString(c.Category)
	b.WriteByte('\n')
	b.WriteString("Приоритет: ")
	b.WriteString(priority)
	b.WriteString("\n\nСобранная информация:\n")
	for _, a := range c.Answers {
		b.WriteString("- ")
		b.WriteString(a.Prompt)
		b.WriteString("\n  ")
		b.WriteString(html.EscapeString(a.Value))
		b.WriteByte('\n')
	}
	return b.String()
}

// confirmationSummary is the client-facing review shown before ticket creation.
func (c *Conversation) confirmationSummary() string {
	var b strings.Builder
	b.WriteString("Пожалуйста, подтвердите данные:\n\n")
	b.WriteString("Сервис: ")
	b.WriteString(c.Service)
	b.WriteByte('\n')
	for _, a := range c.Answers {
		b.WriteString("• ")
		b.WriteString(a.Prompt)
		b.WriteString("\n  ")
		b.WriteString(a.Value)
		b.WriteByte('\n')
	}
	return b.String()
}

// ---- persistence helpers ----

func (b *Bot) loadConversation(ctx context.Context, chatID int64) (*Conversation, error) {
	data, err := b.store.GetFSM(ctx, chatID)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return &Conversation{ChatID: chatID, State: StateIdle}, nil
	}
	var conv Conversation
	if err := json.Unmarshal(data, &conv); err != nil {
		// Corrupt state is not fatal: start fresh rather than wedge the chat.
		b.logger.Warn("corrupt conversation state, resetting", "chat_id", chatID, "error", err)
		return &Conversation{ChatID: chatID, State: StateIdle}, nil
	}
	conv.ChatID = chatID
	return &conv, nil
}

func (b *Bot) saveConversation(ctx context.Context, conv *Conversation) error {
	data, err := json.Marshal(conv)
	if err != nil {
		return err
	}
	return b.store.SetFSM(ctx, conv.ChatID, data)
}
