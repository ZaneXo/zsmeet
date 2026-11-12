//! zsmeet 桌面版的 Rust 部分：本地代理和离线缓存、登录、钥匙串、菜单栏、原生录音。
//! 界面是 `web/` 按桌面版模式构建出来的同一套 React 代码（见 docs/desktop.md）。
//!
//! 原生录音（recorder/）：管着 Swift 写的录音程序 zsmeet-recorder，录好的分片存在本地、在后台传到服务器；
//! 菜单栏（tray.rs）上有录音的时长和控制。关掉窗口录音不停；退出 App 时先结束录音，没传完的下次打开接着传。

mod cache;
mod commands;
mod core;
mod export;
mod proxy;
mod recorder;
mod server;
mod token;
mod tray;

#[cfg(test)]
mod tests;

use std::sync::{Arc, Mutex};

use tauri::{AppHandle, Emitter, Manager, RunEvent, WindowEvent};
use tauri_plugin_deep_link::DeepLinkExt;
use tauri_plugin_opener::OpenerExt;

use crate::core::{AppState, Core, Events, Options};
use crate::recorder::{Recorder, RecorderEvents, RecorderOptions, RecorderState};

/// 发给界面的事件：状态变了；浏览器登录回来以后没能完成登录；录音的状态变了（录音时每 100 毫秒左右还有音量）。
pub const EVENT_STATE: &str = "zsmeet://state";
pub const EVENT_LOGIN_ERROR: &str = "zsmeet://login-error";
pub const EVENT_RECORDER: &str = "recorder://state";

/// App 界面的来源。macOS 上 Tauri 2 用 `tauri://localhost`。
const APP_ORIGIN: &str = "tauri://localhost";

struct TauriEvents(AppHandle);

impl Events for TauriEvents {
    fn state(&self, state: &AppState) {
        if let Err(e) = self.0.emit(EVENT_STATE, state) {
            log::warn!("状态事件发送失败：{e}");
        }
    }

    fn login_error(&self, message: &str) {
        let _ = self.0.emit(EVENT_LOGIN_ERROR, message);
    }

    fn open_url(&self, url: &str) -> Result<(), String> {
        self.0
            .opener()
            .open_url(url, None::<&str>)
            .map_err(|e| e.to_string())
    }
}

struct TauriRecorderEvents(AppHandle);

impl RecorderEvents for TauriRecorderEvents {
    fn state(&self, state: &RecorderState) {
        if let Err(e) = self.0.emit(EVENT_RECORDER, state) {
            log::warn!("录音状态事件发送失败：{e}");
        }
        tray::show_recording(&self.0, state);
    }
}

/// 电脑名（「系统设置 → 通用 → 关于本机」里的名称），显示在授权页和账号页的设备列表里。
fn computer_name() -> String {
    std::process::Command::new("scutil")
        .args(["--get", "ComputerName"])
        .output()
        .ok()
        .filter(|o| o.status.success())
        .and_then(|o| String::from_utf8(o.stdout).ok())
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| "Mac".to_string())
}

/// 收到 deep link（zsmeet://auth?…）：把窗口拿到前面，交给 Core 完成登录。
/// 冷启动时同一个链接可能既从 get_current 又从 on_open_url 来一次：一次性的 code 只处理第一次。
fn dispatch_link(app: &AppHandle, link: String) {
    let last = app.state::<LastLink>();
    {
        let mut last = last.0.lock().unwrap();
        if last.as_deref() == Some(link.as_str()) {
            return;
        }
        *last = Some(link.clone());
    }
    tray::show_main(app);
    let core = app.state::<Arc<Core>>().inner().clone();
    tauri::async_runtime::spawn(async move {
        let _ = core.handle_deep_link(&link).await;
    });
}

#[derive(Default)]
struct LastLink(Mutex<Option<String>>);

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    let app = tauri::Builder::default()
        // 第二次打开 App（或者在 Windows/Linux 上点了登录链接）时，交给已经在运行的那个
        .plugin(tauri_plugin_single_instance::init(|app, _argv, _cwd| {
            tray::show_main(app);
        }))
        .plugin(tauri_plugin_deep_link::init())
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_dialog::init())
        .plugin(
            tauri_plugin_window_state::Builder::default()
                // 不记「窗口是否显示」：关窗口只是藏起来，下次启动还是要显示
                .with_state_flags(
                    tauri_plugin_window_state::StateFlags::all()
                        - tauri_plugin_window_state::StateFlags::VISIBLE,
                )
                .build(),
        )
        .manage(LastLink::default())
        .setup(|app| {
            let handle = app.handle().clone();

            // 本地代理：先占好端口，Core 要知道代理的地址
            let listener = std::net::TcpListener::bind(("127.0.0.1", 0))?;
            listener.set_nonblocking(true)?;
            let addr = listener.local_addr()?;

            let mut origins = vec![APP_ORIGIN.to_string()];
            if cfg!(debug_assertions) {
                // 开发时界面由 Vite 提供
                if let Some(dev) = &app.config().build.dev_url {
                    origins.push(dev.origin().ascii_serialization());
                }
            }

            let paths = app.path();
            let core = Arc::new(Core::new(
                Options {
                    proxy: format!("http://{addr}"),
                    secret: server::random_token(32),
                    settings_path: paths.app_config_dir()?.join("settings.json"),
                    cache_root: paths.app_cache_dir()?.join("offline"),
                    device: computer_name(),
                    version: app.package_info().version.to_string(),
                },
                Arc::new(token::KeychainStore::new(&app.config().identifier)),
                Arc::new(TauriEvents(handle.clone())),
                server::http_client(),
            ));
            app.manage(core.clone());

            // 原生录音：读出这台电脑上的录音（上次没传完的接着传），录音放在 App 的数据目录里
            let recorder = Recorder::new(
                RecorderOptions::new(
                    paths.app_data_dir()?.join("recordings"),
                    recorder::locate_helper(),
                ),
                core.clone(),
                Arc::new(TauriRecorderEvents(handle.clone())),
                server::http_client(),
            );
            app.manage(recorder.clone());

            let proxy = proxy::Proxy::new(core, origins, proxy::upstream_client());
            tauri::async_runtime::spawn(async move {
                match tokio::net::TcpListener::from_std(listener) {
                    Ok(listener) => proxy::serve(listener, proxy).await,
                    Err(e) => log::error!("本地代理启动失败：{e}"),
                }
            });

            // 浏览器登录完跳回来的 zsmeet://auth?…
            let links = handle.clone();
            app.deep_link().on_open_url(move |event| {
                for url in event.urls() {
                    dispatch_link(&links, url.to_string());
                }
            });
            if let Ok(Some(urls)) = app.deep_link().get_current() {
                for url in urls {
                    dispatch_link(&handle, url.to_string());
                }
            }

            tray::build(&handle)?;
            // 菜单栏建好以后再开始上传：一开始的状态（有没有没传完的录音）也要显示在菜单上
            tauri::async_runtime::spawn(recorder.run());
            Ok(())
        })
        .on_window_event(|window, event| {
            // 关窗口不退出，App 留在菜单栏里；录音时关掉窗口录音也不停
            if let WindowEvent::CloseRequested { api, .. } = event {
                if window.label() == tray::MAIN_WINDOW {
                    api.prevent_close();
                    let _ = window.hide();
                }
            }
        })
        .invoke_handler(tauri::generate_handler![
            commands::app_state,
            commands::set_server,
            commands::switch_server,
            commands::login_start,
            commands::login_cancel,
            commands::logout,
            commands::session_expired,
            commands::open_external,
            commands::open_server_page,
            commands::save_export,
            commands::recorder_devices,
            commands::recorder_start,
            commands::recorder_pause,
            commands::recorder_resume,
            commands::recorder_mark,
            commands::recorder_stop,
            commands::recorder_finish,
            commands::recorder_discard,
            commands::recorder_state,
        ])
        .build(tauri::generate_context!())
        .expect("zsmeet 启动失败");

    app.run(|app, event| match event {
        // 退出（菜单栏的「退出」、⌘Q、关机）：先结束录音，和点了结束一样等录音程序写完最后一片（它自己最多花
        // 8 秒，这里最多等 10 秒），还不退出就强制结束，这场录音算中断。没传完的下次打开接着传
        RunEvent::Exit => {
            if let Some(recorder) = app.try_state::<Arc<Recorder>>() {
                let recorder = recorder.inner().clone();
                tauri::async_runtime::block_on(recorder.shutdown());
            }
        }
        // 窗口藏起来以后点 Dock 上的图标：重新显示
        #[cfg(target_os = "macos")]
        RunEvent::Reopen { .. } => tray::show_main(app),
        _ => {}
    });
}
