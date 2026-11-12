//! 一场录音在这台电脑上的样子：`<录音目录>/<会议 id>/` 下面一个 recording.json，加上录好的分片 `<序号>.aac`。
//!
//! 录音程序先写 `<序号>.aac.part`，写完、落盘以后改名成 `<序号>.aac` 再报告，所以 `.aac` 都是完整的。
//! 分片传上去以后就删掉；全部传完、服务器开始处理以后整个目录删掉。
//! 录音目录只有自己能进（0700）：同一台 Mac 上的其他用户看不到别人的会议录音。

use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};

pub const RECORDING_FILE: &str = "recording.json";

/// 一场录音走到哪一步了。
#[derive(Serialize, Deserialize, Clone, Copy, Debug, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum Status {
    /// 正在录（App 重启时还是这个状态，说明录音程序已经不在了，算中断）
    Recording,
    /// 录完了，等分片传完再告诉服务器开始处理
    Stopped,
    /// 录到一半录音程序或者 App 意外退出了：可以接着录，也可以直接结束上传
    Interrupted,
    /// 服务器已经开始处理
    Completed,
    /// 传不上去了（会议被删了、没有权限……），重试也没用
    Failed,
}

/// recording.json 的内容。
#[derive(Serialize, Deserialize, Clone, Debug, PartialEq)]
pub struct Recording {
    pub meeting_id: String,
    /// 录音属于哪台服务器上的哪个用户：只用这个账号的令牌上传
    pub server: String,
    pub user_id: String,
    #[serde(default)]
    pub title: String,
    /// 开始录音的时间（Unix 毫秒）
    #[serde(default)]
    pub started_at: u64,
    /// 下一片的序号，也就是已经录好了多少片
    #[serde(default)]
    pub next_seq: u32,
    /// 已经传上去了多少片（从第 0 片起连续的）
    #[serde(default)]
    pub uploaded: u32,
    /// 标记的重点：录音开始后的秒数，不算暂停的时间
    #[serde(default)]
    pub marks: Vec<f64>,
    /// 已经录好的分片一共多少秒
    #[serde(default)]
    pub seconds: f64,
    pub status: Status,
    /// 中断、传不上去的原因，给用户看
    #[serde(default)]
    pub error: Option<String>,
}

impl Recording {
    /// 是不是这台服务器上这个用户的录音。
    pub fn belongs_to(&self, server: &str, user_id: &str) -> bool {
        self.server == server && self.user_id == user_id
    }
}

/// 会议 id 要当目录名、拼进接口路径：只能是字母、数字、「-」「_」（服务器的会议 id 是 UUID）。
pub fn valid_meeting_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 64
        && id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
}

pub fn chunk_path(dir: &Path, seq: u32) -> PathBuf {
    dir.join(format!("{seq}.aac"))
}

pub fn now_millis() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

/// 建一个只有自己能进的目录（录音是这个产品里最敏感的数据）。
pub fn create_private_dir(dir: &Path) -> io::Result<()> {
    fs::create_dir_all(dir)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(dir, fs::Permissions::from_mode(0o700))?;
    }
    Ok(())
}

/// 写 recording.json：先写临时文件再改名，读的时候不会读到写了一半的。
pub fn save(dir: &Path, rec: &Recording) -> io::Result<()> {
    let tmp = dir.join(format!("{RECORDING_FILE}.tmp"));
    fs::write(&tmp, serde_json::to_vec_pretty(rec)?)?;
    fs::rename(tmp, dir.join(RECORDING_FILE))
}

pub fn load(dir: &Path) -> io::Result<Recording> {
    let data = fs::read(dir.join(RECORDING_FILE))?;
    serde_json::from_slice(&data).map_err(io::Error::other)
}

/// 删掉没写完的分片（`.part`）。只在录音程序没在运行时调用：接着录时它会从这一片重新写。
pub fn remove_partials(dir: &Path) {
    let Ok(entries) = fs::read_dir(dir) else {
        return;
    };
    for entry in entries.flatten() {
        let name = entry.file_name();
        if name.to_string_lossy().ends_with(".part") {
            let _ = fs::remove_file(entry.path());
        }
    }
}

/// 录音程序写好了分片、还没来得及记进 recording.json，App 就退出了（崩溃、断电）：
/// 从 next_seq 起往后找连续的、写完的分片补记上，时长从分片本身算。补记了返回 true。
pub fn recover(dir: &Path, rec: &mut Recording) -> bool {
    let mut changed = false;
    while let Ok(data) = fs::read(chunk_path(dir, rec.next_seq)) {
        rec.seconds += adts_seconds(&data);
        rec.next_seq += 1;
        changed = true;
    }
    changed
}

/// 从文件名认出分片序号："12.aac" 是 12；recording.json、没写完的 "12.aac.part" 都不是。
fn chunk_seq(name: &str) -> Option<u32> {
    let digits = name.strip_suffix(".aac")?;
    if digits.is_empty() || !digits.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    digits.parse().ok()
}

/// 目录里写好的分片中最大的序号，一片都没有时是 None。
pub fn highest_chunk(dir: &Path) -> Option<u32> {
    fs::read_dir(dir)
        .ok()?
        .flatten()
        .filter_map(|e| chunk_seq(&e.file_name().to_string_lossy()))
        .max()
}

/// 接着录一场中断了的录音时，录音程序从第几片开始写：recording.json 记的下一片、这台电脑上已有的分片之后、
/// 服务器已经收到的分片（received）之后，三者取最大。接着录的声音不能和已有的哪一片用同一个序号：
/// 目录里有序号更大的分片时录音程序会拒绝开始；服务器上的那一片会被悄悄换成另一段声音。
pub fn resume_seq(dir: &Path, rec: &Recording, received: &[u32]) -> u32 {
    let after = |seq: Option<u32>| seq.map_or(0, |n| n + 1);
    rec.next_seq
        .max(after(highest_chunk(dir)))
        .max(after(received.iter().copied().max()))
}

/// AAC 的采样率表（ADTS 头里存的是下标）。
const SAMPLE_RATES: [u32; 13] = [
    96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350,
];

/// 一段 ADTS 格式的 AAC 有多少秒：每一帧有 1024 个采样，按帧头里的采样率算。读到不认识的数据就停。
/// 采样数用整数累加，最后再除，免得几百帧加下来有零头。
pub fn adts_seconds(data: &[u8]) -> f64 {
    let mut seconds = 0.0;
    // 同一个采样率下累计的采样数
    let (mut samples, mut rate) = (0u64, 0u32);
    let mut i = 0;
    while i + 7 <= data.len() {
        let h = &data[i..i + 7];
        // 同步字 0xFFF，layer 是 0
        if h[0] != 0xFF || h[1] & 0xF6 != 0xF0 {
            break;
        }
        let Some(&frame_rate) = SAMPLE_RATES.get(usize::from((h[2] & 0x3C) >> 2)) else {
            break;
        };
        let length =
            (usize::from(h[3] & 0x03) << 11) | (usize::from(h[4]) << 3) | usize::from(h[5] >> 5);
        if length < 7 {
            break;
        }
        if frame_rate != rate {
            if rate > 0 {
                seconds += samples as f64 / f64::from(rate);
            }
            (samples, rate) = (0, frame_rate);
        }
        samples += (u64::from(h[6] & 0x03) + 1) * 1024;
        i += length;
    }
    if rate > 0 {
        seconds += samples as f64 / f64::from(rate);
    }
    seconds
}

#[cfg(test)]
pub mod tests {
    use super::*;

    /// 一帧 ADTS（48 kHz、单声道、AAC-LC），帧长 length 字节（含 7 字节的头），内容是 0。
    pub fn adts_frame(length: usize) -> Vec<u8> {
        let mut f = vec![0u8; length];
        f[0] = 0xFF;
        f[1] = 0xF1; // MPEG-4，没有 CRC
        f[2] = (1 << 6) | (3 << 2); // AAC-LC，48 kHz
        f[3] = (1 << 6) | (((length >> 11) as u8) & 0x03); // 单声道
        f[4] = (length >> 3) as u8;
        f[5] = (((length & 0x07) as u8) << 5) | 0x1F;
        f[6] = 0xFC;
        f
    }

    /// n 帧 ADTS 连在一起。
    pub fn adts(frames: usize) -> Vec<u8> {
        (0..frames).flat_map(|_| adts_frame(200)).collect()
    }

    #[test]
    fn counts_adts_duration() {
        assert_eq!(adts_seconds(&[]), 0.0);
        // 48 kHz 下 375 帧正好 8 秒，一点零头都没有
        assert_eq!(adts_seconds(&adts(375)), 8.0);
        // 后面跟着不认识的数据：只算前面的
        let mut data = adts(75);
        data.extend_from_slice(b"garbage");
        assert_eq!(adts_seconds(&data), 1.6);
        assert_eq!(adts_seconds(b"not aac at all"), 0.0);
    }

    #[test]
    fn checks_meeting_ids() {
        assert!(valid_meeting_id("0b5e2c1a-7d3f-4c1e-9a55-2f0d6c1b9e77"));
        assert!(valid_meeting_id("m_1"));
        for bad in ["", "..", "a/b", "a\\b", "a.b", "a b", &"x".repeat(65)] {
            assert!(!valid_meeting_id(bad), "{bad}");
        }
    }

    fn sample(status: Status) -> Recording {
        Recording {
            meeting_id: "m1".into(),
            server: "https://meet.example.com".into(),
            user_id: "u1".into(),
            title: "周会".into(),
            started_at: 1,
            next_seq: 1,
            uploaded: 0,
            marks: vec![3.5],
            seconds: 10.0,
            status,
            error: None,
        }
    }

    #[test]
    fn saves_loads_and_recovers() {
        let tmp = tempfile::tempdir().unwrap();
        let dir = tmp.path().join("m1");
        create_private_dir(&dir).unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mode = fs::metadata(&dir).unwrap().permissions().mode();
            assert_eq!(mode & 0o777, 0o700);
        }
        let mut rec = sample(Status::Recording);
        save(&dir, &rec).unwrap();
        assert_eq!(load(&dir).unwrap(), rec);
        let json: serde_json::Value =
            serde_json::from_slice(&fs::read(dir.join(RECORDING_FILE)).unwrap()).unwrap();
        assert_eq!(json["status"], "recording");
        assert_eq!(json["next_seq"], 1);

        // 第 1、2 片写好了但没记上；第 4 片前面缺了第 3 片，不算；.part 是没写完的
        fs::write(chunk_path(&dir, 1), adts(375)).unwrap();
        fs::write(chunk_path(&dir, 2), adts(75)).unwrap();
        fs::write(chunk_path(&dir, 4), adts(75)).unwrap();
        fs::write(dir.join("3.aac.part"), adts(10)).unwrap();
        remove_partials(&dir);
        assert!(!dir.join("3.aac.part").exists());
        assert!(recover(&dir, &mut rec));
        assert_eq!(rec.next_seq, 3);
        assert!((rec.seconds - 19.6).abs() < 1e-9, "{}", rec.seconds);
        assert!(!recover(&dir, &mut rec));
    }

    #[test]
    fn resumes_after_every_chunk_written_or_received() {
        let tmp = tempfile::tempdir().unwrap();
        let dir = tmp.path().join("m1");
        create_private_dir(&dir).unwrap();
        let mut rec = sample(Status::Interrupted);
        rec.next_seq = 2;
        save(&dir, &rec).unwrap();
        assert_eq!(highest_chunk(&dir), None, "recording.json 不是分片");
        assert_eq!(resume_seq(&dir, &rec, &[]), 2);

        // 没写完的 .part 不算；第 1 片在
        fs::write(dir.join("7.aac.part"), b"half").unwrap();
        fs::write(chunk_path(&dir, 1), adts(1)).unwrap();
        assert_eq!(highest_chunk(&dir), Some(1));
        assert_eq!(resume_seq(&dir, &rec, &[0, 1]), 2);

        // 目录里有更靠后的分片（中间缺了几片）：从它后面接着写，录音程序才肯开始
        fs::write(chunk_path(&dir, 4), adts(1)).unwrap();
        assert_eq!(resume_seq(&dir, &rec, &[]), 5);
        // 服务器收到的比这台电脑记的还多：也不能覆盖服务器上的
        assert_eq!(resume_seq(&dir, &rec, &[0, 1, 2, 3, 4, 5, 6]), 7);

        for name in ["x.aac", ".aac", "1.aac.part", "1a.aac", "recording.json"] {
            assert_eq!(chunk_seq(name), None, "{name}");
        }
        assert_eq!(chunk_seq("12.aac"), Some(12));
    }
}
