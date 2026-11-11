//! 界面能调用的命令（`invoke`）。具体的事都在 core.rs 里做，这里只做和 Tauri 相关的部分。

use std::sync::Arc;

use tauri::{AppHandle, State};
use tauri_plugin_dialog::{DialogExt, MessageDialogKind};
use tauri_plugin_opener::OpenerExt;

use crate::core::{AppState, Core};
use crate::export;

type CoreState<'a> = State<'a, Arc<Core>>;

/// 当前状态：本地代理的地址和 secret、服务器地址、谁登录了。
#[tauri::command]
pub fn app_state(core: CoreState) -> AppState {
    core.state()
}

/// 填服务器地址：检查 `GET {地址}/api/status`，能用就保存。
#[tauri::command]
pub async fn set_server(core: CoreState<'_>, url: String) -> Result<AppState, String> {
    core.set_server(&url).await
}

/// 换服务器：先退出登录，再回到「填服务器地址」。
#[tauri::command]
pub async fn switch_server(core: CoreState<'_>) -> Result<AppState, String> {
    Ok(core.switch_server().await)
}

/// 「在浏览器里登录」：在系统浏览器里打开授权页。
#[tauri::command]
pub fn login_start(core: CoreState) -> Result<AppState, String> {
    core.login_start()
}

#[tauri::command]
pub fn login_cancel(core: CoreState) -> AppState {
    core.login_cancel()
}

#[tauri::command]
pub async fn logout(core: CoreState<'_>) -> Result<AppState, String> {
    Ok(core.logout().await)
}

/// 界面发现服务器已经不认这次登录了。
#[tauri::command]
pub fn session_expired(core: CoreState) -> AppState {
    core.session_expired()
}

/// 在系统浏览器里打开站外链接。只接受 http、https 和 mailto。
#[tauri::command]
pub fn open_external(app: AppHandle, url: String) -> Result<(), String> {
    let parsed = url::Url::parse(&url).map_err(|_| "链接地址不对".to_string())?;
    if !matches!(parsed.scheme(), "http" | "https" | "mailto") {
        return Err("只能打开网页链接".into());
    }
    app.opener()
        .open_url(parsed.as_str(), None::<&str>)
        .map_err(|e| format!("没能打开浏览器（{e}）"))
}

/// 在系统浏览器里打开服务器上的一个页面，比如管理员设置（`/settings`）。
#[tauri::command]
pub fn open_server_page(app: AppHandle, core: CoreState, path: String) -> Result<(), String> {
    if !path.starts_with('/') || path.starts_with("//") {
        return Err("页面地址不对".into());
    }
    let url = core.server_url(&path).ok_or("还没有设置服务器地址")?;
    app.opener()
        .open_url(url, None::<&str>)
        .map_err(|e| format!("没能打开浏览器（{e}）"))
}

/// 导出：从服务器取回文件，弹出保存对话框，写到选的位置。返回保存的路径；取消了返回 None。
/// 出错时弹一个系统提示框（导出菜单里没有地方显示错误），同时把原因返回给界面。
#[tauri::command]
pub async fn save_export(
    app: AppHandle,
    core: CoreState<'_>,
    path: String,
) -> Result<Option<String>, String> {
    let result = save_export_inner(&app, &core, &path).await;
    if let Err(message) = &result {
        app.dialog()
            .message(message)
            .title("导出没有完成")
            .kind(MessageDialogKind::Error)
            .show(|_| {});
    }
    result
}

async fn save_export_inner(
    app: &AppHandle,
    core: &Arc<Core>,
    path: &str,
) -> Result<Option<String>, String> {
    if !export::valid_path(path) {
        return Err("导出的地址不对".into());
    }
    let upstream = core.upstream().ok_or("还没有设置服务器地址")?;
    let fetched = export::fetch(
        core.client(),
        &upstream.server,
        upstream.token.as_deref(),
        path,
    )
    .await;
    let (name, body) = match fetched {
        Ok(v) => v,
        Err((status, message)) => {
            if status == 401 {
                if let Some(token) = &upstream.token {
                    core.unauthorized(token);
                }
            }
            return Err(message);
        }
    };

    // 保存对话框要等用户选，放到阻塞线程里等
    let dialog = app.dialog().file().set_file_name(&name);
    let chosen = tauri::async_runtime::spawn_blocking(move || dialog.blocking_save_file())
        .await
        .map_err(|e| e.to_string())?;
    let Some(chosen) = chosen else {
        return Ok(None);
    };
    let target = chosen
        .into_path()
        .map_err(|_| "选的位置不能保存文件，请换一个位置".to_string())?;
    tokio::fs::write(&target, body)
        .await
        .map_err(|e| format!("文件没能保存到 {}（{e}），请换一个位置", target.display()))?;
    Ok(Some(target.display().to_string()))
}
