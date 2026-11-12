// 录音小程序里在音频线程上跑的部分，用 C 写：保证不加锁、不分配内存、不碰 Swift 的引用计数。
#ifndef ZSMEET_CRECORDER_H
#define ZSMEET_CRECORDER_H

#include <CoreAudio/CoreAudio.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

CF_ASSUME_NONNULL_BEGIN

/// 环形缓冲：Core Audio 的 IO 线程往里写（一路声音、原始采样率、单声道），处理线程往外读。
/// 只能有一个线程写、一个线程读。满了就丢掉新来的采样（计数），不会阻塞音频线程。
/// 每次写进来一批采样，可以顺带记一个时间戳（zr_stamp）：这批的第一个采样是什么时候采到的。
/// 回声消除靠它把麦克风和电脑声音按时间对齐（见 EchoStage.swift）。
typedef struct zr_ring zr_ring;

/// 一批采样（一个 IOProc 缓冲）的时间戳
typedef struct {
    /// 这批的第一个采样的序号：一共写进来过多少个采样（和 zr_ring_total_written 是同一套计数，从 0 开始）
    uint64_t index;
    /// 第一个采样被硬件采到的时间（纳秒）。和 mach_absolute_time 换算成纳秒是同一个时钟（不算睡眠的时间），
    /// 也就是 DispatchTime.now().uptimeNanoseconds
    uint64_t host_ns;
    /// 设备的采样计数（AudioTimeStamp 的 mSampleTime），没有时是 NaN。只写日志、排查用
    double sample_time;
    /// 这批有多少个采样（缓冲满了的话实际写进去的可能少一些，下一个时间戳的 index 能看出来）
    uint32_t frames;
    uint32_t reserved;
} zr_stamp;

/// 时间戳最多攒这么多个（处理线程每 20 毫秒取一次，正常只有几个）。满了就不记新的（计数），采样照常写
#define ZR_STAMP_CAPACITY 1024

/// capacity 会向上取整到 2 的幂。max_channels 是 IOProc 混成单声道时最多用前几个声道（取平均）。
zr_ring *_Nullable zr_ring_create(size_t capacity, uint32_t max_channels);
void zr_ring_destroy(zr_ring *_Nullable ring);

size_t zr_ring_capacity(const zr_ring *ring);
/// 写入 count 个采样，返回写进去的个数（满了的部分丢掉）。不记时间戳
size_t zr_ring_write(zr_ring *ring, const float *samples, size_t count);
/// 和 zr_ring_write 一样，先记一个时间戳：第一个采样的时间是 host_ns（读文件、测试用）
size_t zr_ring_write_stamped(zr_ring *ring, const float *samples, size_t count, uint64_t host_ns);
/// 读出最多 max 个采样，返回读到的个数。
size_t zr_ring_read(zr_ring *ring, float *out, size_t max);
/// 现在能读的采样数。
size_t zr_ring_available(const zr_ring *ring);
/// 一共写进来过多少个采样、因为满了丢掉过多少个。
uint64_t zr_ring_total_written(const zr_ring *ring);
uint64_t zr_ring_total_dropped(const zr_ring *ring);
/// 一共读出过多少个采样（下一个要读的采样的序号）。只在读的线程上用
uint64_t zr_ring_read_position(const zr_ring *ring);

/// 取出最多 max 个时间戳（按写入的先后），返回取到的个数。只在读的线程上用。
/// 先写时间戳、后写采样，所以读到的采样的时间戳一定已经取得到；也可能取到还没读到的采样的时间戳
size_t zr_ring_read_stamps(zr_ring *ring, zr_stamp *out, size_t max);
/// 看一眼最早的那个还没取的时间戳（不取出），没有时返回 false。只在读的线程上用。
/// 和 zr_ring_read_stamps 一样：先看到采样（zr_ring_available）再看时间戳，这些采样的时间戳一定已经在了
bool zr_ring_peek_stamp(const zr_ring *ring, zr_stamp *out);
/// 时间戳攒满了、没记下的个数
uint64_t zr_ring_stamps_dropped(const zr_ring *ring);

/// Core Audio 的 IOProc（和 AudioDeviceIOProc 的签名一样），client 是 zr_ring*：
/// 把输入的前 max_channels 个声道（按 buffer、声道的顺序）取平均，写进环形缓冲。只认 32 位浮点。
/// input_time 带着主机时间时顺带记一个时间戳
OSStatus zr_capture_ioproc(AudioObjectID device,
                           const AudioTimeStamp *now,
                           const AudioBufferList *input,
                           const AudioTimeStamp *input_time,
                           AudioBufferList *output,
                           const AudioTimeStamp *output_time,
                           void *_Nullable client);

/// 和 zr_capture_ioproc 一样的混音，方便测试：把 input 混成单声道写进 ring，返回写进去的帧数。
/// time 带着主机时间（kAudioTimeStampHostTimeValid）时记一个时间戳，可以是 NULL
size_t zr_ring_write_buffer_list(zr_ring *_Nullable ring, const AudioBufferList *_Nullable input,
                                 const AudioTimeStamp *_Nullable time);

/// 主机时间（mach_absolute_time 的单位）换算成纳秒，和 IOProc 里记时间戳用的是同一个换算
uint64_t zr_host_time_to_ns(uint64_t host_time);

CF_ASSUME_NONNULL_END

#endif
