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

// authedHandler is a handler that knows who's calling.
type authedHandler func(w http.ResponseWriter, r *http.Request, username string)

// requireAuth rejects requests without a valid Quine access token and passes
// the token's username to h. Usernames always come from the token, never
// from the request, so nobody can act as someone else.
func requireAuth(v *Verifier, h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "sign in required")
			return
		}
		username, err := v.Verify(r.Context(), token)
		if errors.Is(err, errIdentityUnavailable) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		h(w, r, username)
	}
}
