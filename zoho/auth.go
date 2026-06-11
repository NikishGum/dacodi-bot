package zoho

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	store "supportbot/redis"
)

// tokenSafetyMargin shortens the in-memory validity window so a token is
// refreshed before Zoho actually expires it.
const tokenSafetyMargin = 100 * time.Second

// redisTokenTTL is slightly less than the real 3600s expiry, per spec.
const redisTokenTTL = 3500 * time.Second

// TokenManager exchanges a long-lived refresh token for short-lived access
// tokens, caching them in memory and in Redis (shared across restarts/instances)
// and refreshing transparently before expiry.
type TokenManager struct {
	accountsURL  string
	clientID     string
	clientSecret string
	refreshToken string

	store  *store.Client
	http   *http.Client
	logger *slog.Logger

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewTokenManager constructs a token manager. The accountsURL is the Zoho
// accounts base (for example https://accounts.zoho.eu).
func NewTokenManager(accountsURL, clientID, clientSecret, refreshToken string, st *store.Client, logger *slog.Logger) *TokenManager {
	return &TokenManager{
		accountsURL:  accountsURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		refreshToken: refreshToken,
		store:        st,
		http:         &http.Client{Timeout: 15 * time.Second},
		logger:       logger,
	}
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
	APIDomain   string `json:"api_domain"`
	Error       string `json:"error"`
}

// AccessToken returns a valid access token, refreshing it when the in-memory
// copy is stale and the Redis cache cannot satisfy the request.
func (t *TokenManager) AccessToken(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.token != "" && time.Now().Before(t.expiresAt) {
		return t.token, nil
	}

	// Another instance may have refreshed the token already.
	if cached, ttl, err := t.store.GetToken(ctx); err != nil {
		t.logger.Warn("reading cached zoho token failed", "error", err)
	} else if cached != "" && ttl > tokenSafetyMargin {
		t.token = cached
		t.expiresAt = time.Now().Add(ttl - tokenSafetyMargin)
		return t.token, nil
	}

	return t.refresh(ctx)
}

// Invalidate clears the cached token both in memory and in Redis, forcing the
// next AccessToken call to refresh. Used after a 401 from the Desk API.
func (t *TokenManager) Invalidate(ctx context.Context) {
	t.mu.Lock()
	t.token = ""
	t.expiresAt = time.Time{}
	t.mu.Unlock()
	if err := t.store.DelToken(ctx); err != nil {
		t.logger.Warn("evicting cached zoho token failed", "error", err)
	}
}

// refresh obtains a new access token. The caller must hold t.mu.
func (t *TokenManager) refresh(ctx context.Context) (string, error) {
	endpoint := t.accountsURL + "/oauth/v2/token"
	form := url.Values{}
	form.Set("refresh_token", t.refreshToken)
	form.Set("client_id", t.clientID)
	form.Set("client_secret", t.clientSecret)
	form.Set("grant_type", "refresh_token")

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(backoffDuration(attempt)):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return "", fmt.Errorf("building token request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := t.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("token request: %w", err)
			t.logger.Warn("zoho token refresh attempt failed", "attempt", attempt+1, "error", err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("token endpoint status %d: %s", resp.StatusCode, string(body))
			t.logger.Warn("zoho token refresh retryable status", "attempt", attempt+1, "status", resp.StatusCode)
			continue
		}

		var tr tokenResponse
		if err := json.Unmarshal(body, &tr); err != nil {
			return "", fmt.Errorf("decoding token response (status %d): %w", resp.StatusCode, err)
		}
		if tr.Error != "" || tr.AccessToken == "" {
			return "", fmt.Errorf("zoho token error (status %d): %s", resp.StatusCode, tr.Error)
		}

		expiresIn := tr.ExpiresIn
		if expiresIn <= 0 {
			expiresIn = 3600
		}
		t.token = tr.AccessToken
		t.expiresAt = time.Now().Add(time.Duration(expiresIn)*time.Second - tokenSafetyMargin)

		if err := t.store.SetToken(ctx, t.token, redisTokenTTL); err != nil {
			t.logger.Warn("caching zoho token failed", "error", err)
		}
		t.logger.Info("zoho access token refreshed", "expires_in", expiresIn)
		return t.token, nil
	}

	return "", fmt.Errorf("zoho token refresh exhausted retries: %w", lastErr)
}
