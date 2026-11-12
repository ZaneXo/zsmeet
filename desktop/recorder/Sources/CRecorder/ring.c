#include "CRecorder.h"

#include <mach/mach_time.h>
#include <math.h>
#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>

// 一次最多混几个声道。多路声卡只用前面几路，其余的不管
#define ZR_MAX_MIX_CHANNELS 8

_Static_assert((ZR_STAMP_CAPACITY & (ZR_STAMP_CAPACITY - 1)) == 0, "时间戳的个数要是 2 的幂");

struct zr_ring {
    float *samples;
    size_t capacity; // 2 的幂
    size_t mask;
    uint32_t max_channels;
    _Atomic uint64_t write_index; // 只有写的一方改
    _Atomic uint64_t read_index;  // 只有读的一方改
    _Atomic uint64_t dropped;
    // 时间戳：和采样一样，写的一方只改 stamp_write，读的一方只改 stamp_read
    zr_stamp stamps[ZR_STAMP_CAPACITY];
    _Atomic uint64_t stamp_write;
    _Atomic uint64_t stamp_read;
    _Atomic uint64_t stamps_dropped;
};

// 主机时间换算成纳秒的比例（mach_timebase_info）。建环形缓冲时（不在音频线程上）先读好
static _Atomic uint32_t timebase_numer;
static _Atomic uint32_t timebase_denom;

static void load_timebase(void) {
    if (atomic_load_explicit(&timebase_denom, memory_order_acquire) != 0) {
        return;
    }
    mach_timebase_info_data_t info = {0, 0};
    if (mach_timebase_info(&info) != KERN_SUCCESS || info.numer == 0 || info.denom == 0) {
        info.numer = 1;
        info.denom = 1;
    }
    atomic_store_explicit(&timebase_numer, info.numer, memory_order_relaxed);
    atomic_store_explicit(&timebase_denom, info.denom, memory_order_release);
}

uint64_t zr_host_time_to_ns(uint64_t host_time) {
    load_timebase();
    uint64_t numer = atomic_load_explicit(&timebase_numer, memory_order_relaxed);
    uint64_t denom = atomic_load_explicit(&timebase_denom, memory_order_acquire);
    if (numer == denom) {
        return host_time;
    }
    // 分开算，免得乘的时候溢出（Apple 芯片上是 125/3，24 MHz 的主机时间要上百年才会溢出）
    return host_time / denom * numer + host_time % denom * numer / denom;
}

static size_t round_up_pow2(size_t n) {
    size_t p = 2;
    while (p < n) {
        p <<= 1;
    }
    return p;
}

zr_ring *zr_ring_create(size_t capacity, uint32_t max_channels) {
    load_timebase();
    zr_ring *ring = calloc(1, sizeof(zr_ring));
    if (ring == NULL) {
        return NULL;
    }
    ring->capacity = round_up_pow2(capacity);
    ring->mask = ring->capacity - 1;
    ring->samples = malloc(ring->capacity * sizeof(float));
    if (ring->samples == NULL) {
        free(ring);
        return NULL;
    }
    // 先把内存都写一遍，免得音频线程第一次写到某一页时才去要内存（时间戳在 calloc 时已经清零）
    memset(ring->samples, 0, ring->capacity * sizeof(float));
    if (max_channels == 0) {
        max_channels = 1;
    }
    ring->max_channels = max_channels > ZR_MAX_MIX_CHANNELS ? ZR_MAX_MIX_CHANNELS : max_channels;
    atomic_init(&ring->write_index, 0);
    atomic_init(&ring->read_index, 0);
    atomic_init(&ring->dropped, 0);
    atomic_init(&ring->stamp_write, 0);
    atomic_init(&ring->stamp_read, 0);
    atomic_init(&ring->stamps_dropped, 0);
    return ring;
}

void zr_ring_destroy(zr_ring *ring) {
    if (ring == NULL) {
        return;
    }
    free(ring->samples);
    free(ring);
}

size_t zr_ring_capacity(const zr_ring *ring) {
    return ring->capacity;
}

size_t zr_ring_available(const zr_ring *ring) {
    uint64_t w = atomic_load_explicit(&ring->write_index, memory_order_acquire);
    uint64_t r = atomic_load_explicit(&ring->read_index, memory_order_acquire);
    return (size_t)(w - r);
}

uint64_t zr_ring_total_written(const zr_ring *ring) {
    return atomic_load_explicit(&ring->write_index, memory_order_acquire);
}

uint64_t zr_ring_total_dropped(const zr_ring *ring) {
    return atomic_load_explicit(&ring->dropped, memory_order_relaxed);
}

uint64_t zr_ring_read_position(const zr_ring *ring) {
    return atomic_load_explicit(&ring->read_index, memory_order_relaxed);
}

uint64_t zr_ring_stamps_dropped(const zr_ring *ring) {
    return atomic_load_explicit(&ring->stamps_dropped, memory_order_relaxed);
}

// 记一个时间戳（写的一方）。满了就不记，不等、不覆盖
static void push_stamp(zr_ring *ring, uint64_t index, uint64_t host_ns, double sample_time, uint32_t frames) {
    uint64_t w = atomic_load_explicit(&ring->stamp_write, memory_order_relaxed);
    uint64_t r = atomic_load_explicit(&ring->stamp_read, memory_order_acquire);
    if (w - r >= ZR_STAMP_CAPACITY) {
        atomic_fetch_add_explicit(&ring->stamps_dropped, 1, memory_order_relaxed);
        return;
    }
    zr_stamp *stamp = &ring->stamps[w & (ZR_STAMP_CAPACITY - 1)];
    stamp->index = index;
    stamp->host_ns = host_ns;
    stamp->sample_time = sample_time;
    stamp->frames = frames;
    stamp->reserved = 0;
    atomic_store_explicit(&ring->stamp_write, w + 1, memory_order_release);
}

size_t zr_ring_read_stamps(zr_ring *ring, zr_stamp *out, size_t max) {
    uint64_t r = atomic_load_explicit(&ring->stamp_read, memory_order_relaxed);
    uint64_t w = atomic_load_explicit(&ring->stamp_write, memory_order_acquire);
    size_t available = (size_t)(w - r);
    size_t n = max < available ? max : available;
    for (size_t i = 0; i < n; i++) {
        out[i] = ring->stamps[(r + i) & (ZR_STAMP_CAPACITY - 1)];
    }
    atomic_store_explicit(&ring->stamp_read, r + n, memory_order_release);
    return n;
}

bool zr_ring_peek_stamp(const zr_ring *ring, zr_stamp *out) {
    uint64_t r = atomic_load_explicit(&ring->stamp_read, memory_order_relaxed);
    uint64_t w = atomic_load_explicit(&ring->stamp_write, memory_order_acquire);
    if (w == r) {
        return false;
    }
    *out = ring->stamps[r & (ZR_STAMP_CAPACITY - 1)];
    return true;
}

size_t zr_ring_write(zr_ring *ring, const float *samples, size_t count) {
    uint64_t w = atomic_load_explicit(&ring->write_index, memory_order_relaxed);
    uint64_t r = atomic_load_explicit(&ring->read_index, memory_order_acquire);
    size_t free_space = ring->capacity - (size_t)(w - r);
    size_t n = count < free_space ? count : free_space;
    size_t start = (size_t)(w & ring->mask);
    size_t first = ring->capacity - start;
    if (first > n) {
        first = n;
    }
    memcpy(ring->samples + start, samples, first * sizeof(float));
    memcpy(ring->samples, samples + first, (n - first) * sizeof(float));
    atomic_store_explicit(&ring->write_index, w + n, memory_order_release);
    if (n < count) {
        atomic_fetch_add_explicit(&ring->dropped, count - n, memory_order_relaxed);
    }
    return n;
}

size_t zr_ring_write_stamped(zr_ring *ring, const float *samples, size_t count, uint64_t host_ns) {
    if (count > 0) {
        uint64_t w = atomic_load_explicit(&ring->write_index, memory_order_relaxed);
        push_stamp(ring, w, host_ns, NAN, count > UINT32_MAX ? UINT32_MAX : (uint32_t)count);
    }
    return zr_ring_write(ring, samples, count);
}

size_t zr_ring_read(zr_ring *ring, float *out, size_t max) {
    uint64_t r = atomic_load_explicit(&ring->read_index, memory_order_relaxed);
    uint64_t w = atomic_load_explicit(&ring->write_index, memory_order_acquire);
    size_t available = (size_t)(w - r);
    size_t n = max < available ? max : available;
    size_t start = (size_t)(r & ring->mask);
    size_t first = ring->capacity - start;
    if (first > n) {
        first = n;
    }
    memcpy(out, ring->samples + start, first * sizeof(float));
    memcpy(out + first, ring->samples, (n - first) * sizeof(float));
    atomic_store_explicit(&ring->read_index, r + n, memory_order_release);
    return n;
}

typedef struct {
    const float *data;
    uint32_t stride;
} zr_channel;

// 按 buffer、声道的顺序挑出要混的声道，返回挑到几个；frames 是各个 buffer 里最少的帧数
static size_t pick_channels(const AudioBufferList *input, uint32_t max, zr_channel *channels, size_t *frames) {
    size_t used = 0;
    size_t min_frames = 0;
    for (UInt32 b = 0; b < input->mNumberBuffers && used < max; b++) {
        const AudioBuffer *buffer = &input->mBuffers[b];
        if (buffer->mData == NULL || buffer->mNumberChannels == 0) {
            continue;
        }
        size_t buffer_frames = buffer->mDataByteSize / (sizeof(float) * buffer->mNumberChannels);
        if (used == 0 || buffer_frames < min_frames) {
            min_frames = buffer_frames;
        }
        for (UInt32 c = 0; c < buffer->mNumberChannels && used < max; c++) {
            channels[used].data = (const float *)buffer->mData + c;
            channels[used].stride = buffer->mNumberChannels;
            used++;
        }
    }
    *frames = min_frames;
    return used;
}

size_t zr_ring_write_buffer_list(zr_ring *ring, const AudioBufferList *input, const AudioTimeStamp *time) {
    if (ring == NULL || input == NULL) {
        return 0;
    }
    zr_channel channels[ZR_MAX_MIX_CHANNELS];
    size_t frames = 0;
    size_t used = pick_channels(input, ring->max_channels, channels, &frames);
    if (used == 0 || frames == 0) {
        return 0;
    }

    uint64_t w = atomic_load_explicit(&ring->write_index, memory_order_relaxed);
    // 先记时间戳再写采样：读的一方读到这些采样时，它们的时间戳一定已经在了
    if (time != NULL && (time->mFlags & kAudioTimeStampHostTimeValid) != 0) {
        double sample_time = (time->mFlags & kAudioTimeStampSampleTimeValid) != 0 ? time->mSampleTime : NAN;
        push_stamp(ring, w, zr_host_time_to_ns(time->mHostTime), sample_time,
                   frames > UINT32_MAX ? UINT32_MAX : (uint32_t)frames);
    }
    uint64_t r = atomic_load_explicit(&ring->read_index, memory_order_acquire);
    size_t free_space = ring->capacity - (size_t)(w - r);
    size_t n = frames < free_space ? frames : free_space;
    float scale = 1.0f / (float)used;
    for (size_t i = 0; i < n; i++) {
        float sum = 0;
        for (size_t c = 0; c < used; c++) {
            sum += channels[c].data[i * channels[c].stride];
        }
        ring->samples[(w + i) & ring->mask] = sum * scale;
    }
    atomic_store_explicit(&ring->write_index, w + n, memory_order_release);
    if (n < frames) {
        atomic_fetch_add_explicit(&ring->dropped, frames - n, memory_order_relaxed);
    }
    return n;
}

OSStatus zr_capture_ioproc(AudioObjectID device,
                           const AudioTimeStamp *now,
                           const AudioBufferList *input,
                           const AudioTimeStamp *input_time,
                           AudioBufferList *output,
                           const AudioTimeStamp *output_time,
                           void *client) {
    (void)device;
    (void)now;
    (void)output;
    (void)output_time;
    zr_ring_write_buffer_list((zr_ring *)client, input, input_time);
    return noErr;
}
