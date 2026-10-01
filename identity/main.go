// Command quine-identity is Quine's sign-in service (identity.quineglobal.com):
// a deliberately small OpenID Connect provider.
//
// It supports exactly what Quine's own apps need - the authorization code
// flow with PKCE for public (desktop) clients, refresh tokens, a JWKS for
// relying parties to verify tokens, and userinfo. Accounts are username +
// password, managed from the command line:
//
//	quine-identity                    # serve (the default)
//	quine-identity adduser <username> # create a user / reset a password (reads it from stdin)
//	quine-identity deluser <username> # delete a user
//
// Configuration (environment):
//
//	ISSUER    public base URL, exactly as clients see it (default https://identity.quineglobal.com)
//	ADDR      listen address (default :8081)
//	DATA_DIR  where the signing key and users.json live (default ./data)
package main

import (
	"bufio"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	issuer := strings.TrimRight(envOr("ISSUER", "https://identity.quineglobal.com"), "/")
	addr := envOr("ADDR", ":8081")
	dataDir := envOr("DATA_DIR", "data")

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	users, err := LoadUsers(filepath.Join(dataDir, "users.json"))
	if err != nil {
		log.Fatal(err)
	}

	if len(os.Args) > 1 {
		if err := runCommand(users, os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	signer, err := LoadOrCreateSigner(filepath.Join(dataDir, "signing-key.pem"))
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	NewProvider(issuer, signer, users).Routes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	log.Printf("quine-identity serving issuer %s on %s", issuer, addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func runCommand(users *Users, args []string) error {
	usage := fmt.Errorf("usage: %s [adduser|deluser] <username>", filepath.Base(os.Args[0]))
	if len(args) != 2 {
		return usage
	}
	username := strings.ToLower(args[1])
	switch args[0] {
	case "adduser":
		fmt.Fprintf(os.Stderr, "Password for %s: ", username)
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("reading password: %w", err)
		}
		fmt.Fprintln(os.Stderr)
		if err := users.Set(username, strings.TrimRight(line, "\r\n")); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Saved %s.\n", username)
	case "deluser":
		if err := users.Delete(username); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Deleted %s.\n", username)
	default:
		return usage
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
