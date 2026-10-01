//! Quine sign-in (identity.quineglobal.com), as an OpenID Connect native app
//! per RFC 8252: the sign-in page opens in the system browser (where Google
//! and GitHub logins are allowed and saved passwords work), and comes back to
//! a one-shot HTTP listener on a loopback port. The authorization code is
//! exchanged with PKCE and the ID token verified by the `openidconnect`
//! crate.
//!
//! The refresh token is the long-lived credential: it stays in Rust and on
//! disk (owner-only file), never in the webview. The webview only ever gets
//! short-lived access tokens, to send to the signaling server.

use std::fs;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use openidconnect::core::{
    CoreAuthDisplay, CoreClaimName, CoreClaimType, CoreClient, CoreClientAuthMethod,
    CoreErrorResponseType, CoreGrantType, CoreJsonWebKey, CoreJweContentEncryptionAlgorithm,
    CoreJweKeyManagementAlgorithm, CoreResponseMode, CoreResponseType, CoreRevocableToken,
    CoreSubjectIdentifierType, CoreTokenResponse,
};
use openidconnect::{
    reqwest, AdditionalProviderMetadata, AuthenticationFlow, AuthorizationCode, ClientId,
    CsrfToken, IssuerUrl, Nonce, OAuth2TokenResponse, PkceCodeChallenge, ProviderMetadata,
    RedirectUrl, RefreshToken, RequestTokenError, RevocationUrl, Scope, TokenResponse,
};
use serde::{Deserialize, Serialize};
use tauri::{Manager, Url};
use tauri_plugin_opener::OpenerExt;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::TcpListener;
use tokio::sync::{oneshot, Mutex};

/// Quple's client ID at the identity server.
const CLIENT_ID: &str = "quple";
/// How long to wait for the person to finish signing in in the browser.
const SIGN_IN_TIMEOUT: Duration = Duration::from_secs(10 * 60);
/// Refresh access tokens this long before they actually expire.
const EXPIRY_MARGIN: Duration = Duration::from_secs(60);

/// What the dashboard needs: who you are and a token for the signaling
/// server.
#[derive(Serialize, Clone)]
#[serde(rename_all = "camelCase")]
pub struct Session {
    username: String,
    access_token: String,
    /// Unix milliseconds.
    expires_at: u64,
}

/// Persisted between launches.
#[derive(Serialize, Deserialize)]
struct SavedSession {
    issuer: String,
    username: String,
    refresh_token: String,
}

#[derive(Default)]
struct Inner {
    saved: Option<SavedSession>,
    access_token: Option<(String, Instant, u64)>, // token, local expiry, expires_at for JS
}

#[derive(Default)]
pub struct QuineAuth {
    inner: Mutex<Inner>,
    cancel: std::sync::Mutex<Option<oneshot::Sender<()>>>,
}

// The identity server advertises a revocation endpoint, which core metadata
// doesn't model.
#[derive(Clone, Debug, Deserialize, Serialize)]
struct RevocationMetadata {
    revocation_endpoint: Option<String>,
}
impl AdditionalProviderMetadata for RevocationMetadata {}

type QuineProviderMetadata = ProviderMetadata<
    RevocationMetadata,
    CoreAuthDisplay,
    CoreClientAuthMethod,
    CoreClaimName,
    CoreClaimType,
    CoreGrantType,
    CoreJweContentEncryptionAlgorithm,
    CoreJweKeyManagementAlgorithm,
    CoreJsonWebKey,
    CoreResponseMode,
    CoreResponseType,
    CoreSubjectIdentifierType,
>;

fn http_client() -> Result<reqwest::Client, String> {
    reqwest::ClientBuilder::new()
        // Following redirects from the token endpoint would be an SSRF risk.
        .redirect(reqwest::redirect::Policy::none())
        .timeout(Duration::from_secs(20))
        .build()
        .map_err(|e| e.to_string())
}

async fn discover(issuer: &str, http: &reqwest::Client) -> Result<QuineProviderMetadata, String> {
    let issuer_url = IssuerUrl::new(issuer.trim_end_matches('/').to_string())
        .map_err(|e| format!("invalid identity server URL: {e}"))?;
    QuineProviderMetadata::discover_async(issuer_url, http)
        .await
        .map_err(|e| format!("can't reach the Quine identity server ({issuer}): {e}"))
}

fn session_path(app: &tauri::AppHandle) -> Result<std::path::PathBuf, String> {
    let dir = app.path().app_data_dir().map_err(|e| e.to_string())?;
    fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
    Ok(dir.join("quine-session.json"))
}

fn save_session(app: &tauri::AppHandle, saved: &SavedSession) -> Result<(), String> {
    let path = session_path(app)?;
    let raw = serde_json::to_vec(saved).map_err(|e| e.to_string())?;
    let tmp = path.with_extension("json.tmp");
    fs::write(&tmp, raw).map_err(|e| format!("could not save sign-in: {e}"))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&tmp, fs::Permissions::from_mode(0o600)).map_err(|e| e.to_string())?;
    }
    fs::rename(&tmp, &path).map_err(|e| format!("could not save sign-in: {e}"))
}

fn delete_session(app: &tauri::AppHandle) {
    if let Ok(path) = session_path(app) {
        let _ = fs::remove_file(path);
    }
}

fn now_millis() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

/// Records a fresh access token and returns it as a Session.
fn remember_access(inner: &mut Inner, tokens: &CoreTokenResponse) -> Session {
    let lifetime = tokens.expires_in().unwrap_or(Duration::from_secs(300));
    let expires_at = now_millis() + lifetime.as_millis() as u64;
    let token = tokens.access_token().secret().clone();
    inner.access_token = Some((token.clone(), Instant::now() + lifetime, expires_at));
    Session {
        username: inner.saved.as_ref().map(|s| s.username.clone()).unwrap_or_default(),
        access_token: token,
        expires_at,
    }
}

/// True if the identity server rejected the refresh token itself (expired,
/// revoked, account deleted) - i.e. the person needs to sign in again, as
/// opposed to the server just being unreachable.
fn is_invalid_grant<RE: std::error::Error + 'static>(
    err: &RequestTokenError<RE, openidconnect::StandardErrorResponse<CoreErrorResponseType>>,
) -> bool {
    matches!(err, RequestTokenError::ServerResponse(resp)
        if *resp.error() == CoreErrorResponseType::InvalidGrant)
}

/// Uses the saved refresh token to get a new access token (rotating the
/// refresh token). Err("signed_out") means the person must sign in again.
async fn refresh(app: &tauri::AppHandle, inner: &mut Inner) -> Result<Session, String> {
    let saved = inner.saved.as_ref().ok_or("signed_out")?;
    let http = http_client()?;
    let metadata = discover(&saved.issuer, &http).await?;
    let client = CoreClient::from_provider_metadata(metadata, ClientId::new(CLIENT_ID.into()), None);
    let tokens = client
        .exchange_refresh_token(&RefreshToken::new(saved.refresh_token.clone()))
        .map_err(|e| e.to_string())?
        .request_async(&http)
        .await;
    let tokens = match tokens {
        Ok(t) => t,
        Err(e) if is_invalid_grant(&e) => {
            inner.saved = None;
            inner.access_token = None;
            delete_session(app);
            return Err("signed_out".into());
        }
        Err(e) => return Err(format!("can't reach the Quine identity server: {e}")),
    };
    if let (Some(new_refresh), Some(saved)) = (tokens.refresh_token(), inner.saved.as_mut()) {
        saved.refresh_token = new_refresh.secret().clone();
        save_session(app, saved)?;
    }
    Ok(remember_access(inner, &tokens))
}

/// Waits for the browser to come back to the loopback listener, answers it,
/// and returns the redirect's query parameters.
async fn await_callback(listener: TcpListener) -> Result<Url, String> {
    loop {
        let (mut stream, _) = listener.accept().await.map_err(|e| e.to_string())?;
        let mut request_line = String::new();
        BufReader::new(&mut stream)
            .read_line(&mut request_line)
            .await
            .map_err(|e| e.to_string())?;
        let target = request_line.split_whitespace().nth(1).unwrap_or("/");
        let url = Url::parse(&format!("http://127.0.0.1{target}")).map_err(|e| e.to_string())?;

        // Browsers also ask for things like /favicon.ico; ignore those.
        if url.path() != "/callback" {
            let _ = stream
                .write_all(b"HTTP/1.1 404 Not Found\r\ncontent-length: 0\r\nconnection: close\r\n\r\n")
                .await;
            continue;
        }

        let failed = url.query_pairs().any(|(k, _)| k == "error");
        let body = format!(
            "<!doctype html><meta charset=utf-8><title>Quple</title>\
             <body style=\"font:16px -apple-system,sans-serif;display:grid;place-items:center;height:90vh\">\
             <p>{}</p>",
            if failed {
                "Sign-in didn't complete. You can close this tab and try again in Quple."
            } else {
                "You're signed in to Quple. You can close this tab."
            }
        );
        let response = format!(
            "HTTP/1.1 200 OK\r\ncontent-type: text/html; charset=utf-8\r\ncontent-length: {}\r\n\
             cache-control: no-store\r\nconnection: close\r\n\r\n{body}",
            body.len()
        );
        let _ = stream.write_all(response.as_bytes()).await;
        return Ok(url);
    }
}

/// Signs in through the browser and returns the new session.
#[tauri::command]
pub async fn quine_sign_in(
    app: tauri::AppHandle,
    auth: tauri::State<'_, QuineAuth>,
    issuer: String,
) -> Result<Session, String> {
    let http = http_client()?;
    let metadata = discover(&issuer, &http).await?;

    // Any free port; the identity server accepts any port for loopback.
    let listener = TcpListener::bind("127.0.0.1:0").await.map_err(|e| e.to_string())?;
    let port = listener.local_addr().map_err(|e| e.to_string())?.port();
    let redirect = RedirectUrl::new(format!("http://127.0.0.1:{port}/callback")).map_err(|e| e.to_string())?;

    let client = CoreClient::from_provider_metadata(metadata, ClientId::new(CLIENT_ID.into()), None)
        .set_redirect_uri(redirect);
    let (pkce_challenge, pkce_verifier) = PkceCodeChallenge::new_random_sha256();
    let (authorize_url, csrf_state, nonce) = client
        .authorize_url(
            AuthenticationFlow::<CoreResponseType>::AuthorizationCode,
            CsrfToken::new_random,
            Nonce::new_random,
        )
        .add_scope(Scope::new("profile".into()))
        .add_scope(Scope::new("offline_access".into()))
        .set_pkce_challenge(pkce_challenge)
        .url();

    app.opener()
        .open_url(authorize_url.as_str(), None::<&str>)
        .map_err(|e| format!("could not open the browser: {e}"))?;

    let (cancel_tx, cancel_rx) = oneshot::channel();
    *auth.cancel.lock().unwrap() = Some(cancel_tx);
    let callback = tokio::select! {
        result = await_callback(listener) => result,
        _ = cancel_rx => Err("cancelled".to_string()),
        _ = tokio::time::sleep(SIGN_IN_TIMEOUT) => Err("Sign-in timed out. Please try again.".to_string()),
    };
    auth.cancel.lock().unwrap().take();
    let callback = callback?;

    // Bring Quple back in front of the browser.
    if let Some(main) = app.get_webview_window("main") {
        let _ = main.set_focus();
    }

    let param = |name: &str| {
        callback
            .query_pairs()
            .find(|(k, _)| k == name)
            .map(|(_, v)| v.into_owned())
    };
    if param("state").as_deref() != Some(csrf_state.secret().as_str()) {
        return Err("Sign-in response didn't match this request. Please try again.".into());
    }
    if let Some(error) = param("error") {
        let detail = param("error_description").unwrap_or(error);
        return Err(format!("Sign-in failed: {detail}"));
    }
    let code = param("code").ok_or("Sign-in response had no code.")?;

    let tokens = client
        .exchange_code(AuthorizationCode::new(code))
        .map_err(|e| e.to_string())?
        .set_pkce_verifier(pkce_verifier)
        .request_async(&http)
        .await
        .map_err(|e| format!("Sign-in failed at the token endpoint: {e}"))?;

    let id_token = tokens.id_token().ok_or("identity server returned no ID token")?;
    let claims = id_token
        .claims(&client.id_token_verifier(), &nonce)
        .map_err(|e| format!("identity server returned an invalid ID token: {e}"))?;
    let username = claims
        .preferred_username()
        .map(|u| u.to_string())
        .unwrap_or_else(|| claims.subject().to_string());
    let refresh_token = tokens
        .refresh_token()
        .ok_or("identity server returned no refresh token")?
        .secret()
        .clone();

    let saved = SavedSession {
        issuer: issuer.trim_end_matches('/').to_string(),
        username,
        refresh_token,
    };
    save_session(&app, &saved)?;
    let mut inner = auth.inner.lock().await;
    inner.saved = Some(saved);
    Ok(remember_access(&mut inner, &tokens))
}

/// Gives up on an in-progress browser sign-in.
#[tauri::command]
pub fn quine_cancel_sign_in(auth: tauri::State<'_, QuineAuth>) {
    if let Some(tx) = auth.cancel.lock().unwrap().take() {
        let _ = tx.send(());
    }
}

/// Resumes the saved sign-in, if any, for this identity server. Returns
/// None when the person needs to sign in.
#[tauri::command]
pub async fn quine_restore(
    app: tauri::AppHandle,
    auth: tauri::State<'_, QuineAuth>,
    issuer: String,
) -> Result<Option<Session>, String> {
    let saved = match fs::read(session_path(&app)?) {
        Ok(raw) => serde_json::from_slice::<SavedSession>(&raw).ok(),
        Err(_) => None,
    };
    let Some(saved) = saved.filter(|s| s.issuer == issuer.trim_end_matches('/')) else {
        return Ok(None);
    };
    let mut inner = auth.inner.lock().await;
    inner.saved = Some(saved);
    match refresh(&app, &mut inner).await {
        Ok(session) => Ok(Some(session)),
        Err(e) if e == "signed_out" => Ok(None),
        Err(e) => Err(e),
    }
}

/// Returns a usable access token, refreshing it if it's (nearly) expired or
/// `force` is set (e.g. after the signaling server rejected it).
/// Err("signed_out") means the person must sign in again.
#[tauri::command]
pub async fn quine_access_token(
    app: tauri::AppHandle,
    auth: tauri::State<'_, QuineAuth>,
    force: bool,
) -> Result<Session, String> {
    let mut inner = auth.inner.lock().await;
    if !force {
        if let (Some((token, expiry, expires_at)), Some(saved)) = (&inner.access_token, &inner.saved) {
            if Instant::now() + EXPIRY_MARGIN < *expiry {
                return Ok(Session {
                    username: saved.username.clone(),
                    access_token: token.clone(),
                    expires_at: *expires_at,
                });
            }
        }
    }
    refresh(&app, &mut inner).await
}

/// Signs out of Quine: revokes the refresh token (best effort) and forgets
/// the session.
#[tauri::command]
pub async fn quine_sign_out(
    app: tauri::AppHandle,
    auth: tauri::State<'_, QuineAuth>,
) -> Result<(), String> {
    let mut inner = auth.inner.lock().await;
    if let Some(saved) = inner.saved.take() {
        if let Ok(http) = http_client() {
            if let Ok(metadata) = discover(&saved.issuer, &http).await {
                let revocation = metadata
                    .additional_metadata()
                    .revocation_endpoint
                    .clone()
                    .and_then(|u| RevocationUrl::new(u).ok());
                if let Some(revocation) = revocation {
                    let client = CoreClient::from_provider_metadata(metadata, ClientId::new(CLIENT_ID.into()), None)
                        .set_revocation_url(revocation);
                    let token: CoreRevocableToken = RefreshToken::new(saved.refresh_token).into();
                    if let Ok(request) = client.revoke_token(token) {
                        let _ = request.request_async(&http).await;
                    }
                }
            }
        }
    }
    inner.access_token = None;
    delete_session(&app);
    Ok(())
}
