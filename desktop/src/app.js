import { h, render } from "preact";
import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import htm from "htm";

const html = htm.bind(h);
const { invoke } = window.__TAURI__.core;
const { listen } = window.__TAURI__.event;

const DEFAULTS = {
  serverUrl: "https://meet.quineglobal.com",
  jitsiBase: "https://meet.jit.si",
  identityUrl: "https://identity.quineglobal.com",
};

const POLL_INTERVAL_MS = 3000;
const USERS_REFRESH_MS = 4000;

// ---- Backend helpers ---------------------------------------------------------

async function loadConfig() {
  const saved = await invoke("load_config");
  return {
    serverUrl: saved.server_url || DEFAULTS.serverUrl,
    jitsiBase: saved.jitsi_base || DEFAULTS.jitsiBase,
    identityUrl: saved.identity_url || DEFAULTS.identityUrl,
  };
}

async function saveConfig(config) {
  await invoke("save_config", {
    config: {
      server_url: config.serverUrl,
      jitsi_base: config.jitsiBase,
      identity_url: config.identityUrl,
    },
  });
}

/** meet.jit.si won't start a meeting until a signed-in user joins it. */
function usesPublicJitsi(config) {
  try {
    return new URL(config.jitsiBase).host === "meet.jit.si";
  } catch (_) {
    return false;
  }
}

const sameUrl = (a, b) => a.replace(/\/+$/, "") === b.replace(/\/+$/, "");

// ---- App: gates everything behind Quine, then meet.jit.si --------------------

function App() {
  const [config, setConfig] = useState(null);
  /** @type {[{username: string, accessToken: string, expiresAt: number} | null, Function]} */
  const [session, setSession] = useState(null);
  const [jitsiSignedIn, setJitsiSignedIn] = useState(false);
  const [booting, setBooting] = useState(true);
  const [bootError, setBootError] = useState("");
  const [showSettings, setShowSettings] = useState(false);

  useEffect(() => {
    (async () => {
      const cfg = await loadConfig();
      setConfig(cfg);
      setJitsiSignedIn(await invoke("jitsi_signed_in"));
      try {
        setSession(await invoke("quine_restore", { issuer: cfg.identityUrl }));
      } catch (err) {
        setBootError(String(err));
      }
      setBooting(false);
    })();
    const unlisten = listen("jitsi-auth-changed", (e) => setJitsiSignedIn(e.payload === true));
    return () => unlisten.then((f) => f());
  }, []);

  /**
   * Returns a current access token, refreshing as needed. If the Quine
   * sign-in has lapsed (expired, revoked, account removed), drops back to the
   * sign-in screen and throws.
   */
  const accessToken = useCallback(async (force = false) => {
    try {
      const fresh = await invoke("quine_access_token", { force });
      setSession(fresh);
      return fresh.accessToken;
    } catch (err) {
      if (String(err) === "signed_out") setSession(null);
      throw new Error(String(err));
    }
  }, []);

  /** Signs out of everything: Quine, and meet.jit.si with it. */
  const signOut = useCallback(async () => {
    setSession(null);
    setShowSettings(false);
    await invoke("jitsi_sign_out").catch(() => {});
    await invoke("quine_sign_out");
  }, []);

  const updateConfig = useCallback(
    async (next) => {
      const identityChanged = !sameUrl(next.identityUrl, config.identityUrl);
      await saveConfig(next);
      setConfig(next);
      // A different identity server means a different account.
      if (identityChanged && session) await signOut();
    },
    [config, session, signOut],
  );

  if (booting || !config) {
    return html`<section class="screen"><p class="muted center">Signing you in…</p></section>`;
  }
  if (!session) {
    return html`<${SignIn} config=${config} initialError=${bootError} onConfig=${updateConfig} onSignedIn=${setSession} />`;
  }
  if (usesPublicJitsi(config) && !jitsiSignedIn) {
    return html`<${JitsiGate} username=${session.username} onSignOut=${signOut} />`;
  }
  if (showSettings) {
    return html`<${Settings}
      config=${config}
      username=${session.username}
      canSignOutOfJitsi=${usesPublicJitsi(config) && jitsiSignedIn}
      onSave=${async (next) => {
        await updateConfig(next);
        setShowSettings(false);
      }}
      onCancel=${() => setShowSettings(false)}
      onSignOut=${signOut}
    />`;
  }
  return html`<${Dashboard}
    config=${config}
    username=${session.username}
    accessToken=${accessToken}
    onSettings=${() => setShowSettings(true)}
  />`;
}

// ---- Step 1: Quine sign-in ---------------------------------------------------

function SignIn({ config, initialError, onConfig, onSignedIn }) {
  const [waiting, setWaiting] = useState(false);
  const [error, setError] = useState(initialError);

  const signIn = async () => {
    setError("");
    setWaiting(true);
    try {
      onSignedIn(await invoke("quine_sign_in", { issuer: config.identityUrl }));
    } catch (err) {
      if (String(err) !== "cancelled") setError(String(err));
    }
    setWaiting(false);
  };

  return html`
    <section class="screen">
      <div class="card">
        <h1>Welcome to Quple</h1>
        <p class="muted">Sign in with your Quine account. Your Quine username is what people call you at.</p>
        ${error && html`<p class="error">${error}</p>`}
        ${waiting
          ? html`
              <p class="muted">Finish signing in in your browser, then come back here.</p>
              <button type="button" class="secondary" onClick=${() => invoke("quine_cancel_sign_in")}>Cancel</button>
            `
          : html`<button type="button" onClick=${signIn}>Sign in with Quine</button>`}
        <details>
          <summary>Advanced settings</summary>
          <${ServerFields} config=${config} submitLabel="Save" onSubmit=${onConfig} />
        </details>
      </div>
    </section>
  `;
}

/** The three server URLs, as a form. */
function ServerFields({ config, submitLabel, onSubmit, onCancel }) {
  const [fields, setFields] = useState(config);
  useEffect(() => setFields(config), [config]);
  const bind = (key) => ({
    value: fields[key],
    onInput: (e) => setFields({ ...fields, [key]: e.currentTarget.value }),
    autocomplete: "off",
    spellcheck: false,
  });

  const submit = (e) => {
    e.preventDefault();
    const clean = (key) => fields[key].trim() || DEFAULTS[key];
    onSubmit({ identityUrl: clean("identityUrl"), serverUrl: clean("serverUrl"), jitsiBase: clean("jitsiBase") });
  };

  return html`
    <form onSubmit=${submit}>
      <label for="identity-url">Quine identity server URL</label>
      <input id="identity-url" ...${bind("identityUrl")} />
      <label for="server-url">Signaling server URL</label>
      <input id="server-url" ...${bind("serverUrl")} />
      <label for="jitsi-url">Jitsi Meet server URL</label>
      <input id="jitsi-url" ...${bind("jitsiBase")} />
      <div class="button-row">
        ${onCancel && html`<button type="button" class="secondary" onClick=${onCancel}>Cancel</button>`}
        <button type="submit" class=${onCancel ? "" : "secondary"}>${submitLabel}</button>
      </div>
    </form>
  `;
}

// ---- Step 2: meet.jit.si sign-in ---------------------------------------------

function JitsiGate({ username, onSignOut }) {
  const [error, setError] = useState("");
  const signIn = () => invoke("jitsi_sign_in").catch((err) => setError(String(err)));

  return html`
    <section class="screen">
      <div class="card">
        <h1>One more step</h1>
        <p class="muted">
          Calls run on meet.jit.si, which only starts a meeting once someone signed in joins it. Sign in once here
          and your calls will start straight away.
        </p>
        ${error && html`<p class="error">${error}</p>`}
        <button type="button" onClick=${signIn}>Sign in to meet.jit.si</button>
      </div>
      <p class="muted center">
        Signed in to Quine as <strong>${username}</strong> ·${" "}
        <button type="button" class="link-btn" onClick=${onSignOut}>Sign out</button>
      </p>
    </section>
  `;
}

// ---- Dashboard ---------------------------------------------------------------

/** Like setInterval, but runs immediately and always calls the latest fn. */
function useInterval(fn, ms) {
  const latest = useRef(fn);
  latest.current = fn;
  useEffect(() => {
    latest.current();
    const id = setInterval(() => latest.current(), ms);
    return () => clearInterval(id);
  }, [ms]);
}

function Dashboard({ config, username, accessToken, onSettings }) {
  const [users, setUsers] = useState([]);
  const [status, setStatus] = useState({ text: "", state: "" });
  const [invites, setInvites] = useState([]);
  const [target, setTarget] = useState("");

  const api = useCallback(
    async (path, options = {}) => {
      const send = async (token) =>
        fetch(config.serverUrl.replace(/\/+$/, "") + path, {
          ...options,
          headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
        });
      let res = await send(await accessToken());
      if (res.status === 401) {
        // The token may have been revoked or the key rotated; retry once fresh.
        res = await send(await accessToken(true));
      }
      const body = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(body.error || res.statusText || `request failed (${res.status})`);
      return body;
    },
    [config.serverUrl, accessToken],
  );

  useInterval(async () => {
    try {
      const { users } = await api("/users");
      setUsers((users || []).filter((u) => u !== username));
      setStatus({ text: `Connected to ${config.serverUrl}`, state: "online" });
    } catch (err) {
      setStatus({ text: `Can't reach signaling server: ${err.message}`, state: "offline" });
    }
  }, USERS_REFRESH_MS);

  // Polling also tells the server we're online.
  useInterval(async () => {
    try {
      const { invites } = await api("/poll");
      if (invites && invites.length > 0) setInvites((queue) => [...queue, ...invites]);
    } catch (_) {
      // Failures are already surfaced by the users refresh above.
    }
  }, POLL_INTERVAL_MS);

  const openMeeting = (meetingId) =>
    invoke("open_meeting", { jitsiBase: config.jitsiBase, room: meetingId, displayName: username });

  const call = async (who) => {
    who = who.trim().toLowerCase();
    if (!who || who === username) return;
    try {
      const { meetingId } = await api("/call", { method: "POST", body: JSON.stringify({ to: who }) });
      await openMeeting(meetingId);
    } catch (err) {
      setStatus({ text: `Couldn't call ${who}: ${err.message}`, state: "offline" });
    }
  };

  const [incoming, ...rest] = invites;

  return html`
    <section class="screen">
      <header>
        <div>
          <div class="me-label">You</div>
          <div class="me-username">${username}</div>
        </div>
        <button class="icon-btn" title="Settings" type="button" onClick=${onSettings}>⚙</button>
      </header>

      <div class="card">
        <label for="call-target">Call someone</label>
        <form
          id="call-form"
          onSubmit=${(e) => {
            e.preventDefault();
            call(target);
            setTarget("");
          }}
        >
          <input
            id="call-target"
            autocomplete="off"
            spellcheck=${false}
            placeholder="their username"
            required
            value=${target}
            onInput=${(e) => setTarget(e.currentTarget.value)}
          />
          <button type="submit">Call</button>
        </form>
      </div>

      <div class="card">
        <div class="list-header">
          <span>Online now</span>
          <span class=${`status-dot ${status.state}`} title=${status.text || "Connecting…"}></span>
        </div>
        <ul class="user-list">
          ${users.length === 0
            ? html`<li class="empty">No one else is online right now.</li>`
            : users.map(
                (u) => html`
                  <li class="user-row" key=${u}>
                    <span class="name">${u}</span>
                    <button type="button" onClick=${() => call(u)}>Call</button>
                  </li>
                `,
              )}
        </ul>
      </div>

      <p class="muted status-text">${status.text}</p>
    </section>

    ${incoming &&
    html`<${IncomingCall}
      from=${incoming.from}
      onAccept=${() => {
        setInvites(rest);
        openMeeting(incoming.meetingId);
      }}
      onDecline=${() => setInvites(rest)}
    />`}
  `;
}

function IncomingCall({ from, onAccept, onDecline }) {
  return html`
    <div class="overlay">
      <div class="card call-card">
        <div class="ringing">Incoming call</div>
        <div class="incoming-from">${from}</div>
        <div class="button-row">
          <button type="button" class="secondary" onClick=${onDecline}>Decline</button>
          <button type="button" onClick=${onAccept}>Accept</button>
        </div>
      </div>
    </div>
  `;
}

// ---- Settings ----------------------------------------------------------------

function Settings({ config, username, canSignOutOfJitsi, onSave, onCancel, onSignOut }) {
  return html`
    <section class="screen">
      <div class="card">
        <h1>Settings</h1>
        <p class="muted">Signed in to Quine as <strong>${username}</strong>.</p>
        <${ServerFields} config=${config} submitLabel="Save" onSubmit=${onSave} onCancel=${onCancel} />
      </div>
      <div class="card">
        <div class="button-row">
          ${canSignOutOfJitsi &&
          html`<button type="button" class="secondary" onClick=${() => invoke("jitsi_sign_out")}>
            Sign out of meet.jit.si
          </button>`}
          <button type="button" class="secondary" onClick=${onSignOut}>Sign out</button>
        </div>
      </div>
    </section>
  `;
}

render(html`<${App} />`, document.getElementById("app"));
