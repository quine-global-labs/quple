mod quine;

use serde::{Deserialize, Serialize};
use std::fs;
use std::sync::atomic::{AtomicU32, Ordering};
use tauri::webview::NewWindowResponse;
use tauri::{Emitter, Manager, Url, WebviewUrl, WebviewWindowBuilder};

/// Everything the app needs to remember between launches: which servers to
/// talk to. Saved as plain JSON in the app's data dir - there's nothing
/// sensitive in it (the Quine sign-in is kept separately, see quine.rs).
/// Who you are comes from your Quine account. `jitsi_base` is the Jitsi
/// Meet server you last called someone on, which the next call starts from.
#[derive(Serialize, Deserialize, Clone)]
struct AppConfig {
    #[serde(default = "default_server_url")]
    server_url: String,
    #[serde(default = "default_jitsi_base")]
    jitsi_base: String,
    #[serde(default = "default_identity_url")]
    identity_url: String,
}

fn default_server_url() -> String {
    "https://meet.quineglobal.com".to_string()
}

fn default_jitsi_base() -> String {
    "https://meet.jit.si".to_string()
}

fn default_identity_url() -> String {
    "https://identity.quineglobal.com".to_string()
}

// `#[serde(default = "...")]` only kicks in during deserialization, so a
// derived `Default` impl would silently give these empty strings instead of
// their real defaults. Implement it by hand to match.
impl Default for AppConfig {
    fn default() -> Self {
        Self {
            server_url: default_server_url(),
            jitsi_base: default_jitsi_base(),
            identity_url: default_identity_url(),
        }
    }
}

fn config_path(app: &tauri::AppHandle) -> Result<std::path::PathBuf, String> {
    let dir = app
        .path()
        .app_data_dir()
        .map_err(|e| format!("could not resolve app data dir: {e}"))?;
    fs::create_dir_all(&dir).map_err(|e| format!("could not create app data dir: {e}"))?;
    Ok(dir.join("config.json"))
}

#[tauri::command]
fn load_config(app: tauri::AppHandle) -> Result<AppConfig, String> {
    let path = config_path(&app)?;
    match fs::read_to_string(&path) {
        Ok(raw) => serde_json::from_str(&raw).map_err(|e| format!("corrupt config: {e}")),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(AppConfig::default()),
        Err(e) => Err(format!("could not read config: {e}")),
    }
}

#[tauri::command]
fn save_config(app: tauri::AppHandle, config: AppConfig) -> Result<(), String> {
    let path = config_path(&app)?;
    let raw = serde_json::to_string_pretty(&config).map_err(|e| e.to_string())?;
    fs::write(&path, raw).map_err(|e| format!("could not write config: {e}"))
}

// WKWebView's default user agent has no `Version/... Safari/...` token, so
// Jitsi Meet's browser sniffing treats it as an unknown browser and refuses to
// start ("WebRTC is not available in your browser") even though the webview
// supports WebRTC fine. Sign-in providers sniff it too. Present as desktop
// Safari.
#[cfg(target_os = "macos")]
const SAFARI_USER_AGENT: &str = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) \
     AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Safari/605.1.15";

/// meet.jit.si only lets a signed-in user start a meeting. Its sign-in page
/// (Firebase, Google/GitHub) redirects to `https://meet.jit.si/<room>#jwt=...`
/// once signed in - immediately, with a fresh token, if a session already
/// exists. Quple signs in once from the main window, then routes every
/// meeting through this page so it joins as a host without asking again.
const JITSI_PUBLIC_HOST: &str = "meet.jit.si";
const JITSI_SIGNIN_URL: &str =
    "https://meet.jit.si/v1/_cdn/auth-static/meet-jit-si/v1/signin.html";
/// Room name the dedicated sign-in window is sent to; Quple intercepts the
/// redirect to it instead of loading a meeting.
const SIGNIN_DONE_ROOM: &str = "quple-signed-in";
const SIGNIN_LABEL: &str = "jitsi-signin";
const POPUP_LABEL_PREFIX: &str = "popup-";

static POPUP_COUNTER: AtomicU32 = AtomicU32::new(0);

/// Percent-encodes everything except RFC 3986 unreserved characters, so a
/// value can't break out of its `key=value` slot in a URL.
fn percent_encode(s: &str) -> String {
    s.bytes()
        .map(|b| match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'.' | b'_' | b'~' => {
                (b as char).to_string()
            }
            _ => format!("%{b:02X}"),
        })
        .collect()
}

fn signed_in_marker(app: &tauri::AppHandle) -> Result<std::path::PathBuf, String> {
    Ok(config_path(app)?.with_file_name("jitsi-signed-in"))
}

fn is_signed_in(app: &tauri::AppHandle) -> bool {
    signed_in_marker(app).map(|p| p.exists()).unwrap_or(false)
}

fn uses_public_jitsi(jitsi_base: &str) -> bool {
    Url::parse(jitsi_base)
        .map(|u| u.host_str() == Some(JITSI_PUBLIC_HOST))
        .unwrap_or(false)
}

/// The meet.jit.si sign-in page URL that redirects to `room` once signed in.
/// Every other key in `state` is passed through to Jitsi as a URL-fragment
/// config override. The page reads `state` with `URLSearchParams` and then
/// `decodeURIComponent`s it again, hence the double encoding.
fn signin_url(state: &serde_json::Value) -> Result<Url, String> {
    let encoded = percent_encode(&percent_encode(&state.to_string()));
    Url::parse(&format!("{JITSI_SIGNIN_URL}?state={encoded}")).map_err(|e| e.to_string())
}

/// Sign-in providers open their login page with `window.open`; without a
/// handler WKWebView silently blocks it ("Login popup was blocked by your
/// browser"). Open each one as a real window sharing the opener's webview
/// configuration, so the provider can report back to the opener.
fn web_window<'a>(
    app: &'a tauri::AppHandle,
    label: impl Into<String>,
    url: Url,
) -> WebviewWindowBuilder<'a, tauri::Wry, tauri::AppHandle> {
    let builder = WebviewWindowBuilder::new(app, label, WebviewUrl::External(url));

    #[cfg(target_os = "macos")]
    let builder = builder.user_agent(SAFARI_USER_AGENT);

    let app_ = app.clone();
    builder.on_new_window(move |url, features| {
        let n = POPUP_COUNTER.fetch_add(1, Ordering::Relaxed);
        let builder = WebviewWindowBuilder::new(
            &app_,
            format!("{POPUP_LABEL_PREFIX}{n}"),
            WebviewUrl::External("about:blank".parse().unwrap()),
        )
        .window_features(features)
        .title(url.host_str().unwrap_or("Sign in"));

        #[cfg(target_os = "macos")]
        let builder = builder.user_agent(SAFARI_USER_AGENT);

        match builder.build() {
            Ok(window) => NewWindowResponse::Create { window },
            Err(_) => NewWindowResponse::Deny,
        }
    })
}

/// Sign-in popups end by calling `window.close()`, which WKWebView doesn't
/// turn into closing the native window, so close them once sign-in is done.
fn close_popups(app: &tauri::AppHandle) {
    for (label, window) in app.webview_windows() {
        if label.starts_with(POPUP_LABEL_PREFIX) {
            let _ = window.close();
        }
    }
}

#[tauri::command]
fn jitsi_signed_in(app: tauri::AppHandle) -> bool {
    is_signed_in(&app)
}

/// Opens meet.jit.si's sign-in page in its own window. When it redirects
/// (i.e. sign-in succeeded), remember that, tell the main window, and close it.
#[tauri::command]
fn jitsi_sign_in(app: tauri::AppHandle) -> Result<(), String> {
    if let Some(existing) = app.get_webview_window(SIGNIN_LABEL) {
        return existing.set_focus().map_err(|e| e.to_string());
    }

    let url = signin_url(&serde_json::json!({ "room": SIGNIN_DONE_ROOM }))?;
    let app_ = app.clone();
    web_window(&app, SIGNIN_LABEL, url)
        .title("Sign in to meet.jit.si")
        .inner_size(480.0, 640.0)
        .on_navigation(move |url| {
            let done = url.host_str() == Some(JITSI_PUBLIC_HOST)
                && url.path() == format!("/{SIGNIN_DONE_ROOM}");
            if !done {
                return true;
            }
            let app = app_.clone();
            // Don't tear down the webview from inside its own navigation
            // callback; do it on the next turn of the event loop.
            let _ = app_.run_on_main_thread(move || {
                if let Ok(marker) = signed_in_marker(&app) {
                    let _ = fs::write(marker, "");
                }
                let _ = app.emit("jitsi-auth-changed", true);
                if let Some(w) = app.get_webview_window(SIGNIN_LABEL) {
                    let _ = w.close();
                }
                close_popups(&app);
            });
            false
        })
        .build()
        .map_err(|e| format!("could not open sign-in window: {e}"))?;
    Ok(())
}

/// Forgets the meet.jit.si session. The sign-in session lives in the
/// webviews' shared website data, so clearing that is what actually signs out.
#[tauri::command]
fn jitsi_sign_out(app: tauri::AppHandle) -> Result<(), String> {
    if let Some(main) = app.get_webview_window("main") {
        main.clear_all_browsing_data().map_err(|e| e.to_string())?;
    }
    let marker = signed_in_marker(&app)?;
    if marker.exists() {
        fs::remove_file(marker).map_err(|e| e.to_string())?;
    }
    let _ = app.emit("jitsi-auth-changed", false);
    Ok(())
}

/// Opens (or focuses) a dedicated window for a Jitsi meeting. The meeting
/// window is plain remote web content with no Tauri capability grants, so
/// third-party JS running inside Jitsi never gets access to the app's IPC
/// bridge or filesystem.
#[tauri::command]
fn open_meeting(
    app: tauri::AppHandle,
    jitsi_base: String,
    room: String,
    display_name: String,
) -> Result<(), String> {
    let room = room.trim().to_string();
    if room.is_empty() {
        return Err("room is required".into());
    }
    // Whoever calls picks the server, so the callee's window must only ever
    // go to a web page.
    match Url::parse(jitsi_base.trim()) {
        Ok(u) if matches!(u.scheme(), "https" | "http") && u.host_str().is_some() => {}
        _ => return Err(format!("{jitsi_base} isn't a Jitsi Meet server URL")),
    }

    let label = format!("meeting-{room}");
    if let Some(existing) = app.get_webview_window(&label) {
        existing.set_focus().map_err(|e| e.to_string())?;
        return Ok(());
    }

    // Jitsi Meet reads config overrides from the URL fragment as
    // `key=<JSON value>` pairs. Set the display name to the Quple username and
    // skip the pre-join screen so the call starts straight away
    // (`prejoinPageEnabled` is the older name some deployments still use).
    let display_name = display_name.trim();
    let url = if uses_public_jitsi(&jitsi_base) && is_signed_in(&app) {
        signin_url(&serde_json::json!({
            "room": room,
            "userInfo.displayName": display_name,
            "config.prejoinConfig.enabled": false,
            "config.prejoinPageEnabled": false,
        }))?
    } else {
        let name_json = serde_json::to_string(display_name).map_err(|e| e.to_string())?;
        let base = jitsi_base.trim_end_matches('/');
        let url = format!(
            "{base}/{room}#userInfo.displayName={}\
             &config.prejoinConfig.enabled=false&config.prejoinPageEnabled=false",
            percent_encode(&name_json)
        );
        Url::parse(&url).map_err(|_| format!("invalid meeting URL for {base}/{room}"))?
    };

    let app_ = app.clone();
    let room_path = format!("/{room}");
    web_window(&app, label, url)
        .title(format!("Quple — {room}"))
        .inner_size(1000.0, 700.0)
        .min_inner_size(480.0, 360.0)
        .on_navigation(move |url| {
            // Reaching the room means any sign-in popup has done its job.
            if url.path() == room_path {
                let app = app_.clone();
                let _ = app_.run_on_main_thread(move || close_popups(&app));
            }
            true
        })
        .build()
        .map_err(|e| format!("could not open meeting window: {e}"))?;

    Ok(())
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .manage(quine::QuineAuth::default())
        .invoke_handler(tauri::generate_handler![
            load_config,
            save_config,
            open_meeting,
            jitsi_signed_in,
            jitsi_sign_in,
            jitsi_sign_out,
            quine::quine_sign_in,
            quine::quine_cancel_sign_in,
            quine::quine_restore,
            quine::quine_access_token,
            quine::quine_sign_out
        ])
        .run(tauri::generate_context!())
        .expect("error while running Quple");
}
