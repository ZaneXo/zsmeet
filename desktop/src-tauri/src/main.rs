// 正式版不弹出命令行窗口（只对 Windows 有意义，保留 Tauri 的惯例）
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

fn main() {
    zsmeet_desktop::run()
}
