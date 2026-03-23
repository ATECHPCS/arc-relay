package server

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/JeremiahChurch/mcp-wrangler/internal/store"
)

type contextKey string

const userContextKey contextKey = "user"

// UserFromContext retrieves the authenticated user from the request context.
func UserFromContext(ctx context.Context) *store.User {
	u, _ := ctx.Value(userContextKey).(*store.User)
	return u
}

// APIKeyAuth middleware validates Bearer token API keys on MCP proxy endpoints.
func APIKeyAuth(users *store.UserStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if auth == "" {
				http.Error(w, `{"error":"missing Authorization header"}`, http.StatusUnauthorized)
				return
			}

			if !strings.HasPrefix(auth, "Bearer ") {
				http.Error(w, `{"error":"invalid Authorization header, expected Bearer token"}`, http.StatusUnauthorized)
				return
			}

			token := strings.TrimPrefix(auth, "Bearer ")
			user, err := users.ValidateAPIKey(token)
			if err != nil {
				log.Printf("auth: validate api key failed: path=%s remote=%s err=%v", r.URL.Path, r.RemoteAddr, err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			if user == nil {
				http.Error(w, `{"error":"invalid or revoked API key"}`, http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), userContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// AdminOnly middleware ensures the user has admin role.
func AdminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil || user.Role != "admin" {
			http.Error(w, `{"error":"admin access required"}`, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireWriteAccess checks that the authenticated user has write or admin access level.
// Returns true if access is granted, false if a 403 was sent.
func requireWriteAccess(w http.ResponseWriter, r *http.Request) bool {
	user := UserFromContext(r.Context())
	if user == nil {
		http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
		return false
	}
	if user.AccessLevel != "write" && user.AccessLevel != "admin" {
		http.Error(w, `{"error":"write access required"}`, http.StatusForbidden)
		return false
	}
	return true
}

// requireAdminAccess checks that the authenticated user has admin access level.
// Returns true if access is granted, false if a 403 was sent.
func requireAdminAccess(w http.ResponseWriter, r *http.Request) bool {
	user := UserFromContext(r.Context())
	if user == nil {
		http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
		return false
	}
	if user.AccessLevel != "admin" {
		http.Error(w, `{"error":"admin access required"}`, http.StatusForbidden)
		return false
	}
	return true
}
