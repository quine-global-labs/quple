use serde::{Deserialize, Serialize};
use std::fs;
use tauri::{Manager, Url, WebviewUrl, WebviewWindowBuilder};

/// Everything the app needs to remember between launches: who you are, and
/// which servers to talk to. Saved as plain JSON in the app's data dir -
/// there's nothing sensitive in it and nothing worth a database for.
#[derive(Serialize, Deserialize, Clone)]
struct AppConfig {
    #[serde(default)]
    username: String,
    #[serde(default = "default_server_url")]
    server_url: String,
    #[serde(default = "default_jitsi_base")]
    jitsi_base: String,
}

fn default_server_url() -> String {
    "https://meet.quineglobal.com".to_string()
}

fn default_jitsi_base() -> String {
    "https://meet.jit.si".to_string()
}

// `#[serde(default = "...")]` only kicks in during deserialization, so a
// derived `Default` impl would silently give `server_url`/`jitsi_base` empty
// strings instead of their real defaults. Implement it by hand to match.
impl Default for AppConfig {
    fn default() -> Self {
        Self {
            username: String::new(),
            server_url: default_server_url(),
            jitsi_base: default_jitsi_base(),
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

/// Opens (or focuses) a dedicated window for a Jitsi meeting. The meeting
/// window is plain remote web content with no Tauri capability grants, so
/// third-party JS running inside Jitsi never gets access to the app's IPC
/// bridge or filesystem.
#[tauri::command]
fn open_meeting(app: tauri::AppHandle, jitsi_base: String, room: String) -> Result<(), String> {
    let room = room.trim();
    if room.is_empty() {
        return Err("room is required".into());
    }

    let base = jitsi_base.trim_end_matches('/');
    let url = format!("{base}/{room}");
    let parsed = Url::parse(&url).map_err(|e| format!("invalid meeting URL {url}: {e}"))?;

    let label = format!("meeting-{room}");
    if let Some(existing) = app.get_webview_window(&label) {
        existing.set_focus().map_err(|e| e.to_string())?;
        return Ok(());
    }

    WebviewWindowBuilder::new(&app, label, WebviewUrl::External(parsed))
        .title(format!("Quple — {room}"))
        .inner_size(1000.0, 700.0)
        .min_inner_size(480.0, 360.0)
        .build()
        .map_err(|e| format!("could not open meeting window: {e}"))?;

    Ok(())
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .invoke_handler(tauri::generate_handler![
            load_config,
            save_config,
            open_meeting
        ])
        .run(tauri::generate_context!())
        .expect("error while running Quple");
}
