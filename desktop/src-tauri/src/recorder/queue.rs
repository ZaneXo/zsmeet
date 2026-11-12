//! 上传队列：后台一直跑的上传任务，加上登录的账号变了以后的处理。
//!
//! 一次做一件事：存标记、按序号传一片、全部传完以后告诉服务器开始处理（请求在 upload.rs 里发）。
//! 只做当前登录的账号（同一台服务器、同一个用户）的录音，用它的令牌；暂时失败的那一场等一会儿再试，
//! 不耽误别的；重试也没用的记下原因，不再传。
//!
//! 传分片时的「成功」不一定是服务器回的（比如中间的设备冒充服务器回了 200 和一段 JSON），所以这台电脑上的分片
//! 传完也先留着：告诉服务器传完之前先问它收到了哪几片，缺的从这台电脑上重传；服务器开始处理以后才删掉整个目录。

use std::collections::BTreeSet;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use serde_json::Value;
use tokio::time::Instant;

use super::local::{self, Status};
use super::upload::{self, Outcome};
use super::{Local, Recorder, NOT_SAVED, RELOGIN};
use crate::core::Account;

/// 上传任务下一步做什么。
enum Next {
    Job(Job),
    /// 没事做：等着被叫醒，或者等到某个时候再试
    Wait(Option<Instant>),
}

enum Job {
    Marks {
        meeting_id: String,
        account: Account,
        marks: Vec<f64>,
    },
    /// 断网时接着录的那部分（序号 from 起）还没和服务器核对过：前面的分片传完了就先问服务器收到了哪几片，
    /// 核对过了才传其中的分片、告诉服务器传完了
    Check {
        meeting_id: String,
        account: Account,
        from: u32,
    },
    Chunk {
        meeting_id: String,
        account: Account,
        seq: u32,
        path: PathBuf,
    },
    Complete {
        meeting_id: String,
        account: Account,
        chunks: u32,
        marks: Vec<f64>,
    },
}

impl Recorder {
    /// 登录的账号变了：马上再试上传（不用等之前的退避时间）；换了别的账号登录的话，
    /// 正在录的是上一个账号的会议，结束它（录好的留在本地，等那个账号登录了再传）。
    pub(super) async fn account_loop(self: Arc<Self>) {
        let mut accounts = self.core.watch_account();
        while accounts.changed().await.is_ok() {
            let account = accounts.borrow_and_update().clone();
            // 先让上一个账号的录音结束，再告诉界面：新登录的人只看到「正在结束录音」
            let someone_else = {
                let mut inner = self.inner.lock().unwrap();
                for l in inner.recordings.values_mut() {
                    l.ready();
                }
                match (&account, inner.session.as_mut()) {
                    (Some(a), Some(s)) if !s.belongs_to(a) => Some(s.request_stop()),
                    _ => None,
                }
            };
            self.wake.notify_one();
            if someone_else.is_some() {
                self.session_changed();
            }
            self.changed();
            if let Some(id) = someone_else {
                self.wait_ended(id, self.opts.stop_timeout).await;
            }
        }
    }

    /// 上传任务：一次做一件事（存标记、传一片、告诉服务器传完了），没事做就等着被叫醒。
    pub(super) async fn upload_loop(self: Arc<Self>) {
        loop {
            match self.next_job() {
                Next::Job(job) => self.run_job(job).await,
                Next::Wait(until) => {
                    let wait = async move {
                        match until {
                            Some(t) => tokio::time::sleep_until(t).await,
                            None => std::future::pending().await,
                        }
                    };
                    tokio::select! {
                        _ = self.wake.notified() => {}
                        _ = wait => {}
                    }
                }
            }
        }
    }

    /// 挑下一件事：当前账号的录音，按开始录音的时间，先存标记、再按序号传分片、最后告诉服务器传完了。
    /// 刚失败过、还没到再试时间的跳过。
    fn next_job(&self) -> Next {
        let Some(account) = self.core.account() else {
            return Next::Wait(None);
        };
        let now = Instant::now();
        let inner = self.inner.lock().unwrap();
        let mut mine: Vec<&Local> = inner
            .recordings
            .values()
            .filter(|l| l.rec.belongs_to(&account.server, &account.user_id))
            .filter(|l| {
                !l.fatal
                    && matches!(
                        l.rec.status,
                        Status::Recording | Status::Stopped | Status::Interrupted
                    )
            })
            .collect();
        mine.sort_by_key(|l| l.rec.started_at);
        let mut earliest: Option<Instant> = None;
        for l in mine {
            if let Some(at) = l.retry_at.filter(|at| *at > now) {
                earliest = Some(earliest.map_or(at, |e| e.min(at)));
                continue;
            }
            let meeting_id = l.rec.meeting_id.clone();
            let account = account.clone();
            if l.marks_dirty {
                return Next::Job(Job::Marks {
                    meeting_id,
                    account,
                    marks: l.rec.marks.clone(),
                });
            }
            // 断网时接着录的那部分（from 起）还没和服务器核对过：前面的传完了就先核对，再传其中的分片、告诉服务器
            // 传完了。服务器上已经有的序号不能覆盖：核对过之前其中的序号一片也不传，服务器上有的就都不是这场录音传的
            if let Some(from) = l.rec.unverified_from.filter(|from| l.rec.uploaded >= *from) {
                return Next::Job(Job::Check {
                    meeting_id,
                    account,
                    from,
                });
            }
            if l.rec.uploaded < l.rec.next_seq {
                return Next::Job(Job::Chunk {
                    meeting_id,
                    account,
                    seq: l.rec.uploaded,
                    path: local::chunk_path(&l.dir, l.rec.uploaded),
                });
            }
            if l.rec.status == Status::Stopped {
                return Next::Job(Job::Complete {
                    meeting_id,
                    account,
                    chunks: l.rec.next_seq,
                    marks: l.rec.marks.clone(),
                });
            }
        }
        Next::Wait(earliest)
    }

    async fn run_job(&self, job: Job) {
        match job {
            Job::Marks {
                meeting_id,
                account,
                marks,
            } => {
                let outcome = upload::put_marks(&self.client, &account, &meeting_id, &marks).await;
                self.marks_done(&meeting_id, &account, &marks, outcome);
            }
            Job::Check {
                meeting_id,
                account,
                from,
            } => {
                let received =
                    upload::received(&self.client, &account, &meeting_id, upload::REQUEST_TIMEOUT)
                        .await;
                self.check_done(&meeting_id, &account, from, received);
            }
            Job::Chunk {
                meeting_id,
                account,
                seq,
                path,
            } => {
                let outcome = self.send_chunk(&meeting_id, &account, seq, &path).await;
                self.chunk_done(&meeting_id, &account, seq, outcome);
            }
            Job::Complete {
                meeting_id,
                account,
                chunks,
                marks,
            } => {
                // 先核对服务器真的收到了第 0 到 chunks-1 片：传分片时的「成功」不一定是服务器回的。
                // 缺的从这台电脑上还留着的分片重传，都收到了才告诉服务器传完了
                let received = match upload::received(
                    &self.client,
                    &account,
                    &meeting_id,
                    upload::REQUEST_TIMEOUT,
                )
                .await
                {
                    Ok(received) => received.into_iter().collect::<BTreeSet<u32>>(),
                    Err(outcome) => return self.complete_done(&meeting_id, &account, outcome),
                };
                if let Some(missing) = (0..chunks).find(|seq| !received.contains(seq)) {
                    // 服务器开始处理以后，拼好整段录音就删掉分片：上一次其实已经成功、只是没收到回复的话，
                    // 这时也查不到分片。先看一眼会议，已经不在上传状态就算成功；还在等上传才是真缺了，重传
                    let outcome = match upload::meeting(
                        &self.client,
                        &account,
                        &meeting_id,
                        upload::REQUEST_TIMEOUT,
                    )
                    .await
                    {
                        Ok(meeting) if upload::processing_started(&meeting, &meeting_id) => {
                            Outcome::Done(meeting)
                        }
                        Ok(_) => return self.resend(&meeting_id, chunks, missing),
                        Err(outcome) => outcome,
                    };
                    return self.complete_done(&meeting_id, &account, outcome);
                }
                let mut outcome =
                    upload::complete(&self.client, &account, &meeting_id, chunks, &marks).await;
                if let Outcome::Conflict(reason) = &outcome {
                    // 上一次其实已经成功、只是没收到回复的话，再提交会得到 409（「已经在处理了」）：
                    // 看一眼会议，已经不在上传状态就算成功
                    outcome = match upload::meeting(
                        &self.client,
                        &account,
                        &meeting_id,
                        upload::REQUEST_TIMEOUT,
                    )
                    .await
                    {
                        Ok(meeting) if upload::processing_started(&meeting, &meeting_id) => {
                            Outcome::Done(meeting)
                        }
                        Ok(_) => Outcome::Fatal(reason.clone()),
                        Err(Outcome::Conflict(reason)) => Outcome::Fatal(reason),
                        Err(other) => other,
                    };
                }
                self.complete_done(&meeting_id, &account, outcome);
            }
        }
    }

    /// 传一片。这台电脑上没有这一片的文件时（以前的版本传上去就删了）问一下服务器：服务器已经收到了就算传过了。
    async fn send_chunk(
        &self,
        meeting_id: &str,
        account: &Account,
        seq: u32,
        path: &Path,
    ) -> Outcome {
        match tokio::fs::read(path).await {
            // 录音程序不会写出空的分片，服务器也不收：文件坏了
            Ok(body) if body.is_empty() => Outcome::Fatal(format!(
                "这台电脑上存的第 {} 片录音是空的，传不上去",
                seq + 1
            )),
            Ok(body) => upload::put_chunk(&self.client, account, meeting_id, seq, body).await,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                match upload::received(&self.client, account, meeting_id, upload::REQUEST_TIMEOUT)
                    .await
                {
                    Ok(received) if received.contains(&seq) => Outcome::Done(Value::Null),
                    Ok(_) => Outcome::Fatal(format!(
                        "第 {} 片录音在这台电脑上找不到了，传不上去",
                        seq + 1
                    )),
                    Err(other) => other,
                }
            }
            Err(e) => Outcome::Fatal(format!("读不出这台电脑上存的第 {} 片录音（{e}）", seq + 1)),
        }
    }

    fn chunk_done(&self, meeting_id: &str, account: &Account, seq: u32, outcome: Outcome) {
        if let Outcome::Done(_) = outcome {
            let mut inner = self.inner.lock().unwrap();
            if let Some(l) = inner.recordings.get_mut(meeting_id) {
                if l.rec.uploaded == seq {
                    // 记下传到了哪一片（中间 App 退出了，最多重传这一片）。这台电脑上的分片先不删：这次的「成功」
                    // 不一定是服务器回的，告诉服务器传完之前还要核对，服务器开始处理以后整个目录一起删
                    l.rec.uploaded = seq + 1;
                    l.save();
                }
                l.ready();
            }
        } else {
            self.trouble(meeting_id, account, outcome);
        }
        self.changed();
    }

    /// 核对完断网时接着录的序号（from 起）：服务器上一片都没有的话记下核对过了，接着传；
    /// 已经有了的话不能覆盖，这场录音算传不上去了（录下的都留在这台电脑上）。
    fn check_done(
        &self,
        meeting_id: &str,
        account: &Account,
        from: u32,
        received: Result<Vec<u32>, Outcome>,
    ) {
        let received = match received {
            Ok(received) => received,
            Err(outcome) => {
                self.trouble(meeting_id, account, outcome);
                self.changed();
                return;
            }
        };
        {
            let mut inner = self.inner.lock().unwrap();
            if let Some(l) = inner
                .recordings
                .get_mut(meeting_id)
                .filter(|l| l.rec.unverified_from == Some(from))
            {
                if let Some(reason) = l.taken_on_server(&received) {
                    l.fail(reason);
                } else {
                    l.rec.unverified_from = None;
                    // 记下了才能接着传：没记下就传了的话，下次打开 App 再核对时，这台电脑传上去的那几片
                    // 会被当成服务器上本来就有的
                    if l.save() {
                        l.ready();
                    } else {
                        l.rec.unverified_from = Some(from);
                        l.backoff(NOT_SAVED.into(), &self.opts.retry_waits);
                    }
                }
            }
        }
        self.changed();
    }

    /// 告诉服务器传完之前发现它缺第 seq 片（之前那次「成功」不是服务器回的）：从这一片起重传。
    fn resend(&self, meeting_id: &str, chunks: u32, seq: u32) {
        {
            let mut inner = self.inner.lock().unwrap();
            // 核对的时候没变过（还是在等告诉服务器传完了）才改
            if let Some(l) = inner.recordings.get_mut(meeting_id).filter(|l| {
                l.rec.status == Status::Stopped
                    && l.rec.next_seq == chunks
                    && l.rec.uploaded == chunks
            }) {
                l.resend_from(seq, &self.opts.retry_waits);
            }
        }
        self.changed();
    }

    fn marks_done(&self, meeting_id: &str, account: &Account, sent: &[f64], outcome: Outcome) {
        match outcome {
            // 存不进去（会议在别处结束了、被删了）也不再试：结束录音时会和 complete 一起提交，
            // 会议真有问题的话传分片时会发现
            Outcome::Done(_) | Outcome::Conflict(_) | Outcome::Fatal(_) => {
                let mut inner = self.inner.lock().unwrap();
                if let Some(l) = inner.recordings.get_mut(meeting_id) {
                    if l.rec.marks == sent {
                        l.marks_dirty = false;
                    }
                    if matches!(outcome, Outcome::Done(_)) {
                        l.ready();
                    }
                }
            }
            other => self.trouble(meeting_id, account, other),
        }
        self.changed();
    }

    fn complete_done(&self, meeting_id: &str, account: &Account, outcome: Outcome) {
        if let Outcome::Done(meeting) = &outcome {
            let mut inner = self.inner.lock().unwrap();
            if let Some(l) = inner.recordings.get_mut(meeting_id) {
                l.rec.status = Status::Completed;
                l.rec.error = None;
                l.ready();
                // 开始录音时没连上服务器的，到这时才知道会议的标题
                if let Some(title) = meeting
                    .get("title")
                    .and_then(Value::as_str)
                    .filter(|t| !t.is_empty())
                {
                    l.rec.title = title.to_string();
                }
                // 先记下已经完成（万一删目录时 App 退出了，下次打开直接删掉），再删整个目录
                l.save();
                if let Err(e) = std::fs::remove_dir_all(&l.dir) {
                    log::warn!("{} 没能删掉：{e}", l.dir.display());
                }
            }
        } else {
            self.trouble(meeting_id, account, outcome);
        }
        self.changed();
    }

    /// 上传没成功：等一会儿再试，或者记下传不上去了。
    fn trouble(&self, meeting_id: &str, account: &Account, outcome: Outcome) {
        if outcome == Outcome::Unauthorized {
            // 只有出问题的还是当前的令牌时才会退出登录。退出以后上传任务等着，同一个账号重新登录了接着传
            self.core.unauthorized(&account.token);
        }
        let mut inner = self.inner.lock().unwrap();
        let Some(l) = inner.recordings.get_mut(meeting_id) else {
            return;
        };
        match outcome {
            Outcome::Retry(reason) => l.backoff(reason, &self.opts.retry_waits),
            Outcome::Unauthorized => l.backoff(RELOGIN.into(), &self.opts.retry_waits),
            Outcome::Conflict(reason) | Outcome::Fatal(reason) => l.fail(reason),
            Outcome::Done(_) => {}
        }
    }
}
