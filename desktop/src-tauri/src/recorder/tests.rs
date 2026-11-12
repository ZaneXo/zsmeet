//! 录音的测试：假的录音程序（现写的一个 sh 脚本，按约定说话、写分片文件，开始后马上录好几片）
//! 和假的服务器（记下收到的分片、标记、complete）。不碰麦克风，不弹系统的权限提示，不碰钥匙串。

use std::collections::{BTreeMap, HashMap};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Duration;

use axum::body::Bytes;
use axum::extract::{Path as Params, State};
use axum::http::{header, HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post, put};
use axum::{Json, Router};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use tokio::net::TcpListener;
use tokio::task::JoinHandle;

use super::local::tests::adts;
use super::local::{chunk_path, Recording, Status};
use super::*;
use crate::core::testing::Fixture;
use crate::server;

// ---- 假的录音程序 ----

/// 假录音程序怎么表现。
#[derive(Default)]
struct Script {
    /// 开始以后马上录好几片（每片 10 秒），然后等命令
    chunks: u32,
    /// 录好这几片以后直接退出（模拟崩溃），不报 stopped
    crash: bool,
    /// 还没开始录就报错退出：错误码、给用户看的原因
    start_error: Option<(&'static str, &'static str)>,
    /// 开始以后报一个提醒：错误码、给用户看的原因（有 start_error 时在报错之前报）
    warning: Option<(&'static str, &'static str)>,
}

/// 假的 zsmeet-recorder：按约定的协议说话，录音时写 `<序号>.aac`（内容是 audio-<序号>）。
/// 每个测试的表现写在测试目录（录音目录的上一级）的 fake.conf 里，每次运行的参数记在同一个目录的 helper-args.log 里。
const FAKE_HELPER: &str = r#"#!/bin/sh
if [ "$1" = devices ]; then
  echo '{"microphones":[{"uid":"BuiltInMic","name":"MacBook Pro 麦克风","default":true},{"uid":"usb-1","name":"USB 麦克风","default":false}]}'
  exit 0
fi
args="$*"
out=""; seq=0; total=0
while [ $# -gt 0 ]; do
  case "$1" in
    --out) out="$2"; shift 2 ;;
    --start-seq) seq="$2"; shift 2 ;;
    *) shift ;;
  esac
done
dir="${out%/*}"; dir="${dir%/*}"
CHUNKS=0; CRASH=0; START_ERROR=""; WARNING=""
[ -f "$dir/fake.conf" ] && . "$dir/fake.conf"
echo "$args" >> "$dir/helper-args.log"
chunk() {
  printf 'audio-%s' "$seq" > "$out/$seq.aac.part"
  mv "$out/$seq.aac.part" "$out/$seq.aac"
  total=$((total + $1))
  echo "{\"event\":\"chunk\",\"seq\":$seq,\"path\":\"$out/$seq.aac\",\"bytes\":7,\"seconds\":$1}"
  echo "{\"event\":\"level\",\"seconds\":$total,\"mic\":0.5,\"system\":0.25}"
  seq=$((seq + 1))
}
if [ -n "$START_ERROR" ]; then
  [ -n "$WARNING" ] && echo "$WARNING"
  echo "$START_ERROR"
  exit 2
fi
echo '{"event":"started","sample_rate":48000,"channels":1,"mic":"MacBook Pro 麦克风","system":true}'
if [ -n "$WARNING" ]; then
  echo "$WARNING"
fi
i=0
while [ $i -lt "$CHUNKS" ]; do
  chunk 10
  i=$((i + 1))
done
if [ "$CRASH" = 1 ]; then
  exit 3
fi
while IFS= read -r line; do
  case "$line" in
    *'"pause"'*) echo '{"event":"paused"}' ;;
    *'"resume"'*) echo '{"event":"resumed"}' ;;
    *'"stop"'*) break ;;
  esac
done
chunk 4
echo "{\"event\":\"stopped\",\"next_seq\":$seq,\"seconds\":$total}"
"#;

/// 所有测试共用一个脚本文件，放在测试程序旁边（target 目录里），内容不变就一直用它：
/// macOS 第一次运行一个新的可执行文件要先检查半秒左右，而且是排着队一个一个检查的，
/// 每个测试各写一个的话，并行的测试要等好几秒。
fn shared_helper() -> PathBuf {
    static HELPER: OnceLock<PathBuf> = OnceLock::new();
    HELPER
        .get_or_init(|| {
            let digest = hex::encode(&Sha256::digest(FAKE_HELPER.as_bytes())[..6]);
            let exe = std::env::current_exe().unwrap();
            let path = exe
                .parent()
                .unwrap()
                .join(format!("fake-zsmeet-recorder-{digest}"));
            if !path.exists() {
                let tmp = path.with_extension(format!("{}.tmp", std::process::id()));
                std::fs::write(&tmp, FAKE_HELPER).unwrap();
                #[cfg(unix)]
                {
                    use std::os::unix::fs::PermissionsExt;
                    std::fs::set_permissions(&tmp, std::fs::Permissions::from_mode(0o755)).unwrap();
                }
                std::fs::rename(&tmp, &path).unwrap();
            }
            path
        })
        .clone()
}

/// 写好这个测试里假录音程序的表现（dir 是测试目录），返回假录音程序的路径。
fn fake_helper(dir: &Path, script: &Script) -> PathBuf {
    let mut conf = format!(
        "CHUNKS={}\nCRASH={}\n",
        script.chunks,
        u8::from(script.crash)
    );
    if let Some((code, message)) = script.start_error {
        let event = json!({ "event": "error", "code": code, "message": message });
        conf.push_str(&format!("START_ERROR='{event}'\n"));
    }
    if let Some((code, message)) = script.warning {
        let event = json!({ "event": "warning", "code": code, "message": message });
        conf.push_str(&format!("WARNING='{event}'\n"));
    }
    std::fs::write(dir.join("fake.conf"), conf).unwrap();
    shared_helper()
}

fn helper_runs(dir: &Path) -> Vec<String> {
    std::fs::read_to_string(dir.join("helper-args.log"))
        .unwrap_or_default()
        .lines()
        .map(str::to_string)
        .collect()
}

// ---- 假的服务器 ----

/// 收到的一次分片上传。
#[derive(Clone, Debug)]
struct Put {
    meeting: String,
    seq: u32,
    body: Vec<u8>,
    content_type: Option<String>,
    auth: Option<String>,
}

#[derive(Default)]
struct Mock {
    /// 成功收下的分片，按收到的顺序
    chunks: Mutex<Vec<Put>>,
    /// 每一次分片上传的请求（包括返回错误的）：会议、序号、Authorization
    attempts: Mutex<Vec<(String, u32, Option<String>)>>,
    marks: Mutex<Vec<(String, Value)>>,
    completes: Mutex<Vec<(String, Value)>>,
    /// 按会议固定返回的错误
    chunk_error: Mutex<HashMap<String, u16>>,
    complete_error: Mutex<HashMap<String, (u16, String)>>,
    /// GET 会议时的 status，默认 uploading
    meeting_status: Mutex<HashMap<String, String>>,
    /// 浏览器登录换令牌时返回谁
    login_user: Mutex<Value>,
}

impl Mock {
    fn chunks_of(&self, meeting: &str) -> Vec<Put> {
        let chunks = self.chunks.lock().unwrap();
        chunks
            .iter()
            .filter(|c| c.meeting == meeting)
            .cloned()
            .collect()
    }

    fn seqs(&self, meeting: &str) -> Vec<u32> {
        self.chunks_of(meeting).iter().map(|c| c.seq).collect()
    }

    fn completes_of(&self, meeting: &str) -> Vec<Value> {
        let completes = self.completes.lock().unwrap();
        completes
            .iter()
            .filter(|(m, _)| m == meeting)
            .map(|(_, body)| body.clone())
            .collect()
    }
}

fn auth(headers: &HeaderMap) -> Option<String> {
    headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .map(str::to_string)
}

fn problem(status: u16, detail: &str) -> Response {
    (
        StatusCode::from_u16(status).unwrap(),
        Json(json!({ "status": status, "detail": detail })),
    )
        .into_response()
}

async fn mock_meeting(State(m): State<Arc<Mock>>, Params(id): Params<String>) -> Json<Value> {
    let status = m
        .meeting_status
        .lock()
        .unwrap()
        .get(&id)
        .cloned()
        .unwrap_or_else(|| "uploading".into());
    Json(json!({ "id": id, "title": format!("周会 {id}"), "status": status }))
}

async fn mock_chunk(
    State(m): State<Arc<Mock>>,
    Params((id, seq)): Params<(String, u32)>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    m.attempts
        .lock()
        .unwrap()
        .push((id.clone(), seq, auth(&headers)));
    if let Some(status) = m.chunk_error.lock().unwrap().get(&id).copied() {
        return problem(status, "不行");
    }
    m.chunks.lock().unwrap().push(Put {
        meeting: id,
        seq,
        body: body.to_vec(),
        content_type: headers
            .get(header::CONTENT_TYPE)
            .and_then(|v| v.to_str().ok())
            .map(str::to_string),
        auth: auth(&headers),
    });
    StatusCode::NO_CONTENT.into_response()
}

/// 服务器收到了哪几片：和服务器一样，返回从小到大、不重复的序号。
async fn mock_received(State(m): State<Arc<Mock>>, Params(id): Params<String>) -> Json<Value> {
    let mut seqs = m.seqs(&id);
    seqs.sort_unstable();
    seqs.dedup();
    Json(json!({ "received": seqs }))
}

async fn mock_marks(
    State(m): State<Arc<Mock>>,
    Params(id): Params<String>,
    Json(body): Json<Value>,
) -> StatusCode {
    m.marks.lock().unwrap().push((id, body));
    StatusCode::NO_CONTENT
}

async fn mock_complete(
    State(m): State<Arc<Mock>>,
    Params(id): Params<String>,
    Json(body): Json<Value>,
) -> Response {
    if let Some((status, detail)) = m.complete_error.lock().unwrap().get(&id).cloned() {
        return problem(status, &detail);
    }
    m.completes.lock().unwrap().push((id.clone(), body));
    // 和服务器一样，返回整场会议
    let title = format!("周会 {id}");
    Json(json!({ "id": id, "title": title, "status": "queued" })).into_response()
}

/// 浏览器登录换令牌：令牌是 zsm_ 加上授权码，用户是 login_user。
async fn mock_token(State(m): State<Arc<Mock>>, Json(body): Json<Value>) -> Json<Value> {
    let code = body["code"].as_str().unwrap_or_default();
    Json(json!({ "token": format!("zsm_{code}"), "user": m.login_user.lock().unwrap().clone() }))
}

fn app(mock: Arc<Mock>) -> Router {
    Router::new()
        .route("/api/meetings/{id}", get(mock_meeting))
        .route("/api/meetings/{id}/chunks", get(mock_received))
        .route("/api/meetings/{id}/chunks/{seq}", put(mock_chunk))
        .route("/api/meetings/{id}/marks", put(mock_marks))
        .route("/api/meetings/{id}/complete", post(mock_complete))
        .route("/api/apps/token", post(mock_token))
        .route(
            "/api/apps/logout",
            post(|| async { StatusCode::NO_CONTENT }),
        )
        .with_state(mock)
}

async fn serve(listener: TcpListener, mock: Arc<Mock>) -> JoinHandle<()> {
    tokio::spawn(async move {
        axum::serve(listener, app(mock)).await.unwrap();
    })
}

// ---- 测试用的 Recorder ----

/// 记下发给界面的每一个状态。
#[derive(Default)]
struct Seen(Mutex<Vec<RecorderState>>);

impl RecorderEvents for Seen {
    fn state(&self, state: &RecorderState) {
        self.0.lock().unwrap().push(state.clone());
    }
}

impl Seen {
    /// 依次经过了哪些录音状态（连着重复的只算一次）。
    fn phases(&self) -> Vec<Phase> {
        let mut phases: Vec<Phase> = self.0.lock().unwrap().iter().map(|s| s.status).collect();
        phases.dedup();
        phases
    }
}

struct Rig {
    f: Fixture,
    mock: Arc<Mock>,
    server: String,
    root: PathBuf,
    helper: PathBuf,
    recorder: Arc<Recorder>,
    seen: Arc<Seen>,
    run: JoinHandle<()>,
}

/// 等的时间都调短：上传失败以后几十毫秒就再试。
fn options(root: &Path, helper: &Path) -> RecorderOptions {
    RecorderOptions {
        root: root.to_path_buf(),
        helper: Some(helper.to_path_buf()),
        chunk_seconds: CHUNK_SECONDS,
        retry_waits: vec![Duration::from_millis(20), Duration::from_millis(50)],
        start_timeout: Duration::from_secs(20),
        stop_timeout: Duration::from_secs(10),
        input_file: None,
        realtime: false,
    }
}

fn launch(f: &Fixture, root: &Path, helper: &Path) -> (Arc<Recorder>, Arc<Seen>, JoinHandle<()>) {
    launch_with(f, options(root, helper))
}

fn launch_with(f: &Fixture, opts: RecorderOptions) -> (Arc<Recorder>, Arc<Seen>, JoinHandle<()>) {
    let seen = Arc::new(Seen::default());
    let recorder = Recorder::new(opts, f.core.clone(), seen.clone(), server::http_client());
    let run = tokio::spawn(recorder.clone().run());
    (recorder, seen, run)
}

/// 起假服务器，以 u1（令牌 zsm_test）登录，写好假的录音程序，起 Recorder。
async fn rig(script: Script) -> Rig {
    let mock = Arc::new(Mock::default());
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let server = format!("http://{}", listener.local_addr().unwrap());
    serve(listener, mock.clone()).await;
    rig_with(mock, server, script)
}

fn rig_with(mock: Arc<Mock>, server: String, script: Script) -> Rig {
    let f = Fixture::signed_in(&server, "http://127.0.0.1:1");
    let root = f.dir.path().join("recordings");
    let helper = fake_helper(f.dir.path(), &script);
    let (recorder, seen, run) = launch(&f, &root, &helper);
    Rig {
        f,
        mock,
        server,
        root,
        helper,
        recorder,
        seen,
        run,
    }
}

impl Rig {
    /// 模拟 App 重启：停掉原来的 Recorder，照着同一个目录重新起一个。
    fn restart(&mut self) {
        self.run.abort();
        let (recorder, seen, run) = launch(&self.f, &self.root, &self.helper);
        (self.recorder, self.seen, self.run) = (recorder, seen, run);
    }
}

/// 在本地直接写一段录音（像是上次运行时留下的）。chunks：哪几片的文件还在、内容是什么。
fn plant(root: &Path, rec: &Recording, chunks: &[(u32, Vec<u8>)]) {
    let dir = root.join(&rec.meeting_id);
    local::create_private_dir(&dir).unwrap();
    local::save(&dir, rec).unwrap();
    for (seq, body) in chunks {
        std::fs::write(chunk_path(&dir, *seq), body).unwrap();
    }
}

fn recording(server: &str, user: &str, meeting: &str, status: Status) -> Recording {
    Recording {
        meeting_id: meeting.into(),
        server: server.into(),
        user_id: user.into(),
        title: format!("会议 {meeting}"),
        started_at: 1_700_000_000_000,
        next_seq: 1,
        uploaded: 0,
        marks: vec![],
        seconds: 10.0,
        status,
        error: None,
    }
}

fn upload<'a>(state: &'a RecorderState, meeting: &str) -> Option<&'a Upload> {
    state.uploads.iter().find(|u| u.meeting_id == meeting)
}

fn upload_is(state: &RecorderState, meeting: &str, status: UploadStatus) -> bool {
    upload(state, meeting).is_some_and(|u| u.status == status)
}

/// 等到状态满足条件，最多 10 秒。
async fn wait_for(
    recorder: &Recorder,
    what: &str,
    done: impl Fn(&RecorderState) -> bool,
) -> RecorderState {
    let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
    loop {
        let state = recorder.state();
        if done(&state) {
            return state;
        }
        assert!(
            tokio::time::Instant::now() < deadline,
            "等了 10 秒还没有{what}：{state:#?}"
        );
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}

async fn until(what: &str, done: impl Fn() -> bool) {
    let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
    while !done() {
        assert!(
            tokio::time::Instant::now() < deadline,
            "等了 10 秒还没有{what}"
        );
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}

/// 走一遍浏览器登录，授权码是 code（换出的令牌是 zsm_{code}），登录的是 Mock::login_user。
async fn sign_in(f: &Fixture, code: &str) {
    f.core.login_start().unwrap();
    let opened = f.events.opened.lock().unwrap().last().unwrap().clone();
    let state = url::Url::parse(&opened)
        .unwrap()
        .query_pairs()
        .find(|(k, _)| k == "state")
        .unwrap()
        .1
        .into_owned();
    f.core
        .handle_deep_link(&format!("zsmeet://auth?code={code}&state={state}"))
        .await
        .unwrap();
}

// ---- 测试 ----

#[tokio::test]
async fn records_uploads_in_order_and_completes() {
    let r = rig(Script {
        chunks: 2,
        ..Script::default()
    })
    .await;

    let state = r
        .recorder
        .start("m1", Some("default".into()), true)
        .await
        .unwrap();
    assert_eq!(state.status, Phase::Recording);
    assert_eq!(state.meeting_id.as_deref(), Some("m1"));
    assert_eq!(state.title.as_deref(), Some("周会 m1"));
    assert_eq!(state.mic.as_deref(), Some("MacBook Pro 麦克风"));
    assert!(state.system);
    assert_eq!(
        helper_runs(r.f.dir.path()),
        [format!(
            "record --out {} --start-seq 0 --chunk-seconds 10 --mic default",
            r.root.join("m1").display()
        )]
    );
    // 录音只有自己能看
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        for dir in [r.root.clone(), r.root.join("m1")] {
            let mode = std::fs::metadata(&dir).unwrap().permissions().mode();
            assert_eq!(mode & 0o777, 0o700, "{}", dir.display());
        }
    }

    // 录好的两片边录边传，传完的删掉
    let state = wait_for(&r.recorder, "传完前两片", |s| {
        upload(s, "m1").is_some_and(|u| u.uploaded == 2)
    })
    .await;
    assert!(upload_is(&state, "m1", UploadStatus::Recording));
    assert_eq!(state.seconds, 20.0);
    assert_eq!(
        state.level,
        Some(Level {
            mic: 0.5,
            system: 0.25
        })
    );
    assert!(!chunk_path(&r.root.join("m1"), 0).exists());

    // 标记重点：连着点的只算一次
    assert_eq!(r.recorder.mark().unwrap().marks, vec![20.0]);
    assert_eq!(r.recorder.mark().unwrap().marks, vec![20.0]);
    assert_eq!(r.recorder.pause().await.unwrap().status, Phase::Paused);
    assert_eq!(r.recorder.pause().await.unwrap().status, Phase::Paused);
    assert_eq!(r.recorder.resume().await.unwrap().status, Phase::Recording);
    // 一次只录一场
    let err = r.recorder.start("m2", None, true).await.unwrap_err();
    assert!(err.contains("另一场"), "{err}");

    // 结束：最后一片写完，全部传完以后告诉服务器开始处理
    let state = r.recorder.stop().await.unwrap();
    assert_eq!(state.status, Phase::Idle);
    assert_eq!(state.meeting_id, None);
    let state = wait_for(&r.recorder, "传完", |s| {
        upload_is(s, "m1", UploadStatus::Completed)
    })
    .await;
    let u = upload(&state, "m1").unwrap();
    assert_eq!((u.recorded, u.uploaded), (3, 3));
    assert_eq!(u.title, "周会 m1");

    let chunks = r.mock.chunks_of("m1");
    assert_eq!(
        chunks.iter().map(|c| c.seq).collect::<Vec<_>>(),
        [0, 1, 2],
        "按序号传"
    );
    for c in &chunks {
        assert_eq!(c.body, format!("audio-{}", c.seq).into_bytes());
        assert_eq!(c.content_type.as_deref(), Some("audio/aac"));
        assert_eq!(c.auth.as_deref(), Some("Bearer zsm_test"));
    }
    assert_eq!(
        r.mock.marks.lock().unwrap().clone(),
        [("m1".to_string(), json!({ "marks": [20.0] }))]
    );
    assert_eq!(
        r.mock.completes_of("m1"),
        [json!({ "chunks": 3, "marks": [20.0] })]
    );
    // 本地的录音删掉了
    assert!(!r.root.join("m1").exists());

    let phases = r.seen.phases();
    let expected = [
        Phase::Starting,
        Phase::Recording,
        Phase::Paused,
        Phase::Recording,
        Phase::Stopping,
        Phase::Idle,
    ];
    assert!(
        phases.windows(expected.len()).any(|w| w == expected),
        "{phases:?}"
    );
}

#[tokio::test]
async fn waits_while_offline_and_uploads_when_back() {
    // 服务器还没起来：先占一个端口再放掉，连过去会被拒绝
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    drop(listener);
    let mock = Arc::new(Mock::default());
    let r = rig_with(
        mock.clone(),
        format!("http://{addr}"),
        Script {
            chunks: 2,
            ..Script::default()
        },
    );

    // 连不上服务器也照样录（标题等连上了也不要紧）
    let state = r
        .recorder
        .start("m1", Some("default".into()), false)
        .await
        .unwrap();
    assert_eq!(state.status, Phase::Recording);
    assert_eq!(state.title.as_deref(), Some(""));
    r.recorder.stop().await.unwrap();
    let state = wait_for(&r.recorder, "等网络", |s| {
        upload_is(s, "m1", UploadStatus::Waiting)
    })
    .await;
    let error = upload(&state, "m1").unwrap().error.clone().unwrap();
    assert!(error.contains("连不上服务器"), "{error}");
    assert!(r.root.join("m1/0.aac").exists());

    // 服务器起来了：接着传
    let listener = TcpListener::bind(addr).await.unwrap();
    serve(listener, mock.clone()).await;
    wait_for(&r.recorder, "传完", |s| {
        upload_is(s, "m1", UploadStatus::Completed)
    })
    .await;
    let state = wait_for(&r.recorder, "传完", |s| {
        upload_is(s, "m1", UploadStatus::Completed)
    })
    .await;
    // 开始时没取到的标题，传完时从服务器的回复里补上
    assert_eq!(upload(&state, "m1").unwrap().title, "周会 m1");
    assert_eq!(mock.seqs("m1"), [0, 1, 2]);
    assert_eq!(
        mock.completes_of("m1"),
        [json!({ "chunks": 3, "marks": [] })]
    );
    assert!(
        helper_runs(r.f.dir.path())[0].ends_with("--mic default --no-system"),
        "{:?}",
        helper_runs(r.f.dir.path())
    );
}

#[tokio::test]
async fn unauthorized_signs_out_and_resumes_for_the_same_account() {
    let r = rig(Script {
        chunks: 2,
        ..Script::default()
    })
    .await;
    r.mock.chunk_error.lock().unwrap().insert("m1".into(), 401);

    r.recorder.start("m1", None, true).await.unwrap();
    // 401：这个令牌不能用了，App 退出登录；录音照常进行，录好的留在本地
    until("退出登录", || r.f.core.state().expired).await;
    assert!(!r.f.core.state().signed_in);
    let state = r.recorder.state();
    assert_eq!(state.status, Phase::Recording);
    assert!(state.uploads.is_empty(), "没登录时不显示谁的录音");
    let state = r.recorder.stop().await.unwrap();
    assert_eq!(state.status, Phase::Idle);
    assert!(r.root.join("m1/0.aac").exists());
    let tried = r.mock.attempts.lock().unwrap().len();
    tokio::time::sleep(Duration::from_millis(150)).await;
    assert_eq!(
        r.mock.attempts.lock().unwrap().len(),
        tried,
        "退出以后不再传"
    );

    // 同一个账号重新登录（新的令牌）：接着传
    r.mock.chunk_error.lock().unwrap().clear();
    *r.mock.login_user.lock().unwrap() =
        json!({"id": "u1", "name": "王小明", "email": "wang@example.com", "role": "member"});
    sign_in(&r.f, "again").await;
    wait_for(&r.recorder, "传完", |s| {
        upload_is(s, "m1", UploadStatus::Completed)
    })
    .await;
    let chunks = r.mock.chunks_of("m1");
    assert_eq!(chunks.iter().map(|c| c.seq).collect::<Vec<_>>(), [0, 1, 2]);
    assert!(chunks
        .iter()
        .all(|c| c.auth.as_deref() == Some("Bearer zsm_again")));
}

#[tokio::test]
async fn missing_meeting_fails_and_can_be_discarded() {
    let mut r = rig(Script {
        chunks: 1,
        ..Script::default()
    })
    .await;
    r.mock.chunk_error.lock().unwrap().insert("m1".into(), 404);

    r.recorder.start("m1", None, true).await.unwrap();
    // 会议在服务器上没了：录音照常进行，只是传不上去了（界面上要说清楚不是暂时的）
    let state = wait_for(&r.recorder, "发现传不上去", |s| {
        upload(s, "m1").is_some_and(|u| u.error.is_some())
    })
    .await;
    assert_eq!(state.status, Phase::Recording);
    assert!(upload_is(&state, "m1", UploadStatus::Failed));
    let state = r.recorder.stop().await.unwrap();
    let u = upload(&state, "m1").unwrap();
    assert_eq!(u.status, UploadStatus::Failed);
    assert!(
        u.error.as_deref().unwrap().contains("找不到这场会议"),
        "{u:?}"
    );
    assert!(r.mock.completes_of("m1").is_empty());
    assert!(r.root.join("m1/0.aac").exists(), "删掉之前录音还在");
    let tried = r.mock.attempts.lock().unwrap().len();
    tokio::time::sleep(Duration::from_millis(150)).await;
    assert_eq!(r.mock.attempts.lock().unwrap().len(), tried, "不再重试");

    // 重启以后还是失败的，不会再传，也不能接着录
    r.restart();
    assert!(upload_is(&r.recorder.state(), "m1", UploadStatus::Failed));
    let err = r.recorder.start("m1", None, true).await.unwrap_err();
    assert!(err.contains("传不上去"), "{err}");

    let state = r.recorder.discard("m1").unwrap();
    assert!(upload(&state, "m1").is_none());
    assert!(!r.root.join("m1").exists());
    assert!(r.recorder.discard("m1").is_err());
}

#[tokio::test]
async fn conflict_on_complete_is_done_only_if_processing_started() {
    let r = rig(Script::default()).await;
    // m1：上一次 complete 其实成功了、只是没收到回复，再提交得到 409，会议已经在处理
    r.mock
        .complete_error
        .lock()
        .unwrap()
        .insert("m1".into(), (409, "这场会议已经在处理了".into()));
    r.recorder.start("m1", None, true).await.unwrap();
    r.mock
        .meeting_status
        .lock()
        .unwrap()
        .insert("m1".into(), "queued".into());
    r.recorder.stop().await.unwrap();
    wait_for(&r.recorder, "m1 算完成", |s| {
        upload_is(s, "m1", UploadStatus::Completed)
    })
    .await;
    assert!(!r.root.join("m1").exists());

    // m2：409 是因为服务器缺了分片，会议还在等上传：算失败，把服务器说的原因告诉用户
    r.mock.complete_error.lock().unwrap().insert(
        "m2".into(),
        (409, "第 1 片还没有传上来，请补传后再试".into()),
    );
    r.recorder.start("m2", None, true).await.unwrap();
    r.recorder.stop().await.unwrap();
    let state = wait_for(&r.recorder, "m2 失败", |s| {
        upload_is(s, "m2", UploadStatus::Failed)
    })
    .await;
    assert_eq!(
        upload(&state, "m2").unwrap().error.as_deref(),
        Some("第 1 片还没有传上来，请补传后再试")
    );
}

#[tokio::test]
async fn continues_an_interrupted_recording_after_restart() {
    let mut r = rig(Script {
        chunks: 1,
        ..Script::default()
    })
    .await;
    r.run.abort();
    // 上次录音时 App 崩溃了：recording.json 还是 recording，记到了第 0 片；
    // 第 1 片（8 秒）其实也写完了但没来得及记，第 2 片没写完
    let mut rec = recording(&r.server, "u1", "m1", Status::Recording);
    rec.marks = vec![5.0, 15.0];
    plant(&r.root, &rec, &[(0, b"audio-0".to_vec()), (1, adts(375))]);
    std::fs::write(r.root.join("m1/2.aac.part"), b"half").unwrap();
    r.restart();

    let state = r.recorder.state();
    let u = upload(&state, "m1").unwrap();
    assert_eq!(u.status, UploadStatus::Interrupted);
    assert_eq!(u.recorded, 2);
    assert!(u.error.as_deref().unwrap().contains("意外退出"), "{u:?}");
    assert!(!r.root.join("m1/2.aac.part").exists());
    // 录好的先传着（接着录还是直接结束都要传），标记也存上；但还不告诉服务器结束
    wait_for(&r.recorder, "传完录好的两片", |s| {
        upload(s, "m1").is_some_and(|u| u.uploaded == 2)
    })
    .await;
    until("存好标记", || !r.mock.marks.lock().unwrap().is_empty()).await;
    assert_eq!(
        r.mock.marks.lock().unwrap()[0].1,
        json!({ "marks": [5.0, 15.0] })
    );
    assert!(r.mock.completes_of("m1").is_empty());

    // 接着录：从第 2 片开始写，秒数和标记接着上次的
    let state = r
        .recorder
        .start("m1", Some("default".into()), false)
        .await
        .unwrap();
    assert_eq!(state.status, Phase::Recording);
    assert_eq!(state.marks, vec![5.0, 15.0]);
    assert_eq!(state.title.as_deref(), Some("会议 m1"));
    assert!(upload(&state, "m1").unwrap().error.is_none());
    assert!(
        helper_runs(r.f.dir.path())
            .last()
            .unwrap()
            .contains("--start-seq 2 --chunk-seconds 10 --mic default --no-system"),
        "{:?}",
        helper_runs(r.f.dir.path())
    );
    // 之前录了 18 秒（10 + 8），这次又录了 10 秒
    let state = wait_for(&r.recorder, "录好第 2 片", |s| s.seconds == 28.0).await;
    assert_eq!(upload(&state, "m1").unwrap().recorded, 3);
    assert_eq!(r.recorder.mark().unwrap().marks, vec![5.0, 15.0, 28.0]);
    r.recorder.stop().await.unwrap();
    wait_for(&r.recorder, "传完", |s| {
        upload_is(s, "m1", UploadStatus::Completed)
    })
    .await;
    assert_eq!(r.mock.seqs("m1"), [0, 1, 2, 3]);
    assert_eq!(
        r.mock.completes_of("m1"),
        [json!({ "chunks": 4, "marks": [5.0, 15.0, 28.0] })]
    );
}

#[tokio::test]
async fn continuing_drops_marks_from_the_lost_tail() {
    let mut r = rig(Script::default()).await;
    r.run.abort();
    // 录到 25 秒时点了标记，接着录音程序崩溃了：存下来的两片一共 20 秒，25 秒那段声音没存下来
    let mut rec = recording(&r.server, "u1", "m1", Status::Interrupted);
    (rec.next_seq, rec.seconds, rec.marks) = (2, 20.0, vec![5.0, 25.0]);
    plant(
        &r.root,
        &rec,
        &[(0, b"audio-0".to_vec()), (1, b"audio-1".to_vec())],
    );
    r.restart();

    // 接着录：接着录的声音从 20 秒算起，25 秒的标记会指到新录的声音上，不要了
    let state = r.recorder.start("m1", None, true).await.unwrap();
    assert_eq!(state.marks, vec![5.0]);
    // 刚接上就点标记：记在 20 秒（不能因为比 25 秒早就当成连着点的）；连着点的只算一次
    assert_eq!(r.recorder.mark().unwrap().marks, vec![5.0, 20.0]);
    assert_eq!(r.recorder.mark().unwrap().marks, vec![5.0, 20.0]);
    until("服务器上的标记换成新的", || {
        r.mock.marks.lock().unwrap().last().map(|(_, body)| body)
            == Some(&json!({ "marks": [5.0, 20.0] }))
    })
    .await;
    r.recorder.stop().await.unwrap();
    wait_for(&r.recorder, "传完", |s| {
        upload_is(s, "m1", UploadStatus::Completed)
    })
    .await;
    assert_eq!(
        r.mock.completes_of("m1"),
        [json!({ "chunks": 3, "marks": [5.0, 20.0] })]
    );
}

#[tokio::test]
async fn continuing_never_reuses_a_chunk_number() {
    let mut r = rig(Script {
        chunks: 1,
        ..Script::default()
    })
    .await;
    r.run.abort();
    // recording.json 落后了（比如磁盘满了没存上）：记着录到第 0 片、传了第 0 片；其实服务器已经收到了
    // 第 0 到 2 片（这台电脑上的文件传完就删了），第 3 片也写好了、还在这台电脑上
    let mut rec = recording(&r.server, "u1", "m1", Status::Interrupted);
    (rec.next_seq, rec.uploaded) = (1, 1);
    plant(&r.root, &rec, &[(3, b"audio-3".to_vec())]);
    for seq in 0..3 {
        r.mock.chunks.lock().unwrap().push(Put {
            meeting: "m1".into(),
            seq,
            body: format!("audio-{seq}").into_bytes(),
            content_type: Some("audio/aac".into()),
            auth: Some("Bearer zsm_test".into()),
        });
    }
    r.restart();

    // 接着录：从第 4 片开始写，不占用服务器上、这台电脑上已有的序号
    r.recorder.start("m1", None, true).await.unwrap();
    assert!(
        helper_runs(r.f.dir.path())
            .last()
            .unwrap()
            .contains("--start-seq 4 "),
        "{:?}",
        helper_runs(r.f.dir.path())
    );
    r.recorder.stop().await.unwrap();
    wait_for(&r.recorder, "传完", |s| {
        upload_is(s, "m1", UploadStatus::Completed)
    })
    .await;
    // 第 1、2 片这台电脑上没有、服务器上有，算传过了；第 3 片和接着录的第 4、5 片传上去
    assert_eq!(r.mock.seqs("m1"), [0, 1, 2, 3, 4, 5]);
    assert_eq!(
        r.mock.completes_of("m1"),
        [json!({ "chunks": 6, "marks": [] })]
    );
}

#[tokio::test]
async fn never_sends_an_empty_chunk() {
    let mut r = rig(Script::default()).await;
    r.run.abort();
    let rec = recording(&r.server, "u1", "m1", Status::Stopped);
    plant(&r.root, &rec, &[(0, Vec::new())]);
    r.restart();

    let state = wait_for(&r.recorder, "发现分片是空的", |s| {
        upload_is(s, "m1", UploadStatus::Failed)
    })
    .await;
    let error = upload(&state, "m1").unwrap().error.clone().unwrap();
    assert!(error.contains("是空的"), "{error}");
    assert!(
        r.mock.attempts.lock().unwrap().is_empty(),
        "空的分片不发给服务器"
    );
}

#[tokio::test]
async fn a_broken_recording_json_falls_back_to_the_previous_copy() {
    let mut r = rig(Script::default()).await;
    r.run.abort();
    // 录音时断电了：最后一次存的 recording.json 没写下去，变成全是 0；上一份还在
    let mut rec = recording(&r.server, "u1", "m1", Status::Recording);
    plant(&r.root, &rec, &[(0, b"audio-0".to_vec())]);
    rec.marks = vec![3.0];
    local::save(&r.root.join("m1"), &rec).unwrap();
    std::fs::write(r.root.join("m1").join(local::RECORDING_FILE), [0u8; 256]).unwrap();
    r.restart();

    // 这场录音照样在，算中断，录好的照样能传上去
    let state = r.recorder.state();
    assert!(
        upload_is(&state, "m1", UploadStatus::Interrupted),
        "{state:#?}"
    );
    r.recorder.finish("m1").unwrap();
    wait_for(&r.recorder, "传完", |s| {
        upload_is(s, "m1", UploadStatus::Completed)
    })
    .await;
    assert_eq!(r.mock.seqs("m1"), [0]);
}

#[tokio::test]
async fn finishes_or_discards_interrupted_recordings() {
    let mut r = rig(Script::default()).await;
    r.run.abort();
    // m1 中断前传上去了第 0 片（本地已经删了），第 1 片还在；25 秒的标记落在没存下来的那一段里
    let mut m1 = recording(&r.server, "u1", "m1", Status::Recording);
    (m1.next_seq, m1.uploaded, m1.seconds, m1.marks) = (2, 1, 20.0, vec![5.0, 25.0]);
    plant(&r.root, &m1, &[(1, b"audio-1".to_vec())]);
    let mut m2 = recording(&r.server, "u1", "m2", Status::Interrupted);
    m2.started_at += 1;
    plant(&r.root, &m2, &[(0, b"audio-0".to_vec())]);
    // 一片都没录下来就中断的：没有可传的，直接删掉
    let mut m3 = recording(&r.server, "u1", "m3", Status::Recording);
    m3.next_seq = 0;
    plant(&r.root, &m3, &[]);
    r.restart();

    let state = r.recorder.state();
    assert!(upload_is(&state, "m1", UploadStatus::Interrupted));
    assert!(upload_is(&state, "m2", UploadStatus::Interrupted));
    assert!(upload(&state, "m3").is_none());
    assert!(!r.root.join("m3").exists());
    assert!(r.recorder.discard("nope").is_err());

    // m1 不接着录了：传完告诉服务器开始处理，没存下来的那段里的标记不要了
    let state = r.recorder.finish("m1").unwrap();
    assert!(!upload_is(&state, "m1", UploadStatus::Interrupted));
    wait_for(&r.recorder, "m1 传完", |s| {
        upload_is(s, "m1", UploadStatus::Completed)
    })
    .await;
    assert_eq!(r.mock.seqs("m1"), [1]);
    assert_eq!(
        r.mock.completes_of("m1"),
        [json!({ "chunks": 2, "marks": [5.0] })]
    );
    // 已经在传、传完了的不能删
    assert!(r.recorder.discard("m1").is_err());

    // m2 删掉：只删本地的，不告诉服务器结束
    let state = r.recorder.discard("m2").unwrap();
    assert!(upload(&state, "m2").is_none());
    assert!(!r.root.join("m2").exists());
    assert!(r.mock.completes_of("m2").is_empty());
    let err = r.recorder.finish("m2").unwrap_err();
    assert!(err.contains("没有这场会议的录音"), "{err}");
}

#[tokio::test]
async fn uploads_only_the_signed_in_accounts_recordings() {
    let mut r = rig(Script::default()).await;
    r.run.abort();
    let mine = recording(&r.server, "u1", "a", Status::Stopped);
    let mut colleague = recording(&r.server, "u2", "b", Status::Stopped);
    colleague.started_at += 1;
    let other_server = recording("https://other.example.com", "u1", "c", Status::Stopped);
    for rec in [&mine, &colleague, &other_server] {
        plant(&r.root, rec, &[(0, b"audio-0".to_vec())]);
    }
    r.restart();

    // 只传、只显示当前账号（这台服务器上的 u1）的
    let state = wait_for(&r.recorder, "a 传完", |s| {
        upload_is(s, "a", UploadStatus::Completed)
    })
    .await;
    assert_eq!(
        state
            .uploads
            .iter()
            .map(|u| u.meeting_id.as_str())
            .collect::<Vec<_>>(),
        ["a"]
    );
    tokio::time::sleep(Duration::from_millis(150)).await;
    assert!(r
        .mock
        .attempts
        .lock()
        .unwrap()
        .iter()
        .all(|(m, _, _)| m == "a"));

    // 同一台电脑上换成 u2 登录：u2 的录音用 u2 的令牌传；u1 的不再显示
    r.f.core.logout().await;
    assert!(r.recorder.state().uploads.is_empty());
    *r.mock.login_user.lock().unwrap() =
        json!({"id": "u2", "name": "李雷", "email": "li@example.com", "role": "member"});
    sign_in(&r.f, "u2").await;
    let state = wait_for(&r.recorder, "b 传完", |s| {
        upload_is(s, "b", UploadStatus::Completed)
    })
    .await;
    assert_eq!(state.uploads.len(), 1);
    let attempts = r.mock.attempts.lock().unwrap().clone();
    for (meeting, _, auth) in &attempts {
        let expected = if meeting == "a" {
            "Bearer zsm_test"
        } else {
            "Bearer zsm_u2"
        };
        assert_eq!(auth.as_deref(), Some(expected), "{meeting}");
    }
    assert!(
        attempts.iter().all(|(m, _, _)| m != "c"),
        "别的服务器的不传"
    );
    assert!(r.root.join("c/0.aac").exists());
}

#[tokio::test]
async fn another_account_signing_in_stops_the_recording() {
    let r = rig(Script {
        chunks: 1,
        ..Script::default()
    })
    .await;
    r.recorder.start("m1", None, true).await.unwrap();
    wait_for(&r.recorder, "传完第 0 片", |s| {
        upload(s, "m1").is_some_and(|u| u.uploaded == 1)
    })
    .await;
    r.recorder.mark().unwrap();

    // u1 的登录失效了，录音照常（菜单栏上照样显示）；接着 u2 在这台电脑上登录：u1 的录音结束，留着等 u1 回来
    r.f.core.session_expired();
    let state = r.recorder.state();
    assert_eq!(state.status, Phase::Recording);
    assert_eq!(state.meeting_id.as_deref(), Some("m1"));
    *r.mock.login_user.lock().unwrap() =
        json!({"id": "u2", "name": "李雷", "email": "li@example.com", "role": "member"});
    sign_in(&r.f, "u2").await;
    // 结束之前 u2 也看不到 u1 在录哪场会议、录了多久、标了什么
    let state = r.recorder.state();
    assert_eq!(
        (state.meeting_id, state.title, state.seconds, state.marks),
        (None, None, 0.0, vec![])
    );
    let state = wait_for(&r.recorder, "录音结束", |s| s.status == Phase::Idle).await;
    assert!(state.uploads.is_empty(), "u2 看不到 u1 的录音");
    tokio::time::sleep(Duration::from_millis(150)).await;
    assert_eq!(r.mock.seqs("m1"), [0], "不用 u2 的令牌传 u1 的录音");
    assert!(r.mock.completes_of("m1").is_empty());
    let rec = local::load(&r.root.join("m1")).unwrap();
    assert_eq!(rec.status, Status::Stopped);
    assert_eq!((rec.next_seq, rec.uploaded), (2, 1));
}

/// 账号 user 正在录 meeting 的会话（不用真的启动录音程序）。
fn live_session(server: &str, user: &str, meeting: &str) -> Session {
    Session {
        id: 1,
        meeting_id: meeting.into(),
        server: server.into(),
        user_id: user.into(),
        phase: Phase::Recording,
        fresh: true,
        base_seconds: 0.0,
        seconds: 12.0,
        level: Some(Level {
            mic: 0.5,
            system: 0.25,
        }),
        mic: Some("MacBook Pro 麦克风".into()),
        system: true,
        warnings: vec![Warning {
            code: "system_denied".into(),
            message: "没有录电脑声音的权限".into(),
        }],
        error: None,
        stopped: false,
        stop_requested: false,
        commands: tokio::sync::mpsc::unbounded_channel().0,
        kill: None,
    }
}

#[test]
fn only_the_recording_account_sees_what_is_being_recorded() {
    let server = "https://meet.example.com";
    let account = |user: &str| Account {
        server: server.into(),
        user_id: user.into(),
        token: format!("zsm_{user}"),
    };
    let mut rec = recording(server, "u1", "m1", Status::Recording);
    rec.marks = vec![3.0];
    let inner = Inner {
        recordings: BTreeMap::from([("m1".to_string(), Local::new(rec, PathBuf::new()))]),
        session: Some(live_session(server, "u1", "m1")),
        next_session: 2,
        error: None,
        start_warnings: Vec::new(),
        ended: None,
    };

    // 录音的人自己，和没人登录的时候（登录失效了，录音照常，菜单栏上要显示）：都能看
    for who in [Some(account("u1")), None] {
        let state = snapshot(&inner, who.as_ref());
        assert_eq!(state.meeting_id.as_deref(), Some("m1"));
        assert_eq!(state.title.as_deref(), Some("会议 m1"));
        assert_eq!((state.seconds, state.marks.clone()), (12.0, vec![3.0]));
        assert_eq!(state.warnings.len(), 1);
    }
    // 换了别人登录，上一个人的录音还没结束：只知道这台电脑在录音
    let state = snapshot(&inner, Some(&account("u2")));
    assert_eq!(state.status, Phase::Recording);
    assert_eq!(
        (state.meeting_id, state.title, state.seconds, state.level),
        (None, None, 0.0, None)
    );
    assert_eq!((state.mic, state.system), (None, false));
    assert!(state.marks.is_empty() && state.warnings.is_empty() && state.uploads.is_empty());
}

#[tokio::test]
async fn helper_crash_interrupts_and_recording_can_go_on() {
    let r = rig(Script {
        chunks: 1,
        crash: true,
        ..Script::default()
    })
    .await;
    r.recorder.start("m1", None, true).await.unwrap();
    let state = wait_for(&r.recorder, "中断", |s| {
        upload_is(s, "m1", UploadStatus::Interrupted)
    })
    .await;
    assert_eq!(state.status, Phase::Idle);
    assert!(state.error.as_deref().unwrap().contains("意外退出"));
    assert!(upload(&state, "m1")
        .unwrap()
        .error
        .as_deref()
        .unwrap()
        .contains("意外退出"));

    // 接着录：这次录音程序从第 1 片开始写
    let state = r.recorder.start("m1", None, true).await.unwrap();
    assert_eq!(state.error, None);
    assert!(helper_runs(r.f.dir.path())[1].contains("--start-seq 1"));
}

#[tokio::test]
async fn helper_crash_before_the_first_chunk_says_nothing_was_recorded() {
    let r = rig(Script {
        crash: true,
        ..Script::default()
    })
    .await;
    r.recorder.start("m1", None, true).await.unwrap();
    let state = wait_for(&r.recorder, "录音结束", |s| s.status == Phase::Idle).await;
    // 一片都没录下来：不能说「录好的部分都在，可以接着录」，这场录音也不留
    assert_eq!(state.error.as_deref(), Some(NOTHING_RECORDED));
    assert!(upload(&state, "m1").is_none());
    assert!(!r.root.join("m1").exists());
}

#[tokio::test]
async fn reports_why_recording_could_not_start() {
    let r = rig(Script {
        start_error: Some(("no_source", "麦克风和电脑声音都录不了")),
        warning: Some(("system_denied", "没有录电脑声音的权限")),
        ..Script::default()
    })
    .await;
    let err = r.recorder.start("m1", None, true).await.unwrap_err();
    assert_eq!(err, "麦克风和电脑声音都录不了");
    let state = r.recorder.state();
    assert_eq!(state.status, Phase::Idle);
    assert!(state.uploads.is_empty());
    assert!(!r.root.join("m1").exists(), "什么也没录下来，不留东西");
    // 录音程序说了为什么开始不了：留着给界面说明怎么打开权限，下次开始录音时清掉
    assert_eq!(
        state.warnings,
        [Warning {
            code: "system_denied".into(),
            message: "没有录电脑声音的权限".into()
        }]
    );

    // 服务器上的会议已经在处理了：不录
    r.mock
        .meeting_status
        .lock()
        .unwrap()
        .insert("m2".into(), "done".into());
    let err = r.recorder.start("m2", None, true).await.unwrap_err();
    assert!(err.contains("已经开始处理"), "{err}");
    assert!(r.recorder.state().warnings.is_empty());
    // 会议编号不对、麦克风不对、两路声音都不录
    assert!(r.recorder.start("../x", None, true).await.is_err());
    assert!(r
        .recorder
        .start("m3", Some("--no-system".into()), true)
        .await
        .is_err());
    let err = r.recorder.start("m3", None, false).await.unwrap_err();
    assert!(err.contains("至少要录一个"), "{err}");

    // 找不到录音程序
    let missing = Recorder::new(
        options(&r.root, &r.f.dir.path().join("nope")),
        r.f.core.clone(),
        Arc::new(Seen::default()),
        server::http_client(),
    );
    let err = missing.start("m4", None, true).await.unwrap_err();
    assert!(err.contains("找不到录音程序"), "{err}");
    assert!(missing
        .devices()
        .await
        .unwrap_err()
        .contains("找不到录音程序"));
}

#[tokio::test]
async fn passes_on_warnings_and_lists_microphones() {
    let r = rig(Script {
        warning: Some(("system_denied", "没有录制电脑声音的权限")),
        ..Script::default()
    })
    .await;
    let devices = r.recorder.devices().await.unwrap();
    assert_eq!(devices.microphones.len(), 2);
    assert_eq!(devices.microphones[0].name, "MacBook Pro 麦克风");
    assert!(devices.microphones[0].default);

    r.recorder.start("m1", None, true).await.unwrap();
    let state = wait_for(&r.recorder, "提醒", |s| !s.warnings.is_empty()).await;
    assert_eq!(
        state.warnings,
        [Warning {
            code: "system_denied".into(),
            message: "没有录制电脑声音的权限".into()
        }]
    );
    // 一片都还没录好就结束：结束时写出的最后一片照常上传
    r.recorder.stop().await.unwrap();
    wait_for(&r.recorder, "传完", |s| {
        upload_is(s, "m1", UploadStatus::Completed)
    })
    .await;
    assert_eq!(
        r.mock.completes_of("m1"),
        [json!({ "chunks": 1, "marks": [] })]
    );
}

#[tokio::test]
async fn shutdown_stops_and_uploads_continue_next_launch() {
    let mut r = rig(Script {
        chunks: 1,
        ..Script::default()
    })
    .await;
    r.mock.chunk_error.lock().unwrap().insert("m1".into(), 503);
    r.recorder.start("m1", None, true).await.unwrap();
    // 退出 App：结束录音，等最后一片写完
    r.recorder.shutdown(Duration::from_secs(5)).await;
    assert_eq!(r.recorder.state().status, Phase::Idle);
    let rec = local::load(&r.root.join("m1")).unwrap();
    assert_eq!(rec.status, Status::Stopped);
    assert_eq!(rec.next_seq, 2);

    // 下次打开：接着传
    r.mock.chunk_error.lock().unwrap().clear();
    r.restart();
    wait_for(&r.recorder, "传完", |s| {
        upload_is(s, "m1", UploadStatus::Completed)
    })
    .await;
    assert_eq!(r.mock.seqs("m1"), [0, 1]);
}

#[test]
fn state_uses_the_agreed_field_names() {
    let state = RecorderState {
        status: Phase::Recording,
        meeting_id: Some("m1".into()),
        title: Some("周会".into()),
        seconds: 12.5,
        level: Some(Level {
            mic: 0.5,
            system: 0.0,
        }),
        mic: Some("MacBook Pro 麦克风".into()),
        system: true,
        marks: vec![3.0],
        warnings: vec![Warning {
            code: "mic_denied".into(),
            message: "没有麦克风权限".into(),
        }],
        error: None,
        uploads: vec![Upload {
            meeting_id: "m1".into(),
            title: "周会".into(),
            recorded: 2,
            uploaded: 1,
            status: UploadStatus::Recording,
            error: None,
        }],
    };
    assert_eq!(
        serde_json::to_value(&state).unwrap(),
        json!({
            "status": "recording",
            "meetingId": "m1",
            "title": "周会",
            "seconds": 12.5,
            "level": {"mic": 0.5, "system": 0.0},
            "mic": "MacBook Pro 麦克风",
            "system": true,
            "marks": [3.0],
            "warnings": [{"code": "mic_denied", "message": "没有麦克风权限"}],
            "error": null,
            "uploads": [{
                "meetingId": "m1", "title": "周会", "recorded": 2, "uploaded": 1,
                "status": "recording", "error": null
            }]
        })
    );
    for (status, name) in [
        (UploadStatus::Uploading, "uploading"),
        (UploadStatus::Waiting, "waiting"),
        (UploadStatus::Completing, "completing"),
        (UploadStatus::Completed, "completed"),
        (UploadStatus::Interrupted, "interrupted"),
        (UploadStatus::Failed, "failed"),
    ] {
        assert_eq!(serde_json::to_value(status).unwrap(), name);
    }
    for (phase, name) in [
        (Phase::Idle, "idle"),
        (Phase::Starting, "starting"),
        (Phase::Paused, "paused"),
        (Phase::Stopping, "stopping"),
    ] {
        assert_eq!(serde_json::to_value(phase).unwrap(), name);
    }
}

// ---- 和真的录音程序一起跑 ----
//
// 用录音程序的测试模式（RecorderOptions::input_file，即 --input-file）：拿 ffmpeg 生成的音频文件代替麦克风和
// 电脑的声音，不要权限、不弹提示。走一遍开始录音 → 分片 → 上传 → complete，再用 ffmpeg、ffprobe 检查
// 假服务器收到的分片接起来是一路连续、解码不出错的 AAC，时长对得上。平时不跑（要先构建录音程序，还要 ffmpeg）：
//
//   desktop/scripts/build-recorder.sh
//   cd desktop/src-tauri && ZSMEET_RECORDER=$PWD/binaries/zsmeet-recorder-universal-apple-darwin \
//     cargo test --locked real_helper -- --ignored
//
// make desktop-test 和 CI 都会跑。

/// 真的录音程序：和 App 一样用 helper::locate 找，测试时由 ZSMEET_RECORDER 指定。
fn real_helper() -> PathBuf {
    helper::locate()
        .filter(|p| p.is_file())
        .expect("请把 ZSMEET_RECORDER 设成构建好的录音程序（desktop/scripts/build-recorder.sh）")
}

/// ffmpeg、ffprobe。Homebrew 装的可能不在 PATH 里。
fn ff(tool: &str) -> std::process::Command {
    let mut command = std::process::Command::new(tool);
    let path = std::env::var("PATH").unwrap_or_default();
    command.env("PATH", format!("{path}:/opt/homebrew/bin:/usr/local/bin"));
    command
}

/// 生成 seconds 秒的测试声音：44.1 kHz 双声道的 WAV（录音程序要把它转成 48 kHz 单声道），440 Hz，每秒响一下。
fn test_audio(path: &Path, seconds: u32) {
    let made = ff("ffmpeg")
        .args(["-v", "error", "-y", "-f", "lavfi", "-i"])
        .arg(format!(
            "sine=frequency=440:beep_factor=2:duration={seconds}"
        ))
        .args(["-ar", "44100", "-ac", "2"])
        .arg(path)
        .status()
        .expect("要装 ffmpeg");
    assert!(made.success(), "ffmpeg 没能生成测试声音");
}

/// ffprobe 量出来的时长（秒）。
fn probe_seconds(path: &Path) -> f64 {
    let out = ff("ffprobe")
        .args([
            "-v",
            "error",
            "-show_entries",
            "format=duration",
            "-of",
            "csv=p=0",
        ])
        .arg(path)
        .output()
        .expect("要装 ffprobe");
    assert!(out.status.success(), "ffprobe 量不了 {}", path.display());
    let text = String::from_utf8_lossy(&out.stdout);
    text.trim()
        .parse()
        .unwrap_or_else(|_| panic!("ffprobe 输出看不懂：{text}"))
}

/// 把一段 ADTS 解码成 WAV（解码时出一点错就失败），返回解码出来的时长。
fn decoded_seconds(adts: &[u8], dir: &Path, name: &str) -> f64 {
    let aac = dir.join(format!("{name}.aac"));
    let wav = dir.join(format!("{name}.wav"));
    std::fs::write(&aac, adts).unwrap();
    let decoded = ff("ffmpeg")
        .args(["-v", "error", "-xerror", "-y", "-i"])
        .arg(&aac)
        .args(["-f", "wav"])
        .arg(&wav)
        .status()
        .unwrap();
    assert!(decoded.success(), "ffmpeg 解码 {name} 出错");
    probe_seconds(&wav)
}

struct RealRig {
    f: Fixture,
    mock: Arc<Mock>,
    /// 录音程序读的测试声音
    input: PathBuf,
    recorder: Arc<Recorder>,
    seen: Arc<Seen>,
    _run: JoinHandle<()>,
}

/// 起假服务器、登录，Recorder 用真的录音程序读 seconds 秒的测试声音（realtime：按实际时长读）。
async fn real_rig(seconds: u32, realtime: bool) -> RealRig {
    let mock = Arc::new(Mock::default());
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let server = format!("http://{}", listener.local_addr().unwrap());
    serve(listener, mock.clone()).await;
    let f = Fixture::signed_in(&server, "http://127.0.0.1:1");
    let input = f.dir.path().join("input.wav");
    test_audio(&input, seconds);
    let mut opts = options(&f.dir.path().join("recordings"), &real_helper());
    opts.input_file = Some(input.clone());
    opts.realtime = realtime;
    let (recorder, seen, run) = launch_with(&f, opts);
    RealRig {
        f,
        mock,
        input,
        recorder,
        seen,
        _run: run,
    }
}

#[tokio::test]
#[ignore = "要先构建录音程序（ZSMEET_RECORDER），还要 ffmpeg"]
async fn real_helper_records_and_uploads() {
    let RealRig {
        f,
        mock,
        input,
        recorder,
        ..
    } = real_rig(25, false).await;

    let state = recorder
        .start("m1", Some("default".into()), true)
        .await
        .unwrap();
    assert!(state.error.is_none(), "{state:?}");
    // 文件尽快读完，录音程序自己结束（和点了结束一样）：传完以后告诉服务器开始处理
    wait_for(&recorder, "传完", |s| {
        upload_is(s, "m1", UploadStatus::Completed)
    })
    .await;

    let chunks = mock.chunks_of("m1");
    assert_eq!(
        chunks.iter().map(|c| c.seq).collect::<Vec<_>>(),
        [0, 1, 2],
        "25 秒切成 10 + 10 + 5"
    );
    assert!(chunks
        .iter()
        .all(|c| c.content_type.as_deref() == Some("audio/aac")));
    assert_eq!(
        mock.completes_of("m1"),
        [json!({ "chunks": 3, "marks": [] })]
    );
    // 每一片单独也能解码，前两片各约 10 秒
    for c in &chunks[..2] {
        let seconds = decoded_seconds(&c.body, f.dir.path(), &format!("chunk-{}", c.seq));
        assert!(
            (seconds - 10.0).abs() < 0.05,
            "第 {} 片 {seconds} 秒",
            c.seq
        );
    }
    // 服务器把分片按序号接起来就是整场录音：一路连续的 ADTS（整场只有一个编码器），
    // 解码不出错，时长和原来的声音一样（只差编码器开头剩下的 1.3 毫秒和最后补齐的不到一帧）
    let all: Vec<u8> = chunks.iter().flat_map(|c| c.body.clone()).collect();
    let expected = probe_seconds(&input);
    let got = decoded_seconds(&all, f.dir.path(), "all");
    eprintln!("测试声音 {expected} 秒，服务器收到的分片接起来解码出 {got} 秒");
    assert!(
        (got - expected).abs() <= 0.05,
        "原来 {expected} 秒，接起来 {got} 秒"
    );
}

#[tokio::test]
#[ignore = "要先构建录音程序（ZSMEET_RECORDER），还要 ffmpeg"]
async fn real_helper_pauses_marks_and_stops() {
    let RealRig {
        f,
        mock,
        input,
        recorder,
        seen,
        _run,
    } = real_rig(30, true).await;

    let began = std::time::Instant::now();
    let state = recorder.start("m1", None, true).await.unwrap();
    assert_eq!(state.status, Phase::Recording);
    wait_for(&recorder, "录到 1.5 秒", |s| s.seconds >= 1.5).await;
    let marks = recorder.mark().unwrap().marks;
    assert_eq!(marks.len(), 1);
    assert!((1.5..2.5).contains(&marks[0]), "{marks:?}");

    // 暂停：正在写的那一片先写完、传上去；暂停的这一会儿录到的声音不要
    assert_eq!(recorder.pause().await.unwrap().status, Phase::Paused);
    until("暂停前的那一片传上去", || mock.seqs("m1") == [0]).await;
    tokio::time::sleep(Duration::from_millis(1500)).await;
    let paused_at = recorder.state().seconds;
    assert_eq!(recorder.resume().await.unwrap().status, Phase::Recording);
    wait_for(&recorder, "又录了 1 秒", |s| {
        s.seconds >= paused_at + 1.0
    })
    .await;

    // 文件还远没读完时点结束
    let shown = recorder.state().seconds;
    let state = recorder.stop().await.unwrap();
    let wall = began.elapsed().as_secs_f64();
    assert_eq!(state.status, Phase::Idle);
    wait_for(&recorder, "传完", |s| {
        upload_is(s, "m1", UploadStatus::Completed)
    })
    .await;

    let phases = seen.phases();
    let expected = [
        Phase::Recording,
        Phase::Paused,
        Phase::Recording,
        Phase::Stopping,
        Phase::Idle,
    ];
    assert!(
        phases.windows(expected.len()).any(|w| w == expected),
        "{phases:?}"
    );
    let chunks = mock.chunks_of("m1");
    assert_eq!(
        chunks.iter().map(|c| c.seq).collect::<Vec<_>>(),
        [0, 1],
        "暂停前一片，继续以后一片"
    );
    assert_eq!(
        mock.completes_of("m1"),
        [json!({ "chunks": 2, "marks": marks })]
    );
    // 接起来能完整解码；时长就是界面上显示的录了多久（暂停的 1.5 秒不算），多出来的是点结束到录音程序
    // 收到命令之间录的（平时不到 0.1 秒，机器很忙时会多一些）和它攒着的 0.15 秒
    let all: Vec<u8> = chunks.iter().flat_map(|c| c.body.clone()).collect();
    let got = decoded_seconds(&all, f.dir.path(), "all");
    eprintln!("界面上显示录了 {shown} 秒，一共过了 {wall:.3} 秒（暂停了 1.5 秒），接起来解码出 {got} 秒，标记 {marks:?}");
    assert!(
        got >= shown - 0.05 && got <= shown + 1.0,
        "界面上显示录了 {shown} 秒，接起来 {got} 秒"
    );
    assert!(
        got < wall - 1.0,
        "暂停的部分不该录进去：一共过了 {wall} 秒，录了 {got} 秒"
    );
    assert!(got < probe_seconds(&input) - 20.0, "点了结束就该停下");
}
