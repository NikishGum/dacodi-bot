// Command tokengen is a small internal service that issues binding tokens for
// the support bot. An operator (or a Zoho Desk button) calls it with a
// bot_client_id and receives a one-time t.me deep link to send to the client.
//
// It is stateless: it only needs the shared BINDING_TOKEN_SECRET and the bot
// username. Because it can mint a pass for any client, the endpoint is guarded
// by an API key and must only be reachable internally.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"supportbot/auth"
)

func main() {
	_ = godotenv.Load()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// Trim to stay byte-for-byte identical to the bot's secret regardless of how
	// the value is delivered (a systemd EnvironmentFile keeps trailing spaces).
	secret := strings.TrimSpace(os.Getenv("BINDING_TOKEN_SECRET"))
	botUsername := os.Getenv("BOT_USERNAME")
	apiKey := os.Getenv("TOKENGEN_API_KEY")
	addr := getEnv("TOKENGEN_ADDR", "127.0.0.1:8090")
	ttlStr := getEnv("BINDING_TOKEN_TTL", "30m")

	var missing []string
	if secret == "" {
		missing = append(missing, "BINDING_TOKEN_SECRET")
	}
	if botUsername == "" {
		missing = append(missing, "BOT_USERNAME")
	}
	if apiKey == "" {
		missing = append(missing, "TOKENGEN_API_KEY")
	}
	if len(missing) > 0 {
		logger.Error("missing required environment variables", "vars", strings.Join(missing, ", "))
		os.Exit(1)
	}

	ttl, err := time.ParseDuration(ttlStr)
	if err != nil {
		logger.Error("invalid BINDING_TOKEN_TTL", "value", ttlStr, "error", err)
		os.Exit(1)
	}

	manager := auth.NewManager(secret, ttl, botUsername)
	srv := &server{manager: manager, apiKey: apiKey, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("/token", srv.handleToken)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	logger.Info("tokengen listening", "addr", addr)
	if err := httpSrv.ListenAndServe(); err != nil {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
}

type server struct {
	manager *auth.Manager
	apiKey  string
	logger  *slog.Logger
}

type tokenRequest struct {
	BotClientID uint32 `json:"bot_client_id"`
}

type tokenResponse struct {
	Token     string `json:"token"`
	Link      string `json:"link"`
	ExpiresAt string `json:"expires_at"`
}

func (s *server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-API-Key")), []byte(s.apiKey)) != 1 {
		s.logger.Warn("tokengen rejected: bad api key", "remote", r.RemoteAddr)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req tokenRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.BotClientID == 0 {
		http.Error(w, "bot_client_id is required", http.StatusBadRequest)
		return
	}

	token, link, exp, err := s.manager.Mint(req.BotClientID)
	if err != nil {
		s.logger.Error("minting token failed", "bot_client_id", req.BotClientID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.logger.Info("token issued", "bot_client_id", req.BotClientID, "expires_at", exp.UTC().Format(time.RFC3339))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tokenResponse{
		Token:     token,
		Link:      link,
		ExpiresAt: exp.UTC().Format(time.RFC3339),
	})
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}
