package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// fakeVerifier treats the bearer token as the caller's subject.
type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, token string) (string, error) {
	if token == "" || token == "bad" {
		return "", errors.New("invalid")
	}
	return token, nil
}

func newTestApp(t *testing.T) *httptest.Server {
	t.Helper()
	contacts, err := LoadContacts(filepath.Join(t.TempDir(), "contacts.json"))
	if err != nil {
		t.Fatal(err)
	}
	app := &App{
		verifier:        fakeVerifier{},
		store:           NewStore(),
		contacts:        contacts,
		defaultJitsiURL: "https://meet.jit.si",
	}
	mux := http.NewServeMux()
	app.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// do sends a request as `as` (the fake token) and decodes the JSON reply.
func do(t *testing.T, srv *httptest.Server, as, method, path, body string) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, srv.URL+path, r)
	if as != "" {
		req.Header.Set("Authorization", "Bearer "+as)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAuth(t *testing.T) {
	srv := newTestApp(t)
	if status, _ := do(t, srv, "", "GET", "/me", ""); status != 401 {
		t.Errorf("no token: %d", status)
	}
	if status, _ := do(t, srv, "bad", "GET", "/me", ""); status != 401 {
		t.Errorf("bad token: %d", status)
	}
	status, me := do(t, srv, "alice", "GET", "/me", "")
	if status != 200 || me["username"] != "alice" || me["defaultJitsiUrl"] != "https://meet.jit.si" {
		t.Errorf("/me: %d %v", status, me)
	}
}

func TestCallOnDefaultServer(t *testing.T) {
	srv := newTestApp(t)
	status, call := do(t, srv, "alice", "POST", "/call", `{"to":"Bob"}`)
	if status != 200 || call["jitsiUrl"] != "https://meet.jit.si" || call["meetingId"] == "" {
		t.Fatalf("call: %d %v", status, call)
	}
	_, poll := do(t, srv, "bob", "GET", "/poll", "")
	inv := poll["invites"].([]any)[0].(map[string]any)
	if inv["from"] != "alice" || inv["jitsiUrl"] != "https://meet.jit.si" || inv["meetingId"] != call["meetingId"] {
		t.Fatalf("invite: %v", inv)
	}
}

func TestCallOnAnotherServer(t *testing.T) {
	srv := newTestApp(t)
	// The caller picks any Jitsi; the callee is sent to the same one.
	status, call := do(t, srv, "alice", "POST", "/call", `{"to":"bob","jitsiUrl":"Jitsi.Example.org/"}`)
	if status != 200 || call["jitsiUrl"] != "https://jitsi.example.org" {
		t.Fatalf("call: %d %v", status, call)
	}
	_, poll := do(t, srv, "bob", "GET", "/poll", "")
	inv := poll["invites"].([]any)[0].(map[string]any)
	if inv["jitsiUrl"] != "https://jitsi.example.org" || inv["meetingId"] != call["meetingId"] {
		t.Fatalf("invite: %v", inv)
	}
}

func TestCallValidation(t *testing.T) {
	srv := newTestApp(t)
	for _, tc := range []struct{ body, why string }{
		{`{"to":"alice"}`, "self"},
		{`{"to":"not a name"}`, "malformed name"},
		{`{"to":"bob@example.com"}`, "not a username"},
		{`{"to":"bob","jitsiUrl":"javascript:alert(1)"}`, "non-http server"},
		{`{"to":"bob","jitsiUrl":"file:///etc/passwd"}`, "file server"},
		{`{"to":"bob","jitsiUrl":"https://x.example/?a=b"}`, "query in server"},
		{`{"to":"bob","jitsiUrl":"https://user@x.example"}`, "credentials in server"},
	} {
		if status, _ := do(t, srv, "alice", "POST", "/call", tc.body); status != 400 {
			t.Errorf("%s: status %d", tc.why, status)
		}
	}
}

func TestContacts(t *testing.T) {
	srv := newTestApp(t)
	if status, _ := do(t, srv, "alice", "POST", "/contacts", `{"username":"Joe"}`); status != 200 {
		t.Fatalf("add: %d", status)
	}
	do(t, srv, "alice", "POST", "/contacts", `{"username":"bob"}`)
	if status, _ := do(t, srv, "alice", "POST", "/contacts", `{"username":"alice"}`); status != 400 {
		t.Errorf("adding yourself: %d", status)
	}
	do(t, srv, "joe", "GET", "/poll", "") // joe comes online

	_, list := do(t, srv, "alice", "GET", "/contacts", "")
	got, _ := json.Marshal(list["contacts"])
	want := `[{"online":false,"username":"bob"},{"online":true,"username":"joe"}]`
	if string(got) != want {
		t.Fatalf("contacts = %s, want %s", got, want)
	}

	do(t, srv, "alice", "DELETE", "/contacts/bob", "")
	_, list = do(t, srv, "alice", "GET", "/contacts", "")
	if n := len(list["contacts"].([]any)); n != 1 {
		t.Fatalf("after delete: %d contacts", n)
	}
	// Contact lists are per person.
	_, other := do(t, srv, "bob", "GET", "/contacts", "")
	if n := len(other["contacts"].([]any)); n != 0 {
		t.Fatalf("bob sees %d of alice's contacts", n)
	}
}
