package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// pbkdf2Iterations follows OWASP's current recommendation for
// PBKDF2-HMAC-SHA256.
const pbkdf2Iterations = 600_000

// usernamePattern is deliberately narrow: usernames are what people type to
// call each other, and they end up in URLs and meeting display names.
var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// Social sign-in providers an account can be linked to (goth provider names).
const (
	providerGitHub = "github"
	providerGoogle = "google"
)

// Account is one Quine user. The username is the identity (it's the token
// subject and what people call each other by); a password and/or linked
// social logins are ways to prove you own it.
type Account struct {
	PasswordSalt []byte `json:"passwordSalt,omitempty"`
	PasswordHash []byte `json:"passwordHash,omitempty"`
	Links        []Link `json:"links,omitempty"`
}

// Link lets a social login sign in to an account. The admin links by
// something human-readable (GitHub login, Google email); the provider's
// stable user ID is pinned on first use, so a renamed or recycled GitHub
// login or a reassigned email can't take the account over later.
type Link struct {
	Provider string `json:"provider"`
	Match    string `json:"match"`
	Subject  string `json:"subject,omitempty"`
}

// Accounts is the account list, kept as a JSON file. Accounts are invite-only,
// created and linked from the command line. The file is re-read whenever it
// changes on disk, so CLI changes take effect on a running server.
type Accounts struct {
	mu       sync.Mutex
	path     string
	modTime  time.Time
	accounts map[string]*Account
}

func LoadAccounts(path string) (*Accounts, error) {
	a := &Accounts{path: path, accounts: map[string]*Account{}}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.reloadLocked(); err != nil {
		return nil, err
	}
	return a, nil
}

// reloadLocked re-reads the file if it changed since the last read. A
// missing file means no accounts.
func (a *Accounts) reloadLocked() error {
	info, err := os.Stat(a.path)
	if errors.Is(err, os.ErrNotExist) {
		a.accounts, a.modTime = map[string]*Account{}, time.Time{}
		return nil
	}
	if err != nil {
		return err
	}
	if info.ModTime().Equal(a.modTime) {
		return nil
	}
	raw, err := os.ReadFile(a.path)
	if err != nil {
		return err
	}
	accounts := map[string]*Account{}
	if err := json.Unmarshal(raw, &accounts); err != nil {
		return fmt.Errorf("%s: %w", a.path, err)
	}
	a.accounts, a.modTime = accounts, info.ModTime()
	return nil
}

func (a *Accounts) saveLocked() error {
	raw, err := json.MarshalIndent(a.accounts, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(a.path, raw); err != nil {
		return err
	}
	if info, err := os.Stat(a.path); err == nil {
		a.modTime = info.ModTime()
	}
	return nil
}

// update reloads, runs fn on the account map, and saves if fn succeeds.
func (a *Accounts) update(fn func(map[string]*Account) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.reloadLocked(); err != nil {
		return err
	}
	if err := fn(a.accounts); err != nil {
		return err
	}
	return a.saveLocked()
}

// Create adds an account. password may be empty for a social-login-only
// account.
func (a *Accounts) Create(username, password string) error {
	if !usernamePattern.MatchString(username) {
		return errors.New("username must be 1-32 chars of a-z, 0-9, '.', '_' or '-', starting with a letter or digit")
	}
	acct := &Account{}
	if password != "" {
		if err := acct.setPassword(password); err != nil {
			return err
		}
	}
	return a.update(func(m map[string]*Account) error {
		if _, ok := m[username]; ok {
			return fmt.Errorf("%s already exists", username)
		}
		m[username] = acct
		return nil
	})
}

// SetPassword sets or (with an empty password) removes username's password.
func (a *Accounts) SetPassword(username, password string) error {
	return a.update(func(m map[string]*Account) error {
		acct, ok := m[username]
		if !ok {
			return fmt.Errorf("no such user %q", username)
		}
		if password == "" {
			acct.PasswordSalt, acct.PasswordHash = nil, nil
			return nil
		}
		return acct.setPassword(password)
	})
}

func (a *Accounts) Delete(username string) error {
	return a.update(func(m map[string]*Account) error {
		if _, ok := m[username]; !ok {
			return fmt.Errorf("no such user %q", username)
		}
		delete(m, username)
		return nil
	})
}

// Link lets provider's user identified by match (GitHub login or Google
// email) sign in as username.
func (a *Accounts) Link(username, provider, match string) error {
	if provider != providerGitHub && provider != providerGoogle {
		return fmt.Errorf("provider must be %q or %q", providerGitHub, providerGoogle)
	}
	match = strings.ToLower(strings.TrimSpace(match))
	if match == "" {
		return errors.New("a GitHub login or Google email is required")
	}
	return a.update(func(m map[string]*Account) error {
		acct, ok := m[username]
		if !ok {
			return fmt.Errorf("no such user %q", username)
		}
		for name, other := range m {
			for _, l := range other.Links {
				if l.Provider == provider && l.Match == match {
					return fmt.Errorf("%s %s is already linked to %s", provider, match, name)
				}
			}
		}
		acct.Links = append(acct.Links, Link{Provider: provider, Match: match})
		return nil
	})
}

func (a *Accounts) Unlink(username, provider string) error {
	return a.update(func(m map[string]*Account) error {
		acct, ok := m[username]
		if !ok {
			return fmt.Errorf("no such user %q", username)
		}
		kept := acct.Links[:0]
		for _, l := range acct.Links {
			if l.Provider != provider {
				kept = append(kept, l)
			}
		}
		acct.Links = kept
		return nil
	})
}

// List describes every account, for the CLI.
func (a *Accounts) List() ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.reloadLocked(); err != nil {
		return nil, err
	}
	var lines []string
	for name, acct := range a.accounts {
		var how []string
		if acct.PasswordHash != nil {
			how = append(how, "password")
		}
		for _, l := range acct.Links {
			how = append(how, l.Provider+":"+l.Match)
		}
		lines = append(lines, fmt.Sprintf("%s\t%s", name, strings.Join(how, ", ")))
	}
	sort.Strings(lines)
	return lines, nil
}

// Exists reports whether username still has an account. Refreshing tokens
// checks it, so deleting an account signs that person out of every app
// within an access token's lifetime.
func (a *Accounts) Exists(username string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = a.reloadLocked() // on a bad file, keep using the last good copy
	_, ok := a.accounts[username]
	return ok
}

// CheckPassword reports whether password is right for username. Unknown
// users (and accounts without a password) still pay for a hash, so response
// timing doesn't reveal which usernames exist.
func (a *Accounts) CheckPassword(username, password string) bool {
	a.mu.Lock()
	_ = a.reloadLocked()
	acct, ok := a.accounts[username]
	var salt, want []byte
	if ok && acct.PasswordHash != nil {
		salt, want = acct.PasswordSalt, acct.PasswordHash
	} else {
		ok = false
		salt, want = make([]byte, 16), make([]byte, 32)
	}
	a.mu.Unlock()

	got, err := hashPassword(password, salt)
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1 && ok
}

// FindSocial returns the account a social login is linked to. subject is
// the provider's stable user ID; match is the GitHub login or verified
// Google email. A link is pinned to its subject on first use.
func (a *Accounts) FindSocial(provider, subject, match string) (string, bool) {
	match = strings.ToLower(match)
	var found string
	_ = a.update(func(m map[string]*Account) error {
		for name, acct := range m {
			for i, l := range acct.Links {
				if l.Provider != provider {
					continue
				}
				if l.Subject == subject || (l.Subject == "" && l.Match == match) {
					acct.Links[i].Subject = subject
					found = name
					return nil
				}
			}
		}
		return errNotFound
	})
	return found, found != ""
}

var errNotFound = errors.New("not found")

func (acct *Account) setPassword(password string) error {
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	hash, err := hashPassword(password, salt)
	if err != nil {
		return err
	}
	acct.PasswordSalt, acct.PasswordHash = salt, hash
	return nil
}

func hashPassword(password string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, 32)
}

// writeFileAtomic writes via a temp file + rename, so a crash mid-write can't
// leave a truncated file behind.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
