# 第三方代码：WebRTC 音频处理（回声消除用）

录音小程序的回声消除（见 `docs/desktop.md` 的「回声消除」）用的是 WebRTC 的 AEC3。这个目录放着编译它要用到的源码：
从官方发布的源码包里原样挑出来，**没有改过任何一个文件（没有本地补丁）**。

| | 版本 | 下载地址 | SHA-256（核对过） | 许可证 |
|---|---|---|---|---|
| webrtc-audio-processing | 2.1（WebRTC M131 的代码，2025-01-22 发布） | https://freedesktop.org/software/pulseaudio/webrtc-audio-processing/webrtc-audio-processing-2.1.tar.xz | `ae9302824b2038d394f10213cab05312c564a038434269f11dbf68f511f9f9fe`，和官网的 `.sha256sum` 一样 | BSD-3-Clause，外加 Google 的专利授权（`PATENTS`） |
| abseil-cpp | 20240722.1（LTS 20240722 的第 1 个补丁版） | https://github.com/abseil/abseil-cpp/releases/download/20240722.1/abseil-cpp-20240722.1.tar.gz | `40cee67604060a7c8794d931538cb55f4d444073e556980c88b6c49bb9b19bb7`，和 Bazel Central Registry 记录的一样（`sha256-QM7mdgQGCnyHlNkxU4y1X01EQHPlVpgMiLbEm7mxm7c=`） | Apache-2.0 |

webrtc-audio-processing 2.1 要求 abseil 20240722 或更新。用 .1 而不是 .0：.0 的哈希容器有一个安全问题（.1 修了），
WebRTC 用不到哈希容器，那些文件也没挑进来，挑进来的文件两个版本只有 `absl/base/config.h` 里的补丁版本号不一样。

## 目录

- `webrtc-audio-processing/`：源码包里的 `webrtc/`（只留了用到的），加上 `COPYING`、`AUTHORS`、`NEWS`、`README.md`
- `abseil-cpp/`：源码包里的 `absl/`（只留了用到的），加上 `LICENSE`
- `webrtc-sources.txt`：要编译的源文件，一行一个，照 webrtc-audio-processing 的 `meson.build` 列出来，按 macOS 的两种
  架构定好了条件（Apple 芯片加 NEON 的文件；Intel 加 SSE2、AVX2 的文件，AVX2 运行时按 CPU 选），再加上要编译的 13 个
  abseil 的 `.cc`（其余的 abseil 只用到头文件）
- `webrtc-flags.sh`：编译参数（`desktop/scripts/build-recorder.sh`、`desktop/recorder/test.sh`、`prune.sh` 共用）
- `prune.sh`：从解开的源码包重新挑文件（下面「升级」）

## 许可证

都允许商用、和 zsmeet 的 Apache-2.0 兼容。BSD 类的许可证要求发布编译好的程序时带上版权声明，所以仓库根目录的
`THIRD_PARTY_NOTICES` 里附上了下面这些文件的原文，App 打包时带着它（`desktop/src-tauri/tauri.conf.json` 的
`bundle.resources`，在 `zsmeet.app/Contents/Resources/THIRD_PARTY_NOTICES.txt`；发布时 CI 检查它在不在）。

- `webrtc-audio-processing/COPYING`、`webrtc-audio-processing/webrtc/LICENSE`：BSD-3-Clause
- `webrtc-audio-processing/webrtc/PATENTS`：Google 的专利授权
- `webrtc-audio-processing/webrtc/third_party/pffft/LICENSE`：pffft（FFTPACK 的许可证）
- `webrtc-audio-processing/webrtc/third_party/rnnoise/COPYING`：rnnoise（BSD-3-Clause）
- `webrtc-audio-processing/webrtc/common_audio/third_party/ooura/LICENSE`：Ooura FFT
- `webrtc-audio-processing/webrtc/common_audio/third_party/spl_sqrt_floor/LICENSE`：spl_sqrt_floor（公有领域）
- `webrtc-audio-processing/webrtc/modules/third_party/fft/LICENSE`：Mark Olesen 的 FFT
- `abseil-cpp/LICENSE`：Apache-2.0

## 怎么挑的（prune.sh）

1. 把源码包里的 `webrtc/`、`absl/` 整个拷过来。
2. 用 `clang -MM`（只做预处理：不编译，不用 meson，也不运行源码包里的任何脚本）找出 `webrtc-sources.txt` 里的源文件
   和 `Sources/CRecorder/zr_aec.cc` 在 Apple 芯片、Intel 上用到的全部头文件，参数和编译时一样（`webrtc-flags.sh`）。
3. 只留下这些源文件、头文件和上面的许可证文件，别的都删掉（挑完约 5 MB）。

## 编译

`desktop/scripts/build-recorder.sh` 每种架构编成一个静态库 `desktop/recorder/.build/webrtc-<架构>/libwebrtc_apm.a`，
只要 Command Line Tools，不联网。第一次要几分钟（M4 Pro 上两种架构一共约 4 分钟，CI 的机器慢一些）；之后按「两份源码、
`webrtc-flags.sh`、`webrtc-sources.txt` 的内容 + 编译命令 + 编译器、SDK 的版本」算的指纹判断（这里的 `README.md`、
`prune.sh` 不算），没变就直接用，CI 缓存这个目录就行（和文件的修改时间无关；缓存的 key 要包括所有会改变编译结果的文件，
见 `.github/workflows/desktop.yml`）。编到一半被打断了，下次接着编没编完的文件。

## 升级

1. 下载新的源码包，核对 SHA-256（webrtc-audio-processing 的官网有 `.sha256sum`；abseil 看 GitHub Release 或者
   Bazel Central Registry 的 `modules/abseil-cpp/<版本>/source.json`），在一个新建的空目录里解开。
2. 照新版本的 `meson.build` 更新 `webrtc-sources.txt`、`webrtc-flags.sh`。
3. `desktop/recorder/ThirdParty/prune.sh <解开的 webrtc-audio-processing 目录> <解开的 abseil-cpp 目录>`
4. 改这个文件里的版本、地址、SHA-256，`THIRD_PARTY_NOTICES` 里的许可证原文也换成新的。
5. `desktop/recorder/test.sh`：回声消除的测试都要过（消掉了多少 dB、这边说的话保留得怎么样、处理的延迟是不是还是
   `ZR_AEC_LATENCY` 个采样），把测得的数和 `Tests/recorder-tests/EchoQualityTests.swift` 里记的比一比（门槛是按
   2.1 量到的定的）。WebRTC 的 C++ 接口没有稳定的承诺，`Sources/CRecorder/zr_aec.cc` 可能要跟着改。
