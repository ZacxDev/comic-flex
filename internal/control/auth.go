package control

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// TokenEnvVar names the environment variable the systemd unit must carry.
const TokenEnvVar = "COMIC_FLEX_CONTROL_TOKEN"

// MinTokenBytes is the shortest control token that will be accepted.
const MinTokenBytes = 32

// ErrTokenMissing and ErrTokenTooShort are the two fail-closed refusals.
//
// 🔴 Fail-closed, deliberately unlike clawgate's CLAWGATE_HOOK_TOKEN
// "enforce-when-set" behaviour. The Pi has passwordless sudo, so an
// empty-token-means-open control port on it is not a small thing. clawgate's own
// CLAWGATE_TERMINAL_TOKEN is fail-closed for the same reason; this follows that
// one. New() returns one of these and the server never binds.
var (
	ErrTokenMissing  = errors.New("control: " + TokenEnvVar + " is unset")
	ErrTokenTooShort = errors.New("control: " + TokenEnvVar + " is too short")
)

// validateToken enforces the fail-closed precondition. It runs in New(), before
// anything listens, so a bad token means no control surface exists at all
// rather than an open one.
func validateToken(token string) error {
	if token == "" {
		return ErrTokenMissing
	}
	if len(token) < MinTokenBytes {
		return fmt.Errorf("%w: got %d bytes, need at least %d", ErrTokenTooShort, len(token), MinTokenBytes)
	}
	return nil
}

// bearerPrefix is the only accepted authorization scheme.
const bearerPrefix = "Bearer "

// bearerToken extracts the credential from an Authorization header value. ok is
// false when the header is absent or does not carry the Bearer scheme — a bare
// token with no scheme is rejected rather than accepted leniently.
func bearerToken(header string) (token string, ok bool) {
	if len(header) < len(bearerPrefix) {
		return "", false
	}
	if !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	return header[len(bearerPrefix):], true
}

// tokenMatches compares the presented credential against the configured one.
//
// 🔴 subtle.ConstantTimeCompare, not ==. A byte-by-byte string comparison
// short-circuits on the first differing byte, which leaks a prefix oracle to
// anyone who can time the endpoint. That property is not observable from a
// behavioural test — a == mutant passes every status-code assertion — so it is
// pinned structurally instead, by TestTokenComparisonIsConstantTime, which
// parses this function and fails if the ConstantTimeCompare call is not here or
// if the two operands are compared directly.
func tokenMatches(expected, presented string) bool {
	return subtle.ConstantTimeCompare([]byte(expected), []byte(presented)) == 1
}

// requireToken is the authentication middleware. Every route except
// GET /healthz is wrapped in it.
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || !tokenMatches(s.token, presented) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
