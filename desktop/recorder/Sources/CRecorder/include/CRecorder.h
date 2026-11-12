// 录音小程序里在音频线程上跑的部分，用 C 写：保证不加锁、不分配内存、不碰 Swift 的引用计数。
#ifndef ZSMEET_CRECORDER_H
#define ZSMEET_CRECORDER_H

#include <CoreAudio/CoreAudio.h>
#include <stddef.h>
#include <stdint.h>

CF_ASSUME_NONNULL_BEGIN

/// 环形缓冲：Core Audio 的 IO 线程往里写（一路声音、原始采样率、单声道），处理线程往外读。
/// 只能有一个线程写、一个线程读。满了就丢掉新来的采样（计数），不会阻塞音频线程。
typedef struct zr_ring zr_ring;

/// capacity 会向上取整到 2 的幂。max_channels 是 IOProc 混成单声道时最多用前几个声道（取平均）。
zr_ring *_Nullable zr_ring_create(size_t capacity, uint32_t max_channels);
void zr_ring_destroy(zr_ring *_Nullable ring);

size_t zr_ring_capacity(const zr_ring *ring);
/// 写入 count 个采样，返回写进去的个数（满了的部分丢掉）。
size_t zr_ring_write(zr_ring *ring, const float *samples, size_t count);
/// 读出最多 max 个采样，返回读到的个数。
size_t zr_ring_read(zr_ring *ring, float *out, size_t max);
/// 现在能读的采样数。
size_t zr_ring_available(const zr_ring *ring);
/// 一共写进来过多少个采样、因为满了丢掉过多少个。
uint64_t zr_ring_total_written(const zr_ring *ring);
uint64_t zr_ring_total_dropped(const zr_ring *ring);

/// Core Audio 的 IOProc（和 AudioDeviceIOProc 的签名一样），client 是 zr_ring*：
/// 把输入的前 max_channels 个声道（按 buffer、声道的顺序）取平均，写进环形缓冲。只认 32 位浮点。
OSStatus zr_capture_ioproc(AudioObjectID device,
                           const AudioTimeStamp *now,
                           const AudioBufferList *input,
                           const AudioTimeStamp *input_time,
                           AudioBufferList *output,
                           const AudioTimeStamp *output_time,
                           void *_Nullable client);

/// 和 zr_capture_ioproc 一样的混音，方便测试：把 input 混成单声道写进 ring，返回写进去的帧数。
size_t zr_ring_write_buffer_list(zr_ring *_Nullable ring, const AudioBufferList *_Nullable input);

CF_ASSUME_NONNULL_END

#endif
