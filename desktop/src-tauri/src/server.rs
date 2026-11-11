//! 直接和 zsmeet 服务器打交道的几件事：检查服务器地址、PKCE、用一次性的 code 换令牌、退出登录。
//! 界面发出的接口请求不走这里，走本地代理（proxy.rs）。

use std::time::Duration;

use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use rand::RngCore;
use serde::Deserialize;
use serde_json::Value;
use sha2::{Digest, Sha256};
use url::Url;

/// App 需要的最低接口版本（`/api/status` 里的 `api`）。
pub const MIN_API: i64 = 1;

/// 检查服务器、换令牌这些一次性的请求最多等多久。
const REQUEST_TIMEOUT: Duration = Duration::from_secs(15);

/// 把用户填的地址整理成候选的服务器地址，按顺序试。
///
/// - 去掉首尾空白、末尾的「/」，以及查询参数和「#」后面的部分
/// - 没写 `http://` 或 `https://` 时先试 https，再试 http（团队内网里的服务器常常只有 http）
/// - 只接受 http 和 https，不接受带用户名密码的地址
pub fn candidates(input: &str) -> Result<Vec<String>, String> {
    let input = input.trim();
    if input.is_empty() {
        return Err("请填写服务器地址，比如 https://meet.example.com".into());
    }
    let has_scheme = input.contains("://");
    let raw: Vec<String> = if has_scheme {
        vec![input.to_string()]
    } else {
        vec![format!("https://{input}"), format!("http://{input}")]
    };
    raw.iter().map(|s| normalize(s)).collect()
}

fn normalize(raw: &str) -> Result<String, String> {
    let bad =
        || format!("「{raw}」不是有效的服务器地址，请填写像 https://meet.example.com 这样的地址");
    let mut url = Url::parse(raw).map_err(|_| bad())?;
    if url.scheme() != "http" && url.scheme() != "https" {
        return Err("服务器地址要以 http:// 或 https:// 开头".into());
    }
    if url.host_str().is_none_or(str::is_empty) {
        return Err(bad());
    }
    if !url.username().is_empty() || url.password().is_some() {
        return Err("服务器地址里不要带用户名和密码".into());
    }
    url.set_query(None);
    url.set_fragment(None);
    let path = url.path().trim_end_matches('/').to_string();
    url.set_path(&path);
    Ok(url.as_str().trim_end_matches('/').to_string())
}

#[derive(Deserialize)]
struct Status {
    initialized: Option<bool>,
    api: Option<i64>,
}

/// 检查填的地址是不是能用的 zsmeet 服务器，返回整理好的地址。
pub async fn check_server(client: &reqwest::Client, input: &str) -> Result<String, String> {
    let candidates = candidates(input)?;
    let mut last_err = String::new();
    for (i, server) in candidates.iter().enumerate() {
        let resp = client
            .get(format!("{server}/api/status"))
            .timeout(REQUEST_TIMEOUT)
            .send()
            .await;
        let resp = match resp {
            Ok(r) => r,
            Err(e) => {
                last_err = if e.is_timeout() {
                    format!("{server} 很久都没有响应，请检查地址和网络")
                } else {
                    format!("连不上 {server}，请检查地址和网络，或者确认服务器正在运行")
                };
                // 没写协议时 https 连不上，接着试 http
                if i + 1 < candidates.len() && (e.is_connect() || e.is_timeout()) {
                    continue;
                }
                return Err(last_err);
            }
        };
        let not_zsmeet = || format!("{server} 不是 zsmeet 服务器，请检查地址");
        if !resp.status().is_success() {
            return Err(not_zsmeet());
        }
        let status: Status = resp.json().await.map_err(|_| not_zsmeet())?;
        let Some(initialized) = status.initialized else {
            return Err(not_zsmeet());
        };
        if !initialized {
            return Err(format!(
                "这台服务器还没完成初始化：请先在浏览器里打开 {server}，创建管理员账号"
            ));
        }
        if status.api.unwrap_or(0) < MIN_API {
            return Err(
                "这台服务器的 zsmeet 版本太旧，还不支持桌面版。请让管理员把服务器升级到最新版本"
                    .into(),
            );
        }
        return Ok(server.clone());
    }
    Err(last_err)
}

/// 一次「在浏览器里登录」的 PKCE 参数。
pub struct Pkce {
    pub verifier: String,
    pub challenge: String,
    pub state: String,
}

impl Pkce {
    pub fn new() -> Self {
        let verifier = random_token(32);
        Self {
            challenge: challenge(&verifier),
            verifier,
            state: random_token(16),
        }
    }
}

/// S256：BASE64URL(SHA256(code_verifier))，不带「=」。
pub fn challenge(verifier: &str) -> String {
    URL_SAFE_NO_PAD.encode(Sha256::digest(verifier.as_bytes()))
}

/// n 个字节的随机数，编码成 base64url。
pub fn random_token(n: usize) -> String {
    let mut buf = vec![0u8; n];
    rand::rng().fill_bytes(&mut buf);
    URL_SAFE_NO_PAD.encode(buf)
}

/// 浏览器里的授权页地址。
pub fn authorize_url(server: &str, device: &str, pkce: &Pkce) -> String {
    let mut url = Url::parse(&format!("{server}/app/authorize")).expect("服务器地址已经检查过");
    url.query_pairs_mut()
        .append_pair("client", "desktop")
        .append_pair("device", device)
        .append_pair("challenge", &pkce.challenge)
        .append_pair("state", &pkce.state);
    url.into()
}

/// 浏览器登录完跳回来的 `zsmeet://auth?code=…&state=…`。
#[derive(Debug, PartialEq)]
pub struct Callback {
    pub code: String,
    pub state: String,
}

pub fn parse_callback(link: &str) -> Result<Callback, String> {
    let bad = || "收到的登录链接不完整，请回到 zsmeet 重新点「在浏览器里登录」".to_string();
    let url = Url::parse(link).map_err(|_| bad())?;
    if url.scheme() != "zsmeet" || url.host_str() != Some("auth") {
        return Err(bad());
    }
    let mut code = None;
    let mut state = None;
    for (k, v) in url.query_pairs() {
        match k.as_ref() {
            "code" => code = Some(v.into_owned()),
            "state" => state = Some(v.into_owned()),
            _ => {}
        }
    }
    match (code, state) {
        (Some(code), Some(state)) if !code.is_empty() && !state.is_empty() => {
            Ok(Callback { code, state })
        }
        _ => Err(bad()),
    }
}

#[derive(Deserialize)]
struct TokenResponse {
    token: String,
    user: Value,
}

#[derive(Deserialize)]
struct Problem {
    detail: Option<String>,
}

/// 用一次性的 code 换 App 令牌：`POST /api/apps/token {code, code_verifier}` → `{token, user}`。
pub async fn exchange(
    client: &reqwest::Client,
    server: &str,
    code: &str,
    verifier: &str,
) -> Result<(String, Value), String> {
    let resp = client
        .post(format!("{server}/api/apps/token"))
        .timeout(REQUEST_TIMEOUT)
        .json(&serde_json::json!({ "code": code, "code_verifier": verifier }))
        .send()
        .await
        .map_err(|_| format!("连不上 {server}，登录没有完成。请检查网络后重新登录"))?;
    if !resp.status().is_success() {
        let status = resp.status();
        let detail = resp
            .json::<Problem>()
            .await
            .ok()
            .and_then(|p| p.detail)
            .filter(|d| !d.is_empty());
        return Err(match detail {
            Some(d) => format!("登录没有完成：{d}。请重新点「在浏览器里登录」"),
            None => format!(
                "登录没有完成（服务器返回 {}），请重新点「在浏览器里登录」",
                status.as_u16()
            ),
        });
    }
    let body: TokenResponse = resp
        .json()
        .await
        .map_err(|_| "服务器返回的登录结果看不懂，请确认服务器是最新版本后重新登录".to_string())?;
    if body.token.is_empty() || user_id(&body.user).is_none() {
        return Err("服务器返回的登录结果不完整，请确认服务器是最新版本后重新登录".into());
    }
    Ok((body.token, body.user))
}

/// 告诉服务器这台电脑退出了（`POST /api/apps/logout`）。连不上也不要紧，本地照样退出。
pub async fn revoke(client: &reqwest::Client, server: &str, token: &str) {
    let result = client
        .post(format!("{server}/api/apps/logout"))
        .bearer_auth(token)
        .timeout(Duration::from_secs(5))
        .send()
        .await;
    if let Err(e) = result {
        log::warn!("通知服务器退出登录失败：{e}");
    }
}

/// 用户的 id。接口里是字符串，保险起见数字也认。
pub fn user_id(user: &Value) -> Option<String> {
    match user.get("id")? {
        Value::String(s) if !s.is_empty() => Some(s.clone()),
        Value::Number(n) => Some(n.to_string()),
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pkce_matches_rfc7636_example() {
        // RFC 7636 附录 B 的例子
        assert_eq!(
            challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"),
            "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
        );
        let p = Pkce::new();
        assert_eq!(p.verifier.len(), 43);
        assert_eq!(p.challenge, challenge(&p.verifier));
        assert_ne!(Pkce::new().state, p.state);
    }

    #[test]
    fn normalizes_addresses() {
        assert_eq!(
            candidates(" https://Meet.Example.com/ ").unwrap(),
            vec!["https://meet.example.com"]
        );
        assert_eq!(
            candidates("192.168.1.5:8080").unwrap(),
            vec!["https://192.168.1.5:8080", "http://192.168.1.5:8080"]
        );
        assert_eq!(
            candidates("http://host/zsmeet/?a=1#x").unwrap(),
            vec!["http://host/zsmeet"]
        );
        assert!(candidates("").is_err());
        assert!(candidates("ftp://host").is_err());
        assert!(candidates("https://user:pw@host").is_err());
        assert!(candidates("https://").is_err());
    }

    #[test]
    fn builds_authorize_url() {
        let p = Pkce {
            verifier: "v".into(),
            challenge: "c-_".into(),
            state: "s".into(),
        };
        assert_eq!(
            authorize_url("http://host:8080", "张三的 MacBook", &p),
            "http://host:8080/app/authorize?client=desktop&device=%E5%BC%A0%E4%B8%89%E7%9A%84+MacBook&challenge=c-_&state=s"
        );
    }

    #[test]
    fn parses_callback() {
        assert_eq!(
            parse_callback("zsmeet://auth?code=abc&state=xyz").unwrap(),
            Callback {
                code: "abc".into(),
                state: "xyz".into()
            }
        );
        assert!(parse_callback("zsmeet://auth?code=abc").is_err());
        assert!(parse_callback("zsmeet://other?code=a&state=b").is_err());
        assert!(parse_callback("https://auth?code=a&state=b").is_err());
        assert!(parse_callback("not a url").is_err());
    }

    #[test]
    fn reads_user_id() {
        assert_eq!(
            user_id(&serde_json::json!({"id": "u1"})).as_deref(),
            Some("u1")
        );
        assert_eq!(user_id(&serde_json::json!({"id": 7})).as_deref(), Some("7"));
        assert_eq!(user_id(&serde_json::json!({"name": "x"})), None);
    }
}
