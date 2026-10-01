(() => {
  const { invoke } = window.__TAURI__.core;
  const { listen } = window.__TAURI__.event;

  const DEFAULT_SERVER_URL = "https://meet.quineglobal.com";
  const DEFAULT_JITSI_BASE = "https://meet.jit.si";

  const POLL_INTERVAL_MS = 3000;
  const USERS_REFRESH_MS = 4000;

  /** @type {{username: string, serverUrl: string, jitsiBase: string}} */
  let config = { username: "", serverUrl: DEFAULT_SERVER_URL, jitsiBase: DEFAULT_JITSI_BASE };

  let pollTimer = null;
  let usersTimer = null;
  let inviteQueue = [];

  const el = (id) => document.getElementById(id);
  const screens = {
    setup: el("setup"),
    dashboard: el("dashboard"),
    settings: el("settings"),
  };

  function showScreen(name) {
    for (const [key, node] of Object.entries(screens)) {
      node.hidden = key !== name;
    }
  }

  function apiUrl(path) {
    return config.serverUrl.replace(/\/+$/, "") + path;
  }

  async function apiFetch(path, options) {
    const res = await fetch(apiUrl(path), {
      headers: { "Content-Type": "application/json" },
      ...options,
    });
    if (!res.ok) {
      let message = res.statusText;
      try {
        const body = await res.json();
        if (body && body.error) message = body.error;
      } catch (_) {
        /* ignore */
      }
      throw new Error(message || `request failed (${res.status})`);
    }
    return res.json();
  }

  function setStatus(text, dotState) {
    el("status-text").textContent = text;
    const dot = el("status-dot");
    dot.classList.remove("online", "offline");
    if (dotState) dot.classList.add(dotState);
  }

  // ---- Setup / settings persistence -------------------------------------

  async function loadConfig() {
    const saved = await invoke("load_config");
    config = {
      username: saved.username || "",
      serverUrl: saved.server_url || DEFAULT_SERVER_URL,
      jitsiBase: saved.jitsi_base || DEFAULT_JITSI_BASE,
    };
  }

  async function persistConfig() {
    await invoke("save_config", {
      config: {
        username: config.username,
        server_url: config.serverUrl,
        jitsi_base: config.jitsiBase,
      },
    });
  }

  function fillForm(prefix, cfg) {
    el(`${prefix}-server`).value = cfg.serverUrl;
    el(`${prefix}-jitsi`).value = cfg.jitsiBase;
  }

  // ---- Dashboard ----------------------------------------------------------

  function renderUsers(users) {
    const list = el("user-list");
    const empty = el("user-list-empty");
    list.querySelectorAll(".user-row").forEach((n) => n.remove());

    const others = users.filter((u) => u !== config.username);
    empty.hidden = others.length > 0;

    for (const username of others) {
      const li = document.createElement("li");
      li.className = "user-row";

      const name = document.createElement("span");
      name.className = "name";
      name.textContent = username;

      const button = document.createElement("button");
      button.type = "button";
      button.textContent = "Call";
      button.addEventListener("click", () => placeCall(username));

      li.append(name, button);
      list.appendChild(li);
    }
  }

  async function refreshUsers() {
    try {
      const { users } = await apiFetch("/users");
      renderUsers(users || []);
      setStatus(`Connected to ${config.serverUrl}`, "online");
    } catch (err) {
      setStatus(`Can't reach signaling server: ${err.message}`, "offline");
    }
  }

  async function placeCall(target) {
    target = target.trim();
    if (!target || target === config.username) return;
    try {
      const { meetingId } = await apiFetch("/call", {
        method: "POST",
        body: JSON.stringify({ from: config.username, to: target }),
      });
      await invoke("open_meeting", {
        jitsiBase: config.jitsiBase,
        room: meetingId,
        displayName: config.username,
      });
    } catch (err) {
      setStatus(`Couldn't call ${target}: ${err.message}`, "offline");
    }
  }

  // ---- meet.jit.si sign-in --------------------------------------------------

  // meet.jit.si only starts a meeting once a signed-in user joins it. Sign in
  // once here; every meeting window then reuses that session.
  let jitsiSignedIn = false;

  function usesPublicJitsi() {
    try {
      return new URL(config.jitsiBase).host === "meet.jit.si";
    } catch (_) {
      return false;
    }
  }

  function renderJitsiAuth() {
    el("jitsi-auth").hidden = !usesPublicJitsi();
    el("jitsi-auth-text").textContent = jitsiSignedIn
      ? "Signed in. Your calls start without waiting for a host."
      : "Sign in so your calls can start. Otherwise they wait for someone signed in to join.";
    const button = el("jitsi-auth-btn");
    button.textContent = jitsiSignedIn ? "Sign out" : "Sign in to meet.jit.si";
    button.classList.toggle("secondary", jitsiSignedIn);
  }

  el("jitsi-auth-btn").addEventListener("click", async () => {
    try {
      await invoke(jitsiSignedIn ? "jitsi_sign_out" : "jitsi_sign_in");
    } catch (err) {
      setStatus(`meet.jit.si sign-in failed: ${err}`, "offline");
    }
  });

  listen("jitsi-auth-changed", (event) => {
    jitsiSignedIn = event.payload === true;
    renderJitsiAuth();
  });

  // ---- Incoming calls -------------------------------------------------------

  function showNextInvite() {
    if (inviteQueue.length === 0) {
      el("incoming-call").hidden = true;
      return;
    }
    const invite = inviteQueue[0];
    el("incoming-from").textContent = invite.from;
    el("incoming-call").hidden = false;
  }

  async function poll() {
    try {
      const { invites } = await apiFetch(`/poll?username=${encodeURIComponent(config.username)}`);
      if (invites && invites.length > 0) {
        inviteQueue.push(...invites);
        showNextInvite();
      }
    } catch (_) {
      // Presence pings failing is already surfaced by refreshUsers(); stay quiet here.
    }
  }

  function startBackgroundLoops() {
    stopBackgroundLoops();
    poll();
    refreshUsers();
    pollTimer = setInterval(poll, POLL_INTERVAL_MS);
    usersTimer = setInterval(refreshUsers, USERS_REFRESH_MS);
  }

  function stopBackgroundLoops() {
    if (pollTimer) clearInterval(pollTimer);
    if (usersTimer) clearInterval(usersTimer);
    pollTimer = null;
    usersTimer = null;
  }

  // ---- Wiring ---------------------------------------------------------------

  function enterDashboard() {
    el("me-username").textContent = config.username;
    showScreen("dashboard");
    renderJitsiAuth();
    startBackgroundLoops();
  }

  el("setup-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const form = new FormData(e.target);
    config.username = String(form.get("username") || "").trim();
    config.serverUrl = String(el("setup-server").value || DEFAULT_SERVER_URL).trim();
    config.jitsiBase = String(el("setup-jitsi").value || DEFAULT_JITSI_BASE).trim();
    if (!config.username) return;
    await persistConfig();
    enterDashboard();
  });

  el("settings-btn").addEventListener("click", () => {
    fillForm("settings", config);
    el("settings-username").value = config.username;
    showScreen("settings");
  });

  el("settings-cancel").addEventListener("click", () => {
    showScreen("dashboard");
  });

  el("settings-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    config.username = String(el("settings-username").value || "").trim();
    config.serverUrl = String(el("settings-server").value || DEFAULT_SERVER_URL).trim();
    config.jitsiBase = String(el("settings-jitsi").value || DEFAULT_JITSI_BASE).trim();
    if (!config.username) return;
    await persistConfig();
    enterDashboard();
  });

  el("call-form").addEventListener("submit", (e) => {
    e.preventDefault();
    const target = el("call-target").value;
    el("call-target").value = "";
    placeCall(target);
  });

  el("accept-call").addEventListener("click", async () => {
    const invite = inviteQueue.shift();
    showNextInvite();
    if (invite) {
      await invoke("open_meeting", {
        jitsiBase: config.jitsiBase,
        room: invite.meetingId,
        displayName: config.username,
      });
    }
  });

  el("decline-call").addEventListener("click", () => {
    inviteQueue.shift();
    showNextInvite();
  });

  // ---- Boot -------------------------------------------------------------

  (async function init() {
    await loadConfig();
    jitsiSignedIn = await invoke("jitsi_signed_in");
    if (config.username) {
      enterDashboard();
    } else {
      el("setup-server").value = config.serverUrl;
      el("setup-jitsi").value = config.jitsiBase;
      showScreen("setup");
    }
  })();
})();
