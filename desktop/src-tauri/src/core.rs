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
use tokio::sync::watch;

use crate::cache::{Generation, OfflineCache, DEFAULT_MAX_BYTES};
use crate::server::{self, Answer, Pkce};
use crate::token::TokenStore;

/// 点了「在浏览器里登录」以后，多久之内回来的登录链接有效。
const LOGIN_WINDOW: Duration = Duration::from_secs(15 * 60);

/// 状态变了要告诉谁。正式运行时是 Tauri 的事件和系统浏览器，测试时记下来检查。
pub trait Events: Send + Sync {
    /// 状态变了（界面据此切换「填服务器地址」「在浏览器里登录」和主界面）。
    fn state(&self, state: &AppState);
    /// 要在登录页上给用户看的原因：浏览器登录回来以后没能完成登录，或者退出登录、换服务器时设置没能保存。
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

/// 当前登录的账号：哪台服务器上的哪个用户，用的是哪个令牌。
/// 上传录音前要核对录音属于这个账号（服务器和用户都一样），再用这个令牌，绝不拿别人的令牌传。
#[derive(Clone, Debug, PartialEq)]
pub struct Account {
    pub server: String,
    pub user_id: String,
    pub token: String,
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
    /// 第几次登录。重新点登录、取消、退出、换服务器都加一：换令牌的请求回来时对不上，
    /// 说明那次登录已经不算数了，换到的令牌不能用来登录
    login_attempt: u64,
    expired: bool,
}

impl Inner {
    /// 不再等现在这次登录：等着浏览器跳回来的、正在换令牌的，都不算数了。
    fn drop_login(&mut self) {
        self.pending = None;
        self.login_attempt += 1;
    }
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
    /// 登录的账号变了（登录、退出、登录失效、换服务器）就更新，录音的上传任务据此暂停或接着传
    account: watch::Sender<Option<Account>>,
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
        let inner = Inner {
            settings,
            token,
            pending: None,
            login_attempt: 0,
            expired: false,
        };
        let (account, _) = watch::channel(account_of(&inner));
        Self {
            opts,
            tokens,
            events,
            client,
            cache_generation: Generation::default(),
            inner: Mutex::new(inner),
            account,
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
        // 在 watch 的锁里取当前的账号：几处同时变化时，最后发出去的一定是最新的
        self.account.send_if_modified(|current| {
            let account = self.account();
            let modified = *current != account;
            *current = account;
            modified
        });
        self.events.state(&state);
        state
    }

    /// 当前登录的账号。没登录（或者登录失效了）时是 None。
    pub fn account(&self) -> Option<Account> {
        account_of(&self.inner.lock().unwrap())
    }

    /// 订阅登录的账号的变化。
    pub fn watch_account(&self) -> watch::Receiver<Option<Account>> {
        self.account.subscribe()
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

    /// 把设置写进设置文件：先写临时文件再改名，写到一半失败也不会弄坏原来的。
    fn save(&self, settings: &Settings) -> std::io::Result<()> {
        let result = (|| -> std::io::Result<()> {
            if let Some(dir) = self.opts.settings_path.parent() {
                std::fs::create_dir_all(dir)?;
            }
            let tmp = self.opts.settings_path.with_extension("tmp");
            std::fs::write(&tmp, serde_json::to_vec_pretty(settings)?)?;
            std::fs::rename(tmp, &self.opts.settings_path)
        })();
        if let Err(e) = &result {
            log::error!("设置保存失败：{e}");
        }
        result
    }

    /// 改设置：先存进设置文件，存下来了才改内存里的，两边一致。存不下来时什么都不改。
    fn update_settings(
        &self,
        inner: &mut Inner,
        change: impl FnOnce(&mut Settings),
    ) -> std::io::Result<()> {
        let mut settings = inner.settings.clone();
        change(&mut settings);
        self.save(&settings)?;
        inner.settings = settings;
        Ok(())
    }

    /// 在本机退出：删掉令牌、这个用户的离线缓存，忘掉是谁，正在进行的登录也不算数了。服务器地址留着。
    /// 内存里一定退出；设置文件没能保存时返回错误（令牌从钥匙串里删掉了的话，重启以后也不会登录着）。
    fn sign_out_locally(&self, inner: &mut Inner, expired: bool) -> std::io::Result<()> {
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
        inner.drop_login();
        inner.expired = expired;
        self.save(&inner.settings)
    }

    /// 填服务器地址：检查能用以后保存。换了服务器的话，先退出原来那台。存不下来就不换，返回原因。
    pub async fn set_server(&self, input: &str) -> Result<AppState, String> {
        let server = server::check_server(input).await?;
        let same = self.inner.lock().unwrap().settings.server.as_deref() == Some(&server);
        if same {
            return Ok(self.changed());
        }
        // 退出时设置没存下来不要紧：下面连同新的服务器地址整个再存一遍
        let _ = self.logout_inner(false).await;
        let saved = {
            let mut inner = self.inner.lock().unwrap();
            inner.expired = false;
            self.update_settings(&mut inner, |s| s.server = Some(server))
        };
        let state = self.changed();
        saved.map_err(|e| format!("服务器地址没能保存（{e}），请检查磁盘空间后再试"))?;
        Ok(state)
    }

    /// 换服务器：退出登录，忘掉服务器地址，界面回到「填服务器地址」。
    /// 服务器地址没能从设置里清掉时留着它（重启以后也还是它），原因显示在登录页上。
    pub async fn switch_server(&self) -> AppState {
        // 退出时设置没存下来不要紧：下面清掉服务器地址时整个再存一遍
        let _ = self.logout_inner(false).await;
        let saved = {
            let mut inner = self.inner.lock().unwrap();
            inner.expired = false;
            self.update_settings(&mut inner, |s| s.server = None)
        };
        if let Err(e) = saved {
            self.events.login_error(&format!(
                "服务器地址没能清掉（{e}），请检查磁盘空间后再点「换一个服务器」"
            ));
        }
        self.changed()
    }

    /// 点「在浏览器里登录」：生成 PKCE，在系统浏览器里打开授权页。再点一次就重新生成、重新打开，之前那次不算数了。
    pub fn login_start(&self) -> Result<AppState, String> {
        let url = {
            let mut inner = self.inner.lock().unwrap();
            let server = inner.settings.server.clone().ok_or("请先填写服务器地址")?;
            let pkce = Pkce::new();
            let url = server::authorize_url(&server, &self.opts.device, &pkce);
            inner.drop_login();
            inner.pending = Some(Pending {
                pkce,
                server,
                started: Instant::now(),
            });
            url
        };
        if let Err(e) = self.events.open_url(&url) {
            self.inner.lock().unwrap().drop_login();
            self.changed();
            return Err(format!("没能打开浏览器（{e}）"));
        }
        Ok(self.changed())
    }

    /// 取消登录。正在换令牌的话，换到的令牌也不用了（见 finish_login）。
    pub fn login_cancel(&self) -> AppState {
        self.inner.lock().unwrap().drop_login();
        self.changed()
    }

    /// 收到浏览器登录完跳回来的 `zsmeet://auth?…`：核对 state；点了「允许」就用 code 换令牌，存进钥匙串。
    /// 出错时通过 `Events::login_error` 告诉界面，也作为返回值（方便测试）。
    pub async fn handle_deep_link(&self, link: &str) -> Result<(), String> {
        let result = self.finish_login(link).await;
        if let Err(message) = &result {
            self.events.login_error(message);
        }
        self.changed();
        result
    }

    /// 换令牌期间这次登录被取消了（点了取消、重新点了登录、退出、换了服务器）时也返回 Ok：
    /// 不登录，换到的令牌作废，是用户自己取消的，不用提示。
    async fn finish_login(&self, link: &str) -> Result<(), String> {
        let callback = server::parse_callback(link)?;
        let (pending, code, attempt) = {
            let mut inner = self.inner.lock().unwrap();
            let matches = inner
                .pending
                .as_ref()
                .is_some_and(|p| p.pkce.state == callback.state);
            if !matches {
                // 不是这次点「在浏览器里登录」打开的页面跳回来的（旧的标签页、别人发来的链接）：不认，
                // 正在等的登录照样等着
                return Err(
                    "这个登录链接不是这次登录打开的，或者已经用过了。请回到 zsmeet 重新点「在浏览器里登录」"
                        .into(),
                );
            }
            let pending = inner.pending.take().expect("刚检查过");
            let code = match callback.answer {
                Answer::Allowed(code) => code,
                // 在网页上点了「取消」：这次登录到此为止
                Answer::Refused(message) => return Err(message),
            };
            if pending.started.elapsed() > LOGIN_WINDOW {
                return Err("登录等得太久，已经失效了。请重新点「在浏览器里登录」".into());
            }
            if inner.settings.server.as_deref() != Some(&pending.server) {
                return Err("登录期间换了服务器，请重新点「在浏览器里登录」".into());
            }
            (pending, code, inner.login_attempt)
        };
        let (token, user) =
            server::exchange(&self.client, &pending.server, &code, &pending.pkce.verifier).await?;
        let stored = self.store_login(attempt, &pending.server, &token, user);
        if !matches!(stored, Ok(true)) {
            // 换到的令牌用不上了：告诉服务器作废，不让它一直有效
            server::revoke(&self.client, &pending.server, &token).await;
        }
        stored.map(|_| ())
    }

    /// 换到令牌以后：这次登录还算数的话，令牌存进钥匙串、用户存进设置，登录。
    /// Ok(false)：换令牌期间这次登录被取消了，没有登录。
    /// 设置存不下来时不算登录，钥匙串里的令牌也删掉：不然看起来登录了，重启以后却找不到是谁登录的。
    fn store_login(
        &self,
        attempt: u64,
        server: &str,
        token: &str,
        user: Value,
    ) -> Result<bool, String> {
        let mut inner = self.inner.lock().unwrap();
        if inner.login_attempt != attempt {
            return Ok(false);
        }
        self.tokens.set(server, token)?;
        if let Err(e) = self.update_settings(&mut inner, |s| s.user = Some(user)) {
            if let Err(e) = self.tokens.delete(server) {
                log::warn!("{e}");
            }
            return Err(format!(
                "登录信息没能保存（{e}），请检查磁盘空间后重新点「在浏览器里登录」"
            ));
        }
        inner.token = Some(token.to_string());
        inner.expired = false;
        Ok(true)
    }

    /// 退出登录：通知服务器作废这台电脑的令牌，再在本机退出。
    /// 设置没能保存时也照样退出，原因显示在登录页上（点退出的那个页面已经不在了）。
    pub async fn logout(&self) -> AppState {
        if let Err(e) = self.logout_inner(false).await {
            self.events.login_error(&format!(
                "已经退出登录，但设置没能保存（{e}），请检查磁盘空间"
            ));
        }
        self.changed()
    }

    async fn logout_inner(&self, expired: bool) -> std::io::Result<()> {
        let (server, token) = {
            let inner = self.inner.lock().unwrap();
            (inner.settings.server.clone(), inner.token.clone())
        };
        if let (Some(server), Some(token)) = (server, token) {
            server::revoke(&self.client, &server, &token).await;
        }
        let mut inner = self.inner.lock().unwrap();
        self.sign_out_locally(&mut inner, expired)
    }

    /// 服务器说这个令牌不能用了（401）。只在出问题的还是当前令牌时退出，免得刚重新登录又被旧请求的 401 踢出去。
    pub fn unauthorized(&self, token_used: &str) {
        {
            let mut inner = self.inner.lock().unwrap();
            if inner.token.as_deref() != Some(token_used) {
                return;
            }
            // 设置没存下来时没有地方提示（save 记了日志）：令牌从钥匙串里删掉了，重启以后也不会登录着
            let _ = self.sign_out_locally(&mut inner, true);
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

fn account_of(inner: &Inner) -> Option<Account> {
    Some(Account {
        server: inner.settings.server.clone()?,
        user_id: server::user_id(inner.settings.user.as_ref()?)?,
        token: inner.token.clone()?,
    })
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
            crate::server::http_client(),
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
                crate::server::http_client(),
            ));
            Fixture { core, ..f }
        }
    }
}
