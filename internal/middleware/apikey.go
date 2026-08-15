package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/partner"
	"github.com/rs/zerolog/log"
)

// Partner API-key authentication — FR-API-001 | MDS §4.22.
//
// Deliberately a separate middleware from JWTMiddleware rather than an extra
// branch inside it. FR-API-001 requires partner credentials to be distinct
// from staff tokens, and the way to make that true rather than aspirational is
// for a partner key to have no path at all to a staff role: nothing here sets
// ctxKeyRole, so every RequireRole check downstream fails closed for a partner
// key no matter which route it reaches.

const (
	ctxKeyAPIKeyID    contextKey = "api_key_id"
	ctxKeyAPIScopes   contextKey = "api_scopes"
	ctxKeyPartnerName contextKey = "partner_name"
)

// APIKeyAuthenticator resolves a presented key. Satisfied by *db.PartnerStore.
type APIKeyAuthenticator interface {
	AuthenticateKey(ctx context.Context, presented string) (*partner.APIKey, error)
	TouchKeyUsage(ctx context.Context, keyID int)
}

// APIKeyMiddleware authenticates a partner key and injects its scopes.
func APIKeyMiddleware(auth APIKeyAuthenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			if !strings.HasPrefix(header, "Bearer ") {
				writeAPIKeyError(w, http.StatusUnauthorized, "missing or malformed Authorization header")
				return
			}
			presented := strings.TrimPrefix(header, "Bearer ")

			key, err := auth.AuthenticateKey(r.Context(), presented)
			if err != nil {
				log.Error().Err(err).Msg("partner: api key lookup failed")
				writeAPIKeyError(w, http.StatusInternalServerError, "authentication failed")
				return
			}
			// One message for every failure mode — unknown prefix, wrong
			// secret, revoked, expired. Telling an attacker which part of a
			// credential was wrong tells them what to fix.
			if key == nil {
				writeAPIKeyError(w, http.StatusUnauthorized, "invalid API key")
				return
			}

			// Best-effort bookkeeping; see PartnerStore.TouchKeyUsage. Never
			// worth failing a partner's request over.
			auth.TouchKeyUsage(r.Context(), key.ID)

			ctx := context.WithValue(r.Context(), ctxKeyAPIKeyID, key.ID)
			ctx = context.WithValue(ctx, ctxKeyAPIScopes, key.Scopes)
			ctx = context.WithValue(ctx, ctxKeyPartnerName, key.PartnerName)
			// The subject is set so the status-capture triggers (migration 031)
			// attribute a partner's write to that partner rather than to
			// "unknown". It is prefixed to keep it unmistakable in an audit
			// trail: "partner:AcmeCRM" is never confusable with a staff login.
			ctx = context.WithValue(ctx, ctxKeySubject, "partner:"+key.PartnerName)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireScope gates a route on a specific scope.
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scopes := APIScopesFromContext(r.Context())
			if !partner.HasScope(scopes, scope) {
				log.Warn().
					Str("partner", PartnerNameFromContext(r.Context())).
					Str("required_scope", scope).
					Msg("partner: request refused, key lacks the scope")
				writeAPIKeyError(w, http.StatusForbidden, "this API key does not carry the "+scope+" scope")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// APIKeyIDFromContext returns the authenticated key's id, or 0.
func APIKeyIDFromContext(ctx context.Context) int {
	id, _ := ctx.Value(ctxKeyAPIKeyID).(int)
	return id
}

// APIScopesFromContext returns the authenticated key's scopes.
func APIScopesFromContext(ctx context.Context) []string {
	scopes, _ := ctx.Value(ctxKeyAPIScopes).([]string)
	return scopes
}

// PartnerNameFromContext returns the authenticated partner's name.
func PartnerNameFromContext(ctx context.Context) string {
	name, _ := ctx.Value(ctxKeyPartnerName).(string)
	return name
}

// writeAPIKeyError emits the same envelope the rest of the API uses, so a
// partner's error handling does not need a second shape for auth failures.
func writeAPIKeyError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":{"code":"ERR_UNAUTHORIZED","message":"` + message + `"}}`)) //nolint:errcheck
}
