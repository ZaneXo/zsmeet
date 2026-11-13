// 麦克风自动增益的 C 接口（见 include/zr_agc.h）：用 WebRTC 的 AudioProcessing，只开 AGC2
// （自适应的数字增益加限幅器）。回声消除、高通滤波、降噪都不开，除了调音量不改动声音。
// 回声消除（zr_aec.cc）是另一个 AudioProcessing，接在它前面：先去掉回声，再调音量（见 RecorderKit 的 MicGain.swift）。
//
// WebRTC 的 RTC_CHECK 没通过会 abort 整个进程（录音就断了），所以这里先把参数都查好，
// 每个调用都接住 C++ 异常，返回错误码，由 Swift 那边改成直接用原来的麦克风声音。
#include "zr_agc.h"

#include <cmath>
#include <cstdio>
#include <exception>
#include <memory>

#include "api/audio/audio_processing.h"
#include "api/scoped_refptr.h"

namespace {

constexpr int kSampleRate = ZR_AGC_SAMPLE_RATE;
constexpr size_t kFrame = ZR_AGC_FRAME;

static_assert(kFrame * 100 == static_cast<size_t>(kSampleRate), "一帧是 10 毫秒");

// 是有限的数，并且在 low…high 之间
bool within(float value, float low, float high) {
  return std::isfinite(value) && value >= low && value <= high;
}

// AudioProcessing 自己不查 AGC2 的配置（通过 AudioProcessingBuilder 建的时候；WebRTC 只在调试版里 DCHECK），
// 超出范围的值会让增益算错，所以这里先查
bool valid_config(const zr_agc_config& config) {
  return within(config.max_gain_db, 0.0f, 50.0f) && config.max_gain_db > 0.0f &&
         within(config.initial_gain_db, 0.0f, config.max_gain_db) && within(config.headroom_db, 0.0f, 30.0f) &&
         within(config.max_gain_change_db_per_second, 0.0f, 100.0f) && config.max_gain_change_db_per_second > 0.0f &&
         within(config.max_output_noise_level_dbfs, -90.0f, 0.0f);
}

webrtc::StreamConfig mono() {
  return webrtc::StreamConfig(kSampleRate, 1);
}

// 建一个只开 AGC2 的 AudioProcessing，并按 48 kHz 单声道初始化好（之后的调用不会再重新初始化）。
// 只开 AGC2 时它不分频（不晚），也不重新采样（48 kHz 照原样处理）
rtc::scoped_refptr<webrtc::AudioProcessing> make_apm(const zr_agc_config& c, float initial_gain_db) {
  webrtc::AudioProcessing::Config config;
  config.echo_canceller.enabled = false;
  config.high_pass_filter.enabled = false;
  config.noise_suppression.enabled = false;
  config.transient_suppression.enabled = false;
  config.gain_controller1.enabled = false;
  config.pre_amplifier.enabled = false;
  config.capture_level_adjustment.enabled = false;
  config.gain_controller2.enabled = true;
  // 不去改系统里麦克风的输入音量，只调数字增益
  config.gain_controller2.input_volume_controller.enabled = false;
  config.gain_controller2.fixed_digital.gain_db = 0.0f;
  webrtc::AudioProcessing::Config::GainController2::AdaptiveDigital& adaptive = config.gain_controller2.adaptive_digital;
  adaptive.enabled = true;
  adaptive.headroom_db = c.headroom_db;
  adaptive.max_gain_db = c.max_gain_db;
  adaptive.initial_gain_db = initial_gain_db;
  adaptive.max_gain_change_db_per_second = c.max_gain_change_db_per_second;
  adaptive.max_output_noise_level_dbfs = c.max_output_noise_level_dbfs;

  rtc::scoped_refptr<webrtc::AudioProcessing> apm = webrtc::AudioProcessingBuilder().SetConfig(config).Create();
  if (!apm) {
    return nullptr;
  }
  webrtc::ProcessingConfig formats = {{mono(), mono(), mono(), mono()}};
  if (apm->Initialize(formats) != webrtc::AudioProcessing::kNoError) {
    return nullptr;
  }
  // AGC2 真的开着（配置被换成默认的话它是关着的，声音会原样出来，看起来像是在调其实没有）
  const webrtc::AudioProcessing::Config applied = apm->GetConfig();
  if (!applied.gain_controller2.enabled || !applied.gain_controller2.adaptive_digital.enabled) {
    return nullptr;
  }
  return apm;
}

// 送进 WebRTC 之前：不是有限的数（NaN、无穷大）换成 0，超出 -1…1 的削到范围里。
// NaN 进了 RNN VAD、估计音量的那些平均值会一直留在里面（和 zr_aec.cc 一样）
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

struct zr_agc {
  zr_agc_config config;
  rtc::scoped_refptr<webrtc::AudioProcessing> apm;
  float input[kFrame];
  float output[kFrame];
};

extern "C" zr_agc* zr_agc_create(const zr_agc_config* config) {
  if (config == nullptr || !valid_config(*config)) {
    std::fprintf(stderr, "zsmeet-recorder: 自动增益的配置不对，不调麦克风的音量\n");
    return nullptr;
  }
  try {
    std::unique_ptr<zr_agc> agc(new zr_agc());
    agc->config = *config;
    agc->apm = make_apm(*config, config->initial_gain_db);
    if (!agc->apm) {
      std::fprintf(stderr, "zsmeet-recorder: 建不了 WebRTC 的 AudioProcessing（AGC2），不调麦克风的音量\n");
      return nullptr;
    }
    return agc.release();
  } catch (const std::exception& error) {
    std::fprintf(stderr, "zsmeet-recorder: 建自动增益时出错：%s\n", error.what());
  } catch (...) {
    std::fprintf(stderr, "zsmeet-recorder: 建自动增益时出错\n");
  }
  return nullptr;
}

extern "C" void zr_agc_destroy(zr_agc* agc) {
  try {
    delete agc;
  } catch (...) {
  }
}

extern "C" int zr_agc_process(zr_agc* agc, const float* input, float* output, size_t count) {
  if (agc == nullptr || input == nullptr || output == nullptr || count != kFrame || !agc->apm) {
    return ZR_AGC_BAD_ARGUMENT;
  }
  // 先拷一份（input 和 output 可以是同一块内存），出错时把它原样交回去
  sanitize(input, agc->input);
  int result = ZR_AGC_OK;
  try {
    const float* source[1] = {agc->input};
    float* destination[1] = {agc->output};
    if (agc->apm->ProcessStream(source, mono(), mono(), destination) != webrtc::AudioProcessing::kNoError) {
      result = ZR_AGC_FAILED;
    } else {
      for (size_t i = 0; i < kFrame; i++) {
        if (!std::isfinite(agc->output[i])) {
          result = ZR_AGC_FAILED;
          break;
        }
      }
    }
  } catch (...) {
    result = ZR_AGC_EXCEPTION;
  }
  const float* chosen = result == ZR_AGC_OK ? agc->output : agc->input;
  for (size_t i = 0; i < kFrame; i++) {
    // 限幅器已经保证不超过满幅，这里再保险一次
    const float x = chosen[i];
    output[i] = x > 1.0f ? 1.0f : (x < -1.0f ? -1.0f : x);
  }
  return result;
}

extern "C" int zr_agc_reset(zr_agc* agc, float initial_gain_db) {
  if (agc == nullptr || !agc->apm || !within(initial_gain_db, 0.0f, agc->config.max_gain_db)) {
    return ZR_AGC_BAD_ARGUMENT;
  }
  try {
    rtc::scoped_refptr<webrtc::AudioProcessing> fresh = make_apm(agc->config, initial_gain_db);
    if (!fresh) {
      return ZR_AGC_FAILED;
    }
    agc->apm = fresh;
    return ZR_AGC_OK;
  } catch (...) {
    return ZR_AGC_EXCEPTION;
  }
}
