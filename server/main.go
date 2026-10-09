// Command quple-server is the central rendezvous service for Quple
// (meet.quineglobal.com).
//
// Everyone signs in with Quine (identity.quineglobal.com), the one identity
// server, and is known by their Quine username. Signed-in clients announce
// themselves, keep a contact list, place calls, and poll for calls placed to
// them. A call names the Jitsi Meet server it happens on - any one, picked by
// the caller - and this server just tells the callee where to go. The meeting
// itself (media, chat, etc.) happens entirely inside Jitsi Meet, which this
// server never talks to.
//
// Configuration (environment):
//
//	ADDR           listen address (default :8080)
//	ISSUER         Quine identity server whose access tokens are accepted
//	               (default https://identity.quineglobal.com)
//	CLIENT_ID      app the tokens must be issued to (default quple)
//	DEFAULT_JITSI  Jitsi Meet server for calls that don't name one (default https://meet.jit.si)
//	DATA_DIR       contacts (default ./data)
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
)

func main() {
	addr := envOr("ADDR", ":8080")
	issuer := envOr("ISSUER", "https://identity.quineglobal.com")
	dataDir := envOr("DATA_DIR", "data")
	defaultJitsi, err := normalizeJitsiURL(envOr("DEFAULT_JITSI", "https://meet.jit.si"))
	if err != nil {
		log.Fatalf("DEFAULT_JITSI: %v", err)
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	contacts, err := LoadContacts(filepath.Join(dataDir, "contacts.json"))
	if err != nil {
		log.Fatal(err)
	}

	app := &App{
		verifier:        NewVerifier(issuer, envOr("CLIENT_ID", "quple")),
		store:           NewStore(),
		contacts:        contacts,
		defaultJitsiURL: defaultJitsi,
	}
	mux := http.NewServeMux()
	app.Routes(mux)
	mux.HandleFunc("/healthz", withCORS(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))

	log.Printf("quple-server listening on %s, accepting tokens from %s, default Jitsi %s", addr, issuer, defaultJitsi)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
