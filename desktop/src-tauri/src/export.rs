//! 导出（纪要、转写）：网页版是浏览器下载，桌面版先从服务器取回文件，再弹保存对话框，写到用户选的位置。

use std::io::{self, Write};
use std::path::Path;

use percent_encoding::percent_decode_str;

use crate::server;

/// 导出文件最大多少（Word 文档带不了录音，几 MB 就很大了）。
const MAX_EXPORT_BYTES: usize = 64 * 1024 * 1024;

const TOO_LARGE: &str = "导出的文件太大了";
const BROKEN: &str = "下载到一半断了，没能导出。请检查网络后再试";

/// 只允许 `/api/meetings/{id}/export?…`，界面传不了别的路径进来。id 只能是普通的字母数字（见 server::api_segments），
/// 编码过的「..」、反斜杠这些转发出去以后可能落到别的路径上，把令牌带过去。
pub fn valid_path(path_and_query: &str) -> bool {
    let path = path_and_query
        .split_once('?')
        .map_or(path_and_query, |(p, _)| p);
    matches!(
        server::api_segments(path).as_deref(),
        Some(["meetings", _, "export"])
    )
}

/// 从 Content-Disposition 里取文件名：优先 `filename*=UTF-8''…`，其次 `filename="…"`。
/// 去掉路径分隔符这类不能出现在文件名里的字符。
pub fn file_name(disposition: Option<&str>, fallback: &str) -> String {
    let mut plain = None;
    let mut extended = None;
    for part in disposition.unwrap_or("").split(';') {
        let part = part.trim();
        if let Some(v) = part.strip_prefix("filename*=") {
            let v = v.trim_matches('"');
            if let Some(encoded) = v
                .strip_prefix("UTF-8''")
                .or_else(|| v.strip_prefix("utf-8''"))
            {
                extended = percent_decode_str(encoded)
                    .decode_utf8()
                    .ok()
                    .map(|s| s.into_owned());
            }
        } else if let Some(v) = part.strip_prefix("filename=") {
            plain = Some(v.trim_matches('"').to_string());
        }
    }
    let name = extended.or(plain).unwrap_or_default();
    let cleaned: String = name
        .chars()
        .map(|c| match c {
            '/' | '\\' | ':' | '\0' => '_',
            c if c.is_control() => '_',
            c => c,
        })
        .collect();
    let cleaned = cleaned.trim().trim_start_matches('.').to_string();
    if cleaned.is_empty() {
        fallback.to_string()
    } else {
        cleaned
    }
}

/// 从服务器取回导出的文件，返回文件名和内容。失败时返回服务器的状态码（连不上时是 0）和给用户看的原因。
pub async fn fetch(
    client: &reqwest::Client,
    server: &str,
    token: Option<&str>,
    path_and_query: &str,
) -> Result<(String, Vec<u8>), (u16, String)> {
    let Some(url) = server::api_url(server, path_and_query) else {
        return Err((0, "导出的地址不对，没能导出".to_string()));
    };
    let mut req = client.get(url);
    if let Some(token) = token {
        req = req.bearer_auth(token);
    }
    let resp = req
        .send()
        .await
        .map_err(|_| (0, "连不上服务器，没能导出。请检查网络后再试".to_string()))?;
    let status = resp.status().as_u16();
    if !resp.status().is_success() {
        let detail = resp
            .json::<serde_json::Value>()
            .await
            .ok()
            .and_then(|v| v.get("detail")?.as_str().map(str::to_string))
            .filter(|d| !d.is_empty());
        let message =
            detail.unwrap_or_else(|| format!("导出失败（服务器返回 {status}），请稍后再试"));
        return Err((status, message));
    }
    if resp
        .content_length()
        .is_some_and(|n| n > MAX_EXPORT_BYTES as u64)
    {
        return Err((status, TOO_LARGE.into()));
    }
    let name = file_name(
        resp.headers()
            .get(reqwest::header::CONTENT_DISPOSITION)
            .and_then(|v| v.to_str().ok()),
        "zsmeet-export",
    );
    let body = read_body(resp, MAX_EXPORT_BYTES).await?;
    Ok((name, body))
}

/// 一段一段地读响应体，超过 max 个字节就不读了。服务器没说有多大（没有 Content-Length）时也不会无限制地读进内存。
async fn read_body(mut resp: reqwest::Response, max: usize) -> Result<Vec<u8>, (u16, String)> {
    let status = resp.status().as_u16();
    let mut body = Vec::new();
    while let Some(chunk) = resp.chunk().await.map_err(|_| (0, BROKEN.to_string()))? {
        if body.len() + chunk.len() > max {
            return Err((status, TOO_LARGE.into()));
        }
        body.extend_from_slice(&chunk);
    }
    Ok(body)
}

/// 存到用户选的位置：先写到同一个目录里的临时文件、落盘，再改名换掉原来的文件。
/// 直接覆盖的话，写到一半出错（磁盘满了、U 盘拔了）会留下半个文件，原来的也没了。
pub fn save_replacing(target: &Path, body: &[u8]) -> io::Result<()> {
    replace_with(target, |file| file.write_all(body))
}

/// 用 write 写出新文件的内容，写好了才换掉 target。失败时删掉临时文件，target 原来是什么还是什么。
fn replace_with(
    target: &Path,
    write: impl FnOnce(&mut std::fs::File) -> io::Result<()>,
) -> io::Result<()> {
    let name = target
        .file_name()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "没有文件名"))?;
    // 以「.」开头，在 Finder 里看不到；带个随机数，同时存两次也不会撞上
    let tmp = target.with_file_name(format!(
        ".{}.{}.tmp",
        name.to_string_lossy(),
        server::random_token(6)
    ));
    let written = (|| {
        let mut file = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&tmp)?;
        write(&mut file)?;
        file.sync_all()?;
        std::fs::rename(&tmp, target)
    })();
    if written.is_err() {
        let _ = std::fs::remove_file(&tmp);
    }
    written
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use super::*;

    #[test]
    fn accepts_only_export_paths() {
        assert!(valid_path(
            "/api/meetings/abc/export?what=minutes&format=docx"
        ));
        assert!(valid_path("/api/meetings/abc/export"));
        assert!(!valid_path("/api/meetings/abc/audio"));
        assert!(!valid_path("/api/meetings//export"));
        assert!(!valid_path("/api/meetings/../export"));
        assert!(!valid_path("https://evil/api/meetings/a/export"));
        assert!(!valid_path("/api/settings/llm"));
        // 编码过的「..」、反斜杠、被丢掉的控制字符：规范化以后会落到别的路径上
        assert!(!valid_path(
            "/api/meetings/%2e%2e\\%2e%2e\\%2e%2e\\other-app/export"
        ));
        assert!(!valid_path("/api/meetings/.\t./export"));
        assert!(!valid_path("/api/meetings/a%2fb/export"));
    }

    #[test]
    fn reads_file_name() {
        let server = r#"attachment; filename="zsmeet-export.docx"; filename*=UTF-8''%E5%91%A8%E4%BC%9A%20%E7%BA%AA%E8%A6%81.docx"#;
        assert_eq!(file_name(Some(server), "x"), "周会 纪要.docx");
        assert_eq!(
            file_name(Some(r#"attachment; filename="a.txt""#), "x"),
            "a.txt"
        );
        assert_eq!(
            file_name(
                Some("attachment; filename*=UTF-8''..%2F..%2Fetc%2Fpasswd"),
                "x"
            ),
            "_.._etc_passwd"
        );
        assert_eq!(file_name(None, "fallback"), "fallback");
        assert_eq!(file_name(Some("attachment"), "fallback"), "fallback");
    }

    /// 起一个服务器，GET / 返回 make() 做出来的流（分块传，不带 Content-Length），返回它的响应。
    async fn streamed<S>(make: fn() -> S) -> reqwest::Response
    where
        S: futures_util::Stream<Item = io::Result<bytes::Bytes>> + Send + 'static,
    {
        let app = axum::Router::new().route(
            "/",
            axum::routing::get(move || async move { axum::body::Body::from_stream(make()) }),
        );
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}/", listener.local_addr().unwrap());
        tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
        let resp = reqwest::get(url).await.unwrap();
        assert_eq!(
            resp.content_length(),
            None,
            "要测的是不带 Content-Length 的响应"
        );
        resp
    }

    #[tokio::test]
    async fn stops_reading_once_too_large() {
        // 服务器没完没了地发，又不说有多大：读到上限就停，不会一直读下去
        let endless = || {
            futures_util::stream::repeat_with(|| {
                Ok::<_, io::Error>(bytes::Bytes::from(vec![b'x'; 1000]))
            })
        };
        let resp = streamed(endless).await;
        let read = tokio::time::timeout(Duration::from_secs(5), read_body(resp, 10_000))
            .await
            .expect("超过上限就该停下，不该一直读");
        assert_eq!(read, Err((200, TOO_LARGE.to_string())));

        let small = || {
            futures_util::stream::iter(
                ["ab", "cd", "e"].map(|s| Ok::<_, io::Error>(bytes::Bytes::from(s))),
            )
        };
        let resp = streamed(small).await;
        assert_eq!(read_body(resp, 5).await.unwrap(), b"abcde");
    }

    /// 目录里除了 keep 以外的文件（临时文件不该留下）。
    fn leftovers(dir: &Path, keep: &str) -> Vec<String> {
        std::fs::read_dir(dir)
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .filter(|n| n != keep)
            .collect()
    }

    #[test]
    fn replaces_the_file_only_after_writing_it_fully() {
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join("周会纪要.docx");
        std::fs::write(&target, b"old").unwrap();

        save_replacing(&target, b"new").unwrap();
        assert_eq!(std::fs::read(&target).unwrap(), b"new");
        assert!(leftovers(dir.path(), "周会纪要.docx").is_empty());

        // 写到一半出错（比如磁盘满了）：原来的文件不动，也不留下临时文件
        let err = replace_with(&target, |file| {
            file.write_all(b"half")?;
            Err(io::Error::other("磁盘满了"))
        })
        .unwrap_err();
        assert_eq!(err.to_string(), "磁盘满了");
        assert_eq!(std::fs::read(&target).unwrap(), b"new");
        assert!(leftovers(dir.path(), "周会纪要.docx").is_empty());

        // 换不掉（选的是一个文件夹）：报错，也不留下临时文件
        let folder = dir.path().join("文件夹");
        std::fs::create_dir(&folder).unwrap();
        std::fs::write(folder.join("里面的文件"), b"x").unwrap();
        assert!(save_replacing(&folder, b"new").is_err());
        assert_eq!(std::fs::read(folder.join("里面的文件")).unwrap(), b"x");
        let mut left = leftovers(dir.path(), "周会纪要.docx");
        left.sort();
        assert_eq!(left, ["文件夹"]);
    }
}
