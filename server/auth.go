package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Verifier checks Quine access tokens (ES256 JWTs from identity.quineglobal.com)
// locally, using the issuer's published JWKS. Keys are fetched on first use
// and re-fetched when a token names a key we haven't seen, so key rotation
// on the identity side needs no restart here.
type Verifier struct {
	issuer   string
	audience string
	client   *http.Client

	mu        sync.Mutex
	keys      map[string]*ecdsa.PublicKey
	lastFetch time.Time
}

// jwksRefetchInterval bounds how often an unknown key ID can trigger a
// fetch, so garbage tokens can't be used to hammer the identity server.
const jwksRefetchInterval = time.Minute

func NewVerifier(issuer, audience string) *Verifier {
	return &Verifier{
		issuer:   strings.TrimRight(issuer, "/"),
		audience: audience,
		client:   &http.Client{Timeout: 10 * time.Second},
		keys:     map[string]*ecdsa.PublicKey{},
	}
}

var b64 = base64.RawURLEncoding

// Verify checks token and returns the username it was issued to.
func (v *Verifier) Verify(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed token")
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return "", err
	}
	// at+jwt (RFC 9068) marks an access token, so an ID or refresh token
	// can't be passed off as one.
	if header.Alg != "ES256" || header.Typ != "at+jwt" {
		return "", errors.New("not an access token")
	}
	key, err := v.key(header.Kid)
	if err != nil {
		return "", err
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return "", errors.New("malformed signature")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return "", errors.New("bad signature")
	}

	var claims struct {
		Iss      string `json:"iss"`
		Aud      string `json:"aud"`
		Exp      int64  `json:"exp"`
		Username string `json:"preferred_username"`
	}
	if err := decodeSegment(parts[1], &claims); err != nil {
		return "", err
	}
	switch {
	case claims.Iss != v.issuer:
		return "", errors.New("wrong issuer")
	case claims.Aud != v.audience:
		return "", errors.New("wrong audience")
	case time.Now().Unix() >= claims.Exp:
		return "", errors.New("token expired")
	case claims.Username == "":
		return "", errors.New("token has no username")
	}
	return claims.Username, nil
}

func (v *Verifier) key(kid string) (*ecdsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	if time.Since(v.lastFetch) < jwksRefetchInterval {
		return nil, errors.New("unknown signing key")
	}
	v.lastFetch = time.Now()
	keys, err := v.fetchKeys()
	if err != nil {
		return nil, fmt.Errorf("fetching issuer keys: %w", err)
	}
	v.keys = keys
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("unknown signing key")
}

// fetchKeys finds the JWKS via OIDC discovery and parses its P-256 keys.
func (v *Verifier) fetchKeys() (map[string]*ecdsa.PublicKey, error) {
	var discovery struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := v.getJSON(v.issuer+"/.well-known/openid-configuration", &discovery); err != nil {
		return nil, err
	}
	if discovery.Issuer != v.issuer || discovery.JWKSURI == "" {
		return nil, errors.New("discovery document does not match issuer")
	}
	var jwks struct {
		Keys []struct {
			Kty, Crv, X, Y, Kid string
		} `json:"keys"`
	}
	if err := v.getJSON(discovery.JWKSURI, &jwks); err != nil {
		return nil, err
	}
	keys := map[string]*ecdsa.PublicKey{}
	for _, k := range jwks.Keys {
		if k.Kty != "EC" || k.Crv != "P-256" {
			continue
		}
		x, errX := b64.DecodeString(k.X)
		y, errY := b64.DecodeString(k.Y)
		if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
			continue
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	return keys, nil
}

func (v *Verifier) getJSON(url string, out any) error {
	resp, err := v.client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func decodeSegment(seg string, v any) error {
	raw, err := b64.DecodeString(seg)
	if err != nil {
		return errors.New("malformed token")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return errors.New("malformed token")
	}
	return nil
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
		username, err := v.Verify(token)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		h(w, r, username)
	}
}
