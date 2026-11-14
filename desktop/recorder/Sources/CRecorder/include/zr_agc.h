// 麦克风自动增益（WebRTC 的 AGC2：自适应的数字增益加限幅器）的 C 接口。zr_agc.cc 用 C++ 把一个只开
// AGC2 的 webrtc::AudioProcessing 包成这几个函数给 Swift 调用（RecorderKit 的 MicGain.swift）。
//
// 约定（不守约定时返回 ZR_AGC_BAD_ARGUMENT，不会让程序崩溃）：
// - 只在处理线程上调用，不在 Core Audio 的音频线程上；同一个 zr_agc 不能两个线程同时用。
// - 声音是 48 kHz、单声道、-1…1 的浮点，每次正好 ZR_AGC_FRAME（480）个采样，也就是 10 毫秒。
// - 处理过的声音不晚：第 n 个采样出来还是第 n 个（只开 AGC2 时 AudioProcessing 不分频，增益在一帧里平滑地变）。
// WebRTC 内部的检查（RTC_CHECK）没通过会直接 abort 整个进程，所以参数都在这里先查好；
// C++ 的异常（比如内存不够）也在这里接住，变成 ZR_AGC_EXCEPTION。
#ifndef ZSMEET_ZR_AGC_H
#define ZSMEET_ZR_AGC_H

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

/// 采样率和每帧的采样数，固定不变
#define ZR_AGC_SAMPLE_RATE 48000
#define ZR_AGC_FRAME 480

typedef struct zr_agc zr_agc;

/// AGC2 自适应数字增益的参数（AudioProcessing::Config::GainController2::AdaptiveDigital）。
/// 增益只在 RNN VAD 很有把握是在说话（连着 120 毫秒以上）时往上调；没人说话时只会往下调（底噪太响时），不会把底噪越调越大。
/// 限幅器一直开着：调大以后的声音不会超过满幅。
typedef struct {
    /// 最多调大多少 dB，0 到 50（不含 0）
    float max_gain_db;
    /// 一开始调大多少 dB，0 到 max_gain_db。之后按说话的声音调
    float initial_gain_db;
    /// 说话的峰值离满幅留多少 dB，0 到 30。AGC2 让说话时 0.4 秒内的峰值落在 -headroom_db dBFS 左右，
    /// 说话的音量（均方根）比它再低十几 dB
    float headroom_db;
    /// 增益每秒最多变多少 dB，大于 0、不超过 100
    float max_gain_change_db_per_second;
    /// 调大以后的底噪最多多大（dBFS，-90 到 0）：底噪本来就响的话少调一些
    float max_output_noise_level_dbfs;
} zr_agc_config;

enum {
    ZR_AGC_OK = 0,
    /// 参数不对：指针是空的、采样数不是 480、配置超出范围
    ZR_AGC_BAD_ARGUMENT = -1,
    /// WebRTC 返回了错误码，或者处理出来的声音不是有限的数
    ZR_AGC_FAILED = -2,
    /// 抛出了 C++ 异常（比如内存不够）
    ZR_AGC_EXCEPTION = -3,
};

/// 建一个自动增益。配置不对、建不起来时返回 NULL（原因写在标准错误里）
zr_agc *_Nullable zr_agc_create(const zr_agc_config *_Nullable config);
void zr_agc_destroy(zr_agc *_Nullable agc);

/// 一帧麦克风的声音，调好的写到 output（可以和 input 是同一块内存）。count 必须是 ZR_AGC_FRAME。
/// 出错时 output 里是原样的 input（不是有限的数的地方换成 0，超出 -1…1 的削到范围里）
int zr_agc_process(zr_agc *_Nullable agc, const float *_Nullable input, float *_Nullable output, size_t count);

/// 从头开始（重新建一个 AudioProcessing），一开始的增益是 initial_gain_db（0 到配置里的 max_gain_db），
/// 之前估计的说话音量、底噪都不要了。失败时还用原来的那个
int zr_agc_reset(zr_agc *_Nullable agc, float initial_gain_db);

#ifdef __cplusplus
}
#endif

#endif
