// Command quple-server is the minimal central rendezvous service for Quple.
//
// It does no auth and barely more than the job requires: clients register a
// username, look up who else is online, place a call (which mints a meeting
// ID), and poll for calls placed to them. Nothing here is a source of truth
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

	store := NewStore()

	mux := http.NewServeMux()
	mux.HandleFunc("/register", withCORS(store.handleRegister))
	mux.HandleFunc("/users", withCORS(store.handleUsers))
	mux.HandleFunc("/call", withCORS(store.handleCall))
	mux.HandleFunc("/poll", withCORS(store.handlePoll))
	mux.HandleFunc("/healthz", withCORS(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))

	log.Printf("quple-server listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
