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

// withCORS allows any origin, since the app has no auth to protect and the
// desktop webview calls this server directly as a cross-origin fetch.
func withCORS(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

// handleRegister lets a client announce/refresh its presence under a
// username. POST {"username": "alice"}
func (s *Store) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	username := strings.TrimSpace(body.Username)
	if username == "" {
		writeError(w, http.StatusBadRequest, "username is required")
		return
	}
	s.Touch(username)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleUsers lists usernames seen recently, so an app can show who's
// reachable right now. GET
func (s *Store) handleUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"users": s.OnlineUsers()})
}

// handleCall starts a call: mints a meeting ID and queues an invite for the
// callee to discover next time they poll. POST {"from": "alice", "to": "bob"}
func (s *Store) handleCall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	from := strings.TrimSpace(body.From)
	to := strings.TrimSpace(body.To)
	if from == "" || to == "" {
		writeError(w, http.StatusBadRequest, "from and to are required")
		return
	}
	s.Touch(from)
	inv := s.PlaceCall(from, to)
	writeJSON(w, http.StatusOK, map[string]string{"meetingId": inv.MeetingID})
}

// handlePoll returns (and clears) invites waiting for username.
// GET /poll?username=bob
func (s *Store) handlePoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	username := strings.TrimSpace(r.URL.Query().Get("username"))
	if username == "" {
		writeError(w, http.StatusBadRequest, "username query param is required")
		return
	}
	invites := s.DrainInvites(username)
	writeJSON(w, http.StatusOK, map[string][]Invite{"invites": invites})
}
