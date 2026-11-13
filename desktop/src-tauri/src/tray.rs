//! 菜单栏图标：「打开 zsmeet」「退出」；录音时图标旁边显示录了多久（每秒更新），菜单里多出
//! 暂停/继续、标记重点、结束录音。录音的状态一变（开始、暂停、结束、上传的情况）菜单就重建。

use std::sync::{Arc, Mutex};

use tauri::image::Image;
use tauri::menu::{Menu, MenuItem, PredefinedMenuItem};
use tauri::tray::TrayIconBuilder;
use tauri::{AppHandle, Manager, Runtime};

use crate::recorder::{Phase, Recorder, RecorderState, UploadStatus};

pub const MAIN_WINDOW: &str = "main";
const TRAY: &str = "main";

/// 菜单里的录音部分。这些变了才重建菜单（时长每秒都变，只改图标旁边的字）。
#[derive(Clone, Debug, PartialEq, Eq)]
struct Shape {
    phase: Phase,
    /// 正在录的会议的标题
    title: String,
    /// 还没传完的录音有几段
    uploading: usize,
    /// 中断了、传不上去的录音有几段：要打开 zsmeet 处理
    attention: usize,
}

impl Shape {
    fn idle() -> Self {
        Self {
            phase: Phase::Idle,
            title: String::new(),
            uploading: 0,
            attention: 0,
        }
    }

    fn of(state: &RecorderState) -> Self {
        let count =
            |f: fn(UploadStatus) -> bool| state.uploads.iter().filter(|u| f(u.status)).count();
        Self {
            phase: state.status,
            title: state.title.clone().unwrap_or_default(),
            uploading: count(|s| {
                matches!(
                    s,
                    UploadStatus::Uploading | UploadStatus::Waiting | UploadStatus::Completing
                )
            }),
            attention: count(|s| matches!(s, UploadStatus::Interrupted | UploadStatus::Failed)),
        }
    }
}

/// 菜单栏上现在显示的样子：图标旁边的字、菜单。只在主线程上改。
#[derive(Default)]
struct Shown(Mutex<(Option<String>, Option<Shape>)>);

/// 把主窗口显示出来并放到最前面（关窗口只是把它藏起来了）。
pub fn show_main<R: Runtime>(app: &AppHandle<R>) {
    if let Some(window) = app.get_webview_window(MAIN_WINDOW) {
        let _ = window.unminimize();
        let _ = window.show();
        let _ = window.set_focus();
    }
}

pub fn build<R: Runtime>(app: &AppHandle<R>) -> tauri::Result<()> {
    app.manage(Shown::default());
    let menu = menu(app, &Shape::idle())?;
    TrayIconBuilder::with_id(TRAY)
        .icon(Image::from_bytes(include_bytes!("../icons/tray.png"))?)
        // 模板图标：系统按菜单栏的深浅色自动着色
        .icon_as_template(true)
        .tooltip("zsmeet")
        .menu(&menu)
        .show_menu_on_left_click(true)
        .on_menu_event(|app, event| match event.id().as_ref() {
            "open" | "attention" => show_main(app),
            "quit" => app.exit(0),
            id @ ("pause" | "resume" | "mark" | "stop") => recording_action(app, id),
            _ => {}
        })
        .build(app)?;
    Ok(())
}

/// 菜单里的录音控制。录音程序要一会儿才回话，放到后台去等，不卡住菜单。
fn recording_action<R: Runtime>(app: &AppHandle<R>, id: &str) {
    let Some(recorder) = app.try_state::<Arc<Recorder>>() else {
        return;
    };
    let recorder = recorder.inner().clone();
    let id = id.to_string();
    tauri::async_runtime::spawn(async move {
        let result = match id.as_str() {
            "pause" => recorder.pause().await,
            "resume" => recorder.resume().await,
            "mark" => recorder.mark(),
            _ => recorder.stop().await,
        };
        if let Err(e) = result {
            log::warn!("菜单栏上的录音操作没有完成：{e}");
        }
    });
}

/// 录音状态变了：更新图标旁边的时长和菜单。哪个线程都能调用；真正的更新按调用的顺序在主线程上做，
/// 这里不等主线程（主线程可能正在菜单的处理里等录音这边）。
pub fn show_recording<R: Runtime>(app: &AppHandle<R>, state: &RecorderState) {
    let title = title(state);
    let shape = Shape::of(state);
    let handle = app.clone();
    let _ = app.run_on_main_thread(move || apply(&handle, title, shape));
}

fn apply<R: Runtime>(app: &AppHandle<R>, title: Option<String>, shape: Shape) {
    let (Some(tray), Some(shown)) = (app.tray_by_id(TRAY), app.try_state::<Shown>()) else {
        return;
    };
    let mut shown = shown.0.lock().unwrap();
    if shown.0 != title {
        if let Err(e) = tray.set_title(title.as_deref()) {
            log::warn!("菜单栏的录音时长没能更新：{e}");
        }
        shown.0 = title;
    }
    if shown.1.as_ref() != Some(&shape) {
        match menu(app, &shape).and_then(|m| tray.set_menu(Some(m))) {
            Ok(()) => shown.1 = Some(shape),
            Err(e) => log::warn!("菜单栏的菜单没能更新：{e}"),
        }
    }
}

/// 图标旁边的字：录音时是录了多久，暂停时加上「已暂停」，没在录时什么也不显示。
fn title(state: &RecorderState) -> Option<String> {
    match state.status {
        Phase::Recording | Phase::Stopping => Some(elapsed(state.seconds)),
        Phase::Paused => Some(format!("{} 已暂停", elapsed(state.seconds))),
        Phase::Idle | Phase::Starting => None,
    }
}

/// 录了多久：「4:05」，超过一小时是「1:02:05」。
fn elapsed(seconds: f64) -> String {
    let total = seconds.max(0.0) as u64;
    let (h, m, s) = (total / 3600, total / 60 % 60, total % 60);
    if h > 0 {
        format!("{h}:{m:02}:{s:02}")
    } else {
        format!("{m}:{s:02}")
    }
}

/// 菜单里显示的会议标题：太长的截短。
fn short_title(title: &str) -> String {
    const MAX: usize = 20;
    if title.chars().count() > MAX {
        format!("{}…", title.chars().take(MAX).collect::<String>())
    } else {
        title.to_string()
    }
}

fn menu<R: Runtime>(app: &AppHandle<R>, shape: &Shape) -> tauri::Result<Menu<R>> {
    let menu = Menu::new(app)?;
    let info = |id: &str, text: String| MenuItem::with_id(app, id, text, false, None::<&str>);
    let action = |id: &str, text: &str| MenuItem::with_id(app, id, text, true, None::<&str>);
    let status = match shape.phase {
        Phase::Recording => Some("正在录音"),
        Phase::Paused => Some("录音已暂停"),
        Phase::Starting => Some("正在开始录音…"),
        Phase::Stopping => Some("正在结束录音…"),
        Phase::Idle => None,
    };
    if let Some(status) = status {
        let text = match short_title(&shape.title) {
            t if t.is_empty() => status.to_string(),
            t => format!("{status}：{t}"),
        };
        menu.append(&info("status", text)?)?;
    }
    match shape.phase {
        Phase::Recording => menu.append(&action("pause", "暂停")?)?,
        Phase::Paused => menu.append(&action("resume", "继续")?)?,
        _ => {}
    }
    if matches!(shape.phase, Phase::Recording | Phase::Paused) {
        menu.append(&action("mark", "标记重点")?)?;
        menu.append(&action("stop", "结束录音")?)?;
    }
    if status.is_some() {
        menu.append(&PredefinedMenuItem::separator(app)?)?;
    }
    if shape.uploading > 0 {
        menu.append(&info(
            "uploading",
            format!("还有 {} 段录音没传完", shape.uploading),
        )?)?;
    }
    if shape.attention > 0 {
        menu.append(&action(
            "attention",
            &format!("有 {} 段录音需要处理…", shape.attention),
        )?)?;
    }
    if shape.uploading > 0 || shape.attention > 0 {
        menu.append(&PredefinedMenuItem::separator(app)?)?;
    }
    menu.append(&action("open", "打开 zsmeet")?)?;
    menu.append(&PredefinedMenuItem::separator(app)?)?;
    menu.append(&action("quit", "退出")?)?;
    Ok(menu)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::recorder::Upload;

    fn state(status: Phase, seconds: f64) -> RecorderState {
        RecorderState {
            status,
            meeting_id: Some("m1".into()),
            title: Some("周会".into()),
            seconds,
            level: None,
            mic: None,
            system: true,
            echo: None,
            marks: vec![],
            warnings: vec![],
            error: None,
            uploads: vec![],
        }
    }

    #[test]
    fn shows_elapsed_time() {
        assert_eq!(elapsed(0.0), "0:00");
        assert_eq!(elapsed(65.9), "1:05");
        assert_eq!(elapsed(3725.0), "1:02:05");
        assert_eq!(elapsed(-1.0), "0:00");
        assert_eq!(
            title(&state(Phase::Recording, 61.0)).as_deref(),
            Some("1:01")
        );
        assert_eq!(
            title(&state(Phase::Paused, 61.0)).as_deref(),
            Some("1:01 已暂停")
        );
        assert_eq!(title(&state(Phase::Idle, 0.0)), None);
        assert_eq!(title(&state(Phase::Starting, 0.0)), None);
    }

    #[test]
    fn menu_changes_only_with_the_recording() {
        let a = Shape::of(&state(Phase::Recording, 1.0));
        // 时长变了不重建菜单
        assert_eq!(a, Shape::of(&state(Phase::Recording, 99.0)));
        assert_ne!(a, Shape::of(&state(Phase::Paused, 99.0)));

        let mut s = state(Phase::Idle, 0.0);
        let upload = |status| Upload {
            meeting_id: "x".into(),
            title: "x".into(),
            recorded: 3,
            uploaded: 1,
            status,
            error: None,
        };
        s.uploads = vec![
            upload(UploadStatus::Waiting),
            upload(UploadStatus::Uploading),
            upload(UploadStatus::Interrupted),
            upload(UploadStatus::Completed),
        ];
        let shape = Shape::of(&s);
        assert_eq!((shape.uploading, shape.attention), (2, 1));
        assert_eq!(short_title("周会"), "周会");
        assert_eq!(
            short_title("一二三四五六七八九十一二三四五六七八九十多出来的"),
            "一二三四五六七八九十一二三四五六七八九十…"
        );
    }
}
