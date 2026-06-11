// Package auth implements binding tokens used to authenticate a Telegram chat
// against a key-client account. A token is a compact, HMAC-signed value that
// fits in Telegram's 64-char /start payload limit. It is minted by the token
// service and verified by the bot; both share BINDING_TOKEN_SECRET.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Token wire layout (binary, then base64url without padding):
//
//	bot_client_id : uint32 big-endian (4 bytes)
//	exp_unix      : uint32 big-endian (4 bytes)
//	jti           : random           (8 bytes)
//	mac           : HMAC-SHA256(secret, payload)[:16]
//
// payload = first 16 bytes, total = 32 bytes -> ~43 base64url chars (< 64).
const (
	payloadLen = 16
	macLen     = 16
	tokenLen   = payloadLen + macLen
)

var (
	// ErrInvalidToken means the token is malformed or its signature is wrong.
	ErrInvalidToken = errors.New("invalid binding token")
	// ErrExpired means the token signature is valid but it has expired.
	ErrExpired = errors.New("binding token expired")
)

// Manager mints and verifies binding tokens.
type Manager struct {
	secret      []byte
	ttl         time.Duration
	botUsername string
}

// NewManager constructs a token manager. botUsername is only needed for Mint
// (to build the t.me link); the bot may pass an empty string when it only
// verifies.
//
// The secret is trimmed of surrounding whitespace so the minting and verifying
// services agree even when the value reaches them differently — e.g. godotenv
// strips trailing spaces from a .env value but a systemd EnvironmentFile does
// not. A mismatch here silently breaks every signature.
func NewManager(secret string, ttl time.Duration, botUsername string) *Manager {
	return &Manager{secret: []byte(strings.TrimSpace(secret)), ttl: ttl, botUsername: botUsername}
}

// Mint creates a token for the given bot client ID and returns the token, the
// full deep link, and the expiry time.
func (m *Manager) Mint(botClientID uint32) (token, link string, exp time.Time, err error) {
	exp = time.Now().Add(m.ttl)

	buf := make([]byte, tokenLen)
	binary.BigEndian.PutUint32(buf[0:4], botClientID)
	binary.BigEndian.PutUint32(buf[4:8], uint32(exp.Unix()))
	if _, err = rand.Read(buf[8:16]); err != nil {
		return "", "", time.Time{}, fmt.Errorf("generating jti: %w", err)
	}
	mac := m.sign(buf[:payloadLen])
	copy(buf[payloadLen:], mac)

	token = base64.RawURLEncoding.EncodeToString(buf)
	link = fmt.Sprintf("https://t.me/%s?start=%s", m.botUsername, token)
	return token, link, exp, nil
}

// Verify checks the signature and expiry, returning the bot client ID and the
// token's unique jti (used by the caller for one-time-use enforcement).
func (m *Manager) Verify(token string) (botClientID uint32, jti string, err error) {
	// Defensive trim: a token pasted into Telegram may carry leading/trailing
	// whitespace or a stray newline that would corrupt the base64 decode.
	token = strings.TrimSpace(token)
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != tokenLen {
		return 0, "", ErrInvalidToken
	}

	payload := raw[:payloadLen]
	gotMAC := raw[payloadLen:]
	wantMAC := m.sign(payload)
	if subtle.ConstantTimeCompare(gotMAC, wantMAC) != 1 {
		return 0, "", ErrInvalidToken
	}

	botClientID = binary.BigEndian.Uint32(payload[0:4])
	exp := int64(binary.BigEndian.Uint32(payload[4:8]))
	if time.Now().Unix() >= exp {
		return 0, "", ErrExpired
	}
	jti = hex.EncodeToString(payload[8:16])
	return botClientID, jti, nil
}

func (m *Manager) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write(payload)
	return mac.Sum(nil)[:macLen]
}
