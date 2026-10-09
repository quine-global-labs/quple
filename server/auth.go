package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Verifier checks Quine access tokens (JWTs from identity.quineglobal.com)
// locally with go-oidc. The issuer's discovery document and keys are fetched
// on first use rather than at startup, so this server can come up even while
// the identity server is down; go-oidc re-fetches keys when they rotate.
type Verifier struct {
	issuer   string
	clientID string

	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
	lastTry  time.Time
}

// discoveryRetryInterval bounds how often a failed discovery is retried, so
// a down identity server isn't hammered by every poll.
const discoveryRetryInterval = 10 * time.Second

var errIdentityUnavailable = errors.New("identity server unavailable")

func NewVerifier(issuer, clientID string) *Verifier {
	return &Verifier{issuer: strings.TrimRight(issuer, "/"), clientID: clientID}
}

func (v *Verifier) get(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.verifier != nil {
		return v.verifier, nil
	}
	if time.Since(v.lastTry) < discoveryRetryInterval {
		return nil, errIdentityUnavailable
	}
	v.lastTry = time.Now()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, http.DefaultClient), v.issuer)
	if err != nil {
		return nil, errIdentityUnavailable
	}
	// Access tokens are issued with the client ID as their audience, which
	// go-oidc checks along with issuer, expiry and signature.
	v.verifier = provider.Verifier(&oidc.Config{ClientID: v.clientID})
	return v.verifier, nil
}

// Verify checks an access token and returns the username it was issued to.
func (v *Verifier) Verify(ctx context.Context, token string) (string, error) {
	verifier, err := v.get(ctx)
	if err != nil {
		return "", err
	}
	parsed, err := verifier.Verify(ctx, token)
	if err != nil {
		return "", err
	}
	var claims struct {
		ClientID string `json:"client_id"`
		JTI      string `json:"jti"`
		AtHash   string `json:"at_hash"`
		Nonce    string `json:"nonce"`
	}
	if err := parsed.Claims(&claims); err != nil {
		return "", err
	}
	// The identity server (zitadel/oidc) signs ID and access tokens alike -
	// same audience, same typ header - so tell them apart by shape: access
	// tokens have a jti and never the ID-token-only at_hash/nonce. This keeps
	// an ID token from being accepted as an access token.
	if claims.ClientID != v.clientID || claims.JTI == "" || claims.AtHash != "" || claims.Nonce != "" {
		return "", errors.New("not an access token for this app")
	}
	// The subject is the Quine username.
	return parsed.Subject, nil
}
