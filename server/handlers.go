package main

import (
	"encoding/json"
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
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

// handleRegister announces/refreshes the caller's presence. POST
func (s *Store) handleRegister(w http.ResponseWriter, r *http.Request, username string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	s.Touch(username)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleUsers lists usernames seen recently, so an app can show who's
// reachable right now. GET
func (s *Store) handleUsers(w http.ResponseWriter, r *http.Request, _ string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"users": s.OnlineUsers()})
}

// handleCall starts a call from the caller: mints a meeting ID and queues an
// invite for the callee to discover next time they poll. POST {"to": "bob"}
func (s *Store) handleCall(w http.ResponseWriter, r *http.Request, from string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		To string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	to := strings.ToLower(strings.TrimSpace(body.To))
	if to == "" {
		writeError(w, http.StatusBadRequest, "to is required")
		return
	}
	if to == from {
		writeError(w, http.StatusBadRequest, "you can't call yourself")
		return
	}
	s.Touch(from)
	inv := s.PlaceCall(from, to)
	writeJSON(w, http.StatusOK, map[string]string{"meetingId": inv.MeetingID})
}

// handlePoll returns (and clears) invites waiting for the caller. GET
func (s *Store) handlePoll(w http.ResponseWriter, r *http.Request, username string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	invites := s.DrainInvites(username)
	writeJSON(w, http.StatusOK, map[string][]Invite{"invites": invites})
}
