//! 桌面版（Rust 侧：菜单栏、系统提示框、本地代理、服务器检查、录音与自动更新）的中英文切换。
//!
//! 界面启动和切换语言时通过 `set_language` 命令把 `"zh"` 或 `"en"` 同步过来；
//! 界面还没加载完（比如刚启动建菜单栏）时，按系统语言推断一次。测试环境下固定默认中文，不碰宿主机的语言设置。

use std::sync::atomic::{AtomicU8, Ordering};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Lang {
    Zh,
    En,
}

const UNSET: u8 = 0;
const ZH: u8 = 1;
const EN: u8 = 2;

static CURRENT: AtomicU8 = AtomicU8::new(UNSET);

pub fn current() -> Lang {
    match CURRENT.load(Ordering::Relaxed) {
        ZH => Lang::Zh,
        EN => Lang::En,
        _ => {
            let detected = detect_initial();
            let raw = match detected {
                Lang::Zh => ZH,
                Lang::En => EN,
            };
            let _ = CURRENT.compare_exchange(UNSET, raw, Ordering::Relaxed, Ordering::Relaxed);
            match CURRENT.load(Ordering::Relaxed) {
                EN => Lang::En,
                _ => Lang::Zh,
            }
        }
    }
}

pub fn set_from_str(lang: &str) -> Lang {
    let parsed = parse_tag(lang).unwrap_or(Lang::Zh);
    CURRENT.store(
        match parsed {
            Lang::Zh => ZH,
            Lang::En => EN,
        },
        Ordering::Relaxed,
    );
    parsed
}

pub fn is_en() -> bool {
    current() == Lang::En
}

pub fn accept_language() -> &'static str {
    match current() {
        Lang::Zh => "zh-CN",
        Lang::En => "en",
    }
}

pub fn tr<'a>(zh: &'a str, en: &'a str) -> &'a str {
    match current() {
        Lang::Zh => zh,
        Lang::En => en,
    }
}

fn parse_tag(raw: &str) -> Option<Lang> {
    let tag = raw.trim().to_ascii_lowercase();
    if tag.is_empty() {
        return None;
    }
    let primary = tag
        .split(['-', '_', '.', '@'])
        .next()
        .unwrap_or(tag.as_str());
    match primary {
        "en" => Some(Lang::En),
        "zh" => Some(Lang::Zh),
        _ => None,
    }
}

#[cfg(test)]
fn detect_initial() -> Lang {
    Lang::Zh
}

#[cfg(not(test))]
fn detect_initial() -> Lang {
    for var in ["LC_ALL", "LANG"] {
        if let Ok(val) = std::env::var(var) {
            if let Some(lang) = parse_tag(&val) {
                return lang;
            }
        }
    }
    #[cfg(target_os = "macos")]
    if let Ok(out) = std::process::Command::new("defaults")
        .args(["read", "-g", "AppleLanguages"])
        .output()
    {
        if out.status.success() {
            let text = String::from_utf8_lossy(&out.stdout);
            for line in text.lines() {
                let cleaned = line.trim().trim_matches([',', '"', '(', ')', ' ']);
                if let Some(lang) = parse_tag(cleaned) {
                    return lang;
                }
            }
        }
    }
    Lang::Zh
}

/// 把 Swift 录音程序（zsmeet-recorder）发来的中文提醒在英文界面下翻成英文。
pub fn translate_helper_warning_for(lang: Lang, code: &str, msg: &str) -> String {
    if lang != Lang::En || msg.is_empty() {
        return msg.to_string();
    }
    if let Some(rest) = msg
        .strip_prefix("没找到选的麦克风（可能已经拔掉了），改用「")
        .and_then(|s| s.strip_suffix("」。"))
    {
        return format!(
            "Selected microphone not found (it may have been unplugged); switched to \"{rest}\"."
        );
    }
    if let Some((name, after)) = msg
        .strip_prefix("麦克风「")
        .and_then(|s| s.split_once("」"))
    {
        if after.starts_with("断开了，后面录不到你这边的声音") {
            return format!(
                "Microphone \"{name}\" disconnected; your voice can no longer be recorded. Reconnect it, stop this recording, and start again."
            );
        }
        if after.starts_with("打不开，录不到你这边的声音") {
            return format!(
                "Cannot open microphone \"{name}\"; your voice cannot be recorded. Try selecting another microphone."
            );
        }
        if let Some(rate) = after
            .strip_prefix("的采样率是 ")
            .and_then(|s| s.split_once("，处理不了"))
            .map(|(r, _)| r)
        {
            return format!(
                "Microphone \"{name}\" uses unsupported sample rate {rate}; your voice cannot be recorded. Change it to 48000 Hz in Audio MIDI Setup or select another microphone, then start again."
            );
        }
        if after.contains("录到的全是静音") || after.contains("几乎录不到声音") {
            return format!(
                "Microphone \"{name}\" has been silent for an extended period. If you have been speaking, another meeting app may be holding exclusive voice processing on the mic, the mic may be muted, or a different mic is selected. Check your mute switch and System Settings → Sound → Input, or switch to another microphone."
            );
        }
    }
    if let Some(name) = msg
        .strip_prefix("处理麦克风「")
        .and_then(|s| s.split_once("」的声音时出错"))
        .map(|(n, _)| n)
    {
        return format!(
            "An audio glitch occurred while processing microphone \"{name}\" (a brief moment was missed), but recording continues. If this happens often, stop and switch to another microphone."
        );
    }
    if let Some(rate) = msg
        .strip_prefix("电脑声音的采样率是 ")
        .and_then(|s| s.split_once("，处理不了"))
        .map(|(r, _)| r)
    {
        return format!(
            "System audio uses unsupported sample rate {rate}; other participants' voices cannot be recorded. Set your speakers or headphones to 48000 Hz in Audio MIDI Setup, then start again."
        );
    }
    match code {
        "mic_denied" => "Microphone permission denied; your voice cannot be recorded. Enable zsmeet in System Settings → Privacy & Security → Microphone, then start recording again.".into(),
        "mic_unavailable" => {
            if msg.contains("没有能用的麦克风") {
                "No usable microphone found on this Mac; your voice cannot be recorded. Connect a microphone and start recording again.".into()
            } else {
                "Microphone is unavailable; your voice cannot be recorded. Check your microphone connection and System Settings → Sound → Input.".into()
            }
        }
        "system_denied" => "System audio recording permission denied; other participants' voices cannot be recorded. Enable zsmeet under \"System Audio Recording Only\" in System Settings → Privacy & Security → Screen & System Audio Recording, then start recording again.".into(),
        "system_unavailable" => {
            if msg.starts_with("处理电脑声音时出错") {
                "An audio glitch occurred while processing system audio (a brief moment was missed), but recording continues. If this happens often, stop and start recording again.".into()
            } else {
                "Cannot capture system audio; other participants' voices cannot be recorded. Try starting the recording again, or restart your Mac if the issue persists.".into()
            }
        }
        "echo_unavailable" => "Echo cancellation could not be started; microphone audio is being recorded as-is.".into(),
        "mic_silent" => "The microphone has been silent for an extended period. Check your mute switch and System Settings → Sound → Input.".into(),
        _ => msg.to_string(),
    }
}

/// 把 Swift 录音程序（zsmeet-recorder）发来的中文错误在英文界面下翻成英文。
pub fn translate_helper_error_for(lang: Lang, code: &str, msg: &str) -> String {
    if lang != Lang::En || msg.is_empty() {
        return msg.to_string();
    }
    match code {
        "no_source" => "Neither microphone nor system audio can be recorded. Fix the issue shown below and try again.".into(),
        "stop_timeout" => "Recording did not finish cleanly; the last few seconds may not have been saved.".into(),
        "killed" => "Recording was force-stopped; the last few seconds may not have been saved.".into(),
        "out_dir" => "Cannot access the local recording directory. Check disk space and folder permissions.".into(),
        "chunk_exists" => "A recording chunk with the same sequence number already exists in the local folder.".into(),
        "encoder" | "convert_failed" | "unsupported_format" => "Audio encoding failed while recording. Please try starting the recording again.".into(),
        "write_failed" => "Failed to save recording audio to disk. Check available disk space.".into(),
        _ => translate_recorder_text_for(lang, msg),
    }
}

/// 把已经存在内存或 recording.json 里的录音/上传状态说明翻成目标语言。
pub fn translate_recorder_text_for(lang: Lang, msg: &str) -> String {
    if lang != Lang::En || msg.is_empty() {
        return msg.to_string();
    }
    match msg {
        "找不到录音程序（zsmeet-recorder），请重新安装 zsmeet" => {
            "Recorder helper (zsmeet-recorder) not found. Please reinstall zsmeet.".into()
        }
        "现在没有在录音" => "Not currently recording".into(),
        "录音程序没有响应，请再试一次" => "Recorder helper is not responding. Please try again.".into(),
        "这台电脑上没有这场会议的录音" => "No local recording for this meeting on this Mac".into(),
        "录音程序一直没有开始录音，请再试一次。如果弹出了系统的权限提示，请先点「好」或「允许」" => {
            "Recorder helper timed out before starting. If macOS showed a permission prompt, click OK or Allow and try again.".into()
        }
        "录音程序意外退出了。已经录好的部分都在，可以接着录，也可以直接结束、把录好的传上去" => {
            "The recorder exited unexpectedly. Everything recorded so far is safe — you can resume recording or finish and upload what was captured.".into()
        }
        "录音程序一直没能结束，被强制关掉了，最后几秒可能没有保存下来。已经录好的部分都在，可以接着录，也可以直接结束、把录好的传上去" => {
            "The recorder stopped responding while finishing and had to be force-closed; the last few seconds may be missing. Everything recorded before that is safe — you can resume recording or finish and upload it.".into()
        }
        "录音时 zsmeet 意外退出了。已经录好的部分都在，可以接着录，也可以直接结束、把录好的传上去" => {
            "zsmeet quit unexpectedly while recording. Everything recorded so far is safe — you can resume recording or finish and upload it.".into()
        }
        "什么也没有录下来。" => "Nothing was recorded.".into(),
        "这场录音的分片数已经到了服务器能收的上限（一场会议最多 4000 片），不能再接着录了。可以直接结束、把录好的传上去" => {
            "This recording has reached the maximum of 4,000 chunks per meeting and cannot be resumed. Finish and upload what was recorded.".into()
        }
        "这场录音暂停、接着录的次数太多（每次都会多出一片），分片数到了服务器能收的上限（一场会议最多 4000 片），自动结束了。录好的部分会照常上传；还要接着录的话，请新建一场会议" => {
            "This recording reached the 4,000-chunk limit per meeting (each pause/resume creates an extra chunk) and stopped automatically. Recorded audio will upload as usual; create a new meeting to continue recording.".into()
        }
        "已经录了将近 6 个小时，达到单场录音的上限，自动结束了。录好的部分会照常上传" => {
            "Reached the ~6-hour single-recording limit and stopped automatically. Recorded audio will upload as usual.".into()
        }
        "登录已失效，重新登录这个账号后会自动接着传" => {
            "Session expired. Sign back in to this account and uploading will resume automatically.".into()
        }
        "这台电脑上存不下录音的记录了，请检查磁盘空间。空出来以后会自动接着传" => {
            "Cannot save recording metadata on this Mac. Free up disk space and uploading will resume automatically.".into()
        }
        "这场会议已经在别处开始处理了，用的是服务器当时收到的录音，这台电脑上录的可能有一部分不在里面。这台电脑上的录音没有删，确认会议的录音完整以后可以删掉" => {
            "This meeting already started processing elsewhere using the chunks on the server at that time; some audio on this Mac may not be included. Local files were kept — you can delete them after verifying the meeting audio.".into()
        }
        "正在安装 zsmeet 的新版本，装好以后会自动重新打开，请稍等一下再开始录音" => {
            "Installing a new version of zsmeet; it will relaunch automatically. Please wait before starting a recording.".into()
        }
        "连不上服务器，网络恢复后会自动接着传" => {
            "Cannot reach the server. Uploading will resume automatically when back online.".into()
        }
        "服务器很久都没有响应，稍后会自动重试" => {
            "The server took too long to respond; retrying automatically shortly.".into()
        }
        "没有权限往这场会议里传录音，录音传不上去了" => {
            "You do not have permission to upload audio to this meeting.".into()
        }
        "服务器上找不到这场会议了（可能已经被删除），录音传不上去了" => {
            "This meeting was not found on the server (it may have been deleted); cannot upload.".into()
        }
        "这场会议已经结束录音、开始处理了，剩下的录音传不上去了" => {
            "This meeting has already finished recording and started processing; remaining audio cannot be uploaded.".into()
        }
        "服务器地址不对，录音传不上去" => "Invalid server URL; cannot upload recording.".into(),
        "服务器返回的内容看不懂，稍后会自动重试。一直这样的话，看看连的网络是不是要先登录，或者请管理员检查服务器的设置" => {
            "Received an unexpected response from the server; retrying automatically shortly. If this persists, check for a captive Wi-Fi login portal or ask an admin to check server settings.".into()
        }
        "麦克风和电脑声音都录不了，没法开始录音。按提示处理好以后再试。" => {
            "Neither microphone nor system audio can be recorded. Fix the issue shown below and try again.".into()
        }
        "录音没能正常结束，最后几秒可能没有保存下来。" => {
            "Recording did not finish cleanly; the last few seconds may not have been saved.".into()
        }
        "录音被强制结束了，最后几秒可能没有保存下来。" => {
            "Recording was force-stopped; the last few seconds may not have been saved.".into()
        }
        _ => {
            if let Some(seq) = msg
                .strip_prefix("服务器上缺第 ")
                .and_then(|s| s.split_once(" 片录音"))
                .map(|(n, _)| n)
            {
                return format!(
                    "Chunk #{seq} is missing on the server; resending automatically shortly. If this repeats, check whether your network requires sign-in or ask an admin to check server settings."
                );
            }
            if let Some(seq) = msg
                .strip_prefix("服务器上已经有这场会议的第 ")
                .and_then(|s| s.split_once(" 片录音"))
                .map(|(n, _)| n)
            {
                return format!(
                    "Chunk #{seq} already exists on the server without a local record; uploading offline-resumed audio would overwrite it, so upload stopped and all local audio was kept. You can click \"Generate minutes from received audio\" on the meeting page (available 90s after the last chunk)."
                );
            }
            if let Some(seq) = msg
                .strip_prefix("这台电脑上存的第 ")
                .and_then(|s| s.strip_suffix(" 片录音是空的，传不上去"))
            {
                return format!("Local audio chunk #{seq} is empty and cannot be uploaded.");
            }
            if let Some(seq) = msg
                .strip_prefix("第 ")
                .and_then(|s| s.strip_suffix(" 片录音在这台电脑上找不到了，传不上去"))
            {
                return format!("Local audio chunk #{seq} is missing on this Mac and cannot be uploaded.");
            }
            if let Some((seq, err)) = msg
                .strip_prefix("读不出这台电脑上存的第 ")
                .and_then(|s| s.strip_suffix('）'))
                .and_then(|s| s.split_once(" 片录音（"))
            {
                return format!("Cannot read local audio chunk #{seq} ({err}).");
            }
            if let Some(code) = msg
                .strip_prefix("服务器很忙（返回 ")
                .and_then(|s| s.strip_suffix("），稍后会自动重试"))
            {
                return format!("Server is busy (HTTP {code}); retrying automatically shortly.");
            }
            if let Some(code) = msg
                .strip_prefix("服务器暂时出错了（返回 ")
                .and_then(|s| s.strip_suffix("），稍后会自动重试"))
            {
                return format!("Temporary server error (HTTP {code}); retrying automatically shortly.");
            }
            if let Some(code) = msg
                .strip_prefix("服务器把请求转到了别的地址（返回 ")
                .and_then(|s| s.strip_suffix("），请让管理员检查服务器的设置"))
            {
                return format!("Server redirected the request (HTTP {code}); ask an admin to check server settings.");
            }
            if let Some(detail) = msg.strip_prefix("服务器不接受这段录音：") {
                return format!("Server rejected this recording: {detail}");
            }
            if let Some(code) = msg
                .strip_prefix("服务器不接受这段录音（返回 ")
                .and_then(|s| s.strip_suffix('）'))
            {
                return format!("Server rejected this recording (HTTP {code}).");
            }
            msg.to_string()
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_language_tags() {
        assert_eq!(parse_tag("en"), Some(Lang::En));
        assert_eq!(parse_tag("en-US"), Some(Lang::En));
        assert_eq!(parse_tag("en_GB.UTF-8"), Some(Lang::En));
        assert_eq!(parse_tag("zh-Hans-CN"), Some(Lang::Zh));
        assert_eq!(parse_tag("zh_CN.UTF-8"), Some(Lang::Zh));
        assert_eq!(parse_tag(""), None);
        assert_eq!(parse_tag("C"), None);
    }

    #[test]
    fn translates_swift_helper_warnings_and_errors() {
        let w = translate_helper_warning_for(
            Lang::En,
            "mic_denied",
            "没有麦克风权限，录不到你这边的声音。",
        );
        assert!(w.contains("Microphone permission denied"), "{w}");

        let silent = translate_helper_warning_for(
            Lang::En,
            "mic_silent",
            "麦克风「MacBook Pro 麦克风」已经 30 秒录到的全是静音（连一点底噪都没有）。",
        );
        assert!(
            silent.contains("MacBook Pro 麦克风") && silent.contains("silent"),
            "{silent}"
        );

        let err = translate_helper_error_for(
            Lang::En,
            "no_source",
            "麦克风和电脑声音都录不了，没法开始录音。按提示处理好以后再试。",
        );
        assert!(err.contains("Neither microphone nor system audio"), "{err}");

        let persisted = translate_recorder_text_for(
            Lang::En,
            "服务器上缺第 3 片录音，稍后会自动重传。一直这样的话，看看连的网络是不是要先登录，或者请管理员检查服务器的设置",
        );
        assert!(persisted.contains("Chunk #3 is missing"), "{persisted}");
    }
}
