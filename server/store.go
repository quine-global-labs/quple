package main

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// Invite is a pending call: someone tried to reach a user who hasn't
// picked up (polled it) yet.
type Invite struct {
	From      string    `json:"from"`
	MeetingID string    `json:"meetingId"`
	CreatedAt time.Time `json:"createdAt"`
}

// presenceWindow is how long a user is considered "online" after their last
// /register or /poll call. Clients are expected to hit one of those every
// few seconds, so this just needs to comfortably outlast that interval.
const presenceWindow = 90 * time.Second

// inviteTTL bounds how long an undelivered invite is kept around. If the
// callee's app isn't polling (closed, offline, etc.) the invite is dropped
// rather than kept forever.
const inviteTTL = 2 * time.Minute

// Store is a deliberately minimal, in-memory, process-lifetime store with no
// persistence. Accounts live in the Quine identity server; usernames here are
// whatever verified access tokens said (see auth.go).
type Store struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time
	invites  map[string][]Invite
}

func NewStore() *Store {
	return &Store{
		lastSeen: make(map[string]time.Time),
		invites:  make(map[string][]Invite),
	}
}

func (s *Store) Touch(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSeen[username] = time.Now()
}

// OnlineUsers returns usernames seen within presenceWindow, sorted.
func (s *Store) OnlineUsers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-presenceWindow)
	users := make([]string, 0, len(s.lastSeen))
	for u, t := range s.lastSeen {
		if t.After(cutoff) {
			users = append(users, u)
		}
	}
	sort.Strings(users)
	return users
}

// PlaceCall records an invite from -> to and returns it, including the
// freshly minted meeting ID the two sides will rendezvous on.
func (s *Store) PlaceCall(from, to string) Invite {
	inv := Invite{From: from, MeetingID: newMeetingID(), CreatedAt: time.Now()}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invites[to] = append(s.invites[to], inv)
	return inv
}

// DrainInvites returns and clears every pending invite for username,
// filtering out anything older than inviteTTL. Polling also counts as
// presence, so it touches lastSeen too.
func (s *Store) DrainInvites(username string) []Invite {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSeen[username] = time.Now()

	pending := s.invites[username]
	delete(s.invites, username)

	cutoff := time.Now().Add(-inviteTTL)
	fresh := make([]Invite, 0, len(pending))
	for _, inv := range pending {
		if inv.CreatedAt.After(cutoff) {
			fresh = append(fresh, inv)
		}
	}
	return fresh
}

func newMeetingID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return "quple-" + hex.EncodeToString(buf)
}
