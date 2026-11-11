//! 本地代理：只监听 127.0.0.1 的随机端口，界面的接口请求都发给它，它转发给服务器。
//!
//! - 加上 `Authorization: Bearer <App 令牌>`。令牌只在 Rust 这边，界面拿不到。
//! - 请求要带这次启动随机生成的 `X-Zsmeet-App`，没有的拒绝：本机别的程序、浏览器里的网页借不了它。
//!   `<audio>` 加不了请求头，所以录音接口（GET …/audio）也认查询参数 `_s`。
//! - 跨域只认 App 界面的来源（`tauri://localhost`；开发时还有 Vite 的地址）。
//! - 请求体、响应体都是流式转发；`Range`、`Content-Range`、206 原样经过，录音能拖动，进度推送能用。
//! - 服务器返回 401：这个令牌不能用了，App 退出登录并通知界面。
//! - 离线：几种 GET 请求（见 cache.rs）成功的结果存在本地；连不上服务器（连接失败、超时，不包括服务器返回的错误）时
//!   拿存下的结果返回，加上 `X-Zsmeet-Offline: 1`。

use std::sync::Arc;
use std::time::Duration;

use axum::body::Body;
use axum::extract::{Request, State};
use axum::http::{header, HeaderMap, HeaderValue, Method, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::Router;
use bytes::{Bytes, BytesMut};
use futures_util::{stream, StreamExt};

use crate::cache::{self, MAX_ENTRY_BYTES};
use crate::core::Core;
use crate::server;

pub const SECRET_HEADER: &str = "x-zsmeet-app";
pub const OFFLINE_HEADER: &str = "x-zsmeet-offline";

/// 可以缓存的请求等服务器多久（到收到响应头为止）。超过就当连不上，用离线缓存。
/// 其他请求不限时：进度推送是长连接，上传大文件也要很久。
const CACHEABLE_TIMEOUT: Duration = Duration::from_secs(20);

/// 逐跳的请求头和不该转发的头：交给 HTTP 库自己处理，或者这里另外设置。
const SKIP_REQUEST: &[&str] = &[
    "connection",
    "keep-alive",
    "proxy-connection",
    "proxy-authorization",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
    "host",
    "origin",
    "referer",
    "cookie",
    "authorization",
    "accept-encoding",
    SECRET_HEADER,
];

const SKIP_RESPONSE: &[&str] = &[
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
    "set-cookie",
    "access-control-allow-origin",
    "access-control-allow-credentials",
    "access-control-expose-headers",
    "access-control-allow-methods",
    "access-control-allow-headers",
    "access-control-max-age",
];

/// 界面要读到的响应头（跨域时默认读不到）。
const EXPOSE: &str =
    "x-zsmeet-offline, content-disposition, content-range, accept-ranges, content-length";

pub struct Proxy {
    core: Arc<Core>,
    client: reqwest::Client,
    origins: Vec<String>,
}

impl Proxy {
    /// origins：允许跨域访问的来源。client 用来转发（测试里可以关掉连接复用）。
    pub fn new(core: Arc<Core>, origins: Vec<String>, client: reqwest::Client) -> Arc<Self> {
        Arc::new(Self {
            core,
            client,
            origins,
        })
    }

    pub fn router(self: Arc<Self>) -> Router {
        Router::new().fallback(handle).with_state(self)
    }

    fn allowed_origin(&self, headers: &HeaderMap) -> Option<HeaderValue> {
        let origin = headers.get(header::ORIGIN)?;
        let s = origin.to_str().ok()?;
        self.origins.iter().any(|o| o == s).then(|| origin.clone())
    }
}

/// 转发用的 HTTP 客户端：连接超时短一点，好尽快判断成离线；不自动跟随跳转，原样交给界面。
pub fn upstream_client() -> reqwest::Client {
    reqwest::Client::builder()
        .connect_timeout(Duration::from_secs(8))
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .expect("HTTP 客户端初始化失败")
}

pub async fn serve(listener: tokio::net::TcpListener, proxy: Arc<Proxy>) {
    if let Err(e) = axum::serve(listener, proxy.router()).await {
        log::error!("本地代理停止了：{e}");
    }
}

/// 出错时返回和服务器一样格式的错误（problem+json），界面直接显示 detail。
fn problem(status: StatusCode, detail: &str) -> Response {
    let body = serde_json::json!({
        "title": status.canonical_reason().unwrap_or(""),
        "status": status.as_u16(),
        "detail": detail,
    });
    (
        status,
        [(header::CONTENT_TYPE, "application/problem+json")],
        body.to_string(),
    )
        .into_response()
}

/// 长度一样时逐字节比较全部内容，比较时间不泄露 secret 的前几位对不对。
fn same_secret(given: &[u8], secret: &str) -> bool {
    let secret = secret.as_bytes();
    given.len() == secret.len()
        && given
            .iter()
            .zip(secret)
            .fold(0u8, |acc, (a, b)| acc | (a ^ b))
            == 0
}

/// 本地代理只转发 /api/ 下面、界面要用的接口。路径的每一段都要是普通的词或者 id（见 server::api_segments）：
/// 编码过的字符、「..」、空段这些转发出去以后可能落到别的接口、别的路径上，把令牌带过去。
fn check_path(path: &str) -> Result<(), &'static str> {
    let Some(segments) = server::api_segments(path) else {
        return Err("本地代理只转发 /api/ 下的接口");
    };
    // 换令牌、退出登录这些接口只有 App 自己（Rust 这边）用：界面不能借代理拿当前的令牌去调
    if segments[0].eq_ignore_ascii_case("apps") {
        return Err("本地代理不转发登录相关的接口");
    }
    Ok(())
}

/// 录音接口：GET/HEAD /api/meetings/{id}/audio
fn is_audio(method: &Method, path: &str) -> bool {
    if method != Method::GET && method != Method::HEAD {
        return false;
    }
    let parts: Vec<&str> = path.split('/').collect();
    matches!(parts.as_slice(), ["", "api", "meetings", id, "audio"] if !id.is_empty())
}

/// 从查询参数里拿出 `_s`，剩下的原样保留。
fn split_secret(query: Option<&str>) -> (Option<String>, Option<String>) {
    let Some(query) = query else {
        return (None, None);
    };
    let mut secret = None;
    let mut rest = url::form_urlencoded::Serializer::new(String::new());
    let mut kept = 0;
    for (k, v) in url::form_urlencoded::parse(query.as_bytes()) {
        if k == "_s" {
            secret = Some(v.into_owned());
        } else {
            rest.append_pair(&k, &v);
            kept += 1;
        }
    }
    (secret, (kept > 0).then(|| rest.finish()))
}

async fn handle(State(proxy): State<Arc<Proxy>>, req: Request) -> Response {
    let origin = proxy.allowed_origin(req.headers());
    let has_origin = req.headers().contains_key(header::ORIGIN);
    let mut resp = route(&proxy, req, has_origin && origin.is_none()).await;
    if let Some(origin) = origin {
        let h = resp.headers_mut();
        h.insert(header::ACCESS_CONTROL_ALLOW_ORIGIN, origin);
        h.insert(
            header::ACCESS_CONTROL_EXPOSE_HEADERS,
            HeaderValue::from_static(EXPOSE),
        );
    }
    resp.headers_mut()
        .append(header::VARY, HeaderValue::from_static("origin"));
    resp
}

async fn route(proxy: &Proxy, req: Request, foreign_origin: bool) -> Response {
    if foreign_origin {
        return problem(StatusCode::FORBIDDEN, "只接受 zsmeet 桌面版界面发来的请求");
    }
    // 跨域预检：带不了自定义请求头，所以不查 secret，只回答允许什么
    if req.method() == Method::OPTIONS {
        let mut resp = StatusCode::NO_CONTENT.into_response();
        let h = resp.headers_mut();
        h.insert(
            header::ACCESS_CONTROL_ALLOW_METHODS,
            HeaderValue::from_static("GET, HEAD, POST, PUT, PATCH, DELETE"),
        );
        let asked = req
            .headers()
            .get(header::ACCESS_CONTROL_REQUEST_HEADERS)
            .cloned()
            .unwrap_or(HeaderValue::from_static("content-type, x-zsmeet-app"));
        h.insert(header::ACCESS_CONTROL_ALLOW_HEADERS, asked);
        h.insert(
            header::ACCESS_CONTROL_MAX_AGE,
            HeaderValue::from_static("600"),
        );
        return resp;
    }

    // 防 DNS rebinding：别的网站把自己的域名解析到 127.0.0.1 时，Host 是它的域名
    let host = req
        .headers()
        .get(header::HOST)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    if !host.starts_with("127.0.0.1:") && !host.starts_with("localhost:") {
        return problem(StatusCode::FORBIDDEN, "只接受发给 127.0.0.1 的请求");
    }
    let path = req.uri().path().to_string();
    if let Err(detail) = check_path(&path) {
        return problem(StatusCode::NOT_FOUND, detail);
    }
    let (query_secret, query) = if is_audio(req.method(), &path) {
        split_secret(req.uri().query())
    } else {
        (None, req.uri().query().map(str::to_string))
    };
    let header_ok = req
        .headers()
        .get(SECRET_HEADER)
        .is_some_and(|v| same_secret(v.as_bytes(), proxy.core.secret()));
    let query_ok = query_secret.is_some_and(|s| same_secret(s.as_bytes(), proxy.core.secret()));
    if !header_ok && !query_ok {
        return problem(
            StatusCode::FORBIDDEN,
            "缺少 zsmeet 桌面版的本机验证，请求被拒绝",
        );
    }

    let Some(upstream) = proxy.core.upstream() else {
        return problem(
            StatusCode::SERVICE_UNAVAILABLE,
            "还没有设置服务器地址，请先填写服务器地址",
        );
    };
    let path_and_query = match &query {
        Some(q) => format!("{path}?{q}"),
        None => path.clone(),
    };
    let method = req.method().clone();
    let cache = upstream
        .cache
        .clone()
        .filter(|_| cache::cacheable(method.as_str(), &path));
    let cache_key = cache::key(&path_and_query);
    let Some(target) = server::api_url(&upstream.server, &path_and_query) else {
        return problem(StatusCode::NOT_FOUND, "本地代理只转发 /api/ 下的接口");
    };

    // 转发请求
    let mut builder = proxy.client.request(method.clone(), target);
    for (name, value) in req.headers() {
        if !SKIP_REQUEST.contains(&name.as_str()) {
            builder = builder.header(name, value);
        }
    }
    if let Some(token) = &upstream.token {
        builder = builder.bearer_auth(token);
    }
    if method != Method::GET && method != Method::HEAD {
        let body = req.into_body().into_data_stream();
        builder = builder.body(reqwest::Body::wrap_stream(body));
    }

    let sent = if cache.is_some() {
        match tokio::time::timeout(CACHEABLE_TIMEOUT, builder.send()).await {
            Ok(r) => r.map_err(Failure::from),
            Err(_) => Err(Failure::Unreachable),
        }
    } else {
        builder.send().await.map_err(Failure::from)
    };
    let upstream_resp = match sent {
        Ok(r) => r,
        Err(Failure::Unreachable) => {
            if let Some(cache) = &cache {
                if let Some(entry) = cache.get(&cache_key).await {
                    let mut resp = (
                        StatusCode::OK,
                        [(header::CONTENT_TYPE, entry.content_type)],
                        entry.body,
                    )
                        .into_response();
                    resp.headers_mut()
                        .insert(OFFLINE_HEADER, HeaderValue::from_static("1"));
                    return resp;
                }
            }
            let mut detail = format!(
                "连不上服务器 {}，请检查网络，或者确认服务器正在运行",
                upstream.server
            );
            if server::is_lan_address(&upstream.server) {
                detail.push('。');
                detail.push_str(server::LOCAL_NETWORK_HINT);
            }
            let mut resp = problem(StatusCode::BAD_GATEWAY, &detail);
            resp.headers_mut()
                .insert(OFFLINE_HEADER, HeaderValue::from_static("1"));
            return resp;
        }
        Err(Failure::Other(e)) => {
            log::warn!("转发 {method} {path} 失败：{e}");
            return problem(StatusCode::BAD_GATEWAY, "请求没能发到服务器，请重试");
        }
    };

    let status = upstream_resp.status();
    if status == StatusCode::UNAUTHORIZED {
        if let Some(token) = &upstream.token {
            proxy.core.unauthorized(token);
        }
    }

    let mut headers = HeaderMap::new();
    for (name, value) in upstream_resp.headers() {
        if !SKIP_RESPONSE.contains(&name.as_str()) {
            headers.append(name.clone(), value.clone());
        }
    }

    let body = match cache {
        Some(cache) if status == StatusCode::OK => {
            let content_type = headers
                .get(header::CONTENT_TYPE)
                .and_then(|v| v.to_str().ok())
                .unwrap_or("application/json")
                .to_string();
            tee_into_cache(upstream_resp, cache, cache_key, content_type).await
        }
        _ => Body::from_stream(upstream_resp.bytes_stream()),
    };
    let mut resp = Response::new(body);
    *resp.status_mut() = status;
    *resp.headers_mut() = headers;
    resp
}

enum Failure {
    /// 连不上：连接失败、超时。只有这种情况才用离线缓存
    Unreachable,
    Other(reqwest::Error),
}

impl From<reqwest::Error> for Failure {
    fn from(e: reqwest::Error) -> Self {
        if e.is_connect() || e.is_timeout() {
            Failure::Unreachable
        } else {
            Failure::Other(e)
        }
    }
}

/// 可以缓存的响应（都是 JSON，不大）：先读完整，存进缓存，再交给界面。
/// 读到一半发现超过上限，就不存了，已经读到的和剩下的接着流式交给界面。
async fn tee_into_cache(
    resp: reqwest::Response,
    cache: cache::OfflineCache,
    key: String,
    content_type: String,
) -> Body {
    let mut stream = resp.bytes_stream();
    let mut buf = BytesMut::new();
    while let Some(chunk) = stream.next().await {
        match chunk {
            Ok(chunk) => {
                buf.extend_from_slice(&chunk);
                if buf.len() > MAX_ENTRY_BYTES {
                    let head =
                        stream::once(async move { Ok::<Bytes, reqwest::Error>(buf.freeze()) });
                    return Body::from_stream(head.chain(stream));
                }
            }
            Err(e) => {
                // 读到一半断了：已经读到的交给界面，再报个错，界面会当成请求失败
                let head = stream::iter([Ok(buf.freeze()), Err(e)]);
                return Body::from_stream(head);
            }
        }
    }
    let body = buf.freeze();
    let stored = body.clone();
    tokio::spawn(async move { cache.put(&key, &content_type, &stored).await });
    Body::from(body)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_api_paths() {
        for ok in [
            "/api/status",
            "/api/meetings/0b5e2c1a-7d3f-4c1e-9a55-2f0d6c1b9e77/audio",
            "/api/me/devices",
        ] {
            assert_eq!(check_path(ok), Ok(()), "{ok}");
        }
        for bad in [
            "/",
            "/index.html",
            "/api",
            "/api/../app/authorize",
            "/api/./status",
            "/api/%2e%2e/app",
            "/api/meetings/%2E%2E/%2e./x",
            "/api/x%2f..%2f..%2fother",
            "/api/x%5c..%5cother",
            "/api/x\\..\\other",
            "/api/apps/logout",
            "/api/apps/token",
            "/api/apps",
            // 编码过的字母、大小写、空段也绕不过去
            "/api/%61pps/logout",
            "/api/app%73/token",
            "/api/Apps/logout",
            "/api//apps/logout",
        ] {
            assert!(check_path(bad).is_err(), "{bad}");
        }
    }
}
