// 回声消除的 C 接口（见 include/zr_aec.h）：用 WebRTC 的 AudioProcessing 做回声消除，只开 AEC3
// 和它要求的高通滤波。降噪、自动增益都关着，这里除了去掉回声不做别的改动；麦克风的音量由后面单独的一步调
// （zr_agc.cc，另一个只开 AGC2 的 AudioProcessing）。
//
// WebRTC 的 RTC_CHECK 没通过会 abort 整个进程（录音就断了），所以这里先把参数都查好，
// 每个调用都接住 C++ 异常，返回错误码，由 Swift 那边改成直接用原来的麦克风声音。
#include "zr_aec.h"

#include <cmath>
#include <cstdio>
#include <exception>
#include <limits>
#include <memory>
#include <mutex>
#include <new>
#include <optional>

#include "api/audio/audio_processing.h"
#include "api/audio/echo_canceller3_config.h"
#include "api/audio/echo_control.h"
#include "api/make_ref_counted.h"
#include "api/scoped_refptr.h"
#include "modules/audio_processing/aec3/echo_canceller3.h"
#include "modules/audio_processing/residual_echo_detector.h"
#include "system_wrappers/include/field_trial.h"

#ifndef ZR_AEC_TRANSPARENT_HMM
#define ZR_AEC_TRANSPARENT_HMM 1
#endif

namespace {

constexpr int kSampleRate = ZR_AEC_SAMPLE_RATE;
constexpr size_t kFrame = ZR_AEC_FRAME;
constexpr int kMaxFilters = 16;
constexpr int kMaxStreamDelayMs = 500;

static_assert(kFrame * 100 == static_cast<size_t>(kSampleRate), "一帧是 10 毫秒");

// AEC3 的「透明模式」：一直找不到回声（戴着耳机、会议软件的声音其实从别的设备出来）时不再压低麦克风。
// 默认的那种要十几秒才认出来，这期间对方一说话，这边的声音就被压下去；WebRTC 的试验开关
// WebRTC-Aec3TransparentModeHmm 换成按隐马尔可夫模型判断，合成的测试里几秒就认出来，有回声时消得一样好
// （见 Tests/recorder-tests/EchoQualityTests.swift）。试验开关对整个进程有效，建第一个回声消除器之前设一次
void init_field_trials() {
  static std::once_flag once;
  std::call_once(once, [] {
    if (ZR_AEC_TRANSPARENT_HMM) {
      // 字符串要一直有效（WebRTC 只存指针）
      webrtc::field_trial::InitFieldTrialsFromString("WebRTC-Aec3TransparentModeHmm/Enabled/");
    }
  });
}

// AudioProcessing 自己建的 AEC3 用默认配置。这里要改 num_filters（能找的延迟更长），
// 2.1 又没带 echo_canceller3_factory.h，就自己写一个工厂
class Aec3Factory final : public webrtc::EchoControlFactory {
 public:
  explicit Aec3Factory(const webrtc::EchoCanceller3Config& config) : config_(config) {}

  std::unique_ptr<webrtc::EchoControl> Create(int sample_rate_hz,
                                              int num_render_channels,
                                              int num_capture_channels) override {
    return std::make_unique<webrtc::EchoCanceller3>(config_, std::nullopt, sample_rate_hz,
                                                    static_cast<size_t>(num_render_channels),
                                                    static_cast<size_t>(num_capture_channels));
  }

 private:
  webrtc::EchoCanceller3Config config_;
};

bool valid_config(const zr_aec_config& config) {
  return config.num_filters >= 1 && config.num_filters <= kMaxFilters && config.stream_delay_ms >= -1 &&
         config.stream_delay_ms <= kMaxStreamDelayMs;
}

webrtc::StreamConfig mono() {
  return webrtc::StreamConfig(kSampleRate, 1);
}

// 建一个只开 AEC3 的 AudioProcessing，并按 48 kHz 单声道初始化好（之后的调用不会再重新初始化）
rtc::scoped_refptr<webrtc::AudioProcessing> make_apm(const zr_aec_config& config) {
  webrtc::EchoCanceller3Config aec;
  aec.delay.num_filters = static_cast<size_t>(config.num_filters);
  // 参考比麦克风提前多少由 Swift 那边对齐时决定（EchoStage 的提前量），这里不再额外推迟麦克风
  aec.delay.fixed_capture_delay_samples = 0;
  webrtc::EchoCanceller3Config::Validate(&aec);

  webrtc::AudioProcessing::Config apm_config;
  apm_config.echo_canceller.enabled = true;
  apm_config.echo_canceller.mobile_mode = false;
  apm_config.high_pass_filter.enabled = true;
  apm_config.noise_suppression.enabled = false;
  apm_config.transient_suppression.enabled = false;
  apm_config.gain_controller1.enabled = false;
  apm_config.gain_controller2.enabled = false;
  apm_config.pre_amplifier.enabled = false;
  apm_config.capture_level_adjustment.enabled = false;

  rtc::scoped_refptr<webrtc::AudioProcessing> apm =
      webrtc::AudioProcessingBuilder()
          .SetConfig(apm_config)
          .SetEchoControlFactory(std::make_unique<Aec3Factory>(aec))
          // 只用来报「处理后还有回声的可能性」，写日志用
          .SetEchoDetector(rtc::make_ref_counted<webrtc::ResidualEchoDetector>())
          .Create();
  if (!apm) {
    return nullptr;
  }
  webrtc::ProcessingConfig formats = {{mono(), mono(), mono(), mono()}};
  if (apm->Initialize(formats) != webrtc::AudioProcessing::kNoError) {
    return nullptr;
  }
  return apm;
}

// 送进 WebRTC 之前：不是有限的数（NaN、无穷大）换成 0，超出 -1…1 的削到范围里。
// NaN 进了自适应滤波器会一直留在里面，之后出来的全是 NaN
void sanitize(const float* in, float* out) {
  for (size_t i = 0; i < kFrame; i++) {
    float x = in[i];
    if (!std::isfinite(x)) {
      x = 0.0f;
    }
    out[i] = x > 1.0f ? 1.0f : (x < -1.0f ? -1.0f : x);
  }
}

}  // namespace

struct zr_aec {
  zr_aec_config config;
  rtc::scoped_refptr<webrtc::AudioProcessing> apm;
  float input[kFrame];
  float output[kFrame];
  // AudioProcessing 的统计放在一个只能放一份的队列里：没人取的时候，后来的统计都被丢掉，过一会儿再取
  // 拿到的是很久以前的。所以每处理一帧就取一次，存在这里（取一次只是加锁、拷一个结构体）
  webrtc::AudioProcessingStats latest;
};

extern "C" zr_aec* zr_aec_create(const zr_aec_config* config) {
  if (config == nullptr || !valid_config(*config)) {
    std::fprintf(stderr, "zsmeet-recorder: 回声消除的配置不对，不做回声消除\n");
    return nullptr;
  }
  try {
    init_field_trials();
    std::unique_ptr<zr_aec> aec(new zr_aec());
    aec->config = *config;
    aec->apm = make_apm(*config);
    if (!aec->apm) {
      std::fprintf(stderr, "zsmeet-recorder: 建不了 WebRTC 的 AudioProcessing，不做回声消除\n");
      return nullptr;
    }
    return aec.release();
  } catch (const std::exception& error) {
    std::fprintf(stderr, "zsmeet-recorder: 建回声消除器时出错：%s\n", error.what());
  } catch (...) {
    std::fprintf(stderr, "zsmeet-recorder: 建回声消除器时出错\n");
  }
  return nullptr;
}

extern "C" void zr_aec_destroy(zr_aec* aec) {
  try {
    delete aec;
  } catch (...) {
  }
}

extern "C" int zr_aec_render(zr_aec* aec, const float* frame, size_t count) {
  if (aec == nullptr || frame == nullptr || count != kFrame || !aec->apm) {
    return ZR_AEC_BAD_ARGUMENT;
  }
  try {
    sanitize(frame, aec->input);
    const float* channels[1] = {aec->input};
    if (aec->apm->AnalyzeReverseStream(channels, mono()) != webrtc::AudioProcessing::kNoError) {
      return ZR_AEC_FAILED;
    }
    return ZR_AEC_OK;
  } catch (...) {
    return ZR_AEC_EXCEPTION;
  }
}

extern "C" int zr_aec_capture(zr_aec* aec, const float* input, float* output, size_t count) {
  if (aec == nullptr || input == nullptr || output == nullptr || count != kFrame || !aec->apm) {
    return ZR_AEC_BAD_ARGUMENT;
  }
  // 先拷一份（input 和 output 可以是同一块内存），出错时把它原样交回去
  sanitize(input, aec->input);
  int result = ZR_AEC_OK;
  try {
    if (aec->config.stream_delay_ms >= 0) {
      // 超出 0…500 时它只是返回警告并截到范围里，参数已经查过了
      aec->apm->set_stream_delay_ms(aec->config.stream_delay_ms);
    }
    const float* source[1] = {aec->input};
    float* destination[1] = {aec->output};
    if (aec->apm->ProcessStream(source, mono(), mono(), destination) != webrtc::AudioProcessing::kNoError) {
      result = ZR_AEC_FAILED;
    } else {
      for (size_t i = 0; i < kFrame; i++) {
        if (!std::isfinite(aec->output[i])) {
          result = ZR_AEC_FAILED;
          break;
        }
      }
      aec->latest = aec->apm->GetStatistics();
    }
  } catch (...) {
    result = ZR_AEC_EXCEPTION;
  }
  const float* chosen = result == ZR_AEC_OK ? aec->output : aec->input;
  for (size_t i = 0; i < kFrame; i++) {
    output[i] = chosen[i];
  }
  return result;
}

extern "C" int zr_aec_reset(zr_aec* aec) {
  if (aec == nullptr || !aec->apm) {
    return ZR_AEC_BAD_ARGUMENT;
  }
  try {
    rtc::scoped_refptr<webrtc::AudioProcessing> fresh = make_apm(aec->config);
    if (!fresh) {
      return ZR_AEC_FAILED;
    }
    aec->apm = fresh;
    aec->latest = webrtc::AudioProcessingStats();
    return ZR_AEC_OK;
  } catch (...) {
    return ZR_AEC_EXCEPTION;
  }
}

extern "C" int zr_aec_get_stats(zr_aec* aec, zr_aec_stats* stats) {
  if (aec == nullptr || stats == nullptr || !aec->apm) {
    return ZR_AEC_BAD_ARGUMENT;
  }
  const double none = std::numeric_limits<double>::quiet_NaN();
  stats->delay_ms = none;
  stats->erle_db = none;
  stats->erl_db = none;
  stats->residual_echo_likelihood = none;
  stats->divergent_filter_fraction = none;
  try {
    const webrtc::AudioProcessingStats& s = aec->latest;
    if (s.delay_ms) {
      stats->delay_ms = *s.delay_ms;
    }
    if (s.echo_return_loss_enhancement) {
      stats->erle_db = *s.echo_return_loss_enhancement;
    }
    if (s.echo_return_loss) {
      stats->erl_db = *s.echo_return_loss;
    }
    if (s.residual_echo_likelihood) {
      stats->residual_echo_likelihood = *s.residual_echo_likelihood;
    }
    if (s.divergent_filter_fraction) {
      stats->divergent_filter_fraction = *s.divergent_filter_fraction;
    }
    return ZR_AEC_OK;
  } catch (...) {
    return ZR_AEC_EXCEPTION;
  }
}
