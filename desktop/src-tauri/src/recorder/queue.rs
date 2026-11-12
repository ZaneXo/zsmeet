//! 上传队列：后台一直跑的上传任务，加上登录的账号变了以后的处理。
//!
//! 一次做一件事：存标记、按序号传一片、全部传完以后告诉服务器开始处理（请求在 upload.rs 里发）。
//! 只做当前登录的账号（同一台服务器、同一个用户）的录音，用它的令牌；暂时失败的那一场等一会儿再试，
//! 不耽误别的；重试也没用的记下原因，不再传。

use std::path::{Path, PathBuf};
use std::sync::Arc;

use serde_json::Value;
use tokio::time::Instant;

use super::local::{self, Status};
use super::upload::{self, Outcome};
use super::{Local, Recorder, RELOGIN};
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
                        Ok(meeting) => match meeting.get("status").and_then(Value::as_str) {
                            Some(status) if status != "uploading" => Outcome::Done(meeting),
                            _ => Outcome::Fatal(reason.clone()),
                        },
                        Err(Outcome::Conflict(reason)) => Outcome::Fatal(reason),
                        Err(other) => other,
                    };
                }
                self.complete_done(&meeting_id, &account, outcome);
            }
        }
    }

    /// 传一片。这台电脑上没有这一片的文件时问一下服务器：服务器已经收到了（上次传完还没记下来）就算传过了。
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
                    l.rec.uploaded = seq + 1;
                    // 先记下传到了哪一片，记下了再删文件：中间 App 退出了，最多重传这一片。
                    // 没记下（磁盘满了）就先留着文件：下次打开 App 时照着文件就知道录到了哪一片，
                    // 不会把已经传上去的序号当成没录过、接着录时覆盖掉
                    if l.save() {
                        let _ = std::fs::remove_file(local::chunk_path(&l.dir, seq));
                    }
                }
                l.ready();
            }
        } else {
            self.trouble(meeting_id, account, outcome);
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
