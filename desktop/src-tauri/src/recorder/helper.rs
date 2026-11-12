//! 录音程序 zsmeet-recorder（desktop/recorder/，Swift）：在哪、怎么启动、它说的话。
//!
//! 它从 stdin 一行一个 JSON 收命令（pause、resume、stop；stdin 关掉等于 stop），
//! 从 stdout 一行一个 JSON 报事件（见 Event），日志写在 stderr。

use std::ffi::OsString;
use std::io;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Mutex;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use tokio::process::{self, Child, ChildStderr, ChildStdin, ChildStdout};

pub const HELPER_NAME: &str = "zsmeet-recorder";

/// 列麦克风最多等多久。
const DEVICES_TIMEOUT: Duration = Duration::from_secs(10);

/// 一次只启动一个录音程序。macOS 上 Rust 建管道和给管道设 CLOEXEC 是分两步做的，两个线程同时启动子进程时，
/// 一个子进程可能带走另一个的管道：比如一直在录的录音程序拿着「列麦克风」那个进程的输出管道，
/// 列麦克风就一直等不到它结束。
static SPAWN: Mutex<()> = Mutex::new(());

fn spawn_child(command: &mut process::Command) -> io::Result<Child> {
    let _one_at_a_time = SPAWN.lock().unwrap_or_else(|e| e.into_inner());
    command.spawn()
}

/// 录音程序在哪：环境变量 ZSMEET_RECORDER 优先（开发、测试时用），否则是和 App 主程序放在一起的
/// zsmeet-recorder（Tauri 的 externalBin 把它放在 zsmeet.app/Contents/MacOS/ 下）。
pub fn locate() -> Option<PathBuf> {
    resolve(
        std::env::var_os("ZSMEET_RECORDER"),
        std::env::current_exe().ok(),
    )
}

fn resolve(env: Option<OsString>, exe: Option<PathBuf>) -> Option<PathBuf> {
    if let Some(path) = env.filter(|p| !p.is_empty()) {
        return Some(PathBuf::from(path));
    }
    Some(exe?.parent()?.join(HELPER_NAME))
}

/// 录音程序报的事件。不认识的事件、多出来的字段都不管（以后的版本可能会加）。
#[derive(Deserialize, Debug, Clone, PartialEq)]
#[serde(tag = "event", rename_all = "snake_case")]
pub enum Event {
    /// 开始录了。mic 是麦克风的名字，没录麦克风时是 null；system：在录电脑的声音
    Started {
        #[serde(default)]
        mic: Option<String>,
        #[serde(default)]
        system: bool,
    },
    /// 大约每 100 毫秒一次：录了多少秒（不算暂停），两路声音的音量（0 到 1）
    Level {
        seconds: f64,
        #[serde(default)]
        mic: f64,
        #[serde(default)]
        system: f64,
    },
    /// 一片写完了（已经改名成 `<seq>.aac`），这一片有多少秒
    Chunk {
        seq: u32,
        seconds: f64,
    },
    Paused,
    Resumed,
    /// 有一路声音录不了（没有权限、设备不在），另一路照常录
    Warning {
        code: String,
        message: String,
    },
    /// 录不下去了，接着会以非 0 退出
    Error {
        #[serde(default)]
        code: String,
        message: String,
    },
    /// 停好了，最后一片也写完了，接着以 0 退出
    Stopped {
        next_seq: u32,
        seconds: f64,
    },
    #[serde(other)]
    Unknown,
}

/// stdout 的一行。不是 JSON、不是事件的返回 None。
pub fn parse_event(line: &str) -> Option<Event> {
    serde_json::from_str(line.trim()).ok()
}

/// 发给录音程序的命令。
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Command {
    Pause,
    Resume,
    Stop,
}

impl Command {
    /// 写进 stdin 的一行（带换行）。
    pub fn line(self) -> String {
        let cmd = match self {
            Command::Pause => "pause",
            Command::Resume => "resume",
            Command::Stop => "stop",
        };
        format!("{}\n", serde_json::json!({ "cmd": cmd }))
    }
}

/// `zsmeet-recorder record` 的参数。
pub struct RecordArgs<'a> {
    /// 分片写到哪个目录
    pub out: &'a Path,
    /// 第一片的序号（接着录时从上次的下一片开始）
    pub start_seq: u32,
    pub chunk_seconds: u32,
    /// 麦克风的 uid，或者 "default"（系统默认的麦克风）；None 是不录麦克风
    pub mic: Option<&'a str>,
    /// 录不录电脑的声音
    pub system: bool,
    /// 测试用（--input-file）：拿这个音频文件代替麦克风和电脑的声音，不要权限、不弹提示，读完自动结束
    pub input_file: Option<&'a Path>,
    /// 测试用（--realtime）：和 input_file 一起，按文件的实际时长读，像真的在录
    pub realtime: bool,
}

pub fn record_args(a: &RecordArgs) -> Vec<OsString> {
    let mut args: Vec<OsString> = vec![
        "record".into(),
        "--out".into(),
        a.out.into(),
        "--start-seq".into(),
        a.start_seq.to_string().into(),
        "--chunk-seconds".into(),
        a.chunk_seconds.to_string().into(),
    ];
    match a.mic {
        Some(mic) => {
            args.push("--mic".into());
            args.push(mic.into());
        }
        None => args.push("--no-mic".into()),
    }
    if !a.system {
        args.push("--no-system".into());
    }
    if let Some(file) = a.input_file {
        args.push("--input-file".into());
        args.push(file.into());
        if a.realtime {
            args.push("--realtime".into());
        }
    }
    args
}

/// 麦克风的 uid 看起来对不对。以「-」开头的会被录音程序当成别的参数。
pub fn valid_mic(uid: &str) -> bool {
    !uid.is_empty() && uid.len() <= 512 && !uid.starts_with('-') && !uid.contains('\0')
}

/// `zsmeet-recorder devices` 的结果：这台电脑上的麦克风。
#[derive(Serialize, Deserialize, Clone, Debug, PartialEq)]
pub struct Devices {
    pub microphones: Vec<Microphone>,
}

#[derive(Serialize, Deserialize, Clone, Debug, PartialEq)]
pub struct Microphone {
    pub uid: String,
    pub name: String,
    /// 系统设置里选的默认麦克风
    #[serde(default)]
    pub default: bool,
}

/// 列出麦克风。不需要麦克风权限，不会弹提示。
pub async fn devices(helper: &Path) -> Result<Devices, String> {
    let child = spawn_child(
        process::Command::new(helper)
            .arg("devices")
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .kill_on_drop(true),
    )
    .map_err(|e| spawn_error(&e))?;
    let output = tokio::time::timeout(DEVICES_TIMEOUT, child.wait_with_output())
        .await
        .map_err(|_| "录音程序一直没有列出麦克风，请再试一次".to_string())?
        .map_err(|e| format!("没能列出这台电脑上的麦克风（{e}），请再试一次"))?;
    if !output.status.success() {
        log::warn!(
            "列麦克风失败（{}）：{}",
            output.status,
            String::from_utf8_lossy(&output.stderr)
        );
        return Err("没能列出这台电脑上的麦克风，请再试一次".into());
    }
    parse_devices(&output.stdout)
}

fn parse_devices(stdout: &[u8]) -> Result<Devices, String> {
    String::from_utf8_lossy(stdout)
        .lines()
        .find_map(|line| serde_json::from_str::<Devices>(line.trim()).ok())
        .ok_or_else(|| "录音程序列出的麦克风看不懂，请重新安装 zsmeet".to_string())
}

/// 启动好的录音程序，三个管道都接着。
pub struct Process {
    pub child: Child,
    pub stdin: ChildStdin,
    pub stdout: ChildStdout,
    pub stderr: ChildStderr,
}

pub fn spawn(helper: &Path, args: &[OsString]) -> Result<Process, String> {
    let mut child = spawn_child(
        process::Command::new(helper)
            .args(args)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            // App 自己都没了的话（异常退出），录音程序也不能留着继续录
            .kill_on_drop(true),
    )
    .map_err(|e| spawn_error(&e))?;
    let (Some(stdin), Some(stdout), Some(stderr)) =
        (child.stdin.take(), child.stdout.take(), child.stderr.take())
    else {
        return Err("录音程序没能启动，请再试一次".into());
    };
    Ok(Process {
        child,
        stdin,
        stdout,
        stderr,
    })
}

fn spawn_error(e: &io::Error) -> String {
    match e.kind() {
        io::ErrorKind::NotFound => "找不到录音程序（zsmeet-recorder），请重新安装 zsmeet".into(),
        io::ErrorKind::PermissionDenied => {
            "录音程序打不开（没有执行权限），请重新安装 zsmeet".into()
        }
        _ => format!("录音程序没能启动（{e}），请重新安装 zsmeet 后再试"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn finds_the_helper() {
        let exe = PathBuf::from("/Applications/zsmeet.app/Contents/MacOS/zsmeet");
        assert_eq!(
            resolve(None, Some(exe.clone())),
            Some(PathBuf::from(
                "/Applications/zsmeet.app/Contents/MacOS/zsmeet-recorder"
            ))
        );
        // 环境变量优先，空的不算
        assert_eq!(
            resolve(Some("/tmp/fake-recorder".into()), Some(exe.clone())),
            Some(PathBuf::from("/tmp/fake-recorder"))
        );
        assert_eq!(
            resolve(Some("".into()), Some(exe)),
            Some(PathBuf::from(
                "/Applications/zsmeet.app/Contents/MacOS/zsmeet-recorder"
            ))
        );
        assert_eq!(resolve(None, None), None);
    }

    #[test]
    fn reads_events() {
        assert_eq!(
            parse_event(
                r#"{"event":"started","sample_rate":48000,"channels":1,"mic":"MacBook Pro 麦克风","system":true}"#
            ),
            Some(Event::Started {
                mic: Some("MacBook Pro 麦克风".into()),
                system: true
            })
        );
        assert_eq!(
            parse_event(r#"{"event":"started","mic":null,"system":false}"#),
            Some(Event::Started {
                mic: None,
                system: false
            })
        );
        assert_eq!(
            parse_event(r#"{"event":"level","seconds":12.3,"mic":0.5,"system":0}"#),
            Some(Event::Level {
                seconds: 12.3,
                mic: 0.5,
                system: 0.0
            })
        );
        assert_eq!(
            parse_event(
                r#"{"event":"chunk","seq":3,"path":"/x/3.aac","bytes":80000,"seconds":10.005}"#
            ),
            Some(Event::Chunk {
                seq: 3,
                seconds: 10.005
            })
        );
        assert_eq!(parse_event(r#"{"event":"paused"}"#), Some(Event::Paused));
        assert_eq!(
            parse_event(r#"{"event":"warning","code":"mic_denied","message":"没有麦克风权限"}"#),
            Some(Event::Warning {
                code: "mic_denied".into(),
                message: "没有麦克风权限".into()
            })
        );
        assert_eq!(
            parse_event(r#"{"event":"stopped","next_seq":7,"seconds":65.2}"#),
            Some(Event::Stopped {
                next_seq: 7,
                seconds: 65.2
            })
        );
        // 以后加的事件不认识也不出错
        assert_eq!(
            parse_event(r#"{"event":"device_changed","name":"AirPods"}"#),
            Some(Event::Unknown)
        );
        assert_eq!(parse_event("录音程序的日志"), None);
        assert_eq!(parse_event(r#"{"event":"chunk"}"#), None);
    }

    #[test]
    fn writes_commands() {
        assert_eq!(Command::Pause.line(), "{\"cmd\":\"pause\"}\n");
        assert_eq!(Command::Resume.line(), "{\"cmd\":\"resume\"}\n");
        assert_eq!(Command::Stop.line(), "{\"cmd\":\"stop\"}\n");
    }

    #[test]
    fn builds_record_arguments() {
        let out = PathBuf::from("/data/recordings/m1");
        let args = |mic, system| -> Vec<String> {
            record_args(&RecordArgs {
                out: &out,
                start_seq: 4,
                chunk_seconds: 10,
                mic,
                system,
                input_file: None,
                realtime: false,
            })
            .into_iter()
            .map(|a| a.into_string().unwrap())
            .collect()
        };
        assert_eq!(
            args(Some("default"), true),
            [
                "record",
                "--out",
                "/data/recordings/m1",
                "--start-seq",
                "4",
                "--chunk-seconds",
                "10",
                "--mic",
                "default"
            ]
        );
        assert_eq!(
            args(None, false)[7..],
            ["--no-mic".to_string(), "--no-system".to_string()]
        );
        // 测试模式：拿音频文件代替麦克风和电脑的声音
        let input = PathBuf::from("/tmp/speech.wav");
        let test_args = |realtime| -> Vec<String> {
            record_args(&RecordArgs {
                out: &out,
                start_seq: 0,
                chunk_seconds: 10,
                mic: Some("default"),
                system: true,
                input_file: Some(&input),
                realtime,
            })
            .into_iter()
            .map(|a| a.into_string().unwrap())
            .collect()
        };
        assert_eq!(test_args(false)[9..], ["--input-file", "/tmp/speech.wav"]);
        assert_eq!(
            test_args(true)[9..],
            ["--input-file", "/tmp/speech.wav", "--realtime"]
        );
        assert!(valid_mic("BuiltInMicrophoneDevice"));
        assert!(valid_mic("AppleUSBAudioEngine:Generic:USB Audio:1100000:1"));
        assert!(!valid_mic(""));
        assert!(!valid_mic("--no-system"));
    }

    #[test]
    fn reads_devices() {
        let devices = parse_devices(
            "日志不在这里\n{\"microphones\":[{\"uid\":\"BuiltIn\",\"name\":\"MacBook Pro 麦克风\",\"default\":true},{\"uid\":\"usb\",\"name\":\"USB 麦克风\"}]}\n"
                .as_bytes(),
        )
        .unwrap();
        assert_eq!(devices.microphones.len(), 2);
        assert!(devices.microphones[0].default);
        assert!(!devices.microphones[1].default);
        assert_eq!(
            serde_json::to_value(&devices).unwrap()["microphones"][1],
            serde_json::json!({"uid": "usb", "name": "USB 麦克风", "default": false})
        );
        assert!(parse_devices(b"").is_err());
    }
}
