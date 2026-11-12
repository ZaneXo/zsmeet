//! 界面能调用的命令（`invoke`）。具体的事都在 core.rs、recorder/、update.rs 里做，这里只做和 Tauri 相关的部分。

use std::sync::Arc;

use tauri::{AppHandle, State};
use tauri_plugin_dialog::{DialogExt, MessageDialogKind};
use tauri_plugin_opener::OpenerExt;

use crate::core::{AppState, Core};
use crate::export;
use crate::recorder::{Devices, EchoCancel, Recorder, RecorderState};
use crate::update::{self, CheckResult, Updates};

type CoreState<'a> = State<'a, Arc<Core>>;
type RecorderHandle<'a> = State<'a, Arc<Recorder>>;

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

/// 换服务器：先退出登录，再回到「填服务器地址」。正在录的先结束（见 logout）。
#[tauri::command]
pub async fn switch_server(
    core: CoreState<'_>,
    recorder: RecorderHandle<'_>,
) -> Result<AppState, String> {
    recorder.stop().await?;
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

/// 退出登录。正在录的先结束：录好的留在这台电脑上，这个账号下次登录时接着传，别的账号看不到、也不会用别人的令牌传。
#[tauri::command]
pub async fn logout(core: CoreState<'_>, recorder: RecorderHandle<'_>) -> Result<AppState, String> {
    recorder.stop().await?;
    Ok(core.logout().await)
}

/// 界面发现服务器已经不认这次登录了。
#[tauri::command]
pub fn session_expired(core: CoreState) -> AppState {
    core.session_expired()
}

/// 在系统浏览器里打开站外链接（纪要里的链接，邮件地址用邮件程序打开）。只接受 http、https 和 mailto。
/// 打不开时弹一个系统提示框（点的是页面里的链接，旁边没有地方显示错误），同时把原因返回给界面。
#[tauri::command]
pub fn open_external(app: AppHandle, url: String) -> Result<(), String> {
    let result = open_link(&app, &url);
    if let Err(message) = &result {
        show_error(&app, "没能打开链接", message);
    }
    result
}

fn open_link(app: &AppHandle, url: &str) -> Result<(), String> {
    let parsed = url::Url::parse(url).map_err(|_| "链接地址不对".to_string())?;
    if !matches!(parsed.scheme(), "http" | "https" | "mailto") {
        return Err("只能打开网页链接".into());
    }
    let opener = if parsed.scheme() == "mailto" {
        "邮件程序"
    } else {
        "浏览器"
    };
    app.opener()
        .open_url(parsed.as_str(), None::<&str>)
        .map_err(|e| format!("没能打开这个链接（{e}），请确认这台电脑上设置了默认的{opener}"))
}

/// 弹一个系统提示框说明出了什么错。界面上没有地方显示错误的操作（导出、点链接）用它。
fn show_error(app: &AppHandle, title: &str, message: &str) {
    app.dialog()
        .message(message)
        .title(title)
        .kind(MessageDialogKind::Error)
        .show(|_| {});
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
        show_error(&app, "导出没有完成", message);
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
    // 选的是已有的文件时，写好新文件才换掉它：写到一半出错，原来的文件还在
    let path = target.clone();
    tauri::async_runtime::spawn_blocking(move || export::save_replacing(&path, &body))
        .await
        .map_err(|e| e.to_string())?
        .map_err(|e| format!("文件没能保存到 {}（{e}），请换一个位置", target.display()))?;
    Ok(Some(target.display().to_string()))
}

// ---- 录音（recorder/） ----

/// 这台电脑上的麦克风。
#[tauri::command]
pub async fn recorder_devices(recorder: RecorderHandle<'_>) -> Result<Devices, String> {
    recorder.devices().await
}

/// 开始录音（会议要先在服务器上建好，source 是 desktop）。mic：麦克风的 uid 或者 "default"，
/// null 是不录麦克风；system：录不录电脑的声音；echoCancel：「外放时去掉回声」（实验性，true 是
/// --echo-cancel auto，false 或者不传是 off）。这场会议在这台电脑上有中断了的录音时接着录。
#[tauri::command]
pub async fn recorder_start(
    recorder: RecorderHandle<'_>,
    meeting_id: String,
    mic: Option<String>,
    system: bool,
    echo_cancel: Option<bool>,
) -> Result<RecorderState, String> {
    let echo_cancel = EchoCancel::from_switch(echo_cancel.unwrap_or(false));
    recorder.start(&meeting_id, mic, system, echo_cancel).await
}

#[tauri::command]
pub async fn recorder_pause(recorder: RecorderHandle<'_>) -> Result<RecorderState, String> {
    recorder.pause().await
}

#[tauri::command]
pub async fn recorder_resume(recorder: RecorderHandle<'_>) -> Result<RecorderState, String> {
    recorder.resume().await
}

/// 标记重点：记下现在录到第几秒。
#[tauri::command]
pub fn recorder_mark(recorder: RecorderHandle) -> Result<RecorderState, String> {
    recorder.mark()
}

/// 结束录音。录好的在后台传完，再告诉服务器开始处理。
#[tauri::command]
pub async fn recorder_stop(recorder: RecorderHandle<'_>) -> Result<RecorderState, String> {
    recorder.stop().await
}

/// 中断了的录音不接着录了：把录好的传上去，告诉服务器开始处理。
#[tauri::command]
pub async fn recorder_finish(
    recorder: RecorderHandle<'_>,
    meeting_id: String,
) -> Result<RecorderState, String> {
    recorder.finish(&meeting_id).await
}

/// 删掉这台电脑上一段中断了的、传不上去的录音。
#[tauri::command]
pub fn recorder_discard(
    recorder: RecorderHandle,
    meeting_id: String,
) -> Result<RecorderState, String> {
    recorder.discard(&meeting_id)
}

#[tauri::command]
pub fn recorder_state(recorder: RecorderHandle) -> RecorderState {
    recorder.state()
}

// ---- 自动更新（update.rs） ----

/// 现在的版本，GitHub Releases 上有没有新版本（界面上的「检查更新」）。
#[tauri::command]
pub async fn update_check(app: AppHandle) -> Result<CheckResult, String> {
    update::check_now(&app).await
}

/// 装新版本：下载（事件 update://progress 报进度）、校验签名、换掉 zsmeet.app、重新打开。
/// 正在录音、还有录音没传完时不装，返回原因。
#[tauri::command]
pub async fn update_install(
    app: AppHandle,
    updates: State<'_, Updates>,
    recorder: RecorderHandle<'_>,
) -> Result<(), String> {
    update::install_and_relaunch(&app, &updates, &recorder).await
}
