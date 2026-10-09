package main

import "testing"

func TestPlaceCallAndDrainInvites(t *testing.T) {
	s := NewStore()

	inv := s.PlaceCall("bob", Invite{From: "alice", MeetingID: newMeetingID()})
	if inv.CreatedAt.IsZero() {
		t.Fatal("CreatedAt not set")
	}

	got := s.DrainInvites("bob")
	if len(got) != 1 || got[0].MeetingID != inv.MeetingID {
		t.Fatalf("DrainInvites = %+v, want one invite with meetingId %q", got, inv.MeetingID)
	}

	// Second drain should be empty: invites are delivered once.
	if got := s.DrainInvites("bob"); len(got) != 0 {
		t.Fatalf("DrainInvites after drain = %+v, want empty", got)
	}
}

func TestMeetingIDsAreDistinct(t *testing.T) {
	if newMeetingID() == newMeetingID() {
		t.Fatal("two meeting IDs collided")
	}
}

func TestPresence(t *testing.T) {
	s := NewStore()
	if s.IsOnline("bob") {
		t.Fatal("unknown user reported online")
	}
	s.DrainInvites("bob") // polling counts as presence
	if !s.IsOnline("bob") {
		t.Fatal("polling user not reported online")
	}
}
