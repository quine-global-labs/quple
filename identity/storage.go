package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

const (
	accessTokenTTL  = time.Hour
	refreshTokenTTL = 30 * 24 * time.Hour // sliding: renewed on every use
	authRequestTTL  = 15 * time.Minute    // time allowed to finish signing in
)

// clients is the fixed registry of apps that can sign people in. Quple is a
// native app: no client secret, PKCE required, and its redirect is a loopback
// address on whatever port the app picked (RFC 8252 section 7.3; the library
// ignores the port when matching loopback redirects for native clients).
var clients = map[string]*client{
	"quple": {id: "quple", redirectURIs: []string{"http://127.0.0.1/callback"}},
}

// storage implements op.Storage. Everything the protocol needs is in memory
// except refresh tokens, which are persisted (hashed) so a restart doesn't
// sign everybody out. Access tokens are self-contained JWTs and aren't stored.
type storage struct {
	accounts   *Accounts
	key        *signingKey
	tokensPath string

	mu            sync.Mutex
	authRequests  map[string]*authRequest
	codes         map[string]string // code -> auth request ID
	refreshTokens map[string]*refreshToken
}

var _ op.Storage = (*storage)(nil)

func newStorage(accounts *Accounts, key *signingKey, tokensPath string) (*storage, error) {
	s := &storage{
		accounts:      accounts,
		key:           key,
		tokensPath:    tokensPath,
		authRequests:  map[string]*authRequest{},
		codes:         map[string]string{},
		refreshTokens: map[string]*refreshToken{},
	}
	raw, err := os.ReadFile(tokensPath)
	if err == nil {
		if err := json.Unmarshal(raw, &s.refreshTokens); err != nil {
			return nil, fmt.Errorf("%s: %w", tokensPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return s, nil
}

// ---- Auth requests --------------------------------------------------------

// authRequest is one in-progress sign-in, from /authorize until the code is
// exchanged.
type authRequest struct {
	id            string
	created       time.Time
	clientID      string
	redirectURI   string
	state         string
	nonce         string
	scopes        []string
	responseType  oidc.ResponseType
	responseMode  oidc.ResponseMode
	codeChallenge *oidc.CodeChallenge

	// Set once the user has signed in.
	username string
	authTime time.Time
	amr      []string
	done     bool
}

func (a *authRequest) GetID() string                         { return a.id }
func (a *authRequest) GetACR() string                        { return "" }
func (a *authRequest) GetAMR() []string                      { return a.amr }
func (a *authRequest) GetAudience() []string                 { return []string{a.clientID} }
func (a *authRequest) GetAuthTime() time.Time                { return a.authTime }
func (a *authRequest) GetClientID() string                   { return a.clientID }
func (a *authRequest) GetCodeChallenge() *oidc.CodeChallenge { return a.codeChallenge }
func (a *authRequest) GetNonce() string                      { return a.nonce }
func (a *authRequest) GetRedirectURI() string                { return a.redirectURI }
func (a *authRequest) GetResponseType() oidc.ResponseType    { return a.responseType }
func (a *authRequest) GetResponseMode() oidc.ResponseMode    { return a.responseMode }
func (a *authRequest) GetScopes() []string                   { return a.scopes }
func (a *authRequest) GetState() string                      { return a.state }
func (a *authRequest) GetSubject() string                    { return a.username }
func (a *authRequest) Done() bool                            { return a.done }

func (s *storage) CreateAuthRequest(_ context.Context, req *oidc.AuthRequest, _ string) (op.AuthRequest, error) {
	// There are no login sessions here, so a silent (prompt=none) sign-in can
	// never succeed.
	if slices.Contains(req.Prompt, oidc.PromptNone) {
		return nil, oidc.ErrLoginRequired()
	}
	var challenge *oidc.CodeChallenge
	if req.CodeChallenge != "" {
		challenge = &oidc.CodeChallenge{Challenge: req.CodeChallenge, Method: req.CodeChallengeMethod}
	}
	ar := &authRequest{
		id:            randomID(),
		created:       time.Now(),
		clientID:      req.ClientID,
		redirectURI:   req.RedirectURI,
		state:         req.State,
		nonce:         req.Nonce,
		scopes:        req.Scopes,
		responseType:  req.ResponseType,
		responseMode:  req.ResponseMode,
		codeChallenge: challenge,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, old := range s.authRequests {
		if time.Since(old.created) > authRequestTTL {
			s.deleteAuthRequestLocked(id)
		}
	}
	s.authRequests[ar.id] = ar
	return ar, nil
}

func (s *storage) AuthRequestByID(_ context.Context, id string) (op.AuthRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ar, ok := s.authRequests[id]
	if !ok || time.Since(ar.created) > authRequestTTL {
		return nil, errors.New("sign-in request not found or expired")
	}
	return ar, nil
}

func (s *storage) AuthRequestByCode(ctx context.Context, code string) (op.AuthRequest, error) {
	s.mu.Lock()
	id, ok := s.codes[code]
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("code invalid or expired")
	}
	return s.AuthRequestByID(ctx, id)
}

func (s *storage) SaveAuthCode(_ context.Context, id, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[code] = id
	return nil
}

// DeleteAuthRequest is called once the code has been exchanged, which makes
// codes single-use.
func (s *storage) DeleteAuthRequest(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteAuthRequestLocked(id)
	return nil
}

func (s *storage) deleteAuthRequestLocked(id string) {
	delete(s.authRequests, id)
	for code, reqID := range s.codes {
		if reqID == id {
			delete(s.codes, code)
		}
	}
}

// completeAuthRequest records who signed in, after the login UI has
// verified them.
func (s *storage) completeAuthRequest(id, username, method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ar, ok := s.authRequests[id]
	if !ok || time.Since(ar.created) > authRequestTTL {
		return errors.New("this sign-in has expired; please start again from the app")
	}
	ar.username, ar.authTime, ar.amr, ar.done = username, time.Now(), []string{method}, true
	return nil
}

// ---- Tokens ---------------------------------------------------------------

// refreshToken is persisted keyed by the SHA-256 of the token, so a leaked
// tokens file doesn't hand out working tokens.
type refreshToken struct {
	Username string    `json:"username"`
	ClientID string    `json:"clientId"`
	Scopes   []string  `json:"scopes"`
	AMR      []string  `json:"amr"`
	AuthTime time.Time `json:"authTime"`
	Expires  time.Time `json:"expires"`
}

func (r *refreshToken) GetAMR() []string             { return r.AMR }
func (r *refreshToken) GetAudience() []string        { return []string{r.ClientID} }
func (r *refreshToken) GetAuthTime() time.Time       { return r.AuthTime }
func (r *refreshToken) GetClientID() string          { return r.ClientID }
func (r *refreshToken) GetScopes() []string          { return r.Scopes }
func (r *refreshToken) GetSubject() string           { return r.Username }
func (r *refreshToken) SetCurrentScopes(sc []string) { r.Scopes = sc }

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *storage) saveRefreshTokensLocked() error {
	now := time.Now()
	for k, t := range s.refreshTokens {
		if now.After(t.Expires) {
			delete(s.refreshTokens, k)
		}
	}
	raw, err := json.Marshal(s.refreshTokens)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.tokensPath, raw)
}

func (s *storage) CreateAccessToken(context.Context, op.TokenRequest) (string, time.Time, error) {
	return randomID(), time.Now().Add(accessTokenTTL), nil
}

// CreateAccessAndRefreshTokens issues a refresh token, rotating it if this is
// itself a refresh: the presented token stops working.
func (s *storage) CreateAccessAndRefreshTokens(_ context.Context, req op.TokenRequest, current string) (string, string, time.Time, error) {
	var rt refreshToken
	switch r := req.(type) {
	case *authRequest:
		rt = refreshToken{Username: r.username, ClientID: r.clientID, Scopes: r.scopes, AMR: r.amr, AuthTime: r.authTime}
	case *refreshToken:
		rt = *r
	default:
		return "", "", time.Time{}, fmt.Errorf("unsupported token request %T", req)
	}
	rt.Expires = time.Now().Add(refreshTokenTTL)
	token := randomID()

	s.mu.Lock()
	defer s.mu.Unlock()
	if current != "" {
		if _, ok := s.refreshTokens[hashToken(current)]; !ok {
			return "", "", time.Time{}, op.ErrInvalidRefreshToken
		}
		delete(s.refreshTokens, hashToken(current))
	}
	s.refreshTokens[hashToken(token)] = &rt
	if err := s.saveRefreshTokensLocked(); err != nil {
		return "", "", time.Time{}, err
	}
	return randomID(), token, time.Now().Add(accessTokenTTL), nil
}

func (s *storage) TokenRequestByRefreshToken(_ context.Context, token string) (op.RefreshTokenRequest, error) {
	s.mu.Lock()
	rt, ok := s.refreshTokens[hashToken(token)]
	s.mu.Unlock()
	if !ok || time.Now().After(rt.Expires) {
		return nil, op.ErrInvalidRefreshToken
	}
	if !s.accounts.Exists(rt.Username) {
		return nil, oidc.ErrInvalidGrant().WithDescription("account no longer exists")
	}
	copied := *rt
	return &copied, nil
}

// TerminateSession signs username out of clientID by dropping their refresh
// tokens (end_session). Access tokens run out on their own within the hour.
func (s *storage) TerminateSession(_ context.Context, username, clientID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.refreshTokens {
		if t.Username == username && t.ClientID == clientID {
			delete(s.refreshTokens, k)
		}
	}
	return s.saveRefreshTokensLocked()
}

// RevokeToken handles /revoke. Only refresh tokens are revocable; access
// tokens are short-lived JWTs that aren't tracked. The library passes either
// the refresh token itself or, after GetRefreshTokenInfo, its ID (the hash
// it's stored under).
func (s *storage) RevokeToken(_ context.Context, tokenOrID, _, clientID string) *oidc.Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := tokenOrID
	rt, ok := s.refreshTokens[key]
	if !ok {
		key = hashToken(tokenOrID)
		rt, ok = s.refreshTokens[key]
	}
	if !ok {
		return nil // per RFC 7009, unknown tokens are not an error
	}
	if rt.ClientID != clientID {
		return oidc.ErrInvalidClient().WithDescription("token was not issued to this client")
	}
	delete(s.refreshTokens, key)
	if err := s.saveRefreshTokensLocked(); err != nil {
		return oidc.ErrServerError().WithParent(err)
	}
	return nil
}

func (s *storage) GetRefreshTokenInfo(_ context.Context, clientID, token string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := s.refreshTokens[hashToken(token)]
	if !ok || rt.ClientID != clientID {
		return "", "", op.ErrInvalidRefreshToken
	}
	return rt.Username, hashToken(token), nil
}

// ---- Keys -----------------------------------------------------------------

func (s *storage) SigningKey(context.Context) (op.SigningKey, error) { return s.key, nil }
func (s *storage) SignatureAlgorithms(context.Context) ([]jose.SignatureAlgorithm, error) {
	return []jose.SignatureAlgorithm{jose.ES256}, nil
}
func (s *storage) KeySet(context.Context) ([]op.Key, error) {
	return []op.Key{publicKey{s.key}}, nil
}

// ---- Clients and claims ---------------------------------------------------

func (s *storage) GetClientByClientID(_ context.Context, id string) (op.Client, error) {
	c, ok := clients[id]
	if !ok {
		return nil, errors.New("client not found")
	}
	return c, nil
}

// AuthorizeClientIDSecret is never reached: every client is public.
func (s *storage) AuthorizeClientIDSecret(context.Context, string, string) error {
	return errors.New("client secrets are not supported")
}

// SetUserinfoFromScopes is deprecated in favor of SetUserinfoFromRequest.
func (s *storage) SetUserinfoFromScopes(context.Context, *oidc.UserInfo, string, string, []string) error {
	return nil
}

func (s *storage) SetUserinfoFromRequest(_ context.Context, info *oidc.UserInfo, req op.IDTokenRequest, scopes []string) error {
	return s.setUserinfo(info, req.GetSubject(), scopes)
}

func (s *storage) SetUserinfoFromToken(_ context.Context, info *oidc.UserInfo, _, subject, _ string) error {
	return s.setUserinfo(info, subject, []string{oidc.ScopeOpenID, oidc.ScopeProfile})
}

func (s *storage) SetIntrospectionFromToken(_ context.Context, resp *oidc.IntrospectionResponse, _, subject, clientID string) error {
	info := new(oidc.UserInfo)
	if err := s.setUserinfo(info, subject, []string{oidc.ScopeOpenID, oidc.ScopeProfile}); err != nil {
		return err
	}
	resp.SetUserInfo(info)
	resp.ClientID = clientID
	return nil
}

func (s *storage) setUserinfo(info *oidc.UserInfo, username string, scopes []string) error {
	if !s.accounts.Exists(username) {
		return errors.New("account no longer exists")
	}
	for _, scope := range scopes {
		switch scope {
		case oidc.ScopeOpenID:
			info.Subject = username
		case oidc.ScopeProfile:
			info.PreferredUsername = username
		}
	}
	return nil
}

// GetPrivateClaimsFromScopes adds preferred_username to access tokens, so
// resource servers know the username without a userinfo call.
func (s *storage) GetPrivateClaimsFromScopes(_ context.Context, username, _ string, scopes []string) (map[string]any, error) {
	if slices.Contains(scopes, oidc.ScopeProfile) {
		return map[string]any{"preferred_username": username}, nil
	}
	return nil, nil
}

// GetKeyByIDAndClientID and ValidateJWTProfileScopes back the JWT-profile
// grant (service accounts), which Quine doesn't offer.
func (s *storage) GetKeyByIDAndClientID(context.Context, string, string) (*jose.JSONWebKey, error) {
	return nil, errors.New("JWT profile grant is not supported")
}

func (s *storage) ValidateJWTProfileScopes(context.Context, string, []string) ([]string, error) {
	return nil, errors.New("JWT profile grant is not supported")
}

func (s *storage) Health(context.Context) error { return nil }

func randomID() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
