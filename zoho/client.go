package zoho

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"mime/multipart"
	"net/http"
	"strconv"
	"time"
)

// maxRetries is the maximum number of retries (in addition to the first
// attempt) for any Zoho API call, per the hard constraints.
const maxRetries = 3

// backoffDuration returns an exponential backoff with jitter for the given
// 1-based attempt number.
func backoffDuration(attempt int) time.Duration {
	base := 500 * time.Millisecond
	d := time.Duration(float64(base) * math.Pow(2, float64(attempt-1)))
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	jitter := time.Duration(rand.Int63n(int64(250 * time.Millisecond)))
	return d + jitter
}

// Client is the base authenticated HTTP client for the Zoho Desk API. It
// injects auth/org headers and retries transient failures with exponential
// backoff.
type Client struct {
	http    *http.Client
	baseURL string
	orgID   string
	tokens  *TokenManager
	logger  *slog.Logger
}

// NewClient builds a Desk HTTP client. baseURL is the Desk base (for example
// https://desk.zoho.eu).
func NewClient(baseURL, orgID string, tokens *TokenManager, logger *slog.Logger) *Client {
	return &Client{
		http:    &http.Client{Timeout: 20 * time.Second},
		baseURL: baseURL,
		orgID:   orgID,
		tokens:  tokens,
		logger:  logger,
	}
}

// apiError represents a non-success Desk response.
type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("zoho desk status %d: %s", e.Status, e.Body)
}

// do executes an authenticated request against the Desk API, decoding a JSON
// success body into out (which may be nil). It retries on network errors, 429
// and 5xx, and refreshes the token once on a 401.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshaling request body: %w", err)
		}
	}

	url := c.baseURL + path
	var lastErr error
	refreshedOn401 := false

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoffDuration(attempt)):
			}
		}

		token, err := c.tokens.AccessToken(ctx)
		if err != nil {
			lastErr = fmt.Errorf("acquiring access token: %w", err)
			c.logger.Warn("zoho token acquisition failed", "attempt", attempt+1, "error", err)
			continue
		}

		var reqBody io.Reader
		if payload != nil {
			reqBody = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
		if err != nil {
			return fmt.Errorf("building request: %w", err)
		}
		req.Header.Set("Authorization", "Zoho-oauthtoken "+token)
		req.Header.Set("orgId", c.orgID)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s %s: %w", method, path, err)
			c.logger.Warn("zoho request transport error", "method", method, "path", path, "attempt", attempt+1, "error", err)
			continue
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusUnauthorized && !refreshedOn401:
			refreshedOn401 = true
			c.logger.Warn("zoho returned 401, refreshing token", "method", method, "path", path)
			c.tokens.Invalidate(ctx)
			// Retry immediately without consuming an attempt's backoff.
			attempt--
			continue

		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = &apiError{Status: resp.StatusCode, Body: string(respBody)}
			if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(ra):
				}
			}
			c.logger.Warn("zoho retryable status", "method", method, "path", path, "status", resp.StatusCode, "attempt", attempt+1)
			continue

		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if out != nil && len(respBody) > 0 {
				if err := json.Unmarshal(respBody, out); err != nil {
					return fmt.Errorf("decoding response: %w", err)
				}
			}
			return nil

		default:
			return &apiError{Status: resp.StatusCode, Body: string(respBody)}
		}
	}

	return fmt.Errorf("%s %s exhausted retries: %w", method, path, lastErr)
}

// doMultipart uploads a single file as multipart/form-data, sharing do's auth,
// 401-refresh and transient-retry behaviour. The body is built once into a byte
// buffer so it can be safely replayed across retries.
func (c *Client) doMultipart(ctx context.Context, method, path, fileField, filename string, fileData []byte, fields map[string]string, out any) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return fmt.Errorf("writing multipart field %q: %w", k, err)
		}
	}
	fw, err := mw.CreateFormFile(fileField, filename)
	if err != nil {
		return fmt.Errorf("creating multipart file part: %w", err)
	}
	if _, err := fw.Write(fileData); err != nil {
		return fmt.Errorf("writing multipart file part: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("closing multipart writer: %w", err)
	}
	contentType := mw.FormDataContentType()
	payload := buf.Bytes()

	url := c.baseURL + path
	var lastErr error
	refreshedOn401 := false

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoffDuration(attempt)):
			}
		}

		token, err := c.tokens.AccessToken(ctx)
		if err != nil {
			lastErr = fmt.Errorf("acquiring access token: %w", err)
			c.logger.Warn("zoho token acquisition failed", "attempt", attempt+1, "error", err)
			continue
		}

		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("building request: %w", err)
		}
		req.Header.Set("Authorization", "Zoho-oauthtoken "+token)
		req.Header.Set("orgId", c.orgID)
		req.Header.Set("Content-Type", contentType)

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s %s: %w", method, path, err)
			c.logger.Warn("zoho request transport error", "method", method, "path", path, "attempt", attempt+1, "error", err)
			continue
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusUnauthorized && !refreshedOn401:
			refreshedOn401 = true
			c.logger.Warn("zoho returned 401, refreshing token", "method", method, "path", path)
			c.tokens.Invalidate(ctx)
			attempt--
			continue

		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = &apiError{Status: resp.StatusCode, Body: string(respBody)}
			if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(ra):
				}
			}
			c.logger.Warn("zoho retryable status", "method", method, "path", path, "status", resp.StatusCode, "attempt", attempt+1)
			continue

		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if out != nil && len(respBody) > 0 {
				if err := json.Unmarshal(respBody, out); err != nil {
					return fmt.Errorf("decoding response: %w", err)
				}
			}
			return nil

		default:
			return &apiError{Status: resp.StatusCode, Body: string(respBody)}
		}
	}

	return fmt.Errorf("%s %s exhausted retries: %w", method, path, lastErr)
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}
