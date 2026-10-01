# Quple

A tiny "call someone by username" desktop app built around
[Jitsi Meet](https://jitsi.org). Modeled on [quintodrome](../quintodrome), it
wraps a web app in a [Tauri](https://tauri.app) shell — but instead of hosting
a local sidecar server, it points at a hosted (or self-hosted) Jitsi Meet
deployment and uses a minimal central service to let two users find each
other and agree on a meeting room.

## How it works

1. **Sign in.** Everything is behind a Quine account
   (`identity.quineglobal.com`, an OpenID Connect provider in `identity/`).
   The app opens the sign-in page in the system browser; your Quine username
   is the address people call you at. Then, when calls run on the public
   `meet.jit.si`, the app asks you to sign in there once too (meet.jit.si only
   starts a meeting when a signed-in user joins), and every meeting reuses
   that session.
2. **Presence.** While the app is open, it polls the signaling server every
   few seconds, which doubles as "I'm online" and "did anyone call me?".
3. **Calling.** You call someone by username. The app asks the signaling
   server for a meeting ID, then opens a window pointed at
   `<jitsi-server>/<meeting-id>`.
4. **Receiving.** The callee's app sees the pending invite on its next poll,
   shows an "Incoming call from X" prompt, and — if accepted — opens the same
   meeting URL. Jitsi Meet's own WebRTC signaling (XMPP + the videobridge)
   takes it from there; Quple's server never touches media or call content,
   it only introduces two clients to the same room name.

Every request to the signaling server carries a Quine access token; the
server takes the caller's username from it, never from the request.

```
   Caller's Quple                Signaling server              Callee's Quple
   --------------                ----------------              --------------
   POST /call {to}       ----->  mint meetingId,          
                                 queue invite for "to"
                         <-----  {meetingId}
   open window:
   jitsi.example/<id>
                                                            GET /poll
                                                     <----- (polling every ~3s)
                                 return queued invite ----->
                                                            show "Incoming call"
                                                            accept -> open window:
                                                            jitsi.example/<id>
```

Both windows are now just two independent visitors to the same Jitsi Meet
room — the same as if they'd both manually opened
`https://meet.jit.si/quple-abc123` and shared the link.

## Layout

```
quple/
├── identity/     # Go: Quine sign-in (identity.quineglobal.com) - an OIDC
│                 # provider on zitadel/oidc, with GitHub/Google via goth
├── server/       # Go: the minimal central signaling service
├── desktop/      # Tauri desktop app (the thing each user runs); the UI is
│                 # Preact + htm, vendored, so there's no frontend build
└── jitsi-meet/   # Vendored upstream jitsi-meet source (reference / future
                  # self-hosting; not built as part of this app — see below)
```

### Why `jitsi-meet/` isn't built

Quple doesn't build or serve jitsi-meet's frontend itself. It just navigates
a window to a room URL on *some* Jitsi Meet deployment, the same way a
browser tab would. By default that's the public `meet.jit.si`, which requires
no infrastructure at all. Running your own deployment needs Prosody (XMPP),
Jicofo, and JVB in addition to the web frontend in this repo — that's a
separate, heavier project (see jitsi-meet's own `doc/quick-install.md` and
`docker-compose` setup). The source is vendored here so it's easy to build a
custom/branded frontend and point `jitsiBase` at it later; swap it in via the
app's Settings screen (or `meet.quineglobal.com`'s own default) once you have
one running.

## Running it

Requires Go 1.25+.

### 1. Identity server (Quine sign-in)

```sh
cd identity
go run .              # serves issuer https://identity.quineglobal.com on :8081
```

Environment: `ISSUER` (the public base URL, exactly as clients see it),
`ADDR`, `DATA_DIR` (default `./data`: the signing key, `accounts.json`, and
hashed refresh tokens - back it up and keep it private).
`GITHUB_CLIENT_ID`/`GITHUB_CLIENT_SECRET` and
`GOOGLE_CLIENT_ID`/`GOOGLE_CLIENT_SECRET` turn on "Continue with GitHub/Google";
register `$ISSUER/login/github/callback` and `$ISSUER/login/google/callback` as
the OAuth apps' callback URLs.

Accounts are invite-only, managed with the same binary (it re-reads
`accounts.json`, so this works while the server runs):

```sh
go run . adduser alice               # prompts for a password (empty = social sign-in only)
go run . link alice github octocat   # this GitHub user may sign in as alice
go run . link alice google a@b.com   # this (verified) Google address may sign in as alice
go run . list
go run . passwd alice / unlink alice github / deluser alice
```

A social link is pinned to the provider's stable user ID on first use, so a
later rename or recycled login can't take the account over. Deleting an
account signs it out everywhere within an hour (refresh is refused).

It runs behind TLS termination in production; plain `http://` issuers are
allowed for local testing only.

### 2. Signaling server

```sh
cd server
go run .              # listens on :8080; set ADDR to change it
```

`ISSUER` (default `https://identity.quineglobal.com`) is the identity server
whose access tokens it accepts and `CLIENT_ID` (default `quple`) the app they
must be issued to. Every endpoint needs `Authorization: Bearer <access token>`.

Deploy this wherever `meet.quineglobal.com` (or whatever host name you pick)
resolves to — it's stateless-enough to run as a single small process; restart
just clears presence and any in-flight invites.

### Local end-to-end setup

```sh
(cd identity && ISSUER=http://127.0.0.1:8081 ADDR=127.0.0.1:8081 go run . &)
(cd server && ISSUER=http://127.0.0.1:8081 ADDR=127.0.0.1:8090 go run . &)
(cd identity && go run . adduser alice)
```

Then in the app's **Advanced settings** set the identity server to
`http://127.0.0.1:8081` and the signaling server to `http://127.0.0.1:8090`.

### 3. Desktop app

Requires a Rust toolchain (pinned via `desktop/rust-toolchain.toml`,
installed automatically by `rustup`) and, on Linux, the system WebView
packages Tauri needs (`webkit2gtk-4.1`, `gtk3`, `libappindicator3`,
`librsvg2` — see [Tauri's Linux prerequisites](https://v2.tauri.app/start/prerequisites/)).
On this machine those aren't installed at the OS level (it's an immutable/
atomic distro), so build it the same way quintodrome does for Linux: inside
the `flatpak-builder` distrobox, or via `rpm-ostree install` + reboot if you
want it on the host directly. macOS and Windows need no extra system
packages beyond Xcode CLT / the MSVC Build Tools.

```sh
cd desktop/src-tauri
cargo run
```

On first launch you'll land on **Sign in with Quine**, which finishes in your
browser and comes back to the app (RFC 8252: a one-shot listener on
`127.0.0.1`). The refresh token is kept in the app's data dir, readable only
by you; the webview only ever sees short-lived access tokens. Server URLs
(identity, signaling, Jitsi) are under **Advanced settings**, and later the
gear icon.

To build a standalone app bundle:

```sh
cd desktop/src-tauri
cargo tauri build
```

## Verification status

- **`identity/`**: `go vet`-clean, with end-to-end tests over real HTTP
  (`go test ./...`): password sign-in through the zitadel provider, PKCE and
  single-use codes, refresh-token rotation and revocation, refusal after an
  account is deleted, unregistered redirect URIs refused, social links pinned
  to the provider's user ID, and tokens verified with `coreos/go-oidc` the
  way the signaling server does it. The GitHub/Google round trip itself
  (goth) has **not** been exercised: it needs real OAuth app credentials.
- **`server/`**: `go vet`-clean with unit tests for call/drain/presence, and
  exercised end-to-end against a local identity server: no token, garbage
  tokens and ID tokens are refused; a `from` in the body is ignored in favour
  of the token's user; refreshed access tokens work, and refresh tokens
  survive an identity-server restart.
- **`desktop/`**: builds with `cargo build` on macOS and runs. The Preact UI's
  gating (Quine sign-in, then meet.jit.si, then dashboard; sign-out; lapsed
  sessions) is checked headlessly against a mocked backend. The browser
  sign-in round trip in the real app hasn't been driven automatically.

## Server API

All endpoints require `Authorization: Bearer <Quine access token>`; the
caller's username is the token's subject.

| Method & path     | Body           | Returns                                   |
|--------------------|----------------|--------------------------------------------|
| `POST /register`   | —              | `{"ok":true}` — marks you online           |
| `GET /users`       | —              | `{"users":["alice","bob"]}` — online now   |
| `POST /call`       | `{"to":"bob"}` | `{"meetingId":"quple-ab12cd34"}`           |
| `GET /poll`        | —              | `{"invites":[{"from":"alice","meetingId":"quple-ab12cd34","createdAt":"..."}]}` — also marks you online, and clears delivered invites |

Presence expires 90s after the last `/register` or `/poll`. Undelivered
invites expire after 2 minutes.
