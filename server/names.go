package main

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// usernamePattern matches the identity server's: usernames are what people
// type to call each other, and they end up in URLs and meeting display names.
var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// normalizeUsername lowercases s and checks it's a well-formed Quine username.
func normalizeUsername(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !usernamePattern.MatchString(s) {
		return "", errors.New("a username is 1-32 chars of a-z, 0-9, '.', '_' or '-', starting with a letter or digit")
	}
	return s, nil
}

// normalizeJitsiURL checks s is the base URL of a Jitsi Meet deployment, like
// https://meet.jit.si, and returns it without a trailing slash. Any server
// will do - the callee's app opens it, so it only has to be a plain http(s)
// URL that can't smuggle anything else along.
func normalizeJitsiURL(s string) (string, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(s) > 200 {
		return "", errors.New("the server must be a Jitsi Meet URL, like https://meet.jit.si")
	}
	return u.Scheme + "://" + strings.ToLower(u.Host) + u.EscapedPath(), nil
}
