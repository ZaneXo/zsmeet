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

/// 一个请求的结果。
#[derive(Debug, Clone, PartialEq)]
pub enum Outcome {
    /// 成功（2xx），带着响应的 JSON（没有内容、看不懂时是 Null）
    Done(Value),
    /// 等一会儿再试就可能好：连不上、超时、服务器出错（5xx）、408、429、跳转到了别处。带着给用户看的原因
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

/// 服务器已经收到了哪几片（序号），最多等 timeout。
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
        Outcome::Done(v) => Ok(received_seqs(&v)),
        other => Err(other),
    }
}

/// `{"received":[0,1,2]}` 里的序号。看不懂的当作一片也没收到。
fn received_seqs(body: &Value) -> Vec<u32> {
    body.get("received")
        .and_then(Value::as_array)
        .map(|seqs| {
            seqs.iter()
                .filter_map(|s| u32::try_from(s.as_u64()?).ok())
                .collect()
        })
        .unwrap_or_default()
}

async fn send(req: reqwest::RequestBuilder) -> Outcome {
    let resp = match req.send().await {
        Ok(resp) => resp,
        Err(e) if e.is_timeout() => return Outcome::Retry(SLOW.into()),
        Err(_) => return Outcome::Retry(OFFLINE.into()),
    };
    let status = resp.status().as_u16();
    let body = resp.bytes().await.ok();
    let json = body
        .as_deref()
        .and_then(|b| serde_json::from_slice::<Value>(b).ok());
    if (200..300).contains(&status) {
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
        assert_eq!(received_seqs(&json!({ "received": [0, 1, 5] })), [0, 1, 5]);
        assert!(received_seqs(&json!({ "received": [] })).is_empty());
        assert!(received_seqs(&json!({ "received": null })).is_empty());
        assert_eq!(received_seqs(&json!({ "received": [-1, "x", 2] })), [2]);
        assert!(received_seqs(&Value::Null).is_empty());
    }
}
