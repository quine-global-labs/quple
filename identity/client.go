package main

import (
	"net/url"
	"time"

	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// client is a native (public) app: authorization code + PKCE, refresh
// tokens, JWT access tokens so resource servers can verify them locally.
type client struct {
	id           string
	redirectURIs []string
}

var _ op.Client = (*client)(nil)

func (c *client) GetID() string                       { return c.id }
func (c *client) RedirectURIs() []string              { return c.redirectURIs }
func (c *client) PostLogoutRedirectURIs() []string    { return nil }
func (c *client) ApplicationType() op.ApplicationType { return op.ApplicationTypeNative }
func (c *client) AuthMethod() oidc.AuthMethod         { return oidc.AuthMethodNone }
func (c *client) ResponseTypes() []oidc.ResponseType {
	return []oidc.ResponseType{oidc.ResponseTypeCode}
}
func (c *client) GrantTypes() []oidc.GrantType {
	return []oidc.GrantType{oidc.GrantTypeCode, oidc.GrantTypeRefreshToken}
}

// LoginURL is where the library sends the browser to sign in for auth
// request id; see login.go.
func (c *client) LoginURL(id string) string {
	return "/login?" + url.Values{"authRequestID": {id}}.Encode()
}

func (c *client) AccessTokenType() op.AccessTokenType { return op.AccessTokenTypeJWT }
func (c *client) IDTokenLifetime() time.Duration      { return accessTokenTTL }
func (c *client) DevMode() bool                       { return false }
func (c *client) RestrictAdditionalIdTokenScopes() func([]string) []string {
	return func(scopes []string) []string { return scopes }
}
func (c *client) RestrictAdditionalAccessTokenScopes() func([]string) []string {
	return func(scopes []string) []string { return scopes }
}

// IsScopeAllowed only governs non-standard scopes; there are none.
func (c *client) IsScopeAllowed(string) bool { return false }

// IDTokenUserinfoClaimsAssertion puts userinfo claims (preferred_username)
// straight into the ID token, saving the app a userinfo round trip.
func (c *client) IDTokenUserinfoClaimsAssertion() bool { return true }
func (c *client) ClockSkew() time.Duration             { return 0 }
