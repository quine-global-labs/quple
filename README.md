# Quple

A tiny "call someone by username" desktop app built around
[Jitsi Meet](https://jitsi.org). Modeled on [quintodrome](../quintodrome), it
wraps a web app in a [Tauri](https://tauri.app) shell — but instead of hosting
a local sidecar server, it points at a hosted (or self-hosted) Jitsi Meet
deployment and uses a minimal central service to let two users find each
other and agree on a meeting room.

## How it works

1. **Setup.** Each person picks a username on first launch. No password, no
   account — the username *is* the address people call you at.
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

```
   Caller's Quple                Signaling server              Callee's Quple
   --------------                ----------------              --------------
   POST /call {from,to}  ----->  mint meetingId,          
                                 queue invite for "to"
                         <-----  {meetingId}
   open window:
   jitsi.example/<id>
                                                            GET /poll?username=me
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
├── server/       # Go: the minimal central signaling service
├── desktop/      # Tauri desktop app (the thing each user runs)
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

### 1. Signaling server

```sh
cd server
go run .              # listens on :8080 by default; set ADDR to change it
```

Quick smoke test:

```sh
curl -X POST localhost:8080/register -d '{"username":"alice"}'
curl -X POST localhost:8080/call -d '{"from":"alice","to":"bob"}'
curl 'localhost:8080/poll?username=bob'
```

Deploy this wherever `meet.quineglobal.com` (or whatever host name you pick)
resolves to — it's stateless-enough to run as a single small process; restart
just clears presence and any in-flight invites.

### 2. Desktop app

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

On first launch you'll land on the setup screen — pick a username, and
optionally adjust the signaling server / Jitsi server URLs (defaults are
`https://meet.quineglobal.com` and `https://meet.jit.si`; for local testing
against the server you just started, set the signaling server URL to
`http://localhost:8080`). Both are editable later from the gear icon.

To build a standalone app bundle:

```sh
cd desktop/src-tauri
cargo tauri build
```

## Verification status

- **`server/`**: built, `go vet`-clean, has unit tests covering call/drain/
  presence (`go test ./...`), and was manually exercised end-to-end with
  `curl` — register two users, place a call, confirm the invite is delivered
  exactly once on poll, confirm CORS preflight and validation errors work.
  Fully verified.
- **`desktop/`**: hand-reviewed against the working quintodrome reference and
  the actual `tauri` 2.12 crate source (confirmed permission identifiers like
  `core:window:default` are real, confirmed the JS `invoke()` argument-name
  casing matches the Rust command signatures). This caught one real bug,
  since fixed: `#[derive(Default)]` on `AppConfig` doesn't honor
  `#[serde(default = "...")]`, which only applies to deserialization — so
  `AppConfig::default()` needed a hand-written impl to actually return
  `https://meet.quineglobal.com` / `https://meet.jit.si` instead of empty
  strings.

  `cargo check` was **not** able to fully compile on the machine this was
  built on: it's an immutable/atomic Linux distro without the
  `webkit2gtk-4.1`/`glib-2.0`/`gtk3` dev headers Tauri's Linux backend needs,
  and installing them wasn't possible in the environment available at the
  time. `cargo check` did get far enough to resolve the full dependency graph
  and download every crate, failing only once it reached compiling
  `glib-sys`'s native bindings — i.e. `Cargo.toml`/`Cargo.lock` are known
  good, but `lib.rs` itself has not been type-checked by `rustc`.

  Before relying on this, build it for real on a machine with Tauri's
  [Linux prerequisites](https://v2.tauri.app/start/prerequisites/) installed
  (or macOS/Windows, which need no extra system packages) and run
  `cargo run` / `cargo tauri build`.

## Server API

No auth, by design — usernames are the entire trust model, matching "I have
your username, so I can call you."

| Method & path              | Body / query                  | Returns                                  |
|-----------------------------|--------------------------------|-------------------------------------------|
| `POST /register`            | `{"username":"alice"}`        | `{"ok":true}` — marks you online          |
| `GET /users`                 | —                              | `{"users":["alice","bob"]}` — online now  |
| `POST /call`                 | `{"from":"alice","to":"bob"}` | `{"meetingId":"quple-ab12cd34"}`          |
| `GET /poll?username=bob`     | —                              | `{"invites":[{"from":"alice","meetingId":"quple-ab12cd34","createdAt":"..."}]}` — also marks `bob` online, and clears delivered invites |

Presence expires 90s after the last `/register` or `/poll`. Undelivered
invites expire after 2 minutes.
