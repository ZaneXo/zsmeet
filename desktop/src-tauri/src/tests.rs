//! 本地代理和登录的测试：起一个假的 zsmeet 服务器（mock upstream），不碰钥匙串、不开浏览器。

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use axum::body::Body;
use axum::extract::{Request, State};
use axum::http::{header, HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::{any, get, post};
use axum::{Json, Router};
use serde_json::{json, Value};
use tokio::net::TcpListener;
use tokio::sync::Notify;
use tokio::task::JoinHandle;

use crate::core::testing::{fixture, Fixture};
use crate::proxy::{self, Proxy};
use crate::server;
use crate::token::TokenStore;

const ORIGIN: &str = "tauri://localhost";
const AUDIO: &[u8] = b"0123456789";

/// 假服务器记下收到了什么。
#[derive(Default)]
struct Seen {
    auth: Mutex<Vec<Option<String>>>,
    leaked: AtomicBool,
    queries: Mutex<Vec<String>>,
    logout_auth: Mutex<Option<String>>,
    token_body: Mutex<Option<Value>>,
    /// /api/status 里有没有 api 字段、是不是初始化了
    status: Mutex<Value>,
    release: Notify,
}

fn record(seen: &Seen, headers: &HeaderMap) {
    seen.auth.lock().unwrap().push(
        headers
            .get(header::AUTHORIZATION)
            .and_then(|v| v.to_str().ok())
            .map(str::to_string),
    );
    if headers.contains_key(proxy::SECRET_HEADER) || headers.contains_key(header::COOKIE) {
        seen.leaked.store(true, Ordering::SeqCst);
    }
}

async fn mock_status(State(seen): State<Arc<Seen>>, headers: HeaderMap) -> Json<Value> {
    record(&seen, &headers);
    Json(seen.status.lock().unwrap().clone())
}

async fn mock_meetings(State(seen): State<Arc<Seen>>, req: Request) -> Json<Value> {
    record(&seen, req.headers());
    seen.queries
        .lock()
        .unwrap()
        .push(req.uri().query().unwrap_or("").to_string());
    Json(json!({"items": [{"id": "m1", "title": "周会"}], "q": req.uri().query()}))
}

async fn mock_audio(State(seen): State<Arc<Seen>>, req: Request) -> Response {
    record(&seen, req.headers());
    seen.queries
        .lock()
        .unwrap()
        .push(req.uri().query().unwrap_or("").to_string());
    match req
        .headers()
        .get(header::RANGE)
        .and_then(|v| v.to_str().ok())
    {
        Some("bytes=2-5") => (
            StatusCode::PARTIAL_CONTENT,
            [
                (header::CONTENT_RANGE, "bytes 2-5/10"),
                (header::ACCEPT_RANGES, "bytes"),
                (header::CONTENT_TYPE, "audio/mp4"),
            ],
            &AUDIO[2..6],
        )
            .into_response(),
        _ => (
            [
                (header::ACCEPT_RANGES, "bytes"),
                (header::CONTENT_TYPE, "audio/mp4"),
            ],
            AUDIO,
        )
            .into_response(),
    }
}

/// 进度推送：先推一条，等测试放行以后再推第二条然后结束。
async fn mock_events(State(seen): State<Arc<Seen>>) -> Response {
    let stream = futures_util::stream::unfold(0, move |n| {
        let seen = seen.clone();
        async move {
            match n {
                0 => Some((Ok::<_, std::io::Error>("event: meeting\ndata: 1\n\n"), 1)),
                1 => {
                    seen.release.notified().await;
                    Some((Ok("event: meeting\ndata: 2\n\n"), 2))
                }
                _ => None,
            }
        }
    });
    (
        [(header::CONTENT_TYPE, "text/event-stream")],
        Body::from_stream(stream),
    )
        .into_response()
}

async fn mock_upload(req: Request) -> Json<Value> {
    let length = req.headers().get(header::CONTENT_LENGTH).cloned();
    let body = axum::body::to_bytes(req.into_body(), usize::MAX)
        .await
        .unwrap();
    Json(
        json!({"received": body.len(), "content_length": length.map(|v| v.to_str().unwrap().to_string())}),
    )
}

async fn mock_token(State(seen): State<Arc<Seen>>, Json(body): Json<Value>) -> Response {
    *seen.token_body.lock().unwrap() = Some(body.clone());
    if body["code"] == "good" {
        Json(json!({"token": "zsm_new", "user": {"id": "u2", "name": "李雷", "email": "li@example.com", "role": "admin"}}))
            .into_response()
    } else {
        (
            StatusCode::UNPROCESSABLE_ENTITY,
            Json(json!({"detail": "授权码已经失效"})),
        )
            .into_response()
    }
}

async fn mock_logout(State(seen): State<Arc<Seen>>, headers: HeaderMap) -> StatusCode {
    *seen.logout_auth.lock().unwrap() = headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .map(str::to_string);
    StatusCode::NO_CONTENT
}

struct Upstream {
    url: String,
    seen: Arc<Seen>,
    task: JoinHandle<()>,
}

async fn upstream() -> Upstream {
    let seen = Arc::new(Seen::default());
    *seen.status.lock().unwrap() = json!({"initialized": true, "api": 1, "version": "test"});
    let app = Router::new()
        .route("/api/status", get(mock_status))
        .route("/api/meetings", get(mock_meetings))
        .route("/api/meetings/m1", get(mock_meetings))
        .route("/api/meetings/m1/audio", get(mock_audio))
        .route("/api/meetings/m1/events", get(mock_events))
        .route("/api/meetings/m1/chunks/0", any(mock_upload))
        .route(
            "/api/templates",
            get(|| async { (StatusCode::INTERNAL_SERVER_ERROR, "boom") }),
        )
        .route(
            "/api/me/devices",
            get(|| async {
                (
                    StatusCode::UNAUTHORIZED,
                    Json(json!({"detail": "登录已失效"})),
                )
            }),
        )
        .route("/api/apps/token", post(mock_token))
        .route("/api/apps/logout", post(mock_logout))
        .with_state(seen.clone());
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let task = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    Upstream { url, seen, task }
}

struct Harness {
    proxy: String,
    up: Upstream,
    f: Fixture,
    http: reqwest::Client,
}

/// 起假服务器和本地代理。signed_in：是不是已经有用户登录（令牌 zsm_test）。
async fn harness(signed_in: bool) -> Harness {
    let up = upstream().await;
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let proxy_url = format!("http://{}", listener.local_addr().unwrap());
    let f = if signed_in {
        Fixture::signed_in(&up.url, &proxy_url)
    } else {
        let f = fixture(&proxy_url);
        f.core.set_server(&up.url).await.unwrap();
        f
    };
    // 测试里不复用连接：假服务器停掉以后，下一个请求马上就连不上
    let client = reqwest::Client::builder()
        .pool_max_idle_per_host(0)
        .connect_timeout(Duration::from_secs(2))
        .build()
        .unwrap();
    let p = Proxy::new(f.core.clone(), vec![ORIGIN.to_string()], client);
    tokio::spawn(proxy::serve(listener, p));
    Harness {
        proxy: proxy_url,
        up,
        f,
        http: reqwest::Client::builder()
            .pool_max_idle_per_host(0)
            .build()
            .unwrap(),
    }
}

impl Harness {
    fn get(&self, path: &str) -> reqwest::RequestBuilder {
        self.http
            .get(format!("{}{path}", self.proxy))
            .header(proxy::SECRET_HEADER, "s3cret")
            .header(header::ORIGIN, ORIGIN)
    }
}

#[tokio::test]
async fn forwards_with_token_and_requires_secret() {
    let h = harness(true).await;

    let resp = h
        .get("/api/meetings?q=%E5%91%A8%E4%BC%9A")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);
    assert_eq!(
        resp.headers()["access-control-allow-origin"],
        "tauri://localhost"
    );
    assert!(resp.headers().get(proxy::OFFLINE_HEADER).is_none());
    let body: Value = resp.json().await.unwrap();
    assert_eq!(body["items"][0]["id"], "m1");
    assert_eq!(
        h.up.seen.auth.lock().unwrap().last().unwrap().as_deref(),
        Some("Bearer zsm_test")
    );
    assert_eq!(
        h.up.seen.queries.lock().unwrap().last().unwrap(),
        "q=%E5%91%A8%E4%BC%9A"
    );

    // 本机验证和 Cookie 不会转发给服务器
    let resp = h
        .get("/api/meetings")
        .header(header::COOKIE, "zsmeet_session=x")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);
    assert!(!h.up.seen.leaked.load(Ordering::SeqCst));

    // 没有 secret、secret 不对：拒绝，不转发
    let before = h.up.seen.auth.lock().unwrap().len();
    let resp = h
        .http
        .get(format!("{}/api/meetings", h.proxy))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 403);
    let resp = h
        .http
        .get(format!("{}/api/meetings", h.proxy))
        .header(proxy::SECRET_HEADER, "wrong!")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 403);
    // 查询参数里的 secret 只有录音接口认
    let resp = h
        .http
        .get(format!("{}/api/meetings?_s=s3cret", h.proxy))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 403);
    assert_eq!(h.up.seen.auth.lock().unwrap().len(), before);

    // 只转发 /api/
    let resp = h.get("/index.html").send().await.unwrap();
    assert_eq!(resp.status(), 404);
    let resp = h.get("/api/../app/authorize").send().await.unwrap();
    assert_eq!(resp.status(), 404);
    let resp = h.get("/api/%2e%2e/app").send().await.unwrap();
    assert_eq!(resp.status(), 404);
    let resp = h.get("/api/x%2F..%2F..%2Fapp").send().await.unwrap();
    assert_eq!(resp.status(), 404);
    // 换令牌、退出登录的接口界面不能借代理调用：不转发，服务器上的令牌也没被作废
    let resp = h
        .http
        .post(format!("{}/api/apps/logout", h.proxy))
        .header(proxy::SECRET_HEADER, "s3cret")
        .header(header::ORIGIN, ORIGIN)
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 404);
    assert_eq!(*h.up.seen.logout_auth.lock().unwrap(), None);
    assert_eq!(h.up.seen.auth.lock().unwrap().len(), before);

    // 别的域名解析到 127.0.0.1（DNS rebinding）：Host 不对，拒绝
    let resp = h
        .get("/api/meetings")
        .header(header::HOST, "evil.example")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 403);
}

#[tokio::test]
async fn cors_only_for_the_app() {
    let h = harness(true).await;

    let resp = h
        .http
        .request(
            reqwest::Method::OPTIONS,
            format!("{}/api/meetings", h.proxy),
        )
        .header(header::ORIGIN, ORIGIN)
        .header("access-control-request-method", "PUT")
        .header(
            "access-control-request-headers",
            "content-type,x-zsmeet-app",
        )
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 204);
    assert_eq!(resp.headers()["access-control-allow-origin"], ORIGIN);
    assert_eq!(
        resp.headers()["access-control-allow-headers"],
        "content-type,x-zsmeet-app"
    );
    assert!(resp.headers()["access-control-allow-methods"]
        .to_str()
        .unwrap()
        .contains("PUT"));

    // 别的网页：预检和请求都拒绝，就算带着正确的 secret
    for method in [reqwest::Method::OPTIONS, reqwest::Method::GET] {
        let resp = h
            .http
            .request(method, format!("{}/api/meetings", h.proxy))
            .header(header::ORIGIN, "https://evil.example")
            .header(proxy::SECRET_HEADER, "s3cret")
            .send()
            .await
            .unwrap();
        assert_eq!(resp.status(), 403);
        assert!(resp.headers().get("access-control-allow-origin").is_none());
    }
}

#[tokio::test]
async fn audio_ranges_with_query_secret() {
    let h = harness(true).await;

    // <audio> 发不了请求头：secret 在 _s 里，转发时去掉
    let resp = h
        .http
        .get(format!("{}/api/meetings/m1/audio?_s=s3cret", h.proxy))
        .header(header::RANGE, "bytes=2-5")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 206);
    assert_eq!(resp.headers()["content-range"], "bytes 2-5/10");
    assert_eq!(resp.headers()["accept-ranges"], "bytes");
    assert_eq!(resp.bytes().await.unwrap().as_ref(), b"2345");
    assert_eq!(h.up.seen.queries.lock().unwrap().last().unwrap(), "");
    assert_eq!(
        h.up.seen.auth.lock().unwrap().last().unwrap().as_deref(),
        Some("Bearer zsm_test")
    );

    let resp = h
        .http
        .get(format!("{}/api/meetings/m1/audio?_s=nope", h.proxy))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 403);

    let resp = h.get("/api/meetings/m1/audio").send().await.unwrap();
    assert_eq!(resp.status(), 200);
    assert_eq!(resp.bytes().await.unwrap().as_ref(), AUDIO);
}

#[tokio::test]
async fn streams_events_and_uploads() {
    let h = harness(true).await;

    // 第一条推送不用等整个响应结束就能收到
    let mut resp = h.get("/api/meetings/m1/events").send().await.unwrap();
    assert_eq!(resp.headers()["content-type"], "text/event-stream");
    let first = tokio::time::timeout(Duration::from_secs(5), resp.chunk())
        .await
        .expect("第一条推送应该马上到")
        .unwrap()
        .unwrap();
    assert_eq!(first.as_ref(), b"event: meeting\ndata: 1\n\n");
    h.up.seen.release.notify_one();
    let second = resp.chunk().await.unwrap().unwrap();
    assert_eq!(second.as_ref(), b"event: meeting\ndata: 2\n\n");

    // 上传：请求体原样转发，长度不变
    let body = vec![7u8; 3 * 1024 * 1024 + 5];
    let resp = h
        .http
        .put(format!("{}/api/meetings/m1/chunks/0", h.proxy))
        .header(proxy::SECRET_HEADER, "s3cret")
        .header(header::ORIGIN, ORIGIN)
        .header(header::CONTENT_TYPE, "application/octet-stream")
        .body(body.clone())
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);
    let got: Value = resp.json().await.unwrap();
    assert_eq!(got["received"], body.len());
    assert_eq!(got["content_length"], body.len().to_string());
}

#[tokio::test]
async fn upstream_401_signs_out() {
    let h = harness(true).await;
    assert!(h.f.core.state().signed_in);
    // 先缓存一点东西，退出时要一起删掉
    assert_eq!(h.get("/api/meetings").send().await.unwrap().status(), 200);
    tokio::time::sleep(Duration::from_millis(100)).await;
    let cache_dir = h.f.dir.path().join("offline");
    assert!(std::fs::read_dir(&cache_dir).unwrap().count() > 0);

    let resp = h.get("/api/me/devices").send().await.unwrap();
    assert_eq!(resp.status(), 401);
    let state = h.f.core.state();
    assert!(!state.signed_in);
    assert!(state.expired);
    assert_eq!(state.server.as_deref(), Some(h.up.url.as_str()));
    assert_eq!(h.f.tokens.get(&h.up.url).unwrap(), None);
    assert!(h.f.events.states.lock().unwrap().last().unwrap().expired);
    assert_eq!(std::fs::read_dir(&cache_dir).unwrap().count(), 0);

    // 退出以后不再带令牌
    h.get("/api/status").send().await.unwrap();
    assert_eq!(*h.up.seen.auth.lock().unwrap().last().unwrap(), None);
}

#[tokio::test]
async fn serves_cache_when_offline() {
    let mut h = harness(true).await;

    let online: Value = h
        .get("/api/meetings?mine=true")
        .send()
        .await
        .unwrap()
        .json()
        .await
        .unwrap();
    let resp = h.get("/api/meetings/m1").send().await.unwrap();
    assert_eq!(resp.status(), 200);
    // 服务器返回的错误不是离线：原样交给界面，也不缓存
    let resp = h.get("/api/templates").send().await.unwrap();
    assert_eq!(resp.status(), 500);
    assert!(resp.headers().get(proxy::OFFLINE_HEADER).is_none());
    tokio::time::sleep(Duration::from_millis(100)).await;

    // 服务器停了
    h.up.task.abort();
    let _ = (&mut h.up.task).await;

    let resp = h.get("/api/meetings?mine=true").send().await.unwrap();
    assert_eq!(resp.status(), 200);
    assert_eq!(resp.headers()[proxy::OFFLINE_HEADER], "1");
    assert!(resp.headers()["access-control-expose-headers"]
        .to_str()
        .unwrap()
        .contains("x-zsmeet-offline"));
    assert!(resp.headers()["content-type"]
        .to_str()
        .unwrap()
        .starts_with("application/json"));
    let offline: Value = resp.json().await.unwrap();
    assert_eq!(offline, online);

    // 没缓存过的（别的查询参数）、不缓存的接口：502，也标上离线
    for path in [
        "/api/meetings?mine=false",
        "/api/templates",
        "/api/meetings/m1/audio",
    ] {
        let resp = h.get(path).send().await.unwrap();
        assert_eq!(resp.status(), 502, "{path}");
        assert_eq!(resp.headers()[proxy::OFFLINE_HEADER], "1");
        let problem: Value = resp.json().await.unwrap();
        assert!(problem["detail"].as_str().unwrap().contains("连不上服务器"));
    }
    // 离线时不会被当成登录失效
    assert!(h.f.core.state().signed_in);
}

#[tokio::test]
async fn no_server_yet() {
    let up = upstream().await;
    let f = fixture("http://127.0.0.1:1");
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let proxy_url = format!("http://{}", listener.local_addr().unwrap());
    tokio::spawn(proxy::serve(
        listener,
        Proxy::new(
            f.core.clone(),
            vec![ORIGIN.into()],
            proxy::upstream_client(),
        ),
    ));
    let resp = reqwest::Client::new()
        .get(format!("{proxy_url}/api/status"))
        .header(proxy::SECRET_HEADER, "s3cret")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 503);
    drop(up);
}

#[tokio::test]
async fn set_server_checks_status() {
    let up = upstream().await;
    let f = fixture("http://127.0.0.1:1");

    *up.seen.status.lock().unwrap() = json!({"initialized": false, "api": 1});
    let err = f.core.set_server(&up.url).await.unwrap_err();
    assert!(err.contains("初始化"), "{err}");

    *up.seen.status.lock().unwrap() = json!({"initialized": true});
    let err = f.core.set_server(&up.url).await.unwrap_err();
    assert!(err.contains("升级"), "{err}");

    *up.seen.status.lock().unwrap() = json!({"hello": "world"});
    let err = f.core.set_server(&up.url).await.unwrap_err();
    assert!(err.contains("不是 zsmeet 服务器"), "{err}");

    let err = f.core.set_server("http://127.0.0.1:1").await.unwrap_err();
    assert!(err.contains("连不上"), "{err}");
    assert_eq!(f.core.state().server, None);

    *up.seen.status.lock().unwrap() = json!({"initialized": true, "api": 1});
    // 不写协议：https 连不上，退回 http
    let bare = up.url.trim_start_matches("http://").to_string() + "/";
    let state = f.core.set_server(&bare).await.unwrap();
    assert_eq!(state.server.as_deref(), Some(up.url.as_str()));
    assert!(!state.signed_in);
    // 重启以后还记得
    let saved: Value =
        serde_json::from_slice(&std::fs::read(f.dir.path().join("settings.json")).unwrap())
            .unwrap();
    assert_eq!(saved["server"], up.url.as_str());
}

/// 一台只会把 /api/status 跳转到 to 的服务器，返回它的地址。
async fn redirector(to: String) -> String {
    let app = Router::new().route(
        "/api/status",
        get(move || {
            let to = to.clone();
            async move { axum::response::Redirect::permanent(&to) }
        }),
    );
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    url
}

#[tokio::test]
async fn set_server_keeps_the_address_after_redirects() {
    let up = upstream().await;
    let f = fixture("http://127.0.0.1:1");

    // 旧地址跳到新地址：存新地址（本地代理不跟随跳转，存旧的以后每个请求都会失败）
    let old = redirector(format!("{}/api/status", up.url)).await;
    let state = f.core.set_server(&old).await.unwrap();
    assert_eq!(state.server.as_deref(), Some(up.url.as_str()));

    // 跳到别的页面（比如统一登录页）：不是 zsmeet 服务器
    let login = redirector(format!("{}/login", up.url)).await;
    let err = f.core.set_server(&login).await.unwrap_err();
    assert!(err.contains("不像是 zsmeet"), "{err}");
    assert_eq!(f.core.state().server.as_deref(), Some(up.url.as_str()));

    // 跳到另一台主机：不跟过去、不保存，告诉用户新地址让他自己填（不加密的那一跳谁都能改）
    let elsewhere = up.url.replace("127.0.0.1", "localhost");
    let moved = redirector(format!("{elsewhere}/api/status")).await;
    let asked = up.seen.auth.lock().unwrap().len();
    let err = f.core.set_server(&moved).await.unwrap_err();
    assert!(
        err.contains("另一台服务器") && err.contains(&elsewhere),
        "{err}"
    );
    assert_eq!(f.core.state().server.as_deref(), Some(up.url.as_str()));
    assert_eq!(
        up.seen.auth.lock().unwrap().len(),
        asked,
        "不该跟着跳到另一台主机"
    );
}

#[tokio::test]
async fn browser_login_round_trip() {
    let h = harness(false).await;
    let core = &h.f.core;
    assert!(!core.state().signed_in);

    let state = core.login_start().unwrap();
    assert!(state.login_pending);
    let opened = h.f.events.opened.lock().unwrap().last().unwrap().clone();
    let url = url::Url::parse(&opened).unwrap();
    assert_eq!(url.path(), "/app/authorize");
    let q: std::collections::HashMap<_, _> = url.query_pairs().into_owned().collect();
    assert_eq!(q["client"], "desktop");
    assert_eq!(q["device"], "测试的 Mac");
    let state_param = q["state"].clone();
    let challenge = q["challenge"].clone();

    // state 对不上：不认，也不发请求
    let err = core
        .handle_deep_link("zsmeet://auth?code=good&state=forged")
        .await
        .unwrap_err();
    assert!(err.contains("不是这次登录"), "{err}");
    assert!(h.up.seen.token_body.lock().unwrap().is_none());
    assert!(core.state().login_pending);

    // 服务器说 code 不对：报错，要重新点登录
    let err = core
        .handle_deep_link(&format!("zsmeet://auth?code=bad&state={state_param}"))
        .await
        .unwrap_err();
    assert!(err.contains("授权码已经失效"), "{err}");
    assert!(!core.state().login_pending);
    assert_eq!(h.f.events.errors.lock().unwrap().len(), 2);

    // 重新开始，这次成功
    core.login_start().unwrap();
    let opened = h.f.events.opened.lock().unwrap().last().unwrap().clone();
    let q: std::collections::HashMap<_, _> = url::Url::parse(&opened)
        .unwrap()
        .query_pairs()
        .into_owned()
        .collect();
    assert_ne!(q["challenge"], challenge);
    core.handle_deep_link(&format!("zsmeet://auth?code=good&state={}", q["state"]))
        .await
        .unwrap();
    let body = h.up.seen.token_body.lock().unwrap().clone().unwrap();
    assert_eq!(body["code"], "good");
    // 发给服务器的 verifier 和授权页上的 challenge 对得上
    assert_eq!(
        server::challenge(body["code_verifier"].as_str().unwrap()),
        q["challenge"]
    );
    let state = core.state();
    assert!(state.signed_in);
    assert!(!state.login_pending);
    assert_eq!(state.user.as_ref().unwrap()["name"], "李雷");
    assert_eq!(
        h.f.tokens.get(&h.up.url).unwrap().as_deref(),
        Some("zsm_new")
    );
    assert!(h.f.events.states.lock().unwrap().last().unwrap().signed_in);

    // 同一个链接再来一次：已经用过了
    assert!(core
        .handle_deep_link(&format!("zsmeet://auth?code=good&state={}", q["state"]))
        .await
        .is_err());
    assert!(core.state().signed_in);

    // 之后的请求带上新令牌
    h.get("/api/status").send().await.unwrap();
    assert_eq!(
        h.up.seen.auth.lock().unwrap().last().unwrap().as_deref(),
        Some("Bearer zsm_new")
    );
}

#[tokio::test]
async fn logout_and_switch_server() {
    let h = harness(true).await;
    let core = &h.f.core;

    let state = core.logout().await;
    assert!(!state.signed_in);
    assert!(!state.expired);
    assert_eq!(state.server.as_deref(), Some(h.up.url.as_str()));
    assert_eq!(
        h.up.seen.logout_auth.lock().unwrap().as_deref(),
        Some("Bearer zsm_test")
    );
    assert_eq!(h.f.tokens.get(&h.up.url).unwrap(), None);

    let state = core.switch_server().await;
    assert_eq!(state.server, None);
    // 服务器地址都没了：代理不知道转发给谁
    let resp = h.get("/api/status").send().await.unwrap();
    assert_eq!(resp.status(), 503);
}

#[tokio::test]
async fn session_expired_only_for_current_token() {
    let h = harness(true).await;
    // 旧令牌的 401（比如重新登录前发出的请求）不影响现在的登录
    h.f.core.unauthorized("zsm_old");
    assert!(h.f.core.state().signed_in);
    let state = h.f.core.session_expired();
    assert!(!state.signed_in);
    assert!(state.expired);
}
