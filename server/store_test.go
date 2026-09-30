package main

import "testing"

func TestPlaceCallAndDrainInvites(t *testing.T) {
	s := NewStore()

	inv := s.PlaceCall("alice", "bob")
	if inv.From != "alice" {
		t.Fatalf("From = %q, want alice", inv.From)
	}
	if inv.MeetingID == "" {
		t.Fatal("MeetingID is empty")
	}

	got := s.DrainInvites("bob")
	if len(got) != 1 || got[0].MeetingID != inv.MeetingID {
		t.Fatalf("DrainInvites(bob) = %+v, want one invite with meetingId %q", got, inv.MeetingID)
	}

	// Second drain should be empty: invites are delivered once.
	got = s.DrainInvites("bob")
	if len(got) != 0 {
		t.Fatalf("DrainInvites(bob) after drain = %+v, want empty", got)
	}
}

func TestPlaceCallGeneratesDistinctMeetingIDs(t *testing.T) {
	s := NewStore()
	a := s.PlaceCall("alice", "bob")
	b := s.PlaceCall("alice", "bob")
	if a.MeetingID == b.MeetingID {
		t.Fatalf("two calls minted the same meeting ID: %q", a.MeetingID)
	}
}

func TestOnlineUsers(t *testing.T) {
	s := NewStore()
	s.Touch("bob")
	s.Touch("alice")

	users := s.OnlineUsers()
	if len(users) != 2 || users[0] != "alice" || users[1] != "bob" {
		t.Fatalf("OnlineUsers() = %v, want [alice bob]", users)
	}
}

func TestDrainInvitesTouchesPresence(t *testing.T) {
	s := NewStore()
	s.DrainInvites("bob")

	users := s.OnlineUsers()
	if len(users) != 1 || users[0] != "bob" {
		t.Fatalf("OnlineUsers() after poll = %v, want [bob]", users)
	}
}

func TestOnlineUsersExcludesUnknown(t *testing.T) {
	s := NewStore()
	if users := s.OnlineUsers(); len(users) != 0 {
		t.Fatalf("OnlineUsers() on empty store = %v, want []", users)
	}
}
