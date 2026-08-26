// Session cookies for local login (2026-08-25 — see store.SetLocalLogin's
// doc comment for the feature's full reasoning). A stateless, signed
// cookie rather than a server-side session table: this is a single-JSON-
// file, single-instance tool (store.go's own package comment), and a
// signed cookie needs nothing new persisted beyond the one secret
// SessionSecret already generates — no session table to grow, expire, or
// survive a restart.
//
// Format: base64(username|expiryUnix) + "." + base64(HMAC-SHA256 of that
// payload, keyed by the store's SessionSecret). Verifying re-derives the
// HMAC and compares with hmac.Equal (constant-time), then checks the
// expiry — a forged or expired cookie fails either way, indistinguishably
// from the caller's perspective (both just mean "not logged in").
package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

const sessionCookieName = "ch_session"

// sessionDuration: 30 days. This is a home-LAN tool protecting access to
// a settings UI, not a bank — forcing a re-login every day would be pure
// friction with no real security gained on a network already trusted
// enough to be self-hosting media extraction on in the first place.
const sessionDuration = 30 * 24 * time.Hour

func signSession(secret, username string, expiry time.Time) string {
	payload := username + "|" + strconv.FormatInt(expiry.Unix(), 10)
	sig := hmacSign(secret, payload)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// verifySession returns the username the cookie was signed for, and
// whether it's genuinely valid — a bad signature, malformed token, or
// expired timestamp all just report false, with no distinction the
// caller needs to make between them.
func verifySession(secret, token string) (username string, ok bool) {
	dot := strings.LastIndex(token, ".")
	if dot < 0 {
		return "", false
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(token[:dot])
	if err != nil {
		return "", false
	}
	sigRaw, err := base64.RawURLEncoding.DecodeString(token[dot+1:])
	if err != nil {
		return "", false
	}
	if !hmac.Equal(sigRaw, hmacSign(secret, string(payloadRaw))) {
		return "", false
	}
	pipe := strings.LastIndex(string(payloadRaw), "|")
	if pipe < 0 {
		return "", false
	}
	expiryUnix, err := strconv.ParseInt(string(payloadRaw[pipe+1:]), 10, 64)
	if err != nil || time.Now().Unix() > expiryUnix {
		return "", false
	}
	return string(payloadRaw[:pipe]), true
}

func hmacSign(secret, payload string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}
