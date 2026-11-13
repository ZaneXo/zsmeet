//! 自动更新（见 docs/desktop.md「自动更新」）。
//!
//! - 新版本的信息在 GitHub Releases 上一个固定的地址：desktop-latest 这个 Release 里的 latest.json
//!   （tauri.conf.json 的 plugins.updater.endpoints）。服务器版本也发在同一个仓库的 v* 标签上，不能靠 GitHub 的
//!   「Latest」。
//! - 启动后约 10 秒查一次，之后每 6 小时查一次，有新版本就发 update://available 告诉界面；界面上的「检查更新」
//!   调 update_check。
//! - 只在用户点了「更新」（update_install）以后才装：再查一次最新的版本，下载（update://progress 报进度），
//!   用 tauri.conf.json 里的公钥校验签名，换掉 zsmeet.app，重新打开。正在录音、还有录音没传完时不装，说明原因；
//!   下载的时候开始了录音的，下载完也不装。
//! - 打包时默认不生成更新包（tauri.conf.json 的 bundle.createUpdaterArtifacts 是 false），没有签名用的私钥也能
//!   打包；发布时 CI 用 --config 打开它，用 GitHub Secrets 里的私钥签名。

use std::future::Future;
use std::path::Path;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant, SystemTime};

use serde::Serialize;
use tauri::{AppHandle, Emitter};
use tauri_plugin_updater::{Update, UpdaterExt};
use time::format_description::well_known::Rfc3339;
use url::Url;

use crate::recorder::Recorder;

/// 发给界面的事件：有新版本（内容是 Available）；下载新版本的进度（内容是 Progress）。
pub const EVENT_AVAILABLE: &str = "update://available";
pub const EVENT_PROGRESS: &str = "update://progress";

/// 自动更新坏了的时候，用户自己去哪下载新版本。
const RELEASES_PAGE: &str = "https://github.com/ZaneXo/zsmeet/releases";
/// 启动以后多久第一次检查（不和启动时的其他事抢网络）。
const FIRST_CHECK: Duration = Duration::from_secs(10);
/// 之后多久查一次。
const CHECK_EVERY: Duration = Duration::from_secs(6 * 3600);
/// 定时检查时隔多久看一次到没到时间：电脑睡眠的时候计时器不走，按墙上的时间算。
const TICK: Duration = Duration::from_secs(10 * 60);
/// 取 latest.json 最多等多久。
const CHECK_TIMEOUT: Duration = Duration::from_secs(30);
/// 下载新版本时连上 GitHub 最多等多久、多久收不到数据算断了（整个下载不限时间，网慢也能下完）。
const CONNECT_TIMEOUT: Duration = Duration::from_secs(15);
const READ_TIMEOUT: Duration = Duration::from_secs(60);
/// 下载进度最多每 100 毫秒告诉界面一次。
const PROGRESS_EVERY: Duration = Duration::from_millis(100);

const INSTALLING: &str = "正在更新，装好以后 zsmeet 会自动重新打开，请稍等";
const UP_TO_DATE: &str = "已经是最新版本了，不用更新";

/// update_check 的结果。
#[derive(Serialize, Clone, Debug, PartialEq)]
pub struct CheckResult {
    /// 现在的版本
    pub current: String,
    /// 有新版本时是它的信息，没有时是 null
    pub available: Option<Available>,
}

/// 新版本（update://available 的内容）。
#[derive(Serialize, Clone, Debug, PartialEq)]
pub struct Available {
    pub version: String,
    /// 更新说明（latest.json 的 notes），没有时是 null
    pub notes: Option<String>,
    /// 发布时间（latest.json 的 pub_date），RFC 3339，比如 2026-11-12T08:00:00Z；没有时是 null
    pub date: Option<String>,
}

/// 下载进度（update://progress 的内容）：下载了多少字节，一共多少字节（服务器没说时是 null）。
#[derive(Serialize, Clone, Copy, Debug, PartialEq, Eq)]
pub struct Progress {
    pub downloaded: u64,
    pub total: Option<u64>,
}

/// 新版本从哪来、怎么装。正式运行时是 Tauri 的 updater 插件（Releases），测试时是假的，不连网。
pub(crate) trait Source: Sync {
    type Package: Send + Sync;

    /// 查一次有没有新版本。
    fn latest(&self) -> impl Future<Output = Result<Option<Self::Package>, String>> + Send;

    fn describe(&self, package: &Self::Package) -> Available;

    /// 下载新版本、校验签名。progress：下载了多少字节、一共多少（不知道时是 None）。
    fn download(
        &self,
        package: &Self::Package,
        progress: impl FnMut(u64, Option<u64>) + Send,
    ) -> impl Future<Output = Result<Vec<u8>, String>> + Send;

    /// 换掉现在的 zsmeet.app（装之前再校验一次签名）。
    fn install(
        &self,
        package: &Self::Package,
        bytes: Vec<u8>,
    ) -> impl Future<Output = Result<(), String>> + Send;
}

/// 装好新版本要重启 App，能不能重启由录音那边说了算（recorder::Recorder）。
pub(crate) trait Gate: Sync {
    /// 现在不能重启的原因
    fn blocker(&self) -> Option<String>;
    /// 要装了：能重启的话从现在起不让开始录音，不能的话返回原因
    fn hold(&self) -> Result<(), String>;
    /// 没装上：又可以开始录音了
    fn release(&self);
}

impl Gate for Recorder {
    fn blocker(&self) -> Option<String> {
        self.update_blocker()
    }

    fn hold(&self) -> Result<(), String> {
        self.hold_for_update()
    }

    fn release(&self) {
        self.release_update_hold()
    }
}

/// 查一次有没有新版本。current：现在的版本。
pub(crate) async fn check<S: Source>(source: &S, current: String) -> Result<CheckResult, String> {
    let available = source.latest().await?.map(|p| source.describe(&p));
    Ok(CheckResult { current, available })
}

/// 用户点了「更新」：再查一次最新的版本（下载地址、签名以现在的 latest.json 为准，界面上显示的可能是几个小时前
/// 查到的），下载、校验签名、换掉 zsmeet.app，返回装好的版本，由调用的地方重启 App（从这时起不让开始录音）。
/// 正在录音、还有录音没传完时不装；下载的时候开始了录音的，下载完也不装。installing：正在装，再点一次不再装一遍。
pub(crate) async fn install<S: Source, G: Gate>(
    source: &S,
    gate: &G,
    installing: &AtomicBool,
    mut report: impl FnMut(Progress) + Send,
) -> Result<Available, String> {
    if let Some(reason) = gate.blocker() {
        return Err(reason);
    }
    if installing.swap(true, Ordering::AcqRel) {
        return Err(INSTALLING.into());
    }
    let result = download_and_install(source, gate, &mut report).await;
    // 装好了的马上就重启，不用再放开
    if result.is_err() {
        installing.store(false, Ordering::Release);
    }
    result
}

async fn download_and_install<S: Source, G: Gate>(
    source: &S,
    gate: &G,
    report: &mut (impl FnMut(Progress) + Send),
) -> Result<Available, String> {
    let package = source.latest().await?.ok_or(UP_TO_DATE)?;
    let available = source.describe(&package);
    log::info!("开始下载新版本 {}", available.version);
    let mut throttle = Throttle::new(PROGRESS_EVERY);
    let mut total = None;
    let bytes = source
        .download(&package, |downloaded, size| {
            total = size;
            if throttle.ready(Instant::now()) {
                report(Progress {
                    downloaded,
                    total: size,
                });
            }
        })
        .await?;
    // 最后一次一定报：界面上显示下载完了
    report(Progress {
        downloaded: bytes.len() as u64,
        total,
    });
    // 下载的时候可能开始录音了：再看一遍，能装的话从现在起不让开始录音
    gate.hold()?;
    if let Err(e) = source.install(&package, bytes).await {
        gate.release();
        return Err(e);
    }
    Ok(available)
}

/// 下载进度每收到一块数据就有一次（每秒可能上千次），隔一会儿才告诉界面一次。
struct Throttle {
    every: Duration,
    last: Option<Instant>,
}

impl Throttle {
    fn new(every: Duration) -> Self {
        Self { every, last: None }
    }

    fn ready(&mut self, now: Instant) -> bool {
        if self
            .last
            .is_some_and(|last| now.saturating_duration_since(last) < self.every)
        {
            return false;
        }
        self.last = Some(now);
        true
    }
}

/// 到没到下一次定时检查：上次查过以后过了 6 小时。since_wall：按墙上的时间过了多久（电脑睡眠的时间也算，
/// 表被往回调了时是 None）；since_awake：电脑醒着的时间过了多久。
fn due(since_wall: Option<Duration>, since_awake: Duration) -> bool {
    since_wall.is_some_and(|d| d >= CHECK_EVERY) || since_awake >= CHECK_EVERY
}

/// update_install 用：正在装的时候再点一次不再装一遍。
#[derive(Default)]
pub struct Updates {
    installing: AtomicBool,
}

/// 正式运行时的更新来源：Tauri 的 updater 插件。按 tauri.conf.json 的 plugins.updater 取 GitHub Releases 上的
/// latest.json，用里面的公钥校验签名；系统设置里开了 HTTPS 代理时走代理（见 system_proxy）。
pub struct Releases(pub AppHandle);

impl Source for Releases {
    type Package = Update;

    async fn latest(&self) -> Result<Option<Update>, String> {
        let mut builder = self
            .0
            .updater_builder()
            .timeout(CHECK_TIMEOUT)
            .configure_client(|client| {
                client
                    .connect_timeout(CONNECT_TIMEOUT)
                    .read_timeout(READ_TIMEOUT)
            });
        // 下载新版本的请求跟着这次检查的设置走
        if let Some(proxy) = system_proxy() {
            log::info!("检查更新走系统设置里的代理 {proxy}");
            builder = builder.proxy(proxy);
        }
        let updater = builder.build().map_err(|e| {
            log::error!("没能准备好检查更新：{e}");
            failure_message(&e)
        })?;
        updater.check().await.map_err(|e| {
            log::warn!("检查更新失败：{e}");
            failure_message(&e)
        })
    }

    fn describe(&self, update: &Update) -> Available {
        Available {
            version: update.version.clone(),
            notes: update.body.clone().filter(|notes| !notes.trim().is_empty()),
            date: update.date.and_then(|date| date.format(&Rfc3339).ok()),
        }
    }

    async fn download(
        &self,
        update: &Update,
        mut progress: impl FnMut(u64, Option<u64>) + Send,
    ) -> Result<Vec<u8>, String> {
        let mut downloaded = 0u64;
        update
            .download(
                |chunk, total| {
                    downloaded += chunk as u64;
                    progress(downloaded, total);
                },
                || {},
            )
            .await
            .map_err(|e| {
                log::warn!("下载新版本 {} 失败：{e}", update.version);
                failure_message(&e)
            })
    }

    async fn install(&self, update: &Update, bytes: Vec<u8>) -> Result<(), String> {
        // 解压、换掉 zsmeet.app 是阻塞的文件操作（要管理员密码时还要等用户输入），不占着异步的线程
        let update = update.clone();
        tauri::async_runtime::spawn_blocking(move || {
            update.install(bytes).map_err(|e| {
                log::error!("新版本 {} 没装上：{e}", update.version);
                failure_message(&e)
            })
        })
        .await
        .map_err(|e| format!("新版本没装上（{e}），请再试一次"))?
    }
}

/// 系统设置里的「安全网页代理（HTTPS）」：开没开、服务器、端口。
#[derive(Debug, Default, Clone, PartialEq, Eq)]
struct HttpsProxy {
    enabled: bool,
    host: Option<String>,
    port: Option<i64>,
}

/// 检查、下载新版本走的代理：「系统设置 → 网络 → 详细信息 → 代理」里打开了「安全网页代理（HTTPS）」就用它（比如
/// Clash 的 127.0.0.1:7890，有的网络要靠它才连得上 GitHub）。和 updater 插件自带的读法一样，不看「忽略这些主机与域的
/// 代理设置」（那里一般是局域网里的地址）。设了环境变量 HTTPS_PROXY（开发时）就照环境变量来，这里不管。
/// 只有检查更新用它：App 连 zsmeet 服务器、传录音的请求照旧直连（见 Cargo.toml 里 tauri-plugin-updater 的说明）。
fn system_proxy() -> Option<Url> {
    let from_env = ["HTTPS_PROXY", "https_proxy"]
        .iter()
        .any(|name| std::env::var_os(name).is_some_and(|v| !v.is_empty()));
    if from_env {
        return None;
    }
    proxy_url(&read_https_proxy()?)
}

/// 系统设置里的 HTTPS 代理换成代理的地址 http://服务器:端口（HTTPS 的请求通过它用 CONNECT 连过去）。没开、没填服务器时是 None。
fn proxy_url(proxy: &HttpsProxy) -> Option<Url> {
    if !proxy.enabled {
        return None;
    }
    let host = proxy
        .host
        .as_deref()
        .map(str::trim)
        .filter(|h| !h.is_empty())?;
    // IPv6 地址在网址里要加方括号
    let host = if host.contains(':') && !host.starts_with('[') {
        format!("[{host}]")
    } else {
        host.to_string()
    };
    let address = match proxy.port.filter(|p| (1..=65535).contains(p)) {
        Some(port) => format!("http://{host}:{port}"),
        None => format!("http://{host}"),
    };
    Url::parse(&address).ok()
}

/// 读系统设置里的 HTTPS 代理（和 reqwest、updater 插件读系统代理用的是同一个接口）。
#[cfg(target_os = "macos")]
fn read_https_proxy() -> Option<HttpsProxy> {
    use system_configuration::core_foundation::number::CFNumber;
    use system_configuration::core_foundation::string::{CFString, CFStringRef};
    use system_configuration::dynamic_store::SCDynamicStoreBuilder;
    use system_configuration::sys::schema_definitions::{
        kSCPropNetProxiesHTTPSEnable, kSCPropNetProxiesHTTPSPort, kSCPropNetProxiesHTTPSProxy,
    };

    let store = SCDynamicStoreBuilder::new("zsmeet-update").build()?;
    let proxies = store.get_proxies()?;
    // SAFETY: SystemConfiguration 框架导出的字符串常量，进程运行期间一直有效
    let (enable_key, host_key, port_key) = unsafe {
        (
            kSCPropNetProxiesHTTPSEnable,
            kSCPropNetProxiesHTTPSProxy,
            kSCPropNetProxiesHTTPSPort,
        )
    };
    let number = |key: CFStringRef| {
        proxies
            .find(key)
            .and_then(|value| value.downcast::<CFNumber>())
            .and_then(|n| n.to_i64())
    };
    Some(HttpsProxy {
        enabled: number(enable_key) == Some(1),
        host: proxies
            .find(host_key)
            .and_then(|value| value.downcast::<CFString>())
            .map(|s| s.to_string()),
        port: number(port_key),
    })
}

#[cfg(not(target_os = "macos"))]
fn read_https_proxy() -> Option<HttpsProxy> {
    None
}

fn current_version(app: &AppHandle) -> String {
    app.package_info().version.to_string()
}

/// update_check：现在的版本，有没有新版本。
pub async fn check_now(app: &AppHandle) -> Result<CheckResult, String> {
    check(&Releases(app.clone()), current_version(app)).await
}

/// update_install：下载、校验、装好新版本，重新打开 App。不能装时返回原因。
pub async fn install_and_relaunch(
    app: &AppHandle,
    updates: &Updates,
    recorder: &Recorder,
) -> Result<(), String> {
    let exe = std::env::current_exe().map_err(|e| {
        format!("找不到 zsmeet 装在哪（{e}），没法自动更新。请到 {RELEASES_PAGE} 下载新版本")
    })?;
    if let Some(problem) = location_problem(&exe) {
        return Err(problem);
    }
    let events = app.clone();
    let installed = install(
        &Releases(app.clone()),
        recorder,
        &updates.installing,
        move |progress| {
            if let Err(e) = events.emit(EVENT_PROGRESS, progress) {
                log::warn!("更新进度事件发送失败：{e}");
            }
        },
    )
    .await?;
    log::info!("新版本 {} 装好了，重新打开 zsmeet", installed.version);
    // 和退出一样先走 RunEvent::Exit（没有在录音，录音那边很快就结束），再重新打开
    app.request_restart();
    Ok(())
}

/// 启动后约 10 秒查一次，之后每 6 小时查一次；有新版本就告诉界面，不自动装。
pub fn spawn_checks(app: AppHandle) {
    tauri::async_runtime::spawn(async move {
        tokio::time::sleep(FIRST_CHECK).await;
        loop {
            match check_now(&app).await {
                Ok(CheckResult {
                    available: Some(available),
                    current,
                }) => {
                    log::info!("有新版本 {}（现在是 {current}）", available.version);
                    if let Err(e) = app.emit(EVENT_AVAILABLE, &available) {
                        log::warn!("新版本事件发送失败：{e}");
                    }
                }
                Ok(_) => log::info!("已经是最新版本"),
                // 原因已经记在日志里（Releases::latest）：等下一次
                Err(_) => {}
            }
            let (wall, awake) = (SystemTime::now(), Instant::now());
            loop {
                tokio::time::sleep(TICK).await;
                if due(SystemTime::now().duration_since(wall).ok(), awake.elapsed()) {
                    break;
                }
            }
        }
    });
}

/// 现在运行的 zsmeet 能不能换成新版本，不能的话返回原因。exe：App 主程序的路径。
fn location_problem(exe: &Path) -> Option<String> {
    let Some(bundle) = app_bundle(exe) else {
        return Some("现在运行的不是打包好的 zsmeet.app（是开发版），不能自动更新".into());
    };
    // 没挪过位置、直接从下载的地方（或者安装盘里）打开的 App，macOS 把它放在一个只读的临时位置运行
    if bundle
        .components()
        .any(|c| c.as_os_str() == "AppTranslocation")
    {
        return Some("zsmeet 是从下载的地方直接打开的，macOS 不让它换成新版本。请退出 zsmeet，把它拖到「应用程序」文件夹里，从那里打开以后再更新".into());
    }
    None
}

/// 主程序所在的 .app：…/zsmeet.app/Contents/MacOS/zsmeet 的 …/zsmeet.app。不在 .app 里（开发版）时是 None。
fn app_bundle(exe: &Path) -> Option<&Path> {
    let macos = exe.parent()?;
    let contents = macos.parent()?;
    let bundle = contents.parent()?;
    let is_bundle = macos.file_name()? == "MacOS"
        && contents.file_name()? == "Contents"
        && bundle.extension()? == "app";
    is_bundle.then_some(bundle)
}

/// updater 插件的错误换成给用户看的话：出了什么事、能怎么办。原来的错误记在日志里。
fn failure_message(e: &tauri_plugin_updater::Error) -> String {
    use std::io::ErrorKind;
    use tauri_plugin_updater::Error as E;
    match e {
        E::Reqwest(e) if e.is_decode() => format!(
            "新版本的信息看不懂（连的网络可能要先登录），请稍后再试。一直这样的话，到 {RELEASES_PAGE} 下载新版本"
        ),
        E::Reqwest(_) => format!(
            "连不上 GitHub（新版本放在 GitHub 上），请检查网络（用代理上网的，看看代理开着没有）后再试。也可以到 {RELEASES_PAGE} 下载新版本"
        ),
        // 插件只在下载安装包时 GitHub 回的不是 2xx 才给这个错误（比如 404：这个版本的安装包撤下来了、重新发布时正在换文件）。
        // 连得上，不要让人去查网络和代理
        E::Network(detail) => {
            let status = detail
                .rsplit_once("status: ")
                .map_or(detail.as_str(), |(_, status)| status);
            format!(
                "GitHub 上暂时下载不到新版本的安装包（{status}），请稍后再试。一直这样的话，到 {RELEASES_PAGE} 下载新版本"
            )
        }
        E::ReleaseNotFound => {
            "没取到新版本的信息（GitHub 上暂时没有，或者连的网络要先登录），请稍后再试".into()
        }
        E::Serialization(_) | E::Semver(_) | E::UrlParse(_) => format!(
            "新版本的信息看不懂，请稍后再试。一直这样的话，到 {RELEASES_PAGE} 下载新版本"
        ),
        E::TargetNotFound(_) | E::TargetsNotFound(_) => format!(
            "新版本里还没有这台电脑能用的安装包，请稍后再试，或者到 {RELEASES_PAGE} 下载"
        ),
        E::Minisign(_)
        | E::Base64(_)
        | E::SignatureUtf8(_)
        | E::SignedVersionMismatch { .. }
        | E::MissingSignedVersion => format!(
            "下载的新版本没有通过签名校验（可能是下载出错了，也可能被人改过），没有安装。请稍后再试，或者到 {RELEASES_PAGE} 下载"
        ),
        E::InvalidUpdaterFormat | E::BinaryNotFoundInArchive => format!(
            "下载的新版本安装包看不懂，没有安装。请稍后再试，或者到 {RELEASES_PAGE} 下载"
        ),
        E::PreviousAppNotRestored(kept) => format!(
            "新版本没装上，原来的 zsmeet 被挪到了 {}。请把它拖回「应用程序」文件夹，或者到 {RELEASES_PAGE} 下载新版本重新安装",
            kept.display()
        ),
        E::TempDirNotOnSameMountPoint => "zsmeet 所在的磁盘上放不了临时文件，没法自动更新。请把 zsmeet 拖到「应用程序」文件夹里，从那里打开以后再更新".into(),
        E::Io(e) if e.kind() == ErrorKind::ReadOnlyFilesystem => "zsmeet 所在的位置只能读、不能改（比如是直接从安装盘里打开的），没法自动更新。请把 zsmeet 拖到「应用程序」文件夹里，从那里打开以后再更新".into(),
        E::Io(e) if e.kind() == ErrorKind::PermissionDenied => format!(
            "新版本没装上：换掉 zsmeet 要输入这台电脑的管理员密码。请再点一次「更新」并输入密码，或者到 {RELEASES_PAGE} 下载新版本自己安装"
        ),
        E::Io(e) => format!(
            "新版本没装上（{e}），请再试一次。一直这样的话，到 {RELEASES_PAGE} 下载新版本"
        ),
        _ => format!("没法自动更新（{e}），请到 {RELEASES_PAGE} 下载新版本"),
    }
}

#[cfg(test)]
mod tests {
    use std::path::PathBuf;
    use std::sync::Mutex;

    use base64::Engine;
    use serde_json::{json, Value};

    use super::*;

    /// tauri.conf.json：打包进 App 的配置（updater 插件从这里读公钥和地址）。
    fn config() -> Value {
        serde_json::from_str(include_str!("../tauri.conf.json")).unwrap()
    }

    #[test]
    fn checks_the_fixed_address_with_the_owners_key() {
        let conf = config();
        // 和 updater 插件读配置的方式一样：读不出来的话 App 一启动插件就会出错
        let updater: tauri_plugin_updater::Config =
            serde_json::from_value(conf["plugins"]["updater"].clone()).unwrap();
        assert_eq!(
            updater
                .endpoints
                .iter()
                .map(|u| u.as_str())
                .collect::<Vec<_>>(),
            ["https://github.com/ZaneXo/zsmeet/releases/download/desktop-latest/latest.json"]
        );
        // 签名里要带着版本号，而且要和 latest.json 说的一样：改过的 latest.json 不能把旧版本当新版本装上
        assert!(updater.require_signed_version);
        assert!(!updater.allow_downgrades && !updater.dangerous_insecure_transport_protocol);
        assert!(
            !updater.dangerous_accept_invalid_certs && !updater.dangerous_accept_invalid_hostnames
        );

        // 公钥是 minisign 的公钥文件（两行）整个用 base64 编码，key id 是 A1D64D1C47997E80
        let b64 = base64::engine::general_purpose::STANDARD;
        let file = String::from_utf8(b64.decode(&updater.pubkey).unwrap()).unwrap();
        let mut lines = file.lines();
        assert_eq!(
            lines.next(),
            Some("untrusted comment: minisign public key: A1D64D1C47997E80")
        );
        let key = b64.decode(lines.next().unwrap()).unwrap();
        assert_eq!(key.len(), 42, "Ed + 8 字节 key id + 32 字节公钥");
        assert_eq!(&key[..2], b"Ed");
        let id: Vec<u8> = key[2..10].iter().rev().copied().collect();
        assert_eq!(hex::encode_upper(id), "A1D64D1C47997E80");

        // 打包时默认不生成更新包：本机没有签名用的私钥也能 make desktop-build。发布的 CI 用 --config 打开
        assert_ne!(conf["bundle"]["createUpdaterArtifacts"], json!(true));
    }

    #[test]
    fn payloads_use_the_agreed_field_names() {
        let available = Available {
            version: "0.2.0".into(),
            notes: Some("录音更稳了".into()),
            date: Some("2026-11-12T08:00:00Z".into()),
        };
        assert_eq!(
            serde_json::to_value(CheckResult {
                current: "0.1.0".into(),
                available: Some(available),
            })
            .unwrap(),
            json!({
                "current": "0.1.0",
                "available": {"version": "0.2.0", "notes": "录音更稳了", "date": "2026-11-12T08:00:00Z"}
            })
        );
        assert_eq!(
            serde_json::to_value(CheckResult {
                current: "0.1.0".into(),
                available: None,
            })
            .unwrap(),
            json!({"current": "0.1.0", "available": null})
        );
        assert_eq!(
            serde_json::to_value(Progress {
                downloaded: 1024,
                total: None
            })
            .unwrap(),
            json!({"downloaded": 1024, "total": null})
        );
    }

    #[test]
    fn checks_again_every_six_hours_even_across_sleep() {
        let hours = |h: u64| Duration::from_secs(h * 3600);
        assert!(!due(Some(hours(5)), hours(5)));
        assert!(due(Some(hours(6)), hours(6)));
        // 电脑睡了一夜：醒着的时间不到 6 小时，按墙上的时间已经过了
        assert!(due(Some(hours(9)), hours(1)));
        // 表被往回调了：按醒着的时间算
        assert!(!due(None, hours(1)));
        assert!(due(None, hours(6)));
    }

    #[test]
    fn only_a_packaged_app_in_a_normal_place_updates_itself() {
        let exe = |p: &str| PathBuf::from(p);
        assert_eq!(
            location_problem(&exe("/Applications/zsmeet.app/Contents/MacOS/zsmeet")),
            None
        );
        assert_eq!(
            location_problem(&exe(
                "/Users/me/Applications/zsmeet.app/Contents/MacOS/zsmeet"
            )),
            None
        );
        assert_eq!(
            app_bundle(&exe("/Applications/zsmeet.app/Contents/MacOS/zsmeet")),
            Some(Path::new("/Applications/zsmeet.app"))
        );
        // 开发版（tauri dev、cargo run）：换掉的会是 target/debug 目录
        let dev = location_problem(&exe("/src/zsmeet/desktop/src-tauri/target/debug/zsmeet"));
        assert!(dev.unwrap().contains("开发版"));
        let translocated = location_problem(&exe(
            "/private/var/folders/x1/abc/T/AppTranslocation/2C1F/d/zsmeet.app/Contents/MacOS/zsmeet",
        ));
        assert!(translocated.unwrap().contains("拖到「应用程序」文件夹"));
    }

    #[test]
    fn updates_go_through_the_https_proxy_from_system_settings() {
        let proxy = |enabled, host: Option<&str>, port| {
            proxy_url(&HttpsProxy {
                enabled,
                host: host.map(str::to_string),
                port,
            })
            .map(|url| url.to_string())
        };
        // 比如 Clash：127.0.0.1:7890
        assert_eq!(
            proxy(true, Some("127.0.0.1"), Some(7890)).as_deref(),
            Some("http://127.0.0.1:7890/")
        );
        assert_eq!(
            proxy(true, Some(" proxy.corp.example "), None).as_deref(),
            Some("http://proxy.corp.example/")
        );
        assert_eq!(
            proxy(true, Some("::1"), Some(7890)).as_deref(),
            Some("http://[::1]:7890/")
        );
        // 端口不对的当成没填
        assert_eq!(
            proxy(true, Some("10.0.0.2"), Some(0)).as_deref(),
            Some("http://10.0.0.2/")
        );
        // 关着、没填服务器：直连
        assert_eq!(proxy(false, Some("127.0.0.1"), Some(7890)), None);
        assert_eq!(proxy(true, Some("  "), Some(7890)), None);
        assert_eq!(proxy(true, None, Some(7890)), None);
        // 真的读一次这台电脑的设置：读不到、没开都行，不能出错
        let _ = read_https_proxy();
    }

    #[test]
    fn the_apps_own_requests_never_follow_the_system_proxy() {
        // updater 插件默认的 system-proxy 会让 App 自己的 reqwest 也走系统代理、不认「忽略这些主机」（见 Cargo.toml）
        let manifest = include_str!("../Cargo.toml");
        let updater = manifest
            .lines()
            .find(|line| line.starts_with("tauri-plugin-updater"))
            .unwrap();
        assert!(
            updater.contains("default-features = false") && !updater.contains("system-proxy"),
            "{updater}"
        );
    }

    #[test]
    fn explains_what_went_wrong_and_what_to_do() {
        use tauri_plugin_updater::Error as E;
        let io = |kind| E::Io(std::io::Error::from(kind));
        let cases = [
            (E::ReleaseNotFound, "没取到新版本的信息"),
            // 下载安装包时 GitHub 回了 404、5xx（插件 2.14 的写法）：网是通的
            (
                E::Network("Download request failed with status: 404 Not Found".into()),
                "GitHub 上暂时下载不到新版本的安装包（404 Not Found）",
            ),
            (
                E::Network("Download request failed with status: 503 Service Unavailable".into()),
                "（503 Service Unavailable），请稍后再试",
            ),
            (
                E::TargetsNotFound(vec!["darwin-aarch64".into()]),
                "没有这台电脑能用的安装包",
            ),
            (E::MissingSignedVersion, "没有通过签名校验"),
            (
                E::SignedVersionMismatch {
                    signed: "0.1.0".into(),
                    announced: "0.2.0".into(),
                },
                "没有通过签名校验",
            ),
            (E::InvalidUpdaterFormat, "安装包看不懂"),
            (E::TempDirNotOnSameMountPoint, "拖到「应用程序」文件夹"),
            (
                io(std::io::ErrorKind::ReadOnlyFilesystem),
                "拖到「应用程序」文件夹",
            ),
            (io(std::io::ErrorKind::PermissionDenied), "管理员密码"),
            (
                E::PreviousAppNotRestored("/Applications/zsmeet (previous version).app".into()),
                "/Applications/zsmeet (previous version).app",
            ),
            (E::EmptyEndpoints, RELEASES_PAGE),
        ];
        for (error, expected) in cases {
            let message = failure_message(&error);
            assert!(message.contains(expected), "{error:?}：{message}");
        }
        // 下载不到安装包不是网络的问题，不让人去查网络和代理
        let missing = failure_message(&E::Network(
            "Download request failed with status: 404 Not Found".into(),
        ));
        assert!(
            !missing.contains("连不上") && !missing.contains("代理"),
            "{missing}"
        );
        assert!(missing.contains(RELEASES_PAGE), "{missing}");
    }

    // ---- 装新版本的几步：假的来源、假的录音 ----

    #[derive(Default)]
    struct FakeSource {
        /// latest.json 里的新版本，None 是已经是最新的
        version: Option<&'static str>,
        /// 下载失败的原因
        download_error: Option<&'static str>,
        /// 安装失败的原因
        install_error: Option<&'static str>,
        /// 下载的时候发生的事（比如开始了录音）
        while_downloading: Option<Box<dyn Fn() + Send + Sync>>,
        steps: Mutex<Vec<String>>,
    }

    impl FakeSource {
        fn new(version: &'static str) -> Self {
            Self {
                version: Some(version),
                ..Self::default()
            }
        }

        fn steps(&self) -> Vec<String> {
            self.steps.lock().unwrap().clone()
        }

        fn step(&self, step: String) {
            self.steps.lock().unwrap().push(step);
        }
    }

    impl Source for FakeSource {
        type Package = String;

        async fn latest(&self) -> Result<Option<String>, String> {
            self.step("check".into());
            Ok(self.version.map(str::to_string))
        }

        fn describe(&self, package: &String) -> Available {
            Available {
                version: package.clone(),
                notes: None,
                date: None,
            }
        }

        async fn download(
            &self,
            package: &String,
            mut progress: impl FnMut(u64, Option<u64>) + Send,
        ) -> Result<Vec<u8>, String> {
            self.step(format!("download {package}"));
            // 一块一块地收：1000 块，每块 10 字节
            for i in 1..=1000 {
                progress(i * 10, Some(10_000));
            }
            if let Some(happen) = &self.while_downloading {
                happen();
            }
            match self.download_error {
                Some(e) => Err(e.into()),
                None => Ok(vec![0; 10_000]),
            }
        }

        async fn install(&self, package: &String, bytes: Vec<u8>) -> Result<(), String> {
            self.step(format!("install {package} ({} bytes)", bytes.len()));
            match self.install_error {
                Some(e) => Err(e.into()),
                None => Ok(()),
            }
        }
    }

    /// 假的录音那边：busy 是现在不能重启的原因。
    #[derive(Default)]
    struct FakeGate {
        busy: Mutex<Option<String>>,
        held: AtomicBool,
    }

    impl FakeGate {
        fn busy(&self, reason: Option<&str>) {
            *self.busy.lock().unwrap() = reason.map(str::to_string);
        }

        fn held(&self) -> bool {
            self.held.load(Ordering::SeqCst)
        }
    }

    impl Gate for FakeGate {
        fn blocker(&self) -> Option<String> {
            self.busy.lock().unwrap().clone()
        }

        fn hold(&self) -> Result<(), String> {
            if let Some(reason) = self.blocker() {
                return Err(reason);
            }
            self.held.store(true, Ordering::SeqCst);
            Ok(())
        }

        fn release(&self) {
            self.held.store(false, Ordering::SeqCst);
        }
    }

    #[tokio::test]
    async fn installs_after_downloading_and_keeps_new_recordings_out() {
        let source = FakeSource::new("0.2.0");
        let gate = FakeGate::default();
        let installing = AtomicBool::new(false);
        let mut seen = Vec::new();
        let installed = install(&source, &gate, &installing, |p| seen.push(p))
            .await
            .unwrap();
        assert_eq!(installed.version, "0.2.0");
        assert_eq!(
            source.steps(),
            ["check", "download 0.2.0", "install 0.2.0 (10000 bytes)"]
        );
        // 进度：不是每一块都报，最后一次一定报
        assert!(seen.len() < 20, "{} 次", seen.len());
        assert_eq!(
            seen.last(),
            Some(&Progress {
                downloaded: 10_000,
                total: Some(10_000)
            })
        );
        // 装好了马上重启：不让开始录音，也不再装一遍
        assert!(gate.held());
        let err = install(&source, &gate, &installing, |_| {})
            .await
            .unwrap_err();
        assert_eq!(err, INSTALLING);
        assert_eq!(source.steps().len(), 3);
    }

    #[tokio::test]
    async fn will_not_install_while_recording_or_uploading() {
        let source = FakeSource::new("0.2.0");
        let gate = FakeGate::default();
        gate.busy(Some("正在录音。结束录音、等录好的都传完以后再更新"));
        let installing = AtomicBool::new(false);
        let err = install(&source, &gate, &installing, |_| {})
            .await
            .unwrap_err();
        assert!(err.contains("正在录音"), "{err}");
        // 连下载都不下
        assert!(source.steps().is_empty());
        assert!(!gate.held() && !installing.load(Ordering::SeqCst));

        // 录完、传完了：可以装
        gate.busy(None);
        install(&source, &gate, &installing, |_| {}).await.unwrap();
        assert!(gate.held());
    }

    #[tokio::test]
    async fn a_recording_started_while_downloading_wins() {
        let gate = std::sync::Arc::new(FakeGate::default());
        let mut source = FakeSource::new("0.2.0");
        let started = gate.clone();
        source.while_downloading = Some(Box::new(move || {
            started.busy(Some("正在录音。结束录音、等录好的都传完以后再更新"));
        }));
        let installing = AtomicBool::new(false);
        let err = install(&source, gate.as_ref(), &installing, |_| {})
            .await
            .unwrap_err();
        assert!(err.contains("正在录音"), "{err}");
        assert_eq!(source.steps(), ["check", "download 0.2.0"], "下载完也不装");
        assert!(!gate.held() && !installing.load(Ordering::SeqCst));
    }

    #[tokio::test]
    async fn a_failed_install_can_be_tried_again() {
        let gate = FakeGate::default();
        let installing = AtomicBool::new(false);

        // 已经是最新的了（比如那个版本又撤下来了）
        let source = FakeSource::default();
        let err = install(&source, &gate, &installing, |_| {})
            .await
            .unwrap_err();
        assert_eq!(err, UP_TO_DATE);

        let source = FakeSource {
            download_error: Some("连不上 GitHub"),
            ..FakeSource::new("0.2.0")
        };
        let err = install(&source, &gate, &installing, |_| {})
            .await
            .unwrap_err();
        assert_eq!(err, "连不上 GitHub");
        assert!(!gate.held());

        // 装的时候出错：又可以开始录音了，也可以再点一次
        let source = FakeSource {
            install_error: Some("新版本没装上"),
            ..FakeSource::new("0.2.0")
        };
        let err = install(&source, &gate, &installing, |_| {})
            .await
            .unwrap_err();
        assert_eq!(err, "新版本没装上");
        assert!(!gate.held() && !installing.load(Ordering::SeqCst));
    }

    #[tokio::test]
    async fn check_reports_the_current_and_the_new_version() {
        let found = check(&FakeSource::new("0.2.0"), "0.1.0".into())
            .await
            .unwrap();
        assert_eq!(found.current, "0.1.0");
        assert_eq!(found.available.unwrap().version, "0.2.0");
        let none = check(&FakeSource::default(), "0.1.0".into()).await.unwrap();
        assert_eq!(none.available, None);
    }

    #[test]
    fn progress_is_reported_at_most_every_100_ms() {
        let mut throttle = Throttle::new(Duration::from_millis(100));
        let start = Instant::now();
        assert!(throttle.ready(start));
        assert!(!throttle.ready(start + Duration::from_millis(50)));
        assert!(throttle.ready(start + Duration::from_millis(100)));
        assert!(!throttle.ready(start + Duration::from_millis(150)));
    }
}
