package neutronauth

import "context"

type ctxKey int

const (
	ctxKeyClaims ctxKey = iota
	ctxKeySession
	ctxKeyOAuthUser
)

func withClaims(ctx context.Context, claims Claims) context.Context {
	return context.WithValue(ctx, ctxKeyClaims, claims)
}

// WithClaims stores JWT claims in the request context.
// Use this in custom middlewares that validate tokens outside of the standard
// neutronauth.JWTMiddleware, so ClaimsFromContext works downstream.
func WithClaims(ctx context.Context, claims Claims) context.Context {
	return withClaims(ctx, claims)
}

// WithOAuthUser stores an OAuthUser in the request context.
func WithOAuthUser(ctx context.Context, user *OAuthUser) context.Context {
	return context.WithValue(ctx, ctxKeyOAuthUser, user)
}

// OAuthUserFromContext extracts the OAuthUser from the request context.
func OAuthUserFromContext(ctx context.Context) *OAuthUser {
	u, _ := ctx.Value(ctxKeyOAuthUser).(*OAuthUser)
	return u
}
