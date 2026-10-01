// Command quple-server is the minimal central rendezvous service for Quple.
//
// It does barely more than the job requires: signed-in clients announce
// themselves, look up who else is online, place a call (which mints a meeting
// ID), and poll for calls placed to them. Every request carries a Quine access
// token (from identity.quineglobal.com), which is where the caller's username
// comes from. Nothing here is a source of truth
// beyond "who has said hello recently" and "what calls are outstanding" -
// the actual meeting (media, chat, etc.) happens entirely inside Jitsi Meet,
// which this server never talks to.
package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	// ISSUER is the Quine identity server whose access tokens are accepted;
	// AUDIENCE is the client ID they must have been issued to.
	issuer := os.Getenv("ISSUER")
	if issuer == "" {
		issuer = "https://identity.quineglobal.com"
	}
	audience := os.Getenv("AUDIENCE")
	if audience == "" {
		audience = "quple"
	}
	verifier := NewVerifier(issuer, audience)

	store := NewStore()

	mux := http.NewServeMux()
	mux.HandleFunc("/register", withCORS(requireAuth(verifier, store.handleRegister)))
	mux.HandleFunc("/users", withCORS(requireAuth(verifier, store.handleUsers)))
	mux.HandleFunc("/call", withCORS(requireAuth(verifier, store.handleCall)))
	mux.HandleFunc("/poll", withCORS(requireAuth(verifier, store.handlePoll)))
	mux.HandleFunc("/healthz", withCORS(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))

	log.Printf("quple-server listening on %s, accepting tokens from %s", addr, issuer)
	log.Fatal(http.ListenAndServe(addr, mux))
}
