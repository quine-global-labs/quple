package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// withCORS allows any origin: the desktop webview calls this server as a
// cross-origin fetch, and auth is a bearer token rather than cookies, so
// there's no ambient credential for another origin to ride on.
func withCORS(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

// tokenVerifier checks a Quine access token and returns its subject (the
// caller's username). *Verifier is the real one; tests substitute a fake.
type tokenVerifier interface {
	Verify(ctx context.Context, token string) (string, error)
}

// App is the signaling service: presence, calls and contacts for everyone
// with a Quine account.
type App struct {
	verifier tokenVerifier
	store    *Store
	contacts *Contacts
	// defaultJitsiURL hosts calls that don't name a server.
	defaultJitsiURL string
}

func (a *App) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/me", withCORS(a.requireAuth(a.handleMe)))
	mux.HandleFunc("/register", withCORS(a.requireAuth(a.handleRegister)))
	mux.HandleFunc("/poll", withCORS(a.requireAuth(a.handlePoll)))
	mux.HandleFunc("/call", withCORS(a.requireAuth(a.handleCall)))
	mux.HandleFunc("/contacts", withCORS(a.requireAuth(a.handleContacts)))
	mux.HandleFunc("/contacts/{username}", withCORS(a.requireAuth(a.handleContact)))
}

// authedHandler is a handler that knows who's calling.
type authedHandler func(w http.ResponseWriter, r *http.Request, me string)

// requireAuth rejects requests without a valid Quine access token and passes
// the token's username to h. Usernames always come from the token, never
// from the request, so nobody can act as someone else.
func (a *App) requireAuth(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "sign in required")
			return
		}
		username, err := a.verifier.Verify(r.Context(), token)
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

// handleMe tells the app who it's signed in as, and which Jitsi calls use
// unless the caller picks another. GET
func (a *App) handleMe(w http.ResponseWriter, r *http.Request, me string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": me, "defaultJitsiUrl": a.defaultJitsiURL})
}

// handleRegister announces/refreshes the caller's presence. POST
func (a *App) handleRegister(w http.ResponseWriter, r *http.Request, me string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	a.store.Touch(me)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePoll returns (and clears) invites waiting for the caller, and counts
// as presence. GET
func (a *App) handlePoll(w http.ResponseWriter, r *http.Request, me string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	writeJSON(w, http.StatusOK, map[string][]Invite{"invites": a.store.DrainInvites(me)})
}

// handleCall starts a call from the caller to another user, on whichever
// Jitsi Meet server the caller picks (the default if none). Jitsi servers
// don't need any credentials from us: both sides just open the same room.
// POST {"to": "bob", "jitsiUrl": "https://meet.jit.si"}
func (a *App) handleCall(w http.ResponseWriter, r *http.Request, me string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		To       string `json:"to"`
		JitsiURL string `json:"jitsiUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	to, err := normalizeUsername(body.To)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if to == me {
		writeError(w, http.StatusBadRequest, "you can't call yourself")
		return
	}
	jitsiURL := a.defaultJitsiURL
	if body.JitsiURL != "" {
		if jitsiURL, err = normalizeJitsiURL(body.JitsiURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	a.store.Touch(me)
	inv := a.store.PlaceCall(to, Invite{From: me, MeetingID: newMeetingID(), JitsiURL: jitsiURL})
	writeJSON(w, http.StatusOK, map[string]string{"meetingId": inv.MeetingID, "jitsiUrl": jitsiURL})
}

type contact struct {
	Username string `json:"username"`
	Online   bool   `json:"online"`
}

// handleContacts lists the caller's contacts with presence (GET), or adds
// one (POST {"username": "bob"}).
func (a *App) handleContacts(w http.ResponseWriter, r *http.Request, me string) {
	switch r.Method {
	case http.MethodGet:
		list := []contact{}
		for _, name := range a.contacts.List(me) {
			list = append(list, contact{Username: name, Online: a.store.IsOnline(name)})
		}
		writeJSON(w, http.StatusOK, map[string][]contact{"contacts": list})
	case http.MethodPost:
		var body struct {
			Username string `json:"username"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		name, err := normalizeUsername(body.Username)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if name == me {
			writeError(w, http.StatusBadRequest, "that's you")
			return
		}
		if err := a.contacts.Add(me, name); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, contact{Username: name, Online: a.store.IsOnline(name)})
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or POST only")
	}
}

// handleContact removes one contact. DELETE /contacts/{username}
func (a *App) handleContact(w http.ResponseWriter, r *http.Request, me string) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "DELETE only")
		return
	}
	name, err := normalizeUsername(r.PathValue("username"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.contacts.Remove(me, name); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save contacts")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
