// Command quine-identity is Quine's sign-in service (identity.quineglobal.com):
// an OpenID Connect provider built on zitadel/oidc, with sign-in by password
// or by a linked GitHub/Google account (via goth).
//
// Accounts are invite-only and managed from the command line:
//
//	quine-identity                                # serve (the default)
//	quine-identity adduser <username>             # create an account (password from stdin; empty = social-only)
//	quine-identity passwd <username>              # set a password (empty removes it)
//	quine-identity link <username> github <login> # allow that GitHub user to sign in as <username>
//	quine-identity link <username> google <email> # allow that Google account (verified email) to sign in
//	quine-identity unlink <username> <provider>
//	quine-identity deluser <username>
//	quine-identity list
//
// Configuration (environment):
//
//	ISSUER                public base URL, exactly as clients see it (default https://identity.quineglobal.com)
//	ADDR                  listen address (default :8081)
//	DATA_DIR              signing key, accounts and refresh tokens (default ./data)
//	GITHUB_CLIENT_ID      enables "Continue with GitHub" (with GITHUB_CLIENT_SECRET);
//	                      the OAuth app's callback URL is $ISSUER/login/github/callback
//	GOOGLE_CLIENT_ID      enables "Continue with Google" (with GOOGLE_CLIENT_SECRET);
//	                      the OAuth client's redirect URI is $ISSUER/login/google/callback
package main

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/markbates/goth"
	"github.com/markbates/goth/providers/github"
	"github.com/markbates/goth/providers/google"
	"github.com/zitadel/oidc/v3/pkg/op"
	"golang.org/x/text/language"
)

func main() {
	issuer := strings.TrimRight(envOr("ISSUER", "https://identity.quineglobal.com"), "/")
	addr := envOr("ADDR", ":8081")
	dataDir := envOr("DATA_DIR", "data")

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	accounts, err := LoadAccounts(filepath.Join(dataDir, "accounts.json"))
	if err != nil {
		log.Fatal(err)
	}

	if len(os.Args) > 1 {
		if err := runCommand(accounts, os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	handler, err := newServer(issuer, dataDir, accounts, socialProviders(issuer))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("quine-identity serving issuer %s on %s", issuer, addr)
	log.Fatal(http.ListenAndServe(addr, handler))
}

// newServer wires the OIDC provider and the login UI together.
func newServer(issuer, dataDir string, accounts *Accounts, providers map[string]goth.Provider) (http.Handler, error) {
	key, err := loadOrCreateSigningKey(filepath.Join(dataDir, "signing-key.pem"))
	if err != nil {
		return nil, err
	}
	store, err := newStorage(accounts, key, filepath.Join(dataDir, "refresh-tokens.json"))
	if err != nil {
		return nil, err
	}

	rawKey, err := key.key.Bytes()
	if err != nil {
		return nil, err
	}
	config := &op.Config{
		// Encrypts the library's opaque tokens/codes. Derived from the signing
		// key so there's only one secret to manage.
		CryptoKey:                sha256.Sum256(append([]byte("quine-identity crypto key\x00"), rawKey...)),
		CodeMethodS256:           true,
		GrantTypeRefreshToken:    true,
		SupportedUILocales:       []language.Tag{language.English},
		DefaultLogoutRedirectURI: "/logged-out",
	}
	var opts []op.Option
	if u, err := url.Parse(issuer); err == nil && u.Scheme == "http" {
		opts = append(opts, op.WithAllowInsecure()) // local development only
	}
	provider, err := op.NewOpenIDProvider(issuer, config, store, opts...)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	newLogin(store, accounts, providers, provider).
		Routes(mux, op.NewIssuerInterceptor(provider.IssuerFromRequest))
	mux.HandleFunc("GET /logged-out", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("You're signed out of Quine."))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.Handle("/", provider)
	return mux, nil
}

// socialProviders enables each goth provider whose credentials are set.
func socialProviders(issuer string) map[string]goth.Provider {
	providers := map[string]goth.Provider{}
	if id, secret := os.Getenv("GITHUB_CLIENT_ID"), os.Getenv("GITHUB_CLIENT_SECRET"); id != "" && secret != "" {
		// read:user only: accounts are matched by GitHub login, so there's no
		// need to ask for anyone's email.
		providers[providerGitHub] = github.New(id, secret, issuer+"/login/github/callback", "read:user")
	}
	if id, secret := os.Getenv("GOOGLE_CLIENT_ID"), os.Getenv("GOOGLE_CLIENT_SECRET"); id != "" && secret != "" {
		providers[providerGoogle] = google.New(id, secret, issuer+"/login/google/callback", "email")
	}
	for name := range providers {
		log.Printf("sign-in with %s enabled", providerLabel(name))
	}
	return providers
}

func runCommand(accounts *Accounts, args []string) error {
	usage := fmt.Errorf("usage: %s [adduser|passwd|deluser <username> | link <username> github|google <login-or-email> | unlink <username> <provider> | list]",
		filepath.Base(os.Args[0]))
	if len(args) == 0 {
		return usage
	}
	cmd, args := args[0], args[1:]
	if len(args) > 0 {
		args[0] = strings.ToLower(args[0])
	}
	switch {
	case cmd == "adduser" && len(args) == 1:
		password, err := readPassword(args[0])
		if err != nil {
			return err
		}
		if err := accounts.Create(args[0], password); err != nil {
			return err
		}
	case cmd == "passwd" && len(args) == 1:
		password, err := readPassword(args[0])
		if err != nil {
			return err
		}
		if err := accounts.SetPassword(args[0], password); err != nil {
			return err
		}
	case cmd == "deluser" && len(args) == 1:
		if err := accounts.Delete(args[0]); err != nil {
			return err
		}
	case cmd == "link" && len(args) == 3:
		if err := accounts.Link(args[0], args[1], args[2]); err != nil {
			return err
		}
	case cmd == "unlink" && len(args) == 2:
		if err := accounts.Unlink(args[0], args[1]); err != nil {
			return err
		}
	case cmd == "list" && len(args) == 0:
		lines, err := accounts.List()
		if err != nil {
			return err
		}
		for _, line := range lines {
			fmt.Println(line)
		}
		return nil
	default:
		return usage
	}
	fmt.Fprintln(os.Stderr, "Done.")
	return nil
}

// readPassword reads one line from stdin. Pipe it in (or use `read -s`) to
// keep it out of the terminal and shell history.
func readPassword(username string) (string, error) {
	fmt.Fprintf(os.Stderr, "Password for %s (empty for none): ", username)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	fmt.Fprintln(os.Stderr)
	if err != nil && line == "" {
		return "", fmt.Errorf("reading password: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
