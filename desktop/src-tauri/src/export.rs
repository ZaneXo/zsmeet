//! 导出（纪要、转写）：网页版是浏览器下载，桌面版先从服务器取回文件，再弹保存对话框，写到用户选的位置。

use percent_encoding::percent_decode_str;

use crate::server;

/// 导出文件最大多少（Word 文档带不了录音，几 MB 就很大了）。
const MAX_EXPORT_BYTES: usize = 64 * 1024 * 1024;

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
        .is_some_and(|n| n as usize > MAX_EXPORT_BYTES)
    {
        return Err((status, "导出的文件太大了".into()));
    }
    let name = file_name(
        resp.headers()
            .get(reqwest::header::CONTENT_DISPOSITION)
            .and_then(|v| v.to_str().ok()),
        "zsmeet-export",
    );
    let body = resp
        .bytes()
        .await
        .map_err(|_| (0, "下载到一半断了，没能导出。请检查网络后再试".to_string()))?;
    Ok((name, body.to_vec()))
}

#[cfg(test)]
mod tests {
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
}
