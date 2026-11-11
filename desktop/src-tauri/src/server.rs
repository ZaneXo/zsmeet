//! 直接和 zsmeet 服务器打交道的几件事：检查服务器地址、PKCE、用一次性的 code 换令牌、退出登录。
//! 界面发出的接口请求不走这里，走本地代理（proxy.rs）。

use std::net::IpAddr;
use std::time::Duration;

use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use rand::RngCore;
use serde::Deserialize;
use serde_json::Value;
use sha2::{Digest, Sha256};
use url::{Host, Url};

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

/// 检查填的地址是不是能用的 zsmeet 服务器，返回整理好的地址（有跳转的话是跳转以后的地址）。
pub async fn check_server(client: &reqwest::Client, input: &str) -> Result<String, String> {
    let candidates = candidates(input)?;
    for (i, server) in candidates.iter().enumerate() {
        let sent = client
            .get(format!("{server}/api/status"))
            .timeout(REQUEST_TIMEOUT)
            .send()
            .await;
        let resp = match sent {
            Ok(resp) => resp,
            Err(e) => {
                let reach = reach(server).await;
                let message =
                    unreachable_message(server, e.is_timeout(), is_certificate_error(&e), reach);
                if i + 1 == candidates.len() {
                    return Err(message);
                }
                // 没写协议、https 连不上：内网的服务器常常只有 http，接着试；外网的不悄悄退回不加密的 http
                if reach == Reach::Internet {
                    return Err(format!(
                        "{message}。如果这台服务器只支持 http，请在地址前面写上 http://"
                    ));
                }
                continue;
            }
        };
        return read_status(server, resp).await;
    }
    Err("请填写服务器地址，比如 https://meet.example.com".into())
}

/// 看 `/api/status` 的结果，返回服务器地址。
async fn read_status(requested: &str, resp: reqwest::Response) -> Result<String, String> {
    let server = landed_server(requested, resp.url())?;
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
            "这台服务器的 zsmeet 版本太旧，还不支持桌面版。请让管理员把服务器升级到最新版本".into(),
        );
    }
    Ok(server)
}

/// 请求 `{requested}/api/status` 跟着跳转最后落到了 landed，由此得出服务器地址。
/// 要存跳转以后的地址（比如 http 跳到了 https）：本地代理不跟随跳转，存原来的地址以后每个请求都会失败。
/// 从 https 跳到 http 不接受。
fn landed_server(requested: &str, landed: &Url) -> Result<String, String> {
    if requested.starts_with("https://") && landed.scheme() != "https" {
        return Err(format!(
            "{requested} 把 https 跳转到了不加密的 http，zsmeet 不会这样连接。请检查服务器的设置"
        ));
    }
    let Some(base) = landed.path().strip_suffix("/api/status") else {
        return Err(format!(
            "{requested} 跳转到了 {landed}，不像是 zsmeet 服务器，请检查地址"
        ));
    };
    let mut url = landed.clone();
    url.set_path(base);
    normalize(url.as_str())
}

/// 局域网里的服务器连不上，多半是没允许 zsmeet 访问本地网络（macOS 15 起要用户允许）。
pub const LOCAL_NETWORK_HINT: &str = "如果服务器在局域网里，还要允许 zsmeet 访问本地网络：打开「系统设置 → 隐私与安全性 → 本地网络」，打开 zsmeet 的开关，再试一次";

/// 连不上服务器时给用户看的话。timeout：等了太久；certificate：https 证书不受信任。
fn unreachable_message(server: &str, timeout: bool, certificate: bool, reach: Reach) -> String {
    if certificate {
        return format!(
            "{server} 的 https 证书不受信任，zsmeet 不能安全地连接它。如果是公司内部签发的证书，请先把它的根证书装进「钥匙串访问」并设为信任"
        );
    }
    let mut message = if timeout {
        format!("{server} 很久都没有响应，请检查地址和网络")
    } else {
        format!("连不上 {server}，请检查地址和网络，或者确认服务器正在运行")
    };
    if reach == Reach::Lan {
        message.push('。');
        message.push_str(LOCAL_NETWORK_HINT);
    }
    message
}

/// https 证书的问题。rustls 的错误藏在好几层 source 里，Debug 文本里都有，看它最省事。
fn is_certificate_error(e: &reqwest::Error) -> bool {
    format!("{e:?}").contains("InvalidCertificate")
}

/// 服务器在哪。局域网里的机器要用户允许 zsmeet 访问「本地网络」才连得上。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Reach {
    /// 本机
    Loopback,
    /// 局域网：内网地址、链路本地地址、`.local`、不带点的机器名，或者解析到内网地址的域名
    Lan,
    Internet,
}

fn ip_reach(ip: IpAddr) -> Reach {
    match ip {
        IpAddr::V4(v4) if v4.is_loopback() => Reach::Loopback,
        IpAddr::V4(v4) if v4.is_private() || v4.is_link_local() => Reach::Lan,
        IpAddr::V4(_) => Reach::Internet,
        IpAddr::V6(v6) if v6.is_loopback() => Reach::Loopback,
        IpAddr::V6(v6) if v6.is_unique_local() || v6.is_unicast_link_local() => Reach::Lan,
        IpAddr::V6(v6) => match v6.to_ipv4_mapped() {
            Some(v4) => ip_reach(IpAddr::V4(v4)),
            None => Reach::Internet,
        },
    }
}

/// 不查 DNS 能看出来的：IP 地址、localhost、`.local`、不带点的机器名。看不出来返回 None。
fn host_reach(host: &Host<&str>) -> Option<Reach> {
    match host {
        Host::Ipv4(ip) => Some(ip_reach(IpAddr::V4(*ip))),
        Host::Ipv6(ip) => Some(ip_reach(IpAddr::V6(*ip))),
        Host::Domain(name) => {
            let name = name.trim_end_matches('.');
            if name == "localhost" || name.ends_with(".localhost") {
                Some(Reach::Loopback)
            } else if name.ends_with(".local") || !name.contains('.') {
                Some(Reach::Lan)
            } else {
                None
            }
        }
    }
}

/// 服务器地址在哪。域名要查一下 DNS：解析到内网地址的也算局域网；查不到当外网。
pub async fn reach(server: &str) -> Reach {
    let Ok(url) = Url::parse(server) else {
        return Reach::Internet;
    };
    let Some(host) = url.host() else {
        return Reach::Internet;
    };
    if let Some(reach) = host_reach(&host) {
        return reach;
    }
    let Host::Domain(name) = host else {
        return Reach::Internet;
    };
    let port = url.port_or_known_default().unwrap_or(443);
    let lookup = tokio::net::lookup_host((name, port));
    let Ok(Ok(addrs)) = tokio::time::timeout(Duration::from_secs(3), lookup).await else {
        return Reach::Internet;
    };
    let reaches: Vec<Reach> = addrs.map(|a| ip_reach(a.ip())).collect();
    if reaches.contains(&Reach::Lan) {
        Reach::Lan
    } else if reaches.contains(&Reach::Loopback) {
        Reach::Loopback
    } else {
        Reach::Internet
    }
}

/// 只看地址本身（不查 DNS）是不是局域网里的机器。
pub fn is_lan_address(server: &str) -> bool {
    Url::parse(server)
        .is_ok_and(|url| url.host().and_then(|host| host_reach(&host)) == Some(Reach::Lan))
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
    fn works_out_where_the_server_is() {
        let r = |s: &str| host_reach(&Url::parse(s).unwrap().host().unwrap());
        for lan in [
            "http://10.200.25.191:18090",
            "http://192.168.1.5",
            "http://172.16.0.9",
            "http://169.254.3.4",
            "http://[fd12::1]",
            "http://[fe80::1]",
            "http://[::ffff:10.0.0.1]",
            "http://meet.local",
            "http://Meetbox:8080",
        ] {
            assert_eq!(r(lan), Some(Reach::Lan), "{lan}");
        }
        for local in ["http://127.0.0.1:8080", "http://[::1]", "http://localhost"] {
            assert_eq!(r(local), Some(Reach::Loopback), "{local}");
        }
        assert_eq!(r("http://8.8.8.8"), Some(Reach::Internet));
        assert_eq!(r("http://[2606:4700::1111]"), Some(Reach::Internet));
        assert_eq!(r("https://meet.example.com"), None);
        assert!(is_lan_address("http://10.200.25.191:18090"));
        assert!(!is_lan_address("http://127.0.0.1:8080"));
        assert!(!is_lan_address("https://meet.example.com"));
    }

    #[tokio::test]
    async fn reach_without_dns_for_addresses() {
        assert_eq!(reach("http://10.1.2.3:8080").await, Reach::Lan);
        assert_eq!(reach("http://127.0.0.1:1").await, Reach::Loopback);
        assert_eq!(reach("not a url").await, Reach::Internet);
    }

    #[test]
    fn keeps_the_address_after_redirects() {
        let u = |s: &str| Url::parse(s).unwrap();
        assert_eq!(
            landed_server("http://a", &u("http://a/api/status")).unwrap(),
            "http://a"
        );
        assert_eq!(
            landed_server("http://a", &u("https://a/api/status")).unwrap(),
            "https://a"
        );
        assert_eq!(
            landed_server("http://a/zs", &u("https://b:8443/zs/api/status?x=1")).unwrap(),
            "https://b:8443/zs"
        );
        let err = landed_server("https://a", &u("http://a/api/status")).unwrap_err();
        assert!(err.contains("不加密"), "{err}");
        let err = landed_server("http://a", &u("http://a/login")).unwrap_err();
        assert!(err.contains("不像是 zsmeet"), "{err}");
    }

    #[test]
    fn explains_unreachable_servers() {
        let lan = unreachable_message("http://10.0.0.5", true, false, Reach::Lan);
        assert!(
            lan.contains("很久都没有响应") && lan.contains("本地网络"),
            "{lan}"
        );
        let far = unreachable_message("https://meet.example.com", false, false, Reach::Internet);
        assert!(far.contains("连不上") && !far.contains("本地网络"), "{far}");
        let local = unreachable_message("http://127.0.0.1:1", false, false, Reach::Loopback);
        assert!(!local.contains("本地网络"), "{local}");
        let cert = unreachable_message("https://meet.example.com", false, true, Reach::Lan);
        assert!(
            cert.contains("证书") && !cert.contains("本地网络"),
            "{cert}"
        );
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
