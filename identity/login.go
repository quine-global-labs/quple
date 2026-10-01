package main

import (
	"context"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/markbates/goth"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// socialStateTTL is how long someone has to finish signing in at GitHub or
// Google before the round trip is rejected.
const socialStateTTL = 10 * time.Minute

const socialStateCookie = "quine_social_state"

// login is the sign-in UI the OIDC library hands off to (client.LoginURL).
// It authenticates the person - by password or a linked GitHub/Google
// account - then marks the auth request done and sends the browser back to
// the library to finish the protocol.
type login struct {
	storage   *storage
	accounts  *Accounts
	providers map[string]goth.Provider // configured social providers, by name
	callback  func(context.Context, string) string

	mu      sync.Mutex
	pending map[string]pendingSocial // goth state -> in-flight social sign-in
}

type pendingSocial struct {
	authRequestID string
	provider      string
	session       string // marshaled goth.Session
	created       time.Time
}

func newLogin(s *storage, accounts *Accounts, providers map[string]goth.Provider, provider op.OpenIDProvider) *login {
	return &login{
		storage:   s,
		accounts:  accounts,
		providers: providers,
		callback:  op.AuthCallbackURL(provider),
		pending:   map[string]pendingSocial{},
	}
}

func (l *login) Routes(mux *http.ServeMux, interceptor *op.IssuerInterceptor) {
	mux.HandleFunc("GET /login", l.showLogin)
	mux.HandleFunc("POST /login", interceptor.HandlerFunc(l.passwordLogin))
	mux.HandleFunc("GET /login/{provider}", l.beginSocial)
	mux.HandleFunc("GET /login/{provider}/callback", interceptor.HandlerFunc(l.finishSocial))
}

func (l *login) showLogin(w http.ResponseWriter, r *http.Request) {
	l.render(w, http.StatusOK, r.URL.Query().Get("authRequestID"), "", "")
}

func (l *login) passwordLogin(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("authRequestID")
	username := strings.ToLower(strings.TrimSpace(r.PostFormValue("username")))
	if !l.accounts.CheckPassword(username, r.PostFormValue("password")) {
		l.render(w, http.StatusUnauthorized, id, username, "Wrong username or password.")
		return
	}
	l.complete(w, r, id, username, "pwd")
}

func (l *login) beginSocial(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	provider, ok := l.providers[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	id := r.URL.Query().Get("authRequestID")
	if _, err := l.storage.AuthRequestByID(r.Context(), id); err != nil {
		l.render(w, http.StatusBadRequest, id, "", "This sign-in has expired. Please start again from the app.")
		return
	}

	state := randomID()
	session, err := provider.BeginAuth(state)
	if err != nil {
		l.serverError(w, id, "starting "+name+" sign-in", err)
		return
	}
	authURL, err := session.GetAuthURL()
	if err != nil {
		l.serverError(w, id, "starting "+name+" sign-in", err)
		return
	}

	l.mu.Lock()
	for k, p := range l.pending {
		if time.Since(p.created) > socialStateTTL {
			delete(l.pending, k)
		}
	}
	l.pending[state] = pendingSocial{authRequestID: id, provider: name, session: session.Marshal(), created: time.Now()}
	l.mu.Unlock()

	// Bind the round trip to this browser, so a callback URL someone else
	// started can't be replayed here.
	http.SetCookie(w, &http.Cookie{
		Name:     socialStateCookie,
		Value:    state,
		Path:     "/login/",
		MaxAge:   int(socialStateTTL.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, authURL, http.StatusFound)
}

func (l *login) finishSocial(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	state := r.URL.Query().Get("state")
	cookie, err := r.Cookie(socialStateCookie)

	l.mu.Lock()
	p, ok := l.pending[state]
	delete(l.pending, state)
	l.mu.Unlock()

	if !ok || err != nil || cookie.Value != state || p.provider != name || time.Since(p.created) > socialStateTTL {
		l.render(w, http.StatusBadRequest, p.authRequestID, "", "That sign-in link has expired. Please try again.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: socialStateCookie, Path: "/login/", MaxAge: -1})

	if errCode := r.URL.Query().Get("error"); errCode != "" {
		l.render(w, http.StatusUnauthorized, p.authRequestID, "", "Sign-in was cancelled.")
		return
	}

	provider := l.providers[name]
	session, err := provider.UnmarshalSession(p.session)
	if err == nil {
		_, err = session.Authorize(provider, r.URL.Query())
	}
	var user goth.User
	if err == nil {
		user, err = provider.FetchUser(session)
	}
	if err != nil {
		l.serverError(w, p.authRequestID, "finishing "+name+" sign-in", err)
		return
	}

	var match string
	switch name {
	case providerGitHub:
		match = user.NickName
	case providerGoogle:
		// Only a verified address proves the person owns it.
		if verified, _ := user.RawData["verified_email"].(bool); verified {
			match = user.Email
		}
	}
	username, ok := l.accounts.FindSocial(name, user.UserID, match)
	if !ok {
		l.render(w, http.StatusForbidden, p.authRequestID, "",
			"That "+providerLabel(name)+" account isn't linked to a Quine account. Ask an admin to link it.")
		return
	}
	l.complete(w, r, p.authRequestID, username, "fed")
}

// complete records who signed in and hands back to the OIDC library, which
// issues the code and redirects to the app.
func (l *login) complete(w http.ResponseWriter, r *http.Request, id, username, method string) {
	if err := l.storage.completeAuthRequest(id, username, method); err != nil {
		l.render(w, http.StatusBadRequest, id, username, err.Error())
		return
	}
	http.Redirect(w, r, l.callback(r.Context(), id), http.StatusFound)
}

func (l *login) serverError(w http.ResponseWriter, id, doing string, err error) {
	log.Printf("login: %s: %v", doing, err)
	l.render(w, http.StatusBadGateway, id, "", "Something went wrong "+doing+". Please try again.")
}

func providerLabel(name string) string {
	switch name {
	case providerGitHub:
		return "GitHub"
	case providerGoogle:
		return "Google"
	}
	return name
}

func (l *login) render(w http.ResponseWriter, status int, id, username, errMsg string) {
	type socialButton struct{ Label, URL string }
	var buttons []socialButton
	for _, name := range []string{providerGoogle, providerGitHub} {
		if _, ok := l.providers[name]; ok {
			buttons = append(buttons, socialButton{
				Label: "Continue with " + providerLabel(name),
				URL:   "/login/" + name + "?" + url.Values{"authRequestID": {id}}.Encode(),
			})
		}
	}

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_ = loginPage.Execute(w, map[string]any{
		"ID":       id,
		"Username": username,
		"Error":    errMsg,
		"Social":   buttons,
	})
}

var loginPage = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in to Quine</title>
<style>
  :root { color-scheme: light dark; --bg: #f5f5f7; --card: #fff; --fg: #1d1d1f; --muted: #6e6e73; --accent: #2f6fde; --err: #c62828; --border: #d2d2d7; }
  @media (prefers-color-scheme: dark) { :root { --bg: #1c1c1e; --card: #2c2c2e; --fg: #f5f5f7; --muted: #a1a1a6; --accent: #4c8dff; --err: #ff6b6b; --border: #48484a; } }
  * { box-sizing: border-box; }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center; padding: 16px; background: var(--bg); color: var(--fg); font: 15px/1.4 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
  main { width: 100%; max-width: 360px; background: var(--card); border-radius: 14px; padding: 28px 24px; box-shadow: 0 2px 12px rgba(0,0,0,.08); }
  h1 { margin: 0 0 20px; font-size: 22px; }
  label { display: block; margin: 14px 0 6px; font-weight: 600; font-size: 13px; }
  input { width: 100%; padding: 10px 12px; border: 1px solid var(--border); border-radius: 8px; background: transparent; color: inherit; font: inherit; }
  .btn { display: block; width: 100%; margin-top: 12px; padding: 11px; border-radius: 8px; font: inherit; font-weight: 600; text-align: center; text-decoration: none; cursor: pointer; }
  .primary { margin-top: 22px; border: 0; background: var(--accent); color: #fff; }
  .social { border: 1px solid var(--border); background: transparent; color: inherit; }
  .divider { display: flex; align-items: center; gap: 10px; margin: 22px 0 8px; color: var(--muted); font-size: 13px; }
  .divider::before, .divider::after { content: ""; flex: 1; border-top: 1px solid var(--border); }
  .error { margin: 0 0 16px; color: var(--err); }
</style>
</head>
<body>
<main>
  <h1>Sign in to Quine</h1>
  {{if .Error}}<p class="error" role="alert">{{.Error}}</p>{{end}}
  {{range .Social}}<a class="btn social" href="{{.URL}}">{{.Label}}</a>{{end}}
  {{if .Social}}<div class="divider">or</div>{{end}}
  <form method="post" action="/login">
    <input type="hidden" name="authRequestID" value="{{.ID}}">
    <label for="username">Username</label>
    <input id="username" name="username" autocomplete="username" autocapitalize="none" spellcheck="false" value="{{.Username}}" required>
    <label for="password">Password</label>
    <input id="password" name="password" type="password" autocomplete="current-password" required>
    <button class="btn primary" type="submit">Sign in</button>
  </form>
</main>
</body>
</html>`))
