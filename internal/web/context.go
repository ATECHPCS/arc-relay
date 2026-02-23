package web

import (
	"context"

	"github.com/JeremiahChurch/mcp-wrangler/internal/store"
)

type contextKey string

const userKey contextKey = "user"

func setUser(ctx context.Context, user *store.User) context.Context {
	return context.WithValue(ctx, userKey, user)
}

func getUser(r interface{ Context() context.Context }) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}
