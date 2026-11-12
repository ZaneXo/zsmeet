//! 上传要发的几个请求：传一片（PUT …/chunks/{seq}）、存标记（PUT …/marks）、
//! 告诉服务器传完了（POST …/complete）、看一眼会议（GET …）、服务器收到了哪几片（GET …/chunks）。
//!
//! 这里只管发请求、把结果分好类；什么时候传哪一片、失败了怎么办，由 mod.rs 里的上传任务决定。
//! 和网页录音一样：连不上、服务器出错就等一会儿再试，会议被删了、没有权限这类重试也没用的就不再传。

use std::time::Duration;

use reqwest::header;
use serde_json::{json, Value};

use crate::core::Account;
use crate::server;

/// 一片只有几十 KB，传这么久还没完，多半是连接悄悄断了，断开重来。
const CHUNK_TIMEOUT: Duration = Duration::from_secs(60);
pub const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);

pub const OFFLINE: &str = "连不上服务器，网络恢复后会自动接着传";
const SLOW: &str = "服务器很久都没有响应，稍后会自动重试";
const NO_PERMISSION: &str = "没有权限往这场会议里传录音，录音传不上去了";
const MEETING_GONE: &str = "服务器上找不到这场会议了（可能已经被删除），录音传不上去了";
const ALREADY_FINISHED: &str = "这场会议已经结束录音、开始处理了，剩下的录音传不上去了";
const BAD_ADDRESS: &str = "服务器地址不对，录音传不上去";
const UNREADABLE: &str = "服务器返回的内容看不懂，稍后会自动重试。一直这样的话，看看连的网络是不是要先登录，或者请管理员检查服务器的设置";

/// 一个请求的结果。
#[derive(Debug, Clone, PartialEq)]
pub enum Outcome {
    /// 成功（2xx），带着响应的 JSON（没有内容时是 Null）
    Done(Value),
    /// 等一会儿再试就可能好：连不上、超时、服务器出错（5xx）、408、429、跳转到了别处、回复看不懂或者没收全。
    /// 带着给用户看的原因
    Retry(String),
    /// 登录失效了（401）
    Unauthorized,
    /// 会议已经不在上传状态（409），带着服务器说的原因
    Conflict(String),
    /// 重试也没用：会议被删了、没有权限、服务器不接受。带着给用户看的原因
    Fatal(String),
}

/// 传一片录音。同一片可以重复传（服务器后一次覆盖前一次），所以断网重传是安全的。
pub async fn put_chunk(
    client: &reqwest::Client,
    account: &Account,
    meeting_id: &str,
    seq: u32,
    body: Vec<u8>,
) -> Outcome {
    let Some(url) = server::api_url(
        &account.server,
        &format!("/api/meetings/{meeting_id}/chunks/{seq}"),
    ) else {
        return Outcome::Fatal(BAD_ADDRESS.into());
    };
    let req = client
        .put(url)
        .bearer_auth(&account.token)
        .header(header::CONTENT_TYPE, "audio/aac")
        .timeout(CHUNK_TIMEOUT)
        .body(body);
    send(req).await
}

/// 存录音过程中标记的重点（整份替换）。录音的电脑万一找不回来，用已经收到的部分生成纪要时标记也还在。
pub async fn put_marks(
    client: &reqwest::Client,
    account: &Account,
    meeting_id: &str,
    marks: &[f64],
) -> Outcome {
    let Some(url) = server::api_url(
        &account.server,
        &format!("/api/meetings/{meeting_id}/marks"),
    ) else {
        return Outcome::Fatal(BAD_ADDRESS.into());
    };
    let req = client
        .put(url)
        .bearer_auth(&account.token)
        .timeout(REQUEST_TIMEOUT)
        .json(&json!({ "marks": marks }));
    send(req).await
}

/// 全部传完了：告诉服务器一共几片、标记了哪些重点，服务器开始处理。
pub async fn complete(
    client: &reqwest::Client,
    account: &Account,
    meeting_id: &str,
    chunks: u32,
    marks: &[f64],
) -> Outcome {
    let Some(url) = server::api_url(
        &account.server,
        &format!("/api/meetings/{meeting_id}/complete"),
    ) else {
        return Outcome::Fatal(BAD_ADDRESS.into());
    };
    let req = client
        .post(url)
        .bearer_auth(&account.token)
        .timeout(REQUEST_TIMEOUT)
        .json(&json!({ "chunks": chunks, "marks": marks }));
    send(req).await
}

/// 取会议详情（标题、状态），最多等 timeout。
pub async fn meeting(
    client: &reqwest::Client,
    account: &Account,
    meeting_id: &str,
    timeout: Duration,
) -> Result<Value, Outcome> {
    let Some(url) = server::api_url(&account.server, &format!("/api/meetings/{meeting_id}")) else {
        return Err(Outcome::Fatal(BAD_ADDRESS.into()));
    };
    let req = client.get(url).bearer_auth(&account.token).timeout(timeout);
    match send(req).await {
        Outcome::Done(v) => Ok(v),
        other => Err(other),
    }
}

/// 服务器已经收到了哪几片（序号），最多等 timeout。回复看不懂（比如中间的代理返回了一个网页）时
/// 算暂时出错（Retry），不能当成一片也没收到。
pub async fn received(
    client: &reqwest::Client,
    account: &Account,
    meeting_id: &str,
    timeout: Duration,
) -> Result<Vec<u32>, Outcome> {
    let Some(url) = server::api_url(
        &account.server,
        &format!("/api/meetings/{meeting_id}/chunks"),
    ) else {
        return Err(Outcome::Fatal(BAD_ADDRESS.into()));
    };
    let req = client.get(url).bearer_auth(&account.token).timeout(timeout);
    match send(req).await {
        Outcome::Done(v) => received_seqs(&v).ok_or_else(|| Outcome::Retry(UNREADABLE.into())),
        other => Err(other),
    }
}

/// `{"received":[0,1,2]}` 里的序号（null 是 Go 的空切片，也是一片都没收到）。不是这个样子、
/// 有哪个序号不是非负整数时返回 None：看不懂的回复里可能就有这台电脑上找不到的那一片。
fn received_seqs(body: &Value) -> Option<Vec<u32>> {
    match body.get("received")? {
        Value::Null => Some(Vec::new()),
        Value::Array(seqs) => seqs
            .iter()
            .map(|s| u32::try_from(s.as_u64()?).ok())
            .collect(),
        _ => None,
    }
}

async fn send(req: reqwest::RequestBuilder) -> Outcome {
    let resp = match req.send().await {
        Ok(resp) => resp,
        Err(e) if e.is_timeout() => return Outcome::Retry(SLOW.into()),
        Err(_) => return Outcome::Retry(OFFLINE.into()),
    };
    let status = resp.status().as_u16();
    let success = (200..300).contains(&status);
    let body = match resp.bytes().await {
        Ok(body) => Some(body),
        // 2xx 的回复没收全（说好多长却没给够就断开了，或者一直不给完）：和下面看不懂的回复一样，
        // 可能不是服务器回的，服务器也不一定办成了，不能当成成功
        Err(e) if success && e.is_timeout() => return Outcome::Retry(SLOW.into()),
        Err(_) if success => return Outcome::Retry(UNREADABLE.into()),
        // 出错的回复没收全：只按状态码分类
        Err(_) => None,
    };
    let json = body
        .as_deref()
        .and_then(|b| serde_json::from_slice::<Value>(b).ok());
    if success {
        // 有内容却不是 JSON：不是服务器回的（比如连的网络要先登录，中间的设备返回了一个网页）。
        // 不能当成成功：传分片时会以为传上去了，把这台电脑上的分片删掉
        let unreadable = body
            .as_deref()
            .is_some_and(|b| b.iter().any(|c| !c.is_ascii_whitespace()))
            && json.is_none();
        if unreadable {
            return Outcome::Retry(UNREADABLE.into());
        }
        return Outcome::Done(json.unwrap_or(Value::Null));
    }
    let detail = json
        .as_ref()
        .and_then(|v| v.get("detail")?.as_str())
        .filter(|d| !d.is_empty())
        .map(str::to_string);
    classify(status, detail)
}

/// 按状态码分类。detail 是服务器返回的原因（problem+json 里的 detail）。
pub fn classify(status: u16, detail: Option<String>) -> Outcome {
    match status {
        401 => Outcome::Unauthorized,
        403 => Outcome::Fatal(NO_PERMISSION.into()),
        404 => Outcome::Fatal(MEETING_GONE.into()),
        409 => Outcome::Conflict(detail.unwrap_or_else(|| ALREADY_FINISHED.into())),
        408 | 429 => Outcome::Retry(format!("服务器很忙（返回 {status}），稍后会自动重试")),
        500..=599 => Outcome::Retry(format!("服务器暂时出错了（返回 {status}），稍后会自动重试")),
        // 跳转（带着令牌的请求不跟着跳，见 server::http_client）：多半是服务器的设置问题，改好了还能接着传
        300..=399 => Outcome::Retry(format!(
            "服务器把请求转到了别的地址（返回 {status}），请让管理员检查服务器的设置"
        )),
        _ => Outcome::Fatal(match detail {
            Some(d) => format!("服务器不接受这段录音：{d}"),
            None => format!("服务器不接受这段录音（返回 {status}）"),
        }),
    }
}

#[cfg(test)]
mod tests {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpListener;

    use super::*;

    #[test]
    fn sorts_out_failures() {
        assert_eq!(classify(401, None), Outcome::Unauthorized);
        assert!(matches!(classify(403, None), Outcome::Fatal(m) if m.contains("没有权限")));
        assert!(
            matches!(classify(404, Some("not found".into())), Outcome::Fatal(m) if m.contains("找不到这场会议"))
        );
        assert_eq!(
            classify(409, Some("这场会议已经在处理了".into())),
            Outcome::Conflict("这场会议已经在处理了".into())
        );
        assert!(matches!(classify(409, None), Outcome::Conflict(m) if m.contains("开始处理")));
        for status in [408, 429, 500, 502, 503, 504] {
            assert!(
                matches!(classify(status, None), Outcome::Retry(m) if m.contains(&status.to_string())),
                "{status}"
            );
        }
        assert!(matches!(classify(302, None), Outcome::Retry(_)));
        assert!(
            matches!(classify(400, Some("分片是空的".into())), Outcome::Fatal(m) if m.contains("分片是空的"))
        );
        assert!(matches!(classify(413, None), Outcome::Fatal(m) if m.contains("413")));
    }

    #[test]
    fn reads_received_chunks() {
        assert_eq!(
            received_seqs(&json!({ "received": [0, 1, 5] })),
            Some(vec![0, 1, 5])
        );
        assert_eq!(received_seqs(&json!({ "received": [] })), Some(vec![]));
        assert_eq!(received_seqs(&json!({ "received": null })), Some(vec![]));
        // 看不懂的回复不能当成一片也没收到
        for body in [
            json!({ "received": [-1, 2] }),
            json!({ "received": ["0", 1] }),
            json!({ "received": [1.5] }),
            json!({ "received": [4_294_967_296_u64] }),
            json!({ "received": 3 }),
            json!({ "chunks": [0, 1] }),
            json!("<html>请先登录</html>"),
            Value::Null,
        ] {
            assert_eq!(received_seqs(&body), None, "{body}");
        }
    }

    /// 只答一次话的「服务器」（像是连的网络要先登录，中间的设备冒充服务器回话）：读完一个请求，原样回 reply。
    /// hang 时回完既不断开、也不再说话，等对方放弃；不然回完就断开。返回连它用的账号。
    async fn answer_once(reply: &'static [u8], hang: bool) -> Account {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let account = Account {
            server: format!("http://{}", listener.local_addr().unwrap()),
            user_id: "u1".into(),
            token: "zsm_test".into(),
        };
        tokio::spawn(async move {
            let (mut conn, _) = listener.accept().await.unwrap();
            // 先把请求读完再回：没读完就断开的话，对方收到的可能是连接被重置，测的就不是回复没收全了
            let mut request = Vec::new();
            let mut buf = [0u8; 4096];
            while !request_complete(&request) {
                match conn.read(&mut buf).await {
                    Ok(0) | Err(_) => return,
                    Ok(n) => request.extend_from_slice(&buf[..n]),
                }
            }
            conn.write_all(reply).await.unwrap();
            if hang {
                // 等对方等不及、断开
                while matches!(conn.read(&mut buf).await, Ok(n) if n > 0) {}
            }
        });
        account
    }

    /// 请求头、请求体（按 Content-Length）都收齐了。
    fn request_complete(request: &[u8]) -> bool {
        let Some(end) = request.windows(4).position(|w| w == b"\r\n\r\n") else {
            return false;
        };
        let head = String::from_utf8_lossy(&request[..end]).to_ascii_lowercase();
        let length = head
            .lines()
            .find_map(|line| line.strip_prefix("content-length:"))
            .and_then(|v| v.trim().parse::<usize>().ok())
            .unwrap_or(0);
        request.len() >= end + 4 + length
    }

    #[tokio::test]
    async fn a_2xx_answer_cut_short_is_not_success() {
        // 说是 200、有 1000 字节，只给了几个字节
        const CUT: &[u8] = b"HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\n<html>please sign in";
        let client = server::http_client();
        let chunk = || b"audio-0".to_vec();

        // 给了几个字节就断开了：分片其实没传上去，不能当成传上去了、把这台电脑上的分片删掉
        let account = answer_once(CUT, false).await;
        assert_eq!(
            put_chunk(&client, &account, "m1", 0, chunk()).await,
            Outcome::Retry(UNREADABLE.into())
        );
        // 给了几个字节就不动了，一直等到超时
        let account = answer_once(CUT, true).await;
        let url = server::api_url(&account.server, "/api/meetings/m1/chunks/0").unwrap();
        let req = client
            .put(url)
            .timeout(Duration::from_millis(500))
            .body(chunk());
        assert_eq!(send(req).await, Outcome::Retry(SLOW.into()));

        // 收全了的照常算成功：服务器传分片时回 204、没有内容
        let account = answer_once(b"HTTP/1.1 204 No Content\r\n\r\n", false).await;
        assert_eq!(
            put_chunk(&client, &account, "m1", 0, chunk()).await,
            Outcome::Done(Value::Null)
        );
        // 出错的回复没收全：照状态码分类
        let account = answer_once(
            b"HTTP/1.1 503 Service Unavailable\r\nContent-Length: 1000\r\n\r\n{\"det",
            false,
        )
        .await;
        let outcome = put_chunk(&client, &account, "m1", 0, chunk()).await;
        assert!(
            matches!(&outcome, Outcome::Retry(m) if m.contains("503")),
            "{outcome:?}"
        );
    }
}
