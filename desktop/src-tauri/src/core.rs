//! App 的状态：连的是哪台服务器、谁登录了、令牌、正在进行的「在浏览器里登录」。
//!
//! 不依赖 Tauri：界面的命令（commands.rs）、本地代理（proxy.rs）和 deep link 都调这里，测试也直接测这里。
//! 状态一变就通过 `Events` 通知界面。

use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use serde_json::Value;
use sha2::{Digest, Sha256};

use crate::cache::{Generation, OfflineCache, DEFAULT_MAX_BYTES};
use crate::server::{self, Pkce};
use crate::token::TokenStore;

/// 点了「在浏览器里登录」以后，多久之内回来的登录链接有效。
const LOGIN_WINDOW: Duration = Duration::from_secs(15 * 60);

/// 状态变了要告诉谁。正式运行时是 Tauri 的事件和系统浏览器，测试时记下来检查。
pub trait Events: Send + Sync {
    /// 状态变了（界面据此切换「填服务器地址」「在浏览器里登录」和主界面）。
    fn state(&self, state: &AppState);
    /// 浏览器登录回来以后没能完成登录，原因给用户看。
    fn login_error(&self, message: &str);
    /// 在系统浏览器里打开一个地址。
    fn open_url(&self, url: &str) -> Result<(), String>;
}

/// 交给界面的状态。secret 是本地代理要求的 `X-Zsmeet-App`；令牌不在这里，界面拿不到。
#[derive(Serialize, Clone, Debug, PartialEq)]
pub struct AppState {
    pub proxy: String,
    pub secret: String,
    pub server: Option<String>,
    pub user: Option<Value>,
    pub signed_in: bool,
    pub login_pending: bool,
    /// 登录失效了（令牌被吊销、账号被停用）：界面提示重新登录
    pub expired: bool,
    pub version: String,
}

/// 存在 App 配置目录里的设置。令牌不在这里，在钥匙串里。
#[derive(Serialize, Deserialize, Default, Clone)]
struct Settings {
    server: Option<String>,
    user: Option<Value>,
}

/// 本地代理转发一个请求时要用到的东西。
#[derive(Clone)]
pub struct Upstream {
    pub server: String,
    pub token: Option<String>,
    pub cache: Option<OfflineCache>,
}

struct Pending {
    pkce: Pkce,
    server: String,
    started: Instant,
}

struct Inner {
    settings: Settings,
    token: Option<String>,
    pending: Option<Pending>,
    expired: bool,
}

pub struct Options {
    /// 本地代理的地址，比如 http://127.0.0.1:52345
    pub proxy: String,
    pub secret: String,
    /// 设置文件（JSON）
    pub settings_path: PathBuf,
    /// 离线缓存的根目录，下面每台服务器、每个用户一个子目录
    pub cache_root: PathBuf,
    /// 显示在授权页和账号页上的电脑名
    pub device: String,
    pub version: String,
}

pub struct Core {
    opts: Options,
    tokens: Arc<dyn TokenStore>,
    events: Arc<dyn Events>,
    client: reqwest::Client,
    cache_generation: Generation,
    inner: Mutex<Inner>,
}

impl Core {
    pub fn new(
        opts: Options,
        tokens: Arc<dyn TokenStore>,
        events: Arc<dyn Events>,
        client: reqwest::Client,
    ) -> Self {
        let settings: Settings = std::fs::read(&opts.settings_path)
            .ok()
            .and_then(|b| serde_json::from_slice(&b).ok())
            .unwrap_or_default();
        // 有登录过的用户才去钥匙串里读令牌
        let token = match (&settings.server, &settings.user) {
            (Some(server), Some(_)) => tokens.get(server).unwrap_or_else(|e| {
                log::warn!("{e}");
                None
            }),
            _ => None,
        };
        Self {
            opts,
            tokens,
            events,
            client,
            cache_generation: Generation::default(),
            inner: Mutex::new(Inner {
                settings,
                token,
                pending: None,
                expired: false,
            }),
        }
    }

    pub fn client(&self) -> &reqwest::Client {
        &self.client
    }

    pub fn secret(&self) -> &str {
        &self.opts.secret
    }

    pub fn state(&self) -> AppState {
        let inner = self.inner.lock().unwrap();
        self.snapshot(&inner)
    }

    fn snapshot(&self, inner: &Inner) -> AppState {
        let signed_in = inner.token.is_some() && inner.settings.user.is_some();
        AppState {
            proxy: self.opts.proxy.clone(),
            secret: self.opts.secret.clone(),
            server: inner.settings.server.clone(),
            user: if signed_in {
                inner.settings.user.clone()
            } else {
                None
            },
            signed_in,
            login_pending: inner.pending.is_some(),
            expired: inner.expired,
            version: self.opts.version.clone(),
        }
    }

    fn changed(&self) -> AppState {
        let state = self.state();
        self.events.state(&state);
        state
    }

    /// 本地代理转发请求用：服务器地址、令牌、这个用户的离线缓存。没设置服务器时返回 None。
    pub fn upstream(&self) -> Option<Upstream> {
        let inner = self.inner.lock().unwrap();
        let server = inner.settings.server.clone()?;
        let token = inner.token.clone();
        let cache = match (&token, inner.settings.user.as_ref()) {
            (Some(_), Some(user)) => Some(self.cache_for(&server, user)),
            _ => None,
        };
        Some(Upstream {
            server,
            token,
            cache,
        })
    }

    fn cache_for(&self, server: &str, user: &Value) -> OfflineCache {
        let id = server::user_id(user).unwrap_or_default();
        let digest = hex::encode(Sha256::digest(format!("{server}\n{id}").as_bytes()));
        OfflineCache::new(
            self.opts.cache_root.join(&digest[..24]),
            DEFAULT_MAX_BYTES,
            &self.cache_generation,
        )
    }

    fn save(&self, settings: &Settings) {
        let result = (|| -> std::io::Result<()> {
            if let Some(dir) = self.opts.settings_path.parent() {
                std::fs::create_dir_all(dir)?;
            }
            let tmp = self.opts.settings_path.with_extension("tmp");
            std::fs::write(&tmp, serde_json::to_vec_pretty(settings)?)?;
            std::fs::rename(tmp, &self.opts.settings_path)
        })();
        if let Err(e) = result {
            log::error!("设置保存失败：{e}");
        }
    }

    /// 在本机退出：删掉令牌、这个用户的离线缓存，忘掉是谁。服务器地址留着。
    fn sign_out_locally(&self, inner: &mut Inner, expired: bool) {
        if let Some(server) = inner.settings.server.clone() {
            if let Some(user) = inner.settings.user.as_ref() {
                self.cache_for(&server, user).clear();
            }
            if let Err(e) = self.tokens.delete(&server) {
                log::warn!("{e}");
            }
        }
        inner.token = None;
        inner.settings.user = None;
        inner.pending = None;
        inner.expired = expired;
        self.save(&inner.settings);
    }

    /// 填服务器地址：检查能用以后保存。换了服务器的话，先退出原来那台。
    pub async fn set_server(&self, input: &str) -> Result<AppState, String> {
        let server = server::check_server(&self.client, input).await?;
        let same = self.inner.lock().unwrap().settings.server.as_deref() == Some(&server);
        if !same {
            self.logout_inner(false).await;
            let mut inner = self.inner.lock().unwrap();
            inner.settings.server = Some(server);
            inner.expired = false;
            self.save(&inner.settings);
        }
        Ok(self.changed())
    }

    /// 换服务器：退出登录，忘掉服务器地址，界面回到「填服务器地址」。
    pub async fn switch_server(&self) -> AppState {
        self.logout_inner(false).await;
        {
            let mut inner = self.inner.lock().unwrap();
            inner.settings.server = None;
            inner.expired = false;
            self.save(&inner.settings);
        }
        self.changed()
    }

    /// 点「在浏览器里登录」：生成 PKCE，在系统浏览器里打开授权页。再点一次就重新生成、重新打开。
    pub fn login_start(&self) -> Result<AppState, String> {
        let url = {
            let mut inner = self.inner.lock().unwrap();
            let server = inner.settings.server.clone().ok_or("请先填写服务器地址")?;
            let pkce = Pkce::new();
            let url = server::authorize_url(&server, &self.opts.device, &pkce);
            inner.pending = Some(Pending {
                pkce,
                server,
                started: Instant::now(),
            });
            url
        };
        if let Err(e) = self.events.open_url(&url) {
            self.inner.lock().unwrap().pending = None;
            self.changed();
            return Err(format!("没能打开浏览器（{e}）"));
        }
        Ok(self.changed())
    }

    pub fn login_cancel(&self) -> AppState {
        self.inner.lock().unwrap().pending = None;
        self.changed()
    }

    /// 收到 `zsmeet://auth?code=…&state=…`：核对 state，用 code 换令牌，存进钥匙串。
    /// 出错时通过 `Events::login_error` 告诉界面，也作为返回值（方便测试）。
    pub async fn handle_deep_link(&self, link: &str) -> Result<(), String> {
        let result = self.finish_login(link).await;
        if let Err(message) = &result {
            self.events.login_error(message);
        }
        self.changed();
        result
    }

    async fn finish_login(&self, link: &str) -> Result<(), String> {
        let callback = server::parse_callback(link)?;
        let pending = {
            let mut inner = self.inner.lock().unwrap();
            let matches = inner
                .pending
                .as_ref()
                .is_some_and(|p| p.pkce.state == callback.state);
            if !matches {
                // 不是这次点「在浏览器里登录」打开的页面跳回来的（旧的标签页、别人发来的链接）：不认
                return Err(
                    "这个登录链接不是这次登录打开的，或者已经用过了。请回到 zsmeet 重新点「在浏览器里登录」"
                        .into(),
                );
            }
            let pending = inner.pending.take().expect("刚检查过");
            if pending.started.elapsed() > LOGIN_WINDOW {
                return Err("登录等得太久，已经失效了。请重新点「在浏览器里登录」".into());
            }
            if inner.settings.server.as_deref() != Some(&pending.server) {
                return Err("登录期间换了服务器，请重新点「在浏览器里登录」".into());
            }
            pending
        };
        let (token, user) = server::exchange(
            &self.client,
            &pending.server,
            &callback.code,
            &pending.pkce.verifier,
        )
        .await?;
        self.tokens.set(&pending.server, &token)?;
        let mut inner = self.inner.lock().unwrap();
        if inner.settings.server.as_deref() != Some(&pending.server) {
            let _ = self.tokens.delete(&pending.server);
            return Err("登录期间换了服务器，请重新点「在浏览器里登录」".into());
        }
        inner.token = Some(token);
        inner.settings.user = Some(user);
        inner.expired = false;
        self.save(&inner.settings);
        Ok(())
    }

    /// 退出登录：通知服务器作废这台电脑的令牌，再在本机退出。
    pub async fn logout(&self) -> AppState {
        self.logout_inner(false).await;
        self.changed()
    }

    async fn logout_inner(&self, expired: bool) {
        let (server, token) = {
            let inner = self.inner.lock().unwrap();
            (inner.settings.server.clone(), inner.token.clone())
        };
        if let (Some(server), Some(token)) = (server, token) {
            server::revoke(&self.client, &server, &token).await;
        }
        let mut inner = self.inner.lock().unwrap();
        self.sign_out_locally(&mut inner, expired);
    }

    /// 服务器说这个令牌不能用了（401）。只在出问题的还是当前令牌时退出，免得刚重新登录又被旧请求的 401 踢出去。
    pub fn unauthorized(&self, token_used: &str) {
        {
            let mut inner = self.inner.lock().unwrap();
            if inner.token.as_deref() != Some(token_used) {
                return;
            }
            self.sign_out_locally(&mut inner, true);
        }
        self.changed();
    }

    /// 界面发现服务器不认这个登录了（`/api/status` 里没有用户）：和 401 一样处理。
    pub fn session_expired(&self) -> AppState {
        let token = self.inner.lock().unwrap().token.clone();
        if let Some(token) = token {
            self.unauthorized(&token);
        }
        self.state()
    }

    /// 服务器上的一个页面（比如管理员设置）的完整地址。
    pub fn server_url(&self, path: &str) -> Option<String> {
        let server = self.inner.lock().unwrap().settings.server.clone()?;
        Some(format!("{server}{path}"))
    }
}

#[cfg(test)]
pub mod testing {
    use super::*;
    use crate::token::MemoryStore;

    /// 测试用的 Events：记下发生了什么，打开浏览器时只记下地址。
    #[derive(Default)]
    pub struct Recorder {
        pub states: Mutex<Vec<AppState>>,
        pub errors: Mutex<Vec<String>>,
        pub opened: Mutex<Vec<String>>,
    }

    impl Events for Recorder {
        fn state(&self, state: &AppState) {
            self.states.lock().unwrap().push(state.clone());
        }
        fn login_error(&self, message: &str) {
            self.errors.lock().unwrap().push(message.to_string());
        }
        fn open_url(&self, url: &str) -> Result<(), String> {
            self.opened.lock().unwrap().push(url.to_string());
            Ok(())
        }
    }

    pub struct Fixture {
        pub core: Arc<Core>,
        pub tokens: Arc<MemoryStore>,
        pub events: Arc<Recorder>,
        pub dir: tempfile::TempDir,
    }

    pub fn fixture(proxy: &str) -> Fixture {
        let dir = tempfile::tempdir().unwrap();
        let tokens = Arc::new(MemoryStore::default());
        let events = Arc::new(Recorder::default());
        let core = Arc::new(Core::new(
            Options {
                proxy: proxy.to_string(),
                secret: "s3cret".into(),
                settings_path: dir.path().join("settings.json"),
                cache_root: dir.path().join("offline"),
                device: "测试的 Mac".into(),
                version: "0.0.0".into(),
            },
            tokens.clone(),
            events.clone(),
            reqwest::Client::new(),
        ));
        Fixture {
            core,
            tokens,
            events,
            dir,
        }
    }

    impl Fixture {
        /// 直接写好「已经连上这台服务器、某个用户登录了」的状态，再重新创建 Core（模拟重启）。
        pub fn signed_in(server: &str, proxy: &str) -> Fixture {
            let f = fixture(proxy);
            std::fs::write(
                f.dir.path().join("settings.json"),
                serde_json::to_vec(&serde_json::json!({
                    "server": server,
                    "user": {"id": "u1", "name": "王小明", "email": "wang@example.com", "role": "member"},
                }))
                .unwrap(),
            )
            .unwrap();
            f.tokens.set(server, "zsm_test").unwrap();
            let core = Arc::new(Core::new(
                Options {
                    proxy: proxy.to_string(),
                    secret: "s3cret".into(),
                    settings_path: f.dir.path().join("settings.json"),
                    cache_root: f.dir.path().join("offline"),
                    device: "测试的 Mac".into(),
                    version: "0.0.0".into(),
                },
                f.tokens.clone(),
                f.events.clone(),
                reqwest::Client::new(),
            ));
            Fixture { core, ..f }
        }
    }
}
