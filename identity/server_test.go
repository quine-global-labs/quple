package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	testRedirect = "http://127.0.0.1:53682/callback" // any port is allowed for loopback
	testVerifier = "a-sufficiently-long-pkce-code-verifier-0123456789-abcdefghij"
)

type testEnv struct {
	issuer   string
	accounts *Accounts
	// noRedirectToApp follows redirects within the identity server but stops
	// at the app's loopback redirect, like a browser handing off to the app.
	client *http.Client
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	accounts, err := LoadAccounts(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.Create("alice", "correct horse"); err != nil {
		t.Fatal(err)
	}

	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	handler, err = newServer(srv.URL, dir, accounts, nil)
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Host != strings.TrimPrefix(srv.URL, "http://") {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	return &testEnv{issuer: srv.URL, accounts: accounts, client: client}
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// startAuth begins an authorization request and returns the login page's
// auth request ID.
func (e *testEnv) startAuth(t *testing.T, redirectURI string) (*http.Response, string) {
	t.Helper()
	q := url.Values{
		"client_id":             {"quple"},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {"openid profile offline_access"},
		"state":                 {"st4te"},
		"nonce":                 {"n0nce"},
		"code_challenge":        {challenge(testVerifier)},
		"code_challenge_method": {"S256"},
	}
	resp, err := e.client.Get(e.issuer + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp, resp.Request.URL.Query().Get("authRequestID")
}

// signIn runs the whole browser leg and returns the code handed to the app.
func (e *testEnv) signIn(t *testing.T, password string) (*http.Response, string) {
	t.Helper()
	_, id := e.startAuth(t, testRedirect)
	if id == "" {
		t.Fatal("authorize did not lead to the login page")
	}
	resp, err := e.client.PostForm(e.issuer+"/login", url.Values{
		"authRequestID": {id},
		"username":      {"alice"},
		"password":      {password},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc == nil || !strings.HasPrefix(loc.String(), testRedirect) {
		return resp, ""
	}
	if loc.Query().Get("state") != "st4te" {
		t.Fatalf("state not passed back: %s", loc)
	}
	return resp, loc.Query().Get("code")
}

type tokens struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
}

func (e *testEnv) token(t *testing.T, form url.Values) (int, tokens) {
	t.Helper()
	form.Set("client_id", "quple")
	resp, err := http.PostForm(e.issuer+"/oauth/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var tr tokens
	_ = json.NewDecoder(resp.Body).Decode(&tr)
	return resp.StatusCode, tr
}

func (e *testEnv) exchange(t *testing.T, code, verifier string) (int, tokens) {
	return e.token(t, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {testRedirect},
		"code_verifier": {verifier},
	})
}

func (e *testEnv) refresh(t *testing.T, refreshToken string) (int, tokens) {
	return e.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}})
}

func TestSignInAndVerifyWithGoOIDC(t *testing.T) {
	e := newTestEnv(t)
	_, code := e.signIn(t, "correct horse")
	if code == "" {
		t.Fatal("no code")
	}
	status, tr := e.exchange(t, code, testVerifier)
	if status != http.StatusOK {
		t.Fatalf("token exchange: %d %s", status, tr.Error)
	}

	// Verify both tokens the way a relying party (the signaling server) will.
	ctx := context.Background()
	provider, err := oidc.NewProvider(ctx, e.issuer)
	if err != nil {
		t.Fatal(err)
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: "quple"})

	idToken, err := verifier.Verify(ctx, tr.IDToken)
	if err != nil {
		t.Fatalf("id token: %v", err)
	}
	var idClaims struct {
		Username string `json:"preferred_username"`
		Nonce    string `json:"nonce"`
	}
	_ = idToken.Claims(&idClaims)
	if idToken.Subject != "alice" || idClaims.Username != "alice" || idClaims.Nonce != "n0nce" {
		t.Fatalf("id token: sub %q, claims %+v", idToken.Subject, idClaims)
	}

	access, err := verifier.Verify(ctx, tr.AccessToken)
	if err != nil {
		t.Fatalf("access token: %v", err)
	}
	if access.Subject != "alice" {
		t.Fatalf("access token subject %q", access.Subject)
	}
}

func TestWrongPassword(t *testing.T) {
	e := newTestEnv(t)
	resp, code := e.signIn(t, "wrong password")
	if code != "" || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, code %q", resp.StatusCode, code)
	}
}

func TestCodeIsSingleUseAndNeedsVerifier(t *testing.T) {
	e := newTestEnv(t)
	_, code := e.signIn(t, "correct horse")
	if status, _ := e.exchange(t, code, testVerifier+"x"); status == http.StatusOK {
		t.Fatal("exchange with wrong PKCE verifier succeeded")
	}

	_, code = e.signIn(t, "correct horse")
	if status, tr := e.exchange(t, code, testVerifier); status != http.StatusOK {
		t.Fatalf("first exchange: %d %s", status, tr.Error)
	}
	if status, _ := e.exchange(t, code, testVerifier); status == http.StatusOK {
		t.Fatal("code was accepted twice")
	}
}

func TestRefreshRotatesAndRespectsDeletion(t *testing.T) {
	e := newTestEnv(t)
	_, code := e.signIn(t, "correct horse")
	_, first := e.exchange(t, code, testVerifier)

	status, second := e.refresh(t, first.RefreshToken)
	if status != http.StatusOK || second.AccessToken == "" || second.RefreshToken == first.RefreshToken {
		t.Fatalf("refresh: %d %s", status, second.Error)
	}
	if status, _ := e.refresh(t, first.RefreshToken); status == http.StatusOK {
		t.Fatal("rotated-out refresh token still works")
	}

	if err := e.accounts.Delete("alice"); err != nil {
		t.Fatal(err)
	}
	if status, _ := e.refresh(t, second.RefreshToken); status == http.StatusOK {
		t.Fatal("refresh works after the account was deleted")
	}
}

func TestRevokedRefreshTokenStopsWorking(t *testing.T) {
	e := newTestEnv(t)
	_, code := e.signIn(t, "correct horse")
	_, tr := e.exchange(t, code, testVerifier)

	resp, err := http.PostForm(e.issuer+"/revoke", url.Values{
		"client_id":       {"quple"},
		"token":           {tr.RefreshToken},
		"token_type_hint": {"refresh_token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: status %d", resp.StatusCode)
	}
	if status, _ := e.refresh(t, tr.RefreshToken); status == http.StatusOK {
		t.Fatal("revoked refresh token still works")
	}
}

func TestUnregisteredRedirectIsRefused(t *testing.T) {
	e := newTestEnv(t)
	resp, id := e.startAuth(t, "https://evil.example/callback")
	if id != "" || strings.HasPrefix(resp.Header.Get("Location"), "https://evil.example") {
		t.Fatalf("authorize accepted an unregistered redirect (status %d, location %q)",
			resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestSocialLinkPinsProviderID(t *testing.T) {
	dir := t.TempDir()
	accounts, _ := LoadAccounts(filepath.Join(dir, "accounts.json"))
	if err := accounts.Create("bob", ""); err != nil {
		t.Fatal(err)
	}
	if err := accounts.Link("bob", providerGitHub, "Octocat"); err != nil {
		t.Fatal(err)
	}

	if name, ok := accounts.FindSocial(providerGitHub, "583231", "octocat"); !ok || name != "bob" {
		t.Fatalf("first sign-in: %q %v", name, ok)
	}
	// Once pinned, the link follows the GitHub user ID, not the login...
	if name, ok := accounts.FindSocial(providerGitHub, "583231", "renamed-octocat"); !ok || name != "bob" {
		t.Fatalf("after rename: %q %v", name, ok)
	}
	// ...so someone who later takes over the old login doesn't get in.
	if _, ok := accounts.FindSocial(providerGitHub, "999999", "octocat"); ok {
		t.Fatal("a different GitHub user with the linked login got in")
	}
	// Social-only accounts can't be signed into with an empty password.
	if accounts.CheckPassword("bob", "") {
		t.Fatal("empty password accepted")
	}
}

