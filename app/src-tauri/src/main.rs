// Prevents additional console window on Windows in release
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use futures::StreamExt;
use serde::Serialize;
use std::path::PathBuf;
use std::process::{Child, Command};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use tauri::{AppHandle, Emitter, Manager, State};
use tokio::sync::Mutex;

#[derive(Clone, Serialize, serde::Deserialize)]
struct NomiEvent {
    id: String,
    #[serde(rename = "type")]
    event_type: String,
    #[serde(rename = "run_id")]
    run_id: String,
    #[serde(rename = "step_id")]
    step_id: Option<String>,
    payload: Option<serde_json::Value>,
    timestamp: String,
}

/// A monotonically-increasing generation counter. Each call to start_event_stream
/// bumps the counter; spawned streaming tasks capture their starting value and
/// exit as soon as the counter moves on. This replaces the prior 100ms sleep
/// hack that left a race between the previous stream and the new one.
struct EventStreamState {
    generation: Arc<AtomicU64>,
}

impl Default for EventStreamState {
    fn default() -> Self {
        Self {
            generation: Arc::new(AtomicU64::new(0)),
        }
    }
}

/// Holds the handle to the spawned nomid sidecar process so we can kill it
/// on app exit.
struct NomiDaemonState {
    child: Mutex<Option<Child>>,
}

/// Compute the data directory the Go daemon (nomid) uses on this platform. Must
/// mirror the Go `appDataDir()` logic in internal/storage/db/db.go exactly:
/// macOS:   ~/Library/Application Support/Nomi
/// Windows: %APPDATA%\Nomi
/// Linux:   $XDG_CONFIG_HOME/Nomi or ~/.config/Nomi
fn nomi_data_dir() -> Option<PathBuf> {
    dirs::config_dir().map(|p| p.join("Nomi"))
}

fn read_auth_token() -> Result<String, String> {
    let dir = nomi_data_dir().ok_or_else(|| "cannot locate config dir".to_string())?;
    let path = dir.join("auth.token");
    let raw = std::fs::read_to_string(&path)
        .map_err(|e| format!("failed to read {}: {}", path.display(), e))?;
    let token = raw.trim().to_string();
    if token.is_empty() {
        return Err(format!("empty token file at {}", path.display()));
    }
    Ok(token)
}

#[derive(serde::Deserialize)]
struct ApiEndpoint {
    url: String,
}

/// Read the API endpoint URL nomid published at startup. Falls back to the
/// historic default if the file is missing — keeps existing dev workflows
/// (running an older nomid build, vite preview without Tauri) functional.
fn read_api_endpoint() -> String {
    let fallback = "http://127.0.0.1:8080".to_string();
    let Some(dir) = nomi_data_dir() else {
        return fallback;
    };
    let path = dir.join("api.endpoint");
    let Ok(raw) = std::fs::read_to_string(&path) else {
        return fallback;
    };
    match serde_json::from_str::<ApiEndpoint>(&raw) {
        Ok(ep) if !ep.url.is_empty() => ep.url,
        _ => fallback,
    }
}

#[tauri::command]
async fn get_api_endpoint() -> Result<String, String> {
    Ok(read_api_endpoint())
}

/// Expose the bearer token to the React frontend so it can attach
/// `Authorization: Bearer <token>` to every fetch. The token file is owned by
/// the user account that started nomid, so access here is the same trust
/// boundary as "who can run this desktop app".
#[tauri::command]
async fn get_auth_token() -> Result<String, String> {
    read_auth_token()
}

#[tauri::command]
async fn start_event_stream(
    app: AppHandle,
    state: State<'_, Mutex<EventStreamState>>,
    run_id: Option<String>,
) -> Result<(), String> {
    let guard = state.lock().await;
    // Bump the generation; this both stops any previous task (it will see
    // generation != my_gen) and marks us as "the live stream".
    let my_gen = guard.generation.fetch_add(1, Ordering::SeqCst) + 1;
    let generation = guard.generation.clone();
    drop(guard);

    let token = read_auth_token()?;

    let base = read_api_endpoint();
    let mut url = format!("{}/events/stream", base);
    if let Some(rid) = run_id {
        url.push_str(&format!("?run_id={}", rid));
    }

    tauri::async_runtime::spawn(async move {
        let client = reqwest::Client::new();
        let response = match client
            .get(&url)
            .bearer_auth(&token)
            .send()
            .await
        {
            Ok(r) => r,
            Err(e) => {
                let _ = app.emit("nomi-events-error", format!("Connection failed: {}", e));
                return;
            }
        };

        if !response.status().is_success() {
            let _ = app.emit(
                "nomi-events-error",
                format!("HTTP error: {}", response.status()),
            );
            return;
        }

        let mut stream = response.bytes_stream();
        // Accumulate bytes across chunks. TCP frames are not line-aligned, so
        // the naive "split each chunk on \n" approach would drop events whose
        // payload straddles two chunks and could corrupt multi-byte UTF-8.
        let mut buffer: Vec<u8> = Vec::with_capacity(8192);

        while let Some(chunk_result) = stream.next().await {
            if generation.load(Ordering::SeqCst) != my_gen {
                break;
            }

            let chunk = match chunk_result {
                Ok(c) => c,
                Err(e) => {
                    let _ = app.emit("nomi-events-error", format!("Stream error: {}", e));
                    break;
                }
            };

            buffer.extend_from_slice(&chunk);

            // An SSE message is terminated by a blank line (\n\n). Process
            // every complete message in the buffer, leaving any partial tail
            // for the next chunk.
            loop {
                let Some(sep) = find_sse_separator(&buffer) else {
                    break;
                };
                let message_bytes = buffer.drain(..sep).collect::<Vec<u8>>();
                // Drain the separator itself.
                buffer.drain(..separator_len(&buffer));

                let Ok(message) = std::str::from_utf8(&message_bytes) else {
                    continue;
                };
                for line in message.lines() {
                    if let Some(data) = line.strip_prefix("data:") {
                        let data = data.trim();
                        if data.is_empty() {
                            continue;
                        }
                        if let Ok(event) = serde_json::from_str::<NomiEvent>(data) {
                            let _ = app.emit("nomi-event", event);
                        }
                    }
                    // Comment lines (": ping") and other fields are ignored.
                }
            }
        }
    });

    Ok(())
}

/// Scans the buffer for an SSE message terminator (\n\n or \r\n\r\n) and
/// returns the index where the terminator begins. Returns None if no complete
/// message is buffered yet.
fn find_sse_separator(buf: &[u8]) -> Option<usize> {
    for i in 0..buf.len() {
        if buf[i..].starts_with(b"\n\n") {
            return Some(i);
        }
        if buf[i..].starts_with(b"\r\n\r\n") {
            return Some(i);
        }
    }
    None
}

/// Returns the byte length of the terminator at the start of `buf`. Used after
/// `find_sse_separator` to drain exactly the right amount.
fn separator_len(buf: &[u8]) -> usize {
    if buf.starts_with(b"\r\n\r\n") {
        4
    } else if buf.starts_with(b"\n\n") {
        2
    } else {
        0
    }
}

#[tauri::command]
async fn stop_event_stream(state: State<'_, Mutex<EventStreamState>>) -> Result<(), String> {
    let guard = state.lock().await;
    // Bumping the generation causes the in-flight task (if any) to exit on its
    // next check.
    guard.generation.fetch_add(1, Ordering::SeqCst);
    Ok(())
}

#[tauri::command]
async fn show_window(app: AppHandle) -> Result<(), String> {
    if let Some(window) = app.get_webview_window("main") {
        let _ = window.show();
        let _ = window.set_focus();
    }
    Ok(())
}

#[tauri::command]
async fn hide_window(app: AppHandle) -> Result<(), String> {
    if let Some(window) = app.get_webview_window("main") {
        let _ = window.hide();
    }
    Ok(())
}

/// Update the tray's "Approvals" menu item label so it reflects the current
/// pending-approval count. React-side calls this on startup (initial fetch)
/// and whenever an `approval.requested` / `approval.resolved` event fires.
#[tauri::command]
async fn set_approvals_badge(
    menu: State<'_, tauri::menu::Menu<tauri::Wry>>,
    count: u32,
) -> Result<(), String> {
    let item = menu
        .get("approvals")
        .ok_or_else(|| "approvals menu item missing".to_string())?;
    let label = if count == 0 {
        "Approvals".to_string()
    } else {
        format!("Approvals ({})", count)
    };
    if let tauri::menu::MenuItemKind::MenuItem(mi) = item {
        mi.set_text(&label).map_err(|e| e.to_string())?;
    }
    Ok(())
}

// Tray icon variants embedded at compile time so we don't depend on
// runtime resource resolution. Generated by hand from the icon source;
// each is a 16x16 monochrome PNG with a small dot overlay for the
// non-idle states. macOS treats them as template images (see the icon
// builder for the as_template flag) so the system tints them to match
// dark/light menu bars.
const TRAY_ICON_IDLE: &[u8] = include_bytes!("../icons/tray-idle.png");
const TRAY_ICON_ACTIVE: &[u8] = include_bytes!("../icons/tray-active.png");
const TRAY_ICON_AWAITING: &[u8] = include_bytes!("../icons/tray-awaiting.png");

fn tray_icon_for(state: &str) -> &'static [u8] {
    match state {
        "active" => TRAY_ICON_ACTIVE,
        "awaiting" => TRAY_ICON_AWAITING,
        _ => TRAY_ICON_IDLE,
    }
}

// decode_tray_png turns an embedded PNG byte slice into the RGBA buffer
// + dimensions tauri::image::Image::new expects. Reused at startup (idle
// icon) and at runtime (set_tray_state). Returns an owned Vec because
// Image::new_owned is the only constructor that doesn't borrow from
// caller-owned memory in the 2.10 API.
fn decode_tray_png(bytes: &[u8]) -> Result<tauri::image::Image<'static>, String> {
    let decoder = png::Decoder::new(bytes);
    let mut reader = decoder.read_info().map_err(|e| e.to_string())?;
    let mut buf = vec![0u8; reader.output_buffer_size()];
    let info = reader.next_frame(&mut buf).map_err(|e| e.to_string())?;
    buf.truncate(info.buffer_size());
    // The PIL-generated tray PNGs are RGBA at 8 bpc, which matches
    // tauri's expected layout. If a future asset is in another format,
    // this will surface as a panic in the image::Image constructor;
    // catch it explicitly so packagers see a useful error rather than a
    // mid-startup abort.
    if info.color_type != png::ColorType::Rgba || info.bit_depth != png::BitDepth::Eight {
        return Err(format!(
            "tray icon must be 8-bpc RGBA, got {:?}/{:?}",
            info.color_type, info.bit_depth
        ));
    }
    Ok(tauri::image::Image::new_owned(buf, info.width, info.height))
}

/// Update the tray icon's status indicator. Three states map to three
/// icon variants:
///   "idle"     — neutral N glyph
///   "active"   — N + blue dot (at least one executing run)
///   "awaiting" — N + amber dot (at least one pending approval; wins over active)
///
/// macOS also gets a short title-text fallback (• / !) so users with
/// "Show Symbols Only" off still see something next to the icon.
#[tauri::command]
async fn set_tray_state(app: AppHandle, state: String) -> Result<(), String> {
    let tray = app
        .tray_by_id("main-tray")
        .ok_or_else(|| "tray 'main-tray' not found".to_string())?;
    let icon = decode_tray_png(tray_icon_for(&state))
        .map_err(|e| format!("decode tray icon: {e}"))?;
    tray.set_icon(Some(icon)).map_err(|e| e.to_string())?;
    let title: Option<&str> = match state.as_str() {
        "active" => Some("•"),
        "awaiting" => Some("!"),
        _ => None,
    };
    tray.set_title(title).map_err(|e| e.to_string())?;
    Ok(())
}

#[tauri::command]
async fn check_ollama_reachable() -> Result<bool, String> {
    let client = reqwest::Client::new();
    match client.get("http://127.0.0.1:11434/api/tags").send().await {
        Ok(response) => Ok(response.status().is_success()),
        Err(_) => Ok(false),
    }
}

/// Build metadata for the desktop app shell. Wire shape matches the
/// Go daemon's GET /version response so a single TypeScript type can
/// describe both. Populated at compile time via build.rs (git commit
/// + UTC build date) and Cargo (package version).
#[derive(Serialize)]
struct AppVersion {
    version: String,
    commit: String,
    build_date: String,
}

#[tauri::command]
fn app_version() -> AppVersion {
    AppVersion {
        version: env!("CARGO_PKG_VERSION").to_string(),
        commit: env!("NOMI_COMMIT").to_string(),
        build_date: env!("NOMI_BUILD_DATE").to_string(),
    }
}

#[tauri::command]
async fn pick_workspace_folder(initial_path: Option<String>) -> Result<Option<String>, String> {
    let mut dialog = rfd::FileDialog::new();
    if let Some(path) = initial_path {
        dialog = dialog.set_directory(path);
    }
    let selected = dialog.pick_folder();
    Ok(selected.map(|p| p.display().to_string()))
}

/// Resolve the path to the nomid sidecar binary. In production bundles the
/// binary sits next to the main executable (Tauri externalBin convention). In
/// dev builds we fall back to the project root `bin/nomid`.
fn resolve_nomid_binary() -> Option<PathBuf> {
    // 1. Production / bundled: same directory as the running executable.
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            let sidecar = dir.join("nomid");
            if sidecar.exists() {
                return Some(sidecar);
            }
            // Windows bundled binary
            #[cfg(target_os = "windows")]
            {
                let sidecar_exe = dir.join("nomid.exe");
                if sidecar_exe.exists() {
                    return Some(sidecar_exe);
                }
            }
        }
    }

    // 2. Dev fallback: project root bin/nomid (relative to app/src-tauri/target/{debug,release}).
    let dev_fallback = PathBuf::from(concat!(env!("CARGO_MANIFEST_DIR"), "/../../bin/nomid"));
    if dev_fallback.exists() {
        return Some(dev_fallback);
    }

    // 3. Last resort: hope it's on PATH.
    Some(PathBuf::from("nomid"))
}

/// Spawn the nomid sidecar and wait for it to write the api.endpoint file.
/// Returns an error only if the binary is missing or the process fails to
/// start; slow startup is handled by the caller polling `read_api_endpoint()`.
fn spawn_nomid_sidecar() -> Result<Child, String> {
    let bin = resolve_nomid_binary()
        .ok_or_else(|| "nomid binary not found".to_string())?;

    let child = Command::new(&bin)
        .spawn()
        .map_err(|e| format!("failed to spawn nomid at {}: {}", bin.display(), e))?;

    Ok(child)
}

/// Blocking wait (with timeout) for nomid to publish its api.endpoint file.
fn wait_for_nomid_ready(timeout_secs: u64) -> bool {
    let Some(dir) = nomi_data_dir() else {
        return false;
    };
    let path = dir.join("api.endpoint");
    for _ in 0..timeout_secs * 2 {
        if path.exists() {
            return true;
        }
        std::thread::sleep(std::time::Duration::from_millis(500));
    }
    false
}

/// Check whether an existing nomid process is actually alive by pinging its
/// /health endpoint. A stale api.endpoint file from a previous session will
/// fail this probe, causing us to spawn a fresh sidecar.
async fn probe_nomid_alive() -> bool {
    let url = read_api_endpoint();
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(2))
        .build()
        .unwrap_or_else(|_| reqwest::Client::new());
    match client.get(format!("{}/health", url)).send().await {
        Ok(resp) => resp.status().is_success(),
        Err(_) => false,
    }
}

fn main() {
    let builder = tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_updater::Builder::new().build())
        .plugin(tauri_plugin_notification::init())
        .manage(Mutex::new(EventStreamState::default()))
        .manage(NomiDaemonState {
            child: Mutex::new(None),
        })
        .invoke_handler(tauri::generate_handler![
            get_auth_token,
            get_api_endpoint,
            start_event_stream,
            stop_event_stream,
            show_window,
            hide_window,
            check_ollama_reachable,
            pick_workspace_folder,
            app_version,
            set_approvals_badge,
            set_tray_state
        ])
        .setup(|app| {
            // macOS daemon-app convention: clicking the red traffic-light
            // hides the window instead of terminating. The tray menu's "Quit"
            // remains the canonical exit; New Chat / Open Nomi / Settings
            // already call show()+set_focus() to bring the window back.
            #[cfg(target_os = "macos")]
            if let Some(window) = app.get_webview_window("main") {
                let w = window.clone();
                window.on_window_event(move |event| {
                    if let tauri::WindowEvent::CloseRequested { api, .. } = event {
                        api.prevent_close();
                        let _ = w.hide();
                    }
                });
            }

            let menu = tauri::menu::MenuBuilder::new(app)
                .item(&tauri::menu::MenuItemBuilder::new("New Chat")
                    .id("new-chat")
                    .build(app)?)
                .separator()
                .item(&tauri::menu::MenuItemBuilder::new("Open Nomi")
                    .id("open-nomi")
                    .build(app)?)
                .item(&tauri::menu::MenuItemBuilder::new("Pause All Agents")
                    .id("pause-agents")
                    .build(app)?)
                .item(&tauri::menu::MenuItemBuilder::new("Approvals")
                    .id("approvals")
                    .build(app)?)
                .separator()
                .item(&tauri::menu::MenuItemBuilder::new("Settings")
                    .id("settings")
                    .build(app)?)
                .separator()
                .item(&tauri::menu::MenuItemBuilder::new("Quit")
                    .id("quit")
                    .build(app)?)
                .build()?;

            // Build the tray with or without an icon. Previously this unwrap()
            // panicked in headless test environments where the bundled icon
            // isn't loaded.
            let mut tray = tauri::tray::TrayIconBuilder::with_id("main-tray")
                .menu(&menu)
                .show_menu_on_left_click(true)
                .on_menu_event(|app, event| match event.id.as_ref() {
                    "new-chat" => {
                        let _ = app.emit("tray-menu-clicked", "new-chat");
                        if let Some(window) = app.get_webview_window("main") {
                            let _ = window.show();
                            let _ = window.set_focus();
                        }
                    }
                    "open-nomi" => {
                        if let Some(window) = app.get_webview_window("main") {
                            let _ = window.show();
                            let _ = window.set_focus();
                        }
                    }
                    "pause-agents" => {
                        let _ = app.emit("tray-menu-clicked", "pause-agents");
                    }
                    "approvals" => {
                        let _ = app.emit("tray-menu-clicked", "approvals");
                        if let Some(window) = app.get_webview_window("main") {
                            let _ = window.show();
                            let _ = window.set_focus();
                        }
                    }
                    "settings" => {
                        let _ = app.emit("tray-menu-clicked", "settings");
                        if let Some(window) = app.get_webview_window("main") {
                            let _ = window.show();
                            let _ = window.set_focus();
                        }
                    }
                    "quit" => {
                        // Kill the sidecar before exiting so we don't leave a
                        // ghost nomid process behind.
                        tauri::async_runtime::block_on(async {
                            if let Some(state) = app.try_state::<NomiDaemonState>() {
                                let mut guard = state.child.lock().await;
                                if let Some(mut child) = guard.take() {
                                    let _ = child.kill();
                                }
                            }
                        });
                        app.exit(0);
                    }
                    _ => {}
                })
                .on_tray_icon_event(|tray, event| {
                    if let tauri::tray::TrayIconEvent::DoubleClick { .. } = event {
                        let app = tray.app_handle();
                        if let Some(window) = app.get_webview_window("main") {
                            let _ = window.show();
                            let _ = window.set_focus();
                        }
                    }
                });

            // Use the dedicated monochrome tray glyph instead of the
            // colourful app icon; menu-bar tinting on macOS only works
            // when the image is shaped white-on-transparent and marked
            // as a template. Falls back to the app icon if the embedded
            // PNG fails to decode (defensive — bytes are baked in at
            // compile time so this branch is unreachable in practice).
            match decode_tray_png(TRAY_ICON_IDLE) {
                Ok(icon) => {
                    tray = tray.icon(icon).icon_as_template(true);
                }
                Err(_) => {
                    if let Some(icon) = app.default_window_icon() {
                        tray = tray.icon(icon.clone());
                    }
                }
            }

            let tray = tray.build(app)?;
            app.manage(tray);
            // Stash the Menu so set_approvals_badge can rewrite the
            // Approvals item's label without rebuilding the whole menu.
            app.manage(menu);

            // Spawn the nomid sidecar if it's not already running (e.g. dev
            // workflow where the user started it manually).
            let app_handle = app.handle().clone();
            tauri::async_runtime::spawn(async move {
                // A stale api.endpoint file from a previous session would fool a
                // pure file-existence check. Probe the /health endpoint to
                // confirm the backend is actually alive before skipping spawn.
                let already_up = probe_nomid_alive().await;
                if already_up {
                    return;
                }

                match spawn_nomid_sidecar() {
                    Ok(child) => {
                        if let Some(state) = app_handle.try_state::<NomiDaemonState>() {
                            let mut guard = state.child.lock().await;
                            *guard = Some(child);
                        }
                        // Wait up to 30s for nomid to boot and write its endpoint.
                        let ready = wait_for_nomid_ready(30);
                        if !ready {
                            eprintln!("[nomi-app] warning: nomid sidecar did not publish api.endpoint within 30s");
                        }
                    }
                    Err(e) => {
                        eprintln!("[nomi-app] error: failed to start nomid sidecar: {}", e);
                    }
                }
            });

            Ok(())
        });

    // Build the app so we can attach a RunEvent handler for cleanup on exit.
    let app = builder.build(tauri::generate_context!())
        .expect("error while building tauri application");

    app.run(|_app_handle, event| {
        if let tauri::RunEvent::ExitRequested { .. } = event {
            tauri::async_runtime::block_on(async {
                if let Some(state) = _app_handle.try_state::<NomiDaemonState>() {
                    let mut guard = state.child.lock().await;
                    if let Some(mut child) = guard.take() {
                        let _ = child.kill();
                    }
                }
            });
        }
    });
}
