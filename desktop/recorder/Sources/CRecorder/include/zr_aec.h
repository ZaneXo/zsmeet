// 回声消除（WebRTC 的 AEC3）的 C 接口。zr_aec.cc 用 C++ 把 webrtc::AudioProcessing 包成这几个函数给 Swift 调用。
//
// 约定（不守约定时返回 ZR_AEC_BAD_ARGUMENT，不会让程序崩溃）：
// - 只在处理线程上调用，不在 Core Audio 的音频线程上；同一个 zr_aec 不能两个线程同时用。
// - 声音都是 48 kHz、单声道、-1…1 的浮点，每次正好 ZR_AEC_FRAME（480）个采样，也就是 10 毫秒。
// - 先 zr_aec_render 一帧电脑声音（参考），再 zr_aec_capture 一帧麦克风，一比一交替。
//   参考和麦克风怎么按时间对齐由调用方（Swift 的 EchoStage）负责，这里不再推迟麦克风。
// WebRTC 内部的检查（RTC_CHECK）没通过会直接 abort 整个进程，所以参数都在这里先查好；
// C++ 的异常（比如内存不够）也在这里接住，变成 ZR_AEC_EXCEPTION。
#ifndef ZSMEET_ZR_AEC_H
#define ZSMEET_ZR_AEC_H

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

/// 采样率和每帧的采样数，固定不变
#define ZR_AEC_SAMPLE_RATE 48000
#define ZR_AEC_FRAME 480
/// 处理过的麦克风比送进去的晚多少个采样（约 9 毫秒：分频的滤波器组加上 AEC3 按 4 毫秒一块处理）。
/// 这是 webrtc-audio-processing 2.1 在 48 kHz 单声道时量出来的固定值，测试里会再量一遍（升级 WebRTC 时要核对）。
/// 调用方靠它把做过、没做回声消除的声音无缝接起来
#define ZR_AEC_LATENCY 431

typedef struct zr_aec zr_aec;

typedef struct {
    /// AEC3 找回声延迟用的滤波器个数（EchoCanceller3Config.delay.num_filters），1 到 16。
    /// 能找到的延迟大约是 (24 × (个数 − 1) + 32) × 4 毫秒：6 个是 608 毫秒
    int num_filters;
    /// 告诉 AEC3 大概的回声延迟（毫秒，AudioProcessing::set_stream_delay_ms），它拿来当一开始的对齐；
    /// 0 到 500，-1 表示不告诉
    int stream_delay_ms;
} zr_aec_config;

/// AEC3 的统计。没有的值是 NaN（ERLE、ERL 本身可能是负数，不能拿 -1 表示没有）
typedef struct {
    /// AEC3 估计的回声延迟（毫秒）：参考比麦克风里的回声早多少，包括调用方加的提前量
    double delay_ms;
    /// 回声消掉了多少（ERLE，dB）
    double erle_db;
    /// 回声本来比参考小多少（ERL，dB）
    double erl_db;
    /// 处理后还有回声的可能性（0…1，残余回声检测）
    double residual_echo_likelihood;
    /// 滤波器发散的比例（这一版的 AEC3 一般不报）
    double divergent_filter_fraction;
} zr_aec_stats;

enum {
    ZR_AEC_OK = 0,
    /// 参数不对：指针是空的、采样数不是 480、配置超出范围
    ZR_AEC_BAD_ARGUMENT = -1,
    /// WebRTC 返回了错误码，或者处理出来的声音不是有限的数
    ZR_AEC_FAILED = -2,
    /// 抛出了 C++ 异常（比如内存不够）
    ZR_AEC_EXCEPTION = -3,
};

/// 建一个回声消除器。配置不对、建不起来时返回 NULL（原因写在标准错误里）
zr_aec *_Nullable zr_aec_create(const zr_aec_config *_Nullable config);
void zr_aec_destroy(zr_aec *_Nullable aec);

/// 一帧参考（电脑里放出来的声音）。count 必须是 ZR_AEC_FRAME
int zr_aec_render(zr_aec *_Nullable aec, const float *_Nullable frame, size_t count);

/// 一帧麦克风，处理好的写到 output（可以和 input 是同一块内存）。count 必须是 ZR_AEC_FRAME。
/// 处理好的声音比 input 晚 ZR_AEC_LATENCY 个采样：output 的前 ZR_AEC_LATENCY 个是之前送进去的。
/// 出错时 output 里是原样的 input（不晚，不是有限的数的地方换成 0，超出 -1…1 的削到范围里）
int zr_aec_capture(zr_aec *_Nullable aec, const float *_Nullable input, float *_Nullable output, size_t count);

/// 从头开始（重新建一个 AudioProcessing），之前学到的回声路径都不要了。失败时还用原来的那个
int zr_aec_reset(zr_aec *_Nullable aec);

/// 读统计
int zr_aec_get_stats(zr_aec *_Nullable aec, zr_aec_stats *_Nullable stats);

#ifdef __cplusplus
}
#endif

#endif
