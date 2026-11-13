//! 原生录音（见 docs/desktop.md「录音（第三阶段）」）。
//!
//! - 录音程序 zsmeet-recorder（helper.rs）录麦克风和电脑的声音，每录好一片（约 10 秒）写一个文件并报告。
//!   一次只录一场；暂停、继续、结束都是给它发命令。它意外退出了，这场录音算「中断」：
//!   可以接着录（从下一片的序号开始写），也可以直接结束、把录好的传上去。
//! - 每场录音在本地一个目录（local.rs）：recording.json 记着录到第几片、传到第几片、标记、状态。
//!   App 重启以后照着它接着传；上次还在录的算中断。
//! - 上传任务一直在后台跑（queue.rs，upload.rs 发请求）：按序号一片一片地传，全部传完、核对过服务器都收到了以后
//!   告诉服务器开始处理，服务器开始处理以后才删掉这台电脑上的录音。
//!   只传当前登录的账号（同一台服务器、同一个用户）的录音、用它的令牌；别的账号的录音留在本地、界面上也不显示，
//!   等那个账号登录了再传。断网、服务器出错就等一会儿再试。
//! - 状态一变（录音时还有每 100 毫秒左右的音量）就通过 RecorderEvents 告诉界面和菜单栏。

mod helper;
mod local;
mod queue;
#[cfg(test)]
mod tests;
mod upload;

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::process::ExitStatus;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde::Serialize;
use serde_json::Value;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::sync::{mpsc, oneshot, watch, Notify};
use tokio::time::Instant;

use crate::core::{Account, Core};
pub use helper::{locate as locate_helper, Devices, EchoCancel, EchoMode, MicGain, MicGainMode};
use helper::{Command, Event, RecordArgs};
use local::{Recording, Status};
use upload::Outcome;

/// 每片多少秒。
pub const CHUNK_SECONDS: u32 = 10;
/// 最长录多久：服务器最多处理 6 小时，留两分钟余量（和网页录音一样）。
const MAX_SECONDS: f64 = 6.0 * 3600.0 - 120.0;
/// 一场最多标记多少个重点，和服务器一致。
const MAX_MARKS: usize = 500;
/// 服务器一场会议最多收多少片（序号 0 到 3999，见 internal/service/meetings.go 的 MaxChunks）。
/// 录音程序的 --start-seq 也只收这个范围。下一片是最后一片（MAX_CHUNKS - 1）时就要结束录音：
/// 结束时录音程序把手上剩下的声音写成最后一片，再往后的序号服务器不收
const MAX_CHUNKS: u32 = 4000;
/// 暂停、继续最多等录音程序多久。
const COMMAND_TIMEOUT: Duration = Duration::from_secs(5);
/// 开始录音前取会议标题最多等多久：服务器慢的话不等了，先录着（标题传完时再补上）。
const TITLE_TIMEOUT: Duration = Duration::from_secs(5);
/// 强制结束录音程序以后最多再等多久。
const KILL_WAIT: Duration = Duration::from_secs(3);
/// 上传失败以后隔多久再试，后面一直按最后一个间隔试（和网页录音一样）。登录的账号一变就马上再试。
const RETRY_WAITS: [Duration; 6] = [
    Duration::from_secs(1),
    Duration::from_secs(2),
    Duration::from_secs(5),
    Duration::from_secs(10),
    Duration::from_secs(20),
    Duration::from_secs(30),
];

const NO_HELPER: &str = "找不到录音程序（zsmeet-recorder），请重新安装 zsmeet";
const NOT_RECORDING: &str = "现在没有在录音";
const NO_RESPONSE: &str = "录音程序没有响应，请再试一次";
const NOT_HERE: &str = "这台电脑上没有这场会议的录音";
const START_TIMEOUT: &str =
    "录音程序一直没有开始录音，请再试一次。如果弹出了系统的权限提示，请先点「好」或「允许」";
const CRASHED: &str =
    "录音程序意外退出了。已经录好的部分都在，可以接着录，也可以直接结束、把录好的传上去";
const KILLED: &str = "录音程序一直没能结束，被强制关掉了，最后几秒可能没有保存下来。已经录好的部分都在，可以接着录，也可以直接结束、把录好的传上去";
const APP_QUIT: &str =
    "录音时 zsmeet 意外退出了。已经录好的部分都在，可以接着录，也可以直接结束、把录好的传上去";
const NOTHING_RECORDED: &str = "什么也没有录下来。";
const CHUNKS_FULL: &str = "这场录音的分片数已经到了服务器能收的上限（一场会议最多 4000 片），不能再接着录了。可以直接结束、把录好的传上去";
const CHUNKS_REACHED: &str = "这场录音暂停、接着录的次数太多（每次都会多出一片），分片数到了服务器能收的上限（一场会议最多 4000 片），自动结束了。录好的部分会照常上传；还要接着录的话，请新建一场会议";
const MAX_REACHED: &str =
    "已经录了将近 6 个小时，达到单场录音的上限，自动结束了。录好的部分会照常上传";
const RELOGIN: &str = "登录已失效，重新登录这个账号后会自动接着传";
const NOT_SAVED: &str = "这台电脑上存不下录音的记录了，请检查磁盘空间。空出来以后会自动接着传";
const PROCESSED_ELSEWHERE: &str = "这场会议已经在别处开始处理了，用的是服务器当时收到的录音，这台电脑上录的可能有一部分不在里面。这台电脑上的录音没有删，确认会议的录音完整以后可以删掉";
const UPDATING: &str = "正在安装 zsmeet 的新版本，装好以后会自动重新打开，请稍等一下再开始录音";

/// 发给界面和菜单栏的录音状态（事件 recorder://state，命令的返回值）。
#[derive(Serialize, Clone, Debug, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct RecorderState {
    pub status: Phase,
    /// 正在录的会议。没在录时是 null；别的账号在录时也是 null，和它有关的字段（标题、时长、音量、标记、提醒）都是空的
    pub meeting_id: Option<String>,
    pub title: Option<String>,
    /// 录了多少秒，不算暂停的时间（接着录的从上次录到的地方算起）
    pub seconds: f64,
    /// 两路声音现在的音量（0 到 1）。还没开始录时是 null
    pub level: Option<Level>,
    /// 在录的麦克风的名字。不录麦克风（或者开始时就录不了）时是 null；录到一半断开了还是它的名字，
    /// warnings 里有提醒
    pub mic: Option<String>,
    /// 在录电脑的声音
    pub system: bool,
    /// 回声消除的情况。没在录、录音程序还没开始录（或者是不报这个的旧版本）时是 null
    pub echo: Option<Echo>,
    /// 麦克风自动增益：录音程序开始录时报的模式（auto：在调；off：不调；unavailable：要调却没能打开，录的是原声）。
    /// 没在录、录音程序还没开始录（或者是不报这个的旧版本）时是 null
    pub mic_gain: Option<MicGainMode>,
    pub marks: Vec<f64>,
    /// 有一路声音录不了的提醒（没有权限、设备不在），另一路照常录。同一个 code 说的事不一样时都留着。
    /// 没能开始录音时是录音程序给的提醒，说明为什么开始不了，下次开始录音时清掉
    pub warnings: Vec<Warning>,
    /// 上一场录音不正常结束的原因（中断了、录满时长自动结束了）。下次开始录音时清掉
    pub error: Option<String>,
    /// 当前登录的账号在这台电脑上的录音和上传进度，按开始录音的时间排
    pub uploads: Vec<Upload>,
}

impl RecorderState {
    /// 装好新版本要重启 App，现在不行的话返回给用户看的原因：正在录音（包括正在开始、正在结束），或者当前账号
    /// 还有录音没传完（边录边传、在传、暂时传不上去在等着再试、在告诉服务器传完了）。重启不会丢录音，
    /// 但会打断录音和上传。中断了、传不上去了的录音要等用户处理，可能一直放着，不算。
    pub fn update_blocker(&self) -> Option<String> {
        match self.status {
            Phase::Idle => {}
            Phase::Stopping => {
                return Some("录音正在结束。等录好的都传完以后再更新".into());
            }
            Phase::Starting | Phase::Recording | Phase::Paused => {
                return Some("正在录音。结束录音、等录好的都传完以后再更新".into());
            }
        }
        let pending = self
            .uploads
            .iter()
            .filter(|u| {
                matches!(
                    u.status,
                    UploadStatus::Recording
                        | UploadStatus::Uploading
                        | UploadStatus::Waiting
                        | UploadStatus::Completing
                )
            })
            .count();
        // 暂时传不上去的（断网、连不上服务器）要人去看一眼网络，不然一直等不到传完。
        // 说法和界面上的一样（web/src/platform/updateView.ts 的 updateBlocker）
        let waiting = self
            .uploads
            .iter()
            .filter(|u| u.status == UploadStatus::Waiting)
            .count();
        let stuck = match (pending, waiting) {
            (0, _) => return None,
            (n, 0) => return Some(format!("还有 {n} 段录音没传完。传完以后再更新")),
            (n, w) if w == n => format!("还有 {n} 段录音暂时传不上去"),
            (n, w) => format!("还有 {n} 段录音没传完，其中 {w} 段暂时传不上去"),
        };
        Some(format!(
            "{stuck}，在等着自动再试。请检查网络和服务器，传完以后再更新"
        ))
    }
}

#[derive(Serialize, Clone, Copy, Debug, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum Phase {
    Idle,
    /// 录音程序启动了，还没开始录（可能在等用户在系统的权限提示上点允许）
    Starting,
    Recording,
    Paused,
    /// 在等录音程序写完最后一片
    Stopping,
}

#[derive(Serialize, Clone, Copy, Debug, PartialEq)]
pub struct Level {
    pub mic: f64,
    pub system: f64,
}

/// 回声消除：用户选的模式，现在实际开没开、为什么。
#[derive(Serialize, Clone, Debug, PartialEq, Eq)]
pub struct Echo {
    /// 录音程序开始录时报的模式（off、auto、on；要的模式没能打开是 unavailable）
    pub mode: EchoMode,
    /// 现在正在消除回声
    pub active: bool,
    /// 录音程序说的原因：speakers（外放）、headphones（耳机）、airplay、no_reference（没在录电脑的声音）、
    /// forced（要求一直开着）、off。录音程序还没说过时是 null
    pub reason: Option<String>,
}

#[derive(Serialize, Clone, Debug, PartialEq, Eq)]
pub struct Warning {
    pub code: String,
    pub message: String,
}

#[derive(Serialize, Clone, Debug, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct Upload {
    pub meeting_id: String,
    pub title: String,
    /// 录好了多少片
    pub recorded: u32,
    /// 传上去了多少片
    pub uploaded: u32,
    pub status: UploadStatus,
    /// 传不上去、中断的原因，或者暂时传不上去的原因（waiting 时）
    pub error: Option<String>,
}

#[derive(Serialize, Clone, Copy, Debug, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum UploadStatus {
    /// 正在录，录好的边录边传
    Recording,
    Uploading,
    /// 暂时传不上去（断网、服务器出错），过一会儿自动再试
    Waiting,
    /// 都传完了，在告诉服务器开始处理
    Completing,
    Completed,
    /// 中断了，等用户决定接着录还是直接结束
    Interrupted,
    /// 传不上去了，只能删掉
    Failed,
}

/// 录音状态变了要告诉谁。正式运行时是界面的事件和菜单栏，测试时记下来检查。
pub trait RecorderEvents: Send + Sync {
    fn state(&self, state: &RecorderState);
}

pub struct RecorderOptions {
    /// 本地录音的根目录：<app_data_dir>/recordings
    pub root: PathBuf,
    /// 录音程序在哪（见 helper::locate）
    pub helper: Option<PathBuf>,
    pub chunk_seconds: u32,
    /// 上传失败以后的等待时间
    pub retry_waits: Vec<Duration>,
    /// 开始录音最多等多久（第一次录音时系统会弹出麦克风权限的提示，要等用户点）
    pub start_timeout: Duration,
    /// 结束录音（包括退出 App 时）最多等录音程序多久，到时还不退出就强制结束，这场录音算中断
    pub stop_timeout: Duration,
    /// 测试用：让录音程序拿这个音频文件代替麦克风和电脑的声音（--input-file，不要权限、不弹提示，
    /// 读完自动结束）。正式运行时是 None
    pub input_file: Option<PathBuf>,
    /// 测试用：和 input_file 一起，按文件的实际时长读（--realtime），用来测暂停、继续、标记、结束
    pub realtime: bool,
    /// 麦克风自动增益（--mic-gain）：auto，在回声消除之后把麦克风那一路调到正常的说话音量。界面上没有开关，
    /// 排查时可以用环境变量 ZSMEET_RECORDER_MIC_GAIN=off 关掉（见 MicGain::from_env）
    pub mic_gain: MicGain,
}

impl RecorderOptions {
    pub fn new(root: PathBuf, helper: Option<PathBuf>) -> Self {
        Self {
            root,
            helper,
            chunk_seconds: CHUNK_SECONDS,
            retry_waits: RETRY_WAITS.to_vec(),
            start_timeout: Duration::from_secs(120),
            // 录音程序自己最多花 8 秒（RecordSession.stopTimeout）写完最后一片，到时还没写完会报错退出：
            // 多等一会儿，听它说完原因，不要在它写的时候把它强制关掉
            stop_timeout: Duration::from_secs(10),
            input_file: None,
            realtime: false,
            mic_gain: MicGain::from_env(),
        }
    }
}

pub struct Recorder {
    opts: RecorderOptions,
    core: Arc<Core>,
    events: Arc<dyn RecorderEvents>,
    client: reqwest::Client,
    inner: Mutex<Inner>,
    /// 叫醒上传任务：有了新的分片、标记，状态变了，换了账号
    wake: Notify,
    /// 录音程序那边有了变化（开始了、暂停了、退出了）就加一。等这些变化的命令用
    session_changes: watch::Sender<u64>,
    /// 算状态、发状态时拿着：界面收到的顺序和状态变化的顺序一样，不会被旧的盖掉
    emitting: Mutex<()>,
}

struct Inner {
    /// 这台电脑上的录音（所有账号的），按会议 id
    recordings: BTreeMap<String, Local>,
    /// 正在录的那一场
    session: Option<Session>,
    next_session: u64,
    /// 见 RecorderState::error
    error: Option<String>,
    /// 最近一次没能开始录音时录音程序给的提醒（比如两路声音都没有权限），界面据此说明怎么打开权限。
    /// 下次开始录音时清掉
    start_warnings: Vec<Warning>,
    /// 最近一个没开始录就退出的录音程序：会话编号和原因，开始录音的命令据此报错
    ended: Option<(u64, String)>,
    /// 正在安装新版本、马上要重启 App（见 Recorder::hold_for_update）：不让开始录音
    updating: bool,
}

/// 一场本地的录音，加上上传的情况（只在内存里）。
struct Local {
    rec: Recording,
    dir: PathBuf,
    /// 标记还没存到服务器上
    marks_dirty: bool,
    /// 连着失败了几次，决定下次等多久
    attempts: usize,
    /// 告诉服务器传完之前发现服务器缺了已经「传上去」的分片、重传了几次（见 Local::resend_from）
    resends: usize,
    retry_at: Option<Instant>,
    /// 暂时传不上去的原因，传成功了就清掉
    trouble: Option<String>,
    /// 录音过程中遇到了重试也没用的上传错误：不再传，录完算失败
    fatal: bool,
}

/// 正在运行的录音程序。
struct Session {
    id: u64,
    meeting_id: String,
    /// 谁在录：开始录音时登录的账号（服务器、用户）
    server: String,
    user_id: String,
    phase: Phase,
    /// 新录的（不是接着录）：没录到东西就退出时整个删掉
    fresh: bool,
    /// 接着录时，之前已经录了多少秒（录音程序报的秒数从 0 算起）
    base_seconds: f64,
    seconds: f64,
    level: Option<Level>,
    mic: Option<String>,
    system: bool,
    /// 录音程序报的回声消除模式（started 里的），没报过是 None
    echo_mode: Option<EchoMode>,
    /// 录音程序最近一次说的回声消除开没开、为什么（echo 事件）
    echo_active: bool,
    echo_reason: Option<String>,
    /// 录音程序报的麦克风自动增益模式（started 里的），没报过是 None
    mic_gain: Option<MicGainMode>,
    warnings: Vec<Warning>,
    /// 录音程序说的错误，它退出时用
    error: Option<String>,
    /// 录音程序报了 stopped
    stopped: bool,
    /// 要它结束了（用户点了结束、退出 App、录满了）。只有报了 stopped 才算正常结束：
    /// 没报就退出了（出错了、等太久被强制结束了），最后一片可能没写完，算中断
    stop_requested: bool,
    /// 等太久，强制结束了
    killed: bool,
    commands: mpsc::UnboundedSender<Command>,
    kill: Option<oneshot::Sender<()>>,
}

impl Session {
    /// 是不是这个账号在录。
    fn belongs_to(&self, account: &Account) -> bool {
        self.server == account.server && self.user_id == account.user_id
    }

    /// 让录音程序结束（已经在结束了就不再说一遍）。返回会话编号，好等它退出。
    fn request_stop(&mut self) -> u64 {
        if self.phase != Phase::Stopping {
            self.phase = Phase::Stopping;
            self.stop_requested = true;
            let _ = self.commands.send(Command::Stop);
        }
        self.id
    }
}

/// 录音程序是怎么退出的。
enum Ending {
    /// 正常结束：报了 stopped，最后一片也写完了
    Stopped,
    /// 还没开始录就退出了（没有权限、设备都用不了……）
    NeverStarted(String),
    /// 录到一半退出了，或者结束时没写完最后一片就退出了
    Crashed(String),
}

impl Recorder {
    /// 读出这台电脑上的录音（上次还在录的算中断）。上传要等 run() 跑起来。
    pub fn new(
        opts: RecorderOptions,
        core: Arc<Core>,
        events: Arc<dyn RecorderEvents>,
        client: reqwest::Client,
    ) -> Arc<Self> {
        if let Err(e) = local::create_private_dir(&opts.root) {
            log::error!("录音目录 {} 建不起来：{e}", opts.root.display());
        }
        let recordings = load_all(&opts.root);
        Arc::new(Self {
            opts,
            core,
            events,
            client,
            inner: Mutex::new(Inner {
                recordings,
                session: None,
                next_session: 1,
                error: None,
                start_warnings: Vec::new(),
                ended: None,
                updating: false,
            }),
            wake: Notify::new(),
            session_changes: watch::channel(0).0,
            emitting: Mutex::new(()),
        })
    }

    /// 后台的上传任务，和登录的账号变了以后的处理。一直运行。
    pub async fn run(self: Arc<Self>) {
        // 先报一次现在的状态：菜单栏上要显示有没有没传完的录音
        self.changed();
        tokio::join!(self.clone().upload_loop(), self.account_loop());
    }

    pub fn state(&self) -> RecorderState {
        let account = self.core.account();
        let inner = self.inner.lock().unwrap();
        snapshot(&inner, account.as_ref())
    }

    /// 状态变了：告诉界面和菜单栏。不能拿着 inner 的锁调用。
    fn changed(&self) {
        let _order = self.emitting.lock().unwrap();
        let state = self.state();
        self.events.state(&state);
    }

    fn session_changed(&self) {
        self.session_changes.send_modify(|n| *n = n.wrapping_add(1));
    }

    /// 这台电脑上的麦克风。
    pub async fn devices(&self) -> Result<Devices, String> {
        let helper = self.opts.helper.as_deref().ok_or(NO_HELPER)?;
        helper::devices(helper).await
    }

    /// 开始录音：mic 是麦克风的 uid 或者 "default"，None 是不录麦克风；system：录不录电脑的声音；
    /// echo_cancel：回声消除（两路都在录时才用得上，录音程序自己判断）。麦克风自动增益照 RecorderOptions 的 mic_gain。
    /// 这台电脑上有这场会议中断了的录音时接着录：序号接着上次的，标记留着。
    /// 等到录音程序真的开始录了才返回，开始不了返回原因。
    pub async fn start(
        self: &Arc<Self>,
        meeting_id: &str,
        mic: Option<String>,
        system: bool,
        echo_cancel: EchoCancel,
    ) -> Result<RecorderState, String> {
        if !local::valid_meeting_id(meeting_id) {
            return Err("会议编号不对，请回到录音页重新开始".into());
        }
        if mic.as_deref().is_some_and(|m| !helper::valid_mic(m)) {
            return Err("选的麦克风不对，请重新选一个".into());
        }
        if mic.is_none() && !system {
            return Err("麦克风和电脑的声音至少要录一个".into());
        }
        let helper_path = self.opts.helper.clone().ok_or(NO_HELPER)?;
        let account = self.core.account().ok_or("请先登录，再开始录音")?;

        let (id, fresh, commands) = self.reserve(meeting_id, &account)?;
        self.session_changed();
        self.changed();

        let prepared = if fresh {
            self.create_recording(id, meeting_id, &account).await
        } else {
            match self.received_chunks(meeting_id, &account).await {
                Ok(received) => self.reopen_recording(id, meeting_id, received.as_deref()),
                Err(e) => Err(e),
            }
        };
        let (dir, start_seq) = match prepared {
            Ok(v) => v,
            Err(e) => {
                self.abandon(id);
                return Err(e);
            }
        };
        if start_seq >= MAX_CHUNKS - 1 {
            // 只剩最后一片了：开始了也得马上结束（见 on_event），再往后的分片服务器不收。
            // 录音程序也不接受 4000 以上的 --start-seq，它说的是参数的事，用户看不懂
            self.abandon(id);
            return Err(CHUNKS_FULL.into());
        }
        // 等服务器回话的时候就被结束了（比如退出了 App）：不用再启动录音程序
        let cancelled = {
            let inner = self.inner.lock().unwrap();
            inner
                .session
                .as_ref()
                .is_none_or(|s| s.id != id || s.stop_requested)
        };
        if cancelled {
            self.abandon(id);
            return Err("录音还没开始就被结束了".into());
        }
        let args = helper::record_args(&RecordArgs {
            out: &dir,
            start_seq,
            chunk_seconds: self.opts.chunk_seconds,
            mic: mic.as_deref(),
            system,
            echo_cancel,
            mic_gain: self.opts.mic_gain,
            input_file: self.opts.input_file.as_deref(),
            realtime: self.opts.realtime,
        });
        let process = match helper::spawn(&helper_path, &args) {
            Ok(p) => p,
            Err(e) => {
                self.abandon(id);
                return Err(e);
            }
        };
        self.attach(id, process, commands);

        let settled = self
            .wait_session(id, self.opts.start_timeout, |s| {
                s.is_none_or(|s| s.phase != Phase::Starting)
            })
            .await;
        if !settled {
            self.kill(id);
            self.wait_session(id, KILL_WAIT, |s| s.is_none()).await;
            return Err(START_TIMEOUT.into());
        }
        // 没开始录就退出了的，退出时记下了原因；开始了的（哪怕马上又中断了）都算开始成功，后面的看状态
        let failed = match &self.inner.lock().unwrap().ended {
            Some((ended, reason)) if *ended == id => Some(reason.clone()),
            _ => None,
        };
        match failed {
            None => Ok(self.state()),
            Some(reason) => Err(reason),
        }
    }

    /// 占住「正在录」的位置，免得同时开始两场。返回会话编号、是不是新录的、发命令的通道。
    fn reserve(
        &self,
        meeting_id: &str,
        account: &Account,
    ) -> Result<(u64, bool, mpsc::UnboundedReceiver<Command>), String> {
        let mut inner = self.inner.lock().unwrap();
        if inner.updating {
            return Err(UPDATING.into());
        }
        if let Some(s) = &inner.session {
            return Err(if s.phase == Phase::Stopping {
                "上一场录音还在结束，请稍等几秒再开始".into()
            } else if s.meeting_id == meeting_id {
                "这场会议已经在录音了".into()
            } else {
                "正在录另一场会议，请先结束那一场再开始".into()
            });
        }
        let fresh = match inner.recordings.get(meeting_id) {
            None => true,
            Some(l) if !l.rec.belongs_to(&account.server, &account.user_id) => {
                return Err("这场会议的录音是另一个账号录的，不能接着录".into())
            }
            Some(l) => match l.rec.status {
                Status::Interrupted => false,
                Status::Recording => return Err("这场会议已经在录音了".into()),
                Status::Stopped => return Err("这场会议已经录完了，正在上传".into()),
                Status::Completed => return Err("这场会议已经录完、传完了".into()),
                Status::Failed => return Err(
                    "这场会议的录音传不上去了，不能接着录。可以删掉这段录音，再新建一场会议录音"
                        .into(),
                ),
            },
        };
        let id = inner.next_session;
        inner.next_session += 1;
        let (tx, rx) = mpsc::unbounded_channel();
        inner.session = Some(Session {
            id,
            meeting_id: meeting_id.to_string(),
            server: account.server.clone(),
            user_id: account.user_id.clone(),
            phase: Phase::Starting,
            fresh,
            base_seconds: 0.0,
            seconds: 0.0,
            level: None,
            mic: None,
            system: false,
            echo_mode: None,
            echo_active: false,
            echo_reason: None,
            mic_gain: None,
            warnings: Vec::new(),
            error: None,
            stopped: false,
            stop_requested: false,
            killed: false,
            commands: tx,
            kill: None,
        });
        inner.error = None;
        inner.start_warnings.clear();
        Ok((id, fresh, rx))
    }

    /// 新录一场：从服务器取会议的标题（顺便确认会议还在等录音），在本地建好目录和 recording.json。
    async fn create_recording(
        &self,
        id: u64,
        meeting_id: &str,
        account: &Account,
    ) -> Result<(PathBuf, u32), String> {
        let found = upload::meeting(&self.client, account, meeting_id, TITLE_TIMEOUT).await;
        let title = match found {
            Ok(meeting) => {
                if meeting.get("status").and_then(Value::as_str) != Some("uploading") {
                    return Err("这场会议已经开始处理了，不能再录音。请新建一场会议".into());
                }
                meeting
                    .get("title")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_string()
            }
            Err(Outcome::Unauthorized) => {
                self.core.unauthorized(&account.token);
                return Err("登录已失效，请重新登录后再录音".into());
            }
            // 刚建好会议就连不上了：照样录，录好的等网络恢复再传
            Err(Outcome::Retry(_)) => String::new(),
            Err(_) => {
                return Err(
                    "服务器上找不到这场会议，或者你没有权限往里面录音。请回到录音页重新开始".into(),
                )
            }
        };
        let dir = self.opts.root.join(meeting_id);
        let rec = Recording {
            meeting_id: meeting_id.to_string(),
            server: account.server.clone(),
            user_id: account.user_id.clone(),
            title,
            started_at: local::now_millis(),
            next_seq: 0,
            uploaded: 0,
            marks: Vec::new(),
            seconds: 0.0,
            status: Status::Recording,
            error: None,
            unverified_from: None,
            complete_sent: false,
        };
        // 新录的：目录里要是有以前留下的东西（上次没开始成），都不要了
        let _ = std::fs::remove_dir_all(&dir);
        local::create_private_dir(&self.opts.root)
            .and_then(|_| local::create_private_dir(&dir))
            .and_then(|_| local::save(&dir, &rec))
            .map_err(|e| format!("录音没法存到这台电脑上（{e}），请检查磁盘空间"))?;
        let mut inner = self.inner.lock().unwrap();
        if inner.session.as_ref().is_none_or(|s| s.id != id) {
            let _ = std::fs::remove_dir_all(&dir);
            return Err(NOT_RECORDING.into());
        }
        inner
            .recordings
            .insert(meeting_id.to_string(), Local::new(rec, dir.clone()));
        Ok((dir, 0))
    }

    /// 接着录、直接结束之前问服务器已经收到了哪几片：接着录的分片不能占用它们的序号，complete 的片数也要算上它们
    /// （见 Local::catch_up）。
    ///
    /// 连不上服务器、服务器出错、回复看不懂时返回 None，照这台电脑上记着的来（断网也要能接着录）。这台电脑上录的分片
    /// 要等服务器开始处理以后才删，resume_seq 都会跳过；服务器上却可能有这台电脑不知道的分片（以前的版本传上去就删了、
    /// recording.json 又退回了上一份，或者不是这台电脑传的）。所以接着录时记下从哪一片起没核对过
    /// （Recording::unverified_from），传之前再核对（见 queue.rs），不会悄悄覆盖服务器上的录音。
    async fn received_chunks(
        &self,
        meeting_id: &str,
        account: &Account,
    ) -> Result<Option<Vec<u32>>, String> {
        match upload::received(&self.client, account, meeting_id, TITLE_TIMEOUT).await {
            Ok(received) => Ok(Some(received)),
            Err(Outcome::Retry(reason)) => {
                log::warn!(
                    "{meeting_id}：没能问到服务器收到了哪几片（{reason}），照这台电脑上记着的来"
                );
                Ok(None)
            }
            Err(Outcome::Unauthorized) => {
                self.core.unauthorized(&account.token);
                Err("登录已失效，请重新登录后再试".into())
            }
            // 会议被删了、没有权限了：接着录也传不上去
            Err(Outcome::Fatal(reason) | Outcome::Conflict(reason)) => Err(reason),
            Err(Outcome::Done(_)) => Ok(None),
        }
    }

    /// 接着录一场中断了的：从下一片的序号开始写，秒数接着上次的。received：服务器已经收到的分片，问不到时是 None。
    fn reopen_recording(
        &self,
        id: u64,
        meeting_id: &str,
        received: Option<&[u32]>,
    ) -> Result<(PathBuf, u32), String> {
        let mut inner = self.inner.lock().unwrap();
        let Inner {
            session,
            recordings,
            ..
        } = &mut *inner;
        let session = session
            .as_mut()
            .filter(|s| s.id == id)
            .ok_or(NOT_RECORDING)?;
        let l = recordings.get_mut(meeting_id).ok_or(NOT_HERE)?;
        local::remove_partials(&l.dir);
        local::recover(&l.dir, &mut l.rec);
        if let Some(reason) = received.and_then(|r| l.taken_on_server(r)) {
            // 上次断网时接着录的部分和服务器上已有的分片用了同样的序号：不能再录了。上传任务马上照常传完前面的分片，
            // 再核对、把它算成传不上去
            l.ready();
            self.wake.notify_one();
            return Err(reason);
        }
        l.catch_up(received.unwrap_or_default(), self.opts.chunk_seconds);
        if received.is_none() {
            // 没能和服务器核对：从这一片起录的，传之前先核对（之前就有没核对过的，从更早的那一片起）
            l.rec.unverified_from.get_or_insert(l.rec.next_seq);
        }
        // 接着录的声音从已经存下的部分后面算起：中断前最后一片之后点的标记会指到新录的声音上，不要了
        l.drop_lost_marks();
        l.rec.status = Status::Recording;
        l.fatal = false;
        l.save();
        session.base_seconds = l.rec.seconds;
        Ok((l.dir.clone(), l.rec.next_seq))
    }

    /// 录音程序还没跑起来就开始不了了：放开「正在录」的位置。
    fn abandon(&self, id: u64) {
        {
            let mut inner = self.inner.lock().unwrap();
            let Some(session) = inner.session.take_if(|s| s.id == id) else {
                return;
            };
            if session.fresh {
                if let Some(l) = inner.recordings.remove(&session.meeting_id) {
                    let _ = std::fs::remove_dir_all(&l.dir);
                }
            } else if let Some(l) = inner
                .recordings
                .get_mut(&session.meeting_id)
                // 已经改成接着录了的改回中断；发现传不上去了的（Failed）不动
                .filter(|l| l.rec.status == Status::Recording)
            {
                l.rec.status = Status::Interrupted;
                l.save();
            }
        }
        self.session_changed();
        self.changed();
    }

    /// 接上刚启动的录音程序：一个任务往 stdin 写命令，一个读 stdout 的事件、最后等它退出，一个把 stderr 记进日志。
    fn attach(
        self: &Arc<Self>,
        id: u64,
        process: helper::Process,
        mut commands: mpsc::UnboundedReceiver<Command>,
    ) {
        let helper::Process {
            mut child,
            mut stdin,
            stdout,
            stderr,
        } = process;
        let (kill_tx, mut kill_rx) = oneshot::channel::<()>();
        if let Some(s) = self
            .inner
            .lock()
            .unwrap()
            .session
            .as_mut()
            .filter(|s| s.id == id)
        {
            s.kill = Some(kill_tx);
        }

        // 会话结束、通道关掉时 stdin 跟着关掉：录音程序看到 EOF 也会停
        tokio::spawn(async move {
            while let Some(command) = commands.recv().await {
                let line = command.line();
                if stdin.write_all(line.as_bytes()).await.is_err() || stdin.flush().await.is_err() {
                    break;
                }
            }
        });

        tokio::spawn(async move {
            let mut lines = BufReader::new(stderr).lines();
            while let Ok(Some(line)) = lines.next_line().await {
                log::info!("录音程序：{line}");
            }
        });

        let this = self.clone();
        tokio::spawn(async move {
            let mut lines = BufReader::new(stdout).lines();
            // 会话没了（kill 的发送端被丢掉）也算要强制结束
            let mut killed = false;
            loop {
                tokio::select! {
                    line = lines.next_line() => match line {
                        Ok(Some(line)) => match helper::parse_event(&line) {
                            Some(event) => this.on_event(id, event),
                            None => log::warn!("录音程序输出了看不懂的一行：{line}"),
                        },
                        _ => break,
                    },
                    _ = &mut kill_rx, if !killed => {
                        killed = true;
                        let _ = child.start_kill();
                    }
                }
            }
            let status = tokio::select! {
                status = child.wait() => status.ok(),
                _ = &mut kill_rx, if !killed => {
                    let _ = child.start_kill();
                    child.wait().await.ok()
                }
            };
            this.on_exit(id, status);
        });
    }

    /// 录音程序报了一个事件。
    fn on_event(&self, id: u64, event: Event) {
        if event == Event::Unknown {
            return;
        }
        let mut chunk = false;
        {
            let mut inner = self.inner.lock().unwrap();
            let Inner {
                session,
                recordings,
                error,
                ..
            } = &mut *inner;
            let Some(s) = session.as_mut().filter(|s| s.id == id) else {
                return;
            };
            let l = recordings.get_mut(&s.meeting_id);
            match event {
                Event::Started {
                    mic,
                    system,
                    echo_cancellation,
                    mic_gain,
                } => {
                    if s.phase == Phase::Starting {
                        s.phase = Phase::Recording;
                    }
                    s.mic = mic;
                    s.system = system;
                    // 开没开、为什么由 echo 事件说（可能比 started 先到，不能清掉）
                    s.echo_mode = echo_cancellation;
                    s.mic_gain = mic_gain;
                    // 接着录的：中断的原因用不着了
                    if let Some(l) = l.filter(|l| !l.fatal && l.rec.error.is_some()) {
                        l.rec.error = None;
                        l.save();
                    }
                }
                Event::Echo { active, reason } => {
                    s.echo_active = active;
                    s.echo_reason = reason;
                }
                // 录的是系统默认的麦克风，用户换了默认的麦克风：界面上显示现在在录的这个
                Event::MicChanged { name } => s.mic = Some(name),
                Event::Level {
                    seconds,
                    mic,
                    system,
                } => {
                    s.seconds = seconds;
                    s.level = Some(Level { mic, system });
                    if s.base_seconds + seconds >= MAX_SECONDS && !s.stop_requested {
                        // 录满了：自动结束，录好的照常上传
                        s.phase = Phase::Stopping;
                        s.stop_requested = true;
                        let _ = s.commands.send(Command::Stop);
                        *error = Some(MAX_REACHED.into());
                    }
                }
                Event::Chunk { seq, seconds } => {
                    if let Some(l) = l.filter(|l| seq >= l.rec.next_seq) {
                        if seq > l.rec.next_seq {
                            log::warn!("录音程序跳过了第 {} 到 {} 片", l.rec.next_seq, seq - 1);
                        }
                        l.rec.next_seq = seq + 1;
                        l.rec.seconds += seconds;
                        l.save();
                        chunk = true;
                        // 下一片就是服务器能收的最后一片了（每暂停一次多出一片，暂停得多的话不到 6 小时也会到这里）：
                        // 现在就结束，录音程序把剩下的写成最后一片。再往后的分片服务器不收，这场录音就交不上去了
                        if l.rec.next_seq >= MAX_CHUNKS - 1 && !s.stop_requested {
                            s.request_stop();
                            *error = Some(CHUNKS_REACHED.into());
                        }
                    }
                }
                Event::Paused => {
                    if s.phase == Phase::Recording {
                        s.phase = Phase::Paused;
                    }
                }
                Event::Resumed => {
                    if s.phase == Phase::Paused {
                        s.phase = Phase::Recording;
                    }
                }
                Event::Warning { code, .. } if code == helper::CHUNK_LIMIT => {
                    // 录音程序在写服务器能收的最后一片了，不会再切新的分片（平常收到前一片时就已经结束了，见上面）：
                    // 现在结束。原因放在 error 里，不放进 warnings：它说的「请结束录音」马上就过时了
                    if !s.stop_requested {
                        s.request_stop();
                        *error = Some(CHUNKS_REACHED.into());
                    }
                }
                Event::Warning { code, message } => {
                    // 同一个 code 可能先后有几条说的不是一回事（开始时改用了别的麦克风，后来它又断开了），
                    // 都要给用户看；一模一样的只留一条
                    if !s
                        .warnings
                        .iter()
                        .any(|w| w.code == code && w.message == message)
                    {
                        s.warnings.push(Warning { code, message });
                    }
                }
                Event::Error { message, .. } => s.error = Some(message),
                Event::Stopped { .. } => s.stopped = true,
                Event::Unknown => {}
            }
        }
        if chunk {
            self.wake.notify_one();
        }
        self.session_changed();
        self.changed();
    }

    /// 录音程序退出了：这场录音结束、中断，或者根本没开始。
    fn on_exit(&self, id: u64, status: Option<ExitStatus>) {
        {
            let mut inner = self.inner.lock().unwrap();
            let Some(s) = inner.session.take_if(|s| s.id == id) else {
                return;
            };
            let ending = if s.stopped {
                Ending::Stopped
            } else if s.phase == Phase::Starting {
                Ending::NeverStarted(s.error.clone().unwrap_or_else(|| start_failed(status)))
            } else {
                // 没报 stopped 就退出了：哪怕是要它结束的（录音程序自己说结束卡住了、等太久被强制结束了），
                // 最后一片也可能没写完。不能当成正常结束直接交给服务器处理，算中断，让用户决定接着录还是直接结束
                let fallback = if s.killed { KILLED } else { CRASHED };
                Ending::Crashed(s.error.clone().unwrap_or_else(|| fallback.into()))
            };
            let Inner {
                recordings,
                error,
                start_warnings,
                ended,
                ..
            } = &mut *inner;
            let mut nothing = false;
            if let Some(l) = recordings.get_mut(&s.meeting_id) {
                // 写完了但没来得及报的分片也算上；没写完的不要
                local::remove_partials(&l.dir);
                local::recover(&l.dir, &mut l.rec);
                nothing = l.rec.next_seq == 0;
                if nothing {
                    // 不管是怎么结束的，都只能这么说：「录好的部分都在、可以接着录」不对，这场录音下面就删掉了
                    *error = Some(NOTHING_RECORDED.into());
                } else {
                    match &ending {
                        Ending::Stopped if l.fatal => l.rec.status = Status::Failed,
                        Ending::Stopped => l.rec.status = Status::Stopped,
                        Ending::NeverStarted(_) => l.rec.status = Status::Interrupted,
                        Ending::Crashed(reason) => {
                            *error = Some(reason.clone());
                            if l.fatal {
                                l.rec.status = Status::Failed;
                            } else {
                                l.rec.status = Status::Interrupted;
                                l.rec.error = Some(reason.clone());
                            }
                        }
                    }
                    l.save();
                }
            }
            if nothing {
                // 一片都没录下来：没有可传的，留着也没用（接着录的至少有一片，不会走到这里）
                if let Some(l) = recordings.remove(&s.meeting_id) {
                    let _ = std::fs::remove_dir_all(&l.dir);
                }
            }
            if let Ending::NeverStarted(reason) = ending {
                *ended = Some((id, reason));
                // 为什么开始不了（哪一路没有权限、设备不在）：界面照着它说明怎么处理
                *start_warnings = s.warnings;
            }
        }
        self.wake.notify_one();
        self.session_changed();
        self.changed();
    }

    /// 等正在录的这一场（id）满足条件，最多等 timeout。会话没了时 done 收到 None。
    async fn wait_session(
        &self,
        id: u64,
        timeout: Duration,
        done: impl Fn(Option<&Session>) -> bool,
    ) -> bool {
        let mut changes = self.session_changes.subscribe();
        let deadline = Instant::now() + timeout;
        loop {
            let ok = {
                let inner = self.inner.lock().unwrap();
                done(inner.session.as_ref().filter(|s| s.id == id))
            };
            if ok {
                return true;
            }
            match tokio::time::timeout_at(deadline, changes.changed()).await {
                Ok(Ok(())) => {}
                _ => return false,
            }
        }
    }

    fn kill(&self, id: u64) {
        let mut inner = self.inner.lock().unwrap();
        if let Some(s) = inner.session.as_mut().filter(|s| s.id == id) {
            if let Some(kill) = s.kill.take() {
                s.killed = true;
                let _ = kill.send(());
            }
        }
    }

    pub async fn pause(&self) -> Result<RecorderState, String> {
        self.toggle(Phase::Recording, Command::Pause, Phase::Paused)
            .await
    }

    pub async fn resume(&self) -> Result<RecorderState, String> {
        self.toggle(Phase::Paused, Command::Resume, Phase::Recording)
            .await
    }

    /// 暂停或继续：发命令，等录音程序确认。已经是想要的状态就什么也不做。
    async fn toggle(
        &self,
        from: Phase,
        command: Command,
        to: Phase,
    ) -> Result<RecorderState, String> {
        let id = {
            let inner = self.inner.lock().unwrap();
            let s = inner.session.as_ref().ok_or(NOT_RECORDING)?;
            if s.phase == to {
                None
            } else if s.phase != from {
                return Err(match s.phase {
                    Phase::Starting => "录音还没开始，请稍等",
                    Phase::Stopping => "录音正在结束",
                    _ => NOT_RECORDING,
                }
                .into());
            } else {
                let _ = s.commands.send(command);
                Some(s.id)
            }
        };
        if let Some(id) = id {
            let answered = self
                .wait_session(id, COMMAND_TIMEOUT, |s| s.is_none_or(|s| s.phase != from))
                .await;
            if !answered {
                return Err(NO_RESPONSE.into());
            }
        }
        Ok(self.state())
    }

    /// 标记重点：记下现在录到第几秒（暂停时也可以标）。连着点的只算一次：离已有的哪个标记不到 1 秒都不再记。
    pub fn mark(&self) -> Result<RecorderState, String> {
        {
            let mut inner = self.inner.lock().unwrap();
            let Inner {
                session,
                recordings,
                ..
            } = &mut *inner;
            let s = session
                .as_ref()
                .filter(|s| matches!(s.phase, Phase::Recording | Phase::Paused))
                .ok_or(NOT_RECORDING)?;
            let l = recordings.get_mut(&s.meeting_id).ok_or(NOT_RECORDING)?;
            let at = ((s.base_seconds + s.seconds) * 10.0).round() / 10.0;
            if l.rec.marks.len() >= MAX_MARKS {
                return Err(format!("标记的重点太多了，最多 {MAX_MARKS} 个"));
            }
            if l.rec.marks.iter().all(|m| (at - m).abs() >= 1.0) {
                l.rec.marks.push(at);
                l.marks_dirty = true;
                l.save();
            }
        }
        self.wake.notify_one();
        self.changed();
        Ok(self.state())
    }

    /// 结束录音：等录音程序写完最后一片。录好的接着在后台传，传完告诉服务器开始处理。
    pub async fn stop(&self) -> Result<RecorderState, String> {
        self.end(self.opts.stop_timeout).await;
        Ok(self.state())
    }

    /// 退出 App 前：结束正在录的，和点了结束一样等录音程序写完最后一片（最多 stop_timeout），还不退出就强制结束
    /// （这场录音算中断）。没传完的下次打开 App 接着传。
    pub async fn shutdown(&self) {
        self.end(self.opts.stop_timeout).await;
    }

    async fn end(&self, timeout: Duration) {
        let id = {
            let mut inner = self.inner.lock().unwrap();
            let Some(s) = inner.session.as_mut() else {
                return;
            };
            s.request_stop()
        };
        self.session_changed();
        self.changed();
        self.wait_ended(id, timeout).await;
    }

    /// 等录音程序（会话 id）退出，最多等 timeout，还不退出就强制结束。
    async fn wait_ended(&self, id: u64, timeout: Duration) {
        if !self.wait_session(id, timeout, |s| s.is_none()).await {
            log::warn!("录音程序 {timeout:?} 内没有结束，强制结束");
            self.kill(id);
            self.wait_session(id, KILL_WAIT, |s| s.is_none()).await;
        }
    }

    /// 中断了的录音不接着录了：把录好的传完，告诉服务器开始处理。传不上去的录音也可以用它再试一次。
    /// 和接着录一样，先问服务器收到了哪几片：recording.json 记少了的话，complete 的片数要算上服务器上、
    /// 这台电脑上已有的（不然服务器会说收到的分片比声明的多），时长也要算上，丢掉的标记才对。
    /// 断网时接着录的部分和服务器上已有的分片用了同样的序号时不传（会覆盖服务器上的录音），返回原因。
    pub async fn finish(&self, meeting_id: &str) -> Result<RecorderState, String> {
        let account = self.core.account().ok_or("请先登录")?;
        if finishable(&mut self.inner.lock().unwrap(), meeting_id, &account)?.is_none() {
            return Ok(self.state());
        }
        let received = self.received_chunks(meeting_id, &account).await?;
        let finished = {
            let mut inner = self.inner.lock().unwrap();
            // 等服务器回话的时候可能变了（接着录了、删掉了、已经结束了）：再看一遍
            let finished = match finishable(&mut inner, meeting_id, &account)? {
                Some(l) => match received.as_deref().and_then(|r| l.taken_on_server(r)) {
                    // 上传任务马上照常传完前面的分片，再核对、把它算成传不上去
                    Some(reason) => {
                        l.ready();
                        Err(reason)
                    }
                    None => {
                        l.catch_up(
                            received.as_deref().unwrap_or_default(),
                            self.opts.chunk_seconds,
                        );
                        // 中断前最后一片之后点的标记不要了；服务器上存过的话也换掉，complete 带的标记是空的时
                        // 服务器会留着原来的
                        l.drop_lost_marks();
                        l.rec.status = Status::Stopped;
                        l.rec.error = None;
                        // 从头核对、再告诉服务器传完了：以前告诉过的不算（见 Recording::complete_sent）
                        l.rec.complete_sent = false;
                        l.fatal = false;
                        l.ready();
                        l.save();
                        Ok(true)
                    }
                },
                None => Ok(false),
            };
            if finished == Ok(true) {
                inner.error = None;
            }
            finished
        };
        self.wake.notify_one();
        self.changed();
        finished.map(|_| self.state())
    }

    /// 删掉一段中断了的、传不上去的录音（只删这台电脑上的，服务器上已经收到的部分不动）。
    pub fn discard(&self, meeting_id: &str) -> Result<RecorderState, String> {
        let account = self.core.account().ok_or("请先登录")?;
        {
            let mut inner = self.inner.lock().unwrap();
            if inner
                .session
                .as_ref()
                .is_some_and(|s| s.meeting_id == meeting_id)
            {
                return Err("这场会议正在录音，请先结束录音".into());
            }
            let l = inner
                .recordings
                .get(meeting_id)
                .filter(|l| l.rec.belongs_to(&account.server, &account.user_id))
                .ok_or(NOT_HERE)?;
            if !matches!(l.rec.status, Status::Interrupted | Status::Failed) {
                return Err("只能删掉中断了的、传不上去的录音".into());
            }
            if let Err(e) = std::fs::remove_dir_all(&l.dir) {
                if e.kind() != std::io::ErrorKind::NotFound {
                    return Err(format!("这段录音没能删掉（{e}），请再试一次"));
                }
            }
            inner.recordings.remove(meeting_id);
            inner.error = None;
        }
        self.changed();
        Ok(self.state())
    }

    /// 装好新版本以后要重启 App：现在不能重启的原因（见 RecorderState::update_blocker），能的话是 None。
    pub fn update_blocker(&self) -> Option<String> {
        self.state().update_blocker()
    }

    /// 要装新版本了：现在能重启的话，从现在起不让开始录音（免得刚开始录就被重启打断），直到 App 重启或者
    /// release_update_hold；不能的话返回原因。看能不能和占住是一起做的，中间不会有人开始录音。
    pub fn hold_for_update(&self) -> Result<(), String> {
        let account = self.core.account();
        let mut inner = self.inner.lock().unwrap();
        if let Some(reason) = snapshot(&inner, account.as_ref()).update_blocker() {
            return Err(reason);
        }
        inner.updating = true;
        Ok(())
    }

    /// 新版本没装上：又可以开始录音了。
    pub fn release_update_hold(&self) {
        self.inner.lock().unwrap().updating = false;
    }
}

impl Local {
    fn new(rec: Recording, dir: PathBuf) -> Self {
        Self {
            // 中断了的录音，标记不一定存到服务器上了：再存一次
            marks_dirty: rec.status == Status::Interrupted && !rec.marks.is_empty(),
            rec,
            dir,
            attempts: 0,
            resends: 0,
            retry_at: None,
            trouble: None,
            fatal: false,
        }
    }

    /// 中断前最后一片之后点的标记：对应的那段录音没存下来，不要了。服务器上存的那份也要换掉。
    fn drop_lost_marks(&mut self) {
        let seconds = self.rec.seconds;
        let before = self.rec.marks.len();
        self.rec.marks.retain(|t| *t <= seconds);
        if self.rec.marks.len() != before {
            self.marks_dirty = true;
        }
    }

    /// 接着录、直接结束之前：这台电脑上、服务器上（received）有 recording.json 没记上的分片时（比如 recording.json
    /// 退回了上一份），下一片的序号挪到它们后面（见 local::resume_seq），绝不覆盖已有的分片，complete 的片数也对；
    /// 跳过的分片的时长也算上，新标记的时间、丢掉哪些标记、录满 6 小时自动结束都和服务器上接起来的录音对得上。
    /// 这台电脑上的分片照着文件算；只在服务器上的量不了，按每片 10 秒算（见 local::skipped_seconds），
    /// 其中有暂停时的短片的话会差一点。
    /// 跳过的分片服务器上有的，上传时会认出来；都没有的话这段录音缺了一块，上传时会说明。
    fn catch_up(&mut self, received: &[u32], chunk_seconds: u32) {
        let next = local::resume_seq(&self.dir, &self.rec, received);
        if next <= self.rec.next_seq {
            return;
        }
        let skipped = local::skipped_seconds(&self.dir, self.rec.next_seq, next, chunk_seconds);
        log::warn!(
            "{}：recording.json 记的下一片序号是 {}，这台电脑或服务器上已经有更靠后的分片，从序号 {next} 接着算（跳过的分片约 {skipped:.1} 秒）",
            self.rec.meeting_id,
            self.rec.next_seq
        );
        self.rec.next_seq = next;
        self.rec.seconds += skipped;
    }

    /// 存 recording.json。没存上（比如磁盘满了）返回 false。
    fn save(&self) -> bool {
        match local::save(&self.dir, &self.rec) {
            Ok(()) => true,
            Err(e) => {
                log::error!("{} 没能保存：{e}", self.dir.display());
                false
            }
        }
    }

    /// 上一次成功了（或者该马上再试了）：清掉失败的记录。
    fn ready(&mut self) {
        self.attempts = 0;
        self.retry_at = None;
        self.trouble = None;
    }

    fn backoff(&mut self, reason: String, waits: &[Duration]) {
        self.retry_later(self.attempts, reason, waits);
        self.attempts += 1;
    }

    /// 第 n 次（从 0 算起）没成功：等 waits[n] 再试，后面一直按最后一个间隔。
    fn retry_later(&mut self, n: usize, reason: String, waits: &[Duration]) {
        let wait = waits
            .get(n)
            .or(waits.last())
            .copied()
            .unwrap_or(Duration::from_secs(30));
        self.retry_at = Some(Instant::now() + wait);
        self.trouble = Some(reason);
    }

    /// 告诉服务器传完之前，服务器说没有已经「传上去」了的第 seq 片（那次的「成功」不是服务器回的，比如中间的设备冒充
    /// 服务器回了话）：从这一片起，用这台电脑上留着的分片重传。等一会儿再传；一次次都这样的话等得越来越久，
    /// 传成功了也不从头算，免得一直重传。
    fn resend_from(&mut self, seq: u32, waits: &[Duration]) {
        self.rec.uploaded = seq;
        // 服务器又缺了分片：以前核对过、告诉过服务器传完了也不算了，传完、再核对过才算（见 Recording::complete_sent）
        self.rec.complete_sent = false;
        self.save();
        let reason = format!(
            "服务器上缺第 {} 片录音，稍后会自动重传。一直这样的话，看看连的网络是不是要先登录，或者请管理员检查服务器的设置",
            seq + 1
        );
        self.retry_later(self.resends, reason, waits);
        self.resends += 1;
    }

    /// 断网时接着录的部分（unverified_from 起）用的序号，服务器上是不是已经有了：有的话返回给用户看的原因。
    /// 核对过之前这些序号这台电脑一片也没传过（见 queue.rs），服务器上有的都不是这场录音传的，传上去会把它们覆盖掉。
    /// 还在录的话，后面录的分片也会排到这些序号上。只有上传任务据此把录音算成传不上去（在那之前把前面的分片传完），
    /// 接着录、直接结束时据此拒绝。
    /// 这场会议只能在会议页面上用服务器已经收到的部分结束（web/src/pages/MeetingDetail.tsx 的 ReceivedSoFar，
    /// 在这台电脑的那一行下面）：原因里的按钮名字、什么时候才有这个按钮（服务器 90 秒没再收到分片，
    /// web/src/lib/unfinishedRecording.ts 的 RECORDING_STALE_MS）要和那里一致。
    fn taken_on_server(&self, received: &[u32]) -> Option<String> {
        let from = self.rec.unverified_from?;
        let seq = received.iter().copied().filter(|seq| *seq >= from).min()?;
        Some(format!(
            "服务器上已经有这场会议的第 {} 片录音，这台电脑上却没有记录；断网时接着录的部分也排到了这里，传上去会把服务器上的录音覆盖掉，所以没有传，录下的都还留在这台电脑上。可以在会议页面上点「用已经收到的部分生成纪要」，先用服务器上的录音生成纪要（服务器 90 秒没再收到录音以后才有这个按钮）",
            seq + 1
        ))
    }

    /// 传不上去了。还在录的话录音照常进行，录完算失败。
    fn fail(&mut self, reason: String) {
        self.ready();
        self.rec.error = Some(reason);
        if self.rec.status == Status::Recording {
            self.fatal = true;
        } else {
            self.rec.status = Status::Failed;
        }
        self.save();
    }

    fn upload(&self) -> Upload {
        let rec = &self.rec;
        let status = match rec.status {
            // 还在录，但已经传不上去了（会议被删了、没有权限了）：界面要说清楚，别当成暂时的
            Status::Recording if self.fatal => UploadStatus::Failed,
            Status::Recording => UploadStatus::Recording,
            Status::Interrupted => UploadStatus::Interrupted,
            Status::Completed => UploadStatus::Completed,
            Status::Failed => UploadStatus::Failed,
            Status::Stopped if self.trouble.is_some() => UploadStatus::Waiting,
            Status::Stopped if rec.uploaded < rec.next_seq => UploadStatus::Uploading,
            Status::Stopped => UploadStatus::Completing,
        };
        let error = match rec.status {
            Status::Completed => None,
            Status::Failed | Status::Interrupted => rec.error.clone(),
            Status::Recording | Status::Stopped => {
                rec.error.clone().or_else(|| self.trouble.clone())
            }
        };
        Upload {
            meeting_id: rec.meeting_id.clone(),
            title: rec.title.clone(),
            recorded: rec.next_seq,
            uploaded: rec.uploaded,
            status,
            error,
        }
    }
}

fn snapshot(inner: &Inner, account: Option<&Account>) -> RecorderState {
    let mut state = RecorderState {
        status: Phase::Idle,
        meeting_id: None,
        title: None,
        seconds: 0.0,
        level: None,
        mic: None,
        system: false,
        echo: None,
        mic_gain: None,
        marks: Vec::new(),
        warnings: inner.start_warnings.clone(),
        error: inner.error.clone(),
        uploads: Vec::new(),
    };
    if let Some(s) = &inner.session {
        state.status = s.phase;
        // 别的账号在录的（它的登录失效以后换了人登录，还没结束）：只说这台电脑在录音，不给看是哪场会议、
        // 录了多久、标了什么。没人登录时照常给（菜单栏上要显示录了多久，登录页要提醒正在录音）
        if account.is_none_or(|a| s.belongs_to(a)) {
            state.meeting_id = Some(s.meeting_id.clone());
            state.seconds = s.base_seconds + s.seconds;
            state.level = s.level;
            state.mic = s.mic.clone();
            state.system = s.system;
            state.echo = s.echo_mode.map(|mode| Echo {
                mode,
                active: s.echo_active,
                reason: s.echo_reason.clone(),
            });
            state.mic_gain = s.mic_gain;
            state.warnings = s.warnings.clone();
            if let Some(l) = inner.recordings.get(&s.meeting_id) {
                state.title = Some(l.rec.title.clone());
                state.marks = l.rec.marks.clone();
            }
        }
    }
    // 只给看当前账号的录音：同一台电脑上别人的会议标题也不显示
    if let Some(a) = account {
        let mut mine: Vec<&Local> = inner
            .recordings
            .values()
            .filter(|l| l.rec.belongs_to(&a.server, &a.user_id))
            .collect();
        mine.sort_by_key(|l| l.rec.started_at);
        state.uploads = mine.into_iter().map(Local::upload).collect();
    }
    state
}

/// 这个账号能不能直接结束（finish）这台电脑上的这场录音。返回要结束的那一场（中断了、传不上去了）；
/// 已经在传、已经传完了的不用再做什么，返回 None。
fn finishable<'a>(
    inner: &'a mut Inner,
    meeting_id: &str,
    account: &Account,
) -> Result<Option<&'a mut Local>, String> {
    if inner
        .session
        .as_ref()
        .is_some_and(|s| s.meeting_id == meeting_id)
    {
        return Err("这场会议正在录音，请点「结束录音」".into());
    }
    let l = inner
        .recordings
        .get_mut(meeting_id)
        .filter(|l| l.rec.belongs_to(&account.server, &account.user_id))
        .ok_or(NOT_HERE)?;
    match l.rec.status {
        Status::Interrupted | Status::Failed => Ok(Some(l)),
        // 已经在传、已经传完了
        Status::Stopped | Status::Completed => Ok(None),
        Status::Recording => Err("这场会议正在录音，请点「结束录音」".into()),
    }
}

/// 读出录音目录里的每一场录音。上次 App 退出时还在录的算中断；已经完成的、一片都没录下来的删掉。
fn load_all(root: &Path) -> BTreeMap<String, Local> {
    let mut out = BTreeMap::new();
    let Ok(entries) = std::fs::read_dir(root) else {
        return out;
    };
    for entry in entries.flatten() {
        let dir = entry.path();
        let name = entry.file_name().to_string_lossy().into_owned();
        if !dir.is_dir() || !local::valid_meeting_id(&name) {
            continue;
        }
        let mut rec = match local::load(&dir) {
            Ok(rec) if rec.meeting_id == name => rec,
            _ => {
                // 不知道是谁录的、该传到哪台服务器，传不了；录好的分片也不删，留着排查
                let kept = if local::highest_chunk(&dir).is_some() {
                    "，录好的分片还留在里面"
                } else {
                    ""
                };
                log::warn!("{} 不是一段能用的录音，跳过{kept}", dir.display());
                continue;
            }
        };
        local::remove_partials(&dir);
        let mut changed = local::recover(&dir, &mut rec);
        if rec.status == Status::Recording {
            rec.status = Status::Interrupted;
            rec.error = Some(APP_QUIT.into());
            changed = true;
        }
        let finished = rec.status == Status::Completed;
        let empty = rec.status == Status::Interrupted && rec.next_seq == 0;
        if finished || empty {
            let _ = std::fs::remove_dir_all(&dir);
            continue;
        }
        if changed {
            if let Err(e) = local::save(&dir, &rec) {
                log::error!("{} 没能保存：{e}", dir.display());
            }
        }
        out.insert(name, Local::new(rec, dir));
    }
    out
}

/// 录音程序还没开始录就退出了，又没说为什么。
fn start_failed(status: Option<ExitStatus>) -> String {
    match status.and_then(|s| s.code()) {
        Some(code) => format!("录音程序没能开始录音（退出码 {code}），请再试一次"),
        None => "录音程序没能开始录音，请再试一次".into(),
    }
}
