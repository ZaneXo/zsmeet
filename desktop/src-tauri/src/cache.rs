//! 离线缓存：几种 GET 请求最近一次成功的结果存在本地，连不上服务器时拿出来用。
//!
//! 每台服务器、每个用户一个目录（见 core.rs），每个请求（路径加查询参数）一个文件。
//! 文件内容是一行 JSON 写的元数据，接着是原样的响应体。总大小超过上限时先删最久没更新的。
//! 录音、导出、进度推送这些不缓存。

use std::path::PathBuf;
use std::time::SystemTime;

use bytes::Bytes;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

/// 单个响应超过这么大就不缓存（转写很长的会议也只有几 MB）。
pub const MAX_ENTRY_BYTES: usize = 16 * 1024 * 1024;

/// 一个用户的缓存目录默认最多占这么多空间。
pub const DEFAULT_MAX_BYTES: u64 = 256 * 1024 * 1024;

/// 这个请求要不要存进离线缓存：会议列表（含搜索）、会议详情、纪要、转写、模板、状态。
pub fn cacheable(method: &str, path: &str) -> bool {
    if method != "GET" {
        return false;
    }
    let Some(rest) = path.strip_prefix("/api/") else {
        return false;
    };
    let parts: Vec<&str> = rest.split('/').collect();
    match parts.as_slice() {
        ["status"] | ["meetings"] | ["templates"] => true,
        ["meetings", id] => !id.is_empty(),
        ["meetings", id, "minutes" | "transcript"] => !id.is_empty(),
        _ => false,
    }
}

/// 缓存文件名：路径加查询参数的 SHA-256。
pub fn key(path_and_query: &str) -> String {
    hex::encode(Sha256::digest(path_and_query.as_bytes()))
}

#[derive(Serialize, Deserialize)]
struct Meta {
    content_type: String,
}

pub struct Entry {
    pub content_type: String,
    pub body: Bytes,
}

#[derive(Clone)]
pub struct OfflineCache {
    dir: PathBuf,
    max_bytes: u64,
}

impl OfflineCache {
    pub fn new(dir: PathBuf, max_bytes: u64) -> Self {
        Self { dir, max_bytes }
    }

    pub async fn get(&self, key: &str) -> Option<Entry> {
        let data = tokio::fs::read(self.dir.join(key)).await.ok()?;
        let newline = data.iter().position(|b| *b == b'\n')?;
        let meta: Meta = serde_json::from_slice(&data[..newline]).ok()?;
        let body = Bytes::from(data).slice(newline + 1..);
        Some(Entry {
            content_type: meta.content_type,
            body,
        })
    }

    /// 存一条，然后看看是不是超出上限了。写失败只记日志：缓存只是锦上添花。
    pub async fn put(&self, key: &str, content_type: &str, body: &[u8]) {
        if body.len() > MAX_ENTRY_BYTES {
            return;
        }
        if let Err(e) = self.write(key, content_type, body).await {
            log::warn!("离线缓存写入失败：{e}");
            return;
        }
        let cache = self.clone();
        let _ = tokio::task::spawn_blocking(move || cache.evict()).await;
    }

    async fn write(&self, key: &str, content_type: &str, body: &[u8]) -> std::io::Result<()> {
        tokio::fs::create_dir_all(&self.dir).await?;
        let mut data = serde_json::to_vec(&Meta {
            content_type: content_type.to_string(),
        })?;
        data.push(b'\n');
        data.extend_from_slice(body);
        // 先写临时文件再改名，读的时候不会读到写了一半的文件
        let tmp = self.dir.join(format!("{key}.tmp"));
        tokio::fs::write(&tmp, &data).await?;
        tokio::fs::rename(&tmp, self.dir.join(key)).await
    }

    /// 总大小超过上限时，从最久没更新的开始删，删到上限的八成。
    fn evict(&self) {
        let Ok(entries) = std::fs::read_dir(&self.dir) else {
            return;
        };
        let mut files: Vec<(SystemTime, u64, PathBuf)> = entries
            .filter_map(Result::ok)
            .filter_map(|e| {
                let meta = e.metadata().ok()?;
                meta.is_file().then(|| {
                    (
                        meta.modified().unwrap_or(SystemTime::UNIX_EPOCH),
                        meta.len(),
                        e.path(),
                    )
                })
            })
            .collect();
        let mut total: u64 = files.iter().map(|f| f.1).sum();
        if total <= self.max_bytes {
            return;
        }
        files.sort_by_key(|f| f.0);
        let target = self.max_bytes / 10 * 8;
        for (_, len, path) in files {
            if total <= target {
                break;
            }
            if std::fs::remove_file(&path).is_ok() {
                total -= len;
            }
        }
    }

    /// 删掉整个目录（退出登录时）。
    pub fn clear(&self) {
        if let Err(e) = std::fs::remove_dir_all(&self.dir) {
            if e.kind() != std::io::ErrorKind::NotFound {
                log::warn!("离线缓存删除失败：{e}");
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn decides_what_to_cache() {
        for path in [
            "/api/status",
            "/api/meetings",
            "/api/templates",
            "/api/meetings/abc",
            "/api/meetings/abc/minutes",
            "/api/meetings/abc/transcript",
        ] {
            assert!(cacheable("GET", path), "{path}");
        }
        for path in [
            "/api/meetings/abc/audio",
            "/api/meetings/abc/events",
            "/api/meetings/abc/export",
            "/api/meetings/abc/chunks",
            "/api/meetings//minutes",
            "/api/settings/llm",
            "/api/me/devices",
            "/status",
        ] {
            assert!(!cacheable("GET", path), "{path}");
        }
        assert!(!cacheable("POST", "/api/meetings"));
        assert!(!cacheable("HEAD", "/api/meetings"));
    }

    #[tokio::test]
    async fn stores_and_evicts_oldest() {
        let tmp = tempfile::tempdir().unwrap();
        let cache = OfflineCache::new(tmp.path().join("u"), 3000);
        assert!(cache.get("nothing").await.is_none());

        cache.put("a", "application/json", &[b'a'; 1000]).await;
        let got = cache.get("a").await.unwrap();
        assert_eq!(got.content_type, "application/json");
        assert_eq!(got.body.len(), 1000);

        // 修改时间的精度可能是秒：拉开一点，「最久没更新」才分得清
        let old = SystemTime::now() - std::time::Duration::from_secs(60);
        std::fs::File::options()
            .write(true)
            .open(tmp.path().join("u/a"))
            .unwrap()
            .set_modified(old)
            .unwrap();
        cache.put("b", "application/json", &[b'b'; 1000]).await;
        cache.put("c", "application/json", &[b'c'; 1000]).await;
        // 三条加上元数据超过 3000 字节：最早的 a 被删掉
        assert!(cache.get("a").await.is_none());
        assert!(cache.get("b").await.is_some());
        assert!(cache.get("c").await.is_some());

        // 太大的不存
        cache
            .put("big", "application/json", &vec![0u8; MAX_ENTRY_BYTES + 1])
            .await;
        assert!(cache.get("big").await.is_none());

        cache.clear();
        assert!(cache.get("b").await.is_none());
        cache.clear();
    }
}
