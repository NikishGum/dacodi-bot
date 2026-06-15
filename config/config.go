package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Client is a single key-client entry loaded from clients.json. The bot only
// interacts with chat IDs present in this registry.
type Client struct {
	ChatID   int64    `json:"chat_id"`
	Name     string   `json:"name"`
	Company  string   `json:"company"`
	Services []string `json:"services"`
	Priority string   `json:"priority"`
}

type clientsFile struct {
	Clients []Client `json:"clients"`
}

// Config holds all runtime configuration. Every value originates from an
// environment variable or clients.json; nothing is hardcoded.
type Config struct {
	LogLevel string

	ServerAddr    string
	PublicBaseURL string

	TelegramToken         string
	TelegramMode          string // "webhook" | "polling"
	TelegramWebhookSecret string

	RedisAddr     string
	RedisPassword string
	RedisDB       int

	ZohoAccountsURL   string
	ZohoDeskURL       string
	ZohoClientID      string
	ZohoClientSecret  string
	ZohoRefreshToken  string
	ZohoOrgID         string
	ZohoDeptID        string
	ZohoCFChatID      string
	ZohoCFBotClientID string
	ZohoCFServices    string
	ZohoCFPriority    string
	ZohoContactDomain string

	// ZohoCFRequestID is the API name of a ticket custom field that stores the
	// per-request idempotency key. When empty, idempotency tagging is disabled
	// (a startup warning is logged). See FIX-5.
	ZohoCFRequestID string

	// ZohoPollInterval is how often the poller pulls operator replies and
	// closures from Zoho (the bot does not use inbound Zoho webhooks).
	ZohoPollInterval time.Duration

	// ZohoScanMinInterval bounds how often a cold-cache miss may trigger a full
	// Zoho account scan (the N+1 list+GET path), process-wide. It protects the
	// API-credit budget from unbound strangers messaging the bot (FIX-1).
	ZohoScanMinInterval time.Duration

	BindingTokenSecret string
	BindingTokenTTL    time.Duration

	// AdminAPIKey guards the internal POST /admin/unbind endpoint. When empty the
	// endpoint is not mounted (unbinding is then only possible by editing the Zoho
	// account's cf_telegram_chat_id directly).
	AdminAPIKey string

	ClientsFile string

	// clients indexed by chat ID for O(1) authorization lookups. Read-only
	// after Load, therefore safe for concurrent reads.
	clients map[int64]Client
}

// Load reads the .env file (if present), validates required variables, applies
// defaults, and parses the client registry. It fails fast on any missing
// required value so misconfiguration is caught at startup rather than at
// request time.
func Load() (*Config, error) {
	// godotenv.Load is best-effort: in production variables come from the
	// systemd EnvironmentFile, so a missing .env is not an error.
	_ = godotenv.Load()

	c := &Config{
		LogLevel:              getEnv("LOG_LEVEL", "info"),
		ServerAddr:            getEnv("SERVER_ADDR", "127.0.0.1:8080"),
		PublicBaseURL:         strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/"),
		TelegramToken:         os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramMode:          strings.ToLower(getEnv("TELEGRAM_MODE", "webhook")),
		TelegramWebhookSecret: os.Getenv("TELEGRAM_WEBHOOK_SECRET"),
		RedisAddr:             getEnv("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword:         os.Getenv("REDIS_PASSWORD"),
		ZohoAccountsURL:       strings.TrimRight(getEnv("ZOHO_ACCOUNTS_URL", "https://accounts.zoho.eu"), "/"),
		ZohoDeskURL:           strings.TrimRight(getEnv("ZOHO_DESK_URL", "https://desk.zoho.eu"), "/"),
		ZohoClientID:          os.Getenv("ZOHO_CLIENT_ID"),
		ZohoClientSecret:      os.Getenv("ZOHO_CLIENT_SECRET"),
		ZohoRefreshToken:      os.Getenv("ZOHO_REFRESH_TOKEN"),
		ZohoOrgID:             os.Getenv("ZOHO_ORG_ID"),
		ZohoDeptID:            os.Getenv("ZOHO_DEPT_ID"),
		ZohoCFChatID:          getEnv("ZOHO_CF_CHAT_ID", "cf_cf_telegram_chat_id"),
		ZohoCFBotClientID:     getEnv("ZOHO_CF_BOT_CLIENT_ID", "cf_cf_bot_client_id"),
		ZohoCFServices:        getEnv("ZOHO_CF_SERVICES", "cf_cf_bot_services"),
		ZohoCFPriority:        getEnv("ZOHO_CF_PRIORITY", "cf_cf_bot_priority"),
		ZohoContactDomain:     getEnv("ZOHO_CONTACT_EMAIL_DOMAIN", "telegram.support.local"),
		ZohoCFRequestID:       strings.TrimSpace(os.Getenv("ZOHO_CF_REQUEST_ID")),
		// Trimmed: a systemd EnvironmentFile keeps trailing whitespace that would
		// desynchronize this secret from tokengen and break every signature.
		BindingTokenSecret: strings.TrimSpace(os.Getenv("BINDING_TOKEN_SECRET")),
		AdminAPIKey:        strings.TrimSpace(os.Getenv("ADMIN_API_KEY")),
		ClientsFile:        getEnv("CLIENTS_FILE", "config/clients.json"),
	}

	pollInterval, err := time.ParseDuration(getEnv("ZOHO_POLL_INTERVAL", "30s"))
	if err != nil {
		return nil, fmt.Errorf("ZOHO_POLL_INTERVAL must be a duration (e.g. 30s): %w", err)
	}
	if pollInterval < 5*time.Second {
		return nil, fmt.Errorf("ZOHO_POLL_INTERVAL must be at least 5s to protect Zoho API credits, got %s", pollInterval)
	}
	c.ZohoPollInterval = pollInterval

	scanInterval, err := time.ParseDuration(getEnv("ZOHO_SCAN_MIN_INTERVAL", "60s"))
	if err != nil {
		return nil, fmt.Errorf("ZOHO_SCAN_MIN_INTERVAL must be a duration (e.g. 60s): %w", err)
	}
	if scanInterval < 0 {
		return nil, fmt.Errorf("ZOHO_SCAN_MIN_INTERVAL must not be negative, got %s", scanInterval)
	}
	c.ZohoScanMinInterval = scanInterval

	ttl, err := time.ParseDuration(getEnv("BINDING_TOKEN_TTL", "30m"))
	if err != nil {
		return nil, fmt.Errorf("BINDING_TOKEN_TTL must be a duration (e.g. 30m): %w", err)
	}
	c.BindingTokenTTL = ttl

	db, err := strconv.Atoi(getEnv("REDIS_DB", "0"))
	if err != nil {
		return nil, fmt.Errorf("REDIS_DB must be an integer: %w", err)
	}
	c.RedisDB = db

	if c.TelegramMode != "webhook" && c.TelegramMode != "polling" {
		return nil, fmt.Errorf("TELEGRAM_MODE must be 'webhook' or 'polling', got %q", c.TelegramMode)
	}

	required := map[string]string{
		"TELEGRAM_BOT_TOKEN":   c.TelegramToken,
		"ZOHO_CLIENT_ID":       c.ZohoClientID,
		"ZOHO_CLIENT_SECRET":   c.ZohoClientSecret,
		"ZOHO_REFRESH_TOKEN":   c.ZohoRefreshToken,
		"ZOHO_ORG_ID":          c.ZohoOrgID,
		"ZOHO_DEPT_ID":         c.ZohoDeptID,
		"BINDING_TOKEN_SECRET": c.BindingTokenSecret,
	}
	if c.TelegramMode == "webhook" {
		required["TELEGRAM_WEBHOOK_SECRET"] = c.TelegramWebhookSecret
		required["PUBLIC_BASE_URL"] = c.PublicBaseURL
	}
	var missing []string
	for k, v := range required {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	if err := c.loadClients(); err != nil {
		return nil, err
	}

	return c, nil
}

// loadClients parses the optional bootstrap registry. The authoritative client
// registry is Zoho Desk; clients.json only pre-authorizes a few test/service
// chats, so a missing or empty file is not an error.
func (c *Config) loadClients() error {
	c.clients = make(map[int64]Client)

	data, err := os.ReadFile(c.ClientsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading clients file %q: %w", c.ClientsFile, err)
	}
	var parsed clientsFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("parsing clients file %q: %w", c.ClientsFile, err)
	}
	for _, cl := range parsed.Clients {
		if cl.ChatID == 0 {
			return fmt.Errorf("clients file contains an entry with chat_id 0 (company %q)", cl.Company)
		}
		c.clients[cl.ChatID] = cl
	}
	return nil
}

// ClientByChatID returns the bootstrap client for a chat ID, if present.
func (c *Config) ClientByChatID(chatID int64) (Client, bool) {
	cl, ok := c.clients[chatID]
	return cl, ok
}

// SlogLevel maps the configured log level string to an slog.Level.
func (c *Config) SlogLevel() slog.Level {
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}
