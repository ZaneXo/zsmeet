#include "CRecorder.h"

#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>

// 一次最多混几个声道。多路声卡只用前面几路，其余的不管
#define ZR_MAX_MIX_CHANNELS 8

struct zr_ring {
    float *samples;
    size_t capacity; // 2 的幂
    size_t mask;
    uint32_t max_channels;
    _Atomic uint64_t write_index; // 只有写的一方改
    _Atomic uint64_t read_index;  // 只有读的一方改
    _Atomic uint64_t dropped;
};

static size_t round_up_pow2(size_t n) {
    size_t p = 2;
    while (p < n) {
        p <<= 1;
    }
    return p;
}

zr_ring *zr_ring_create(size_t capacity, uint32_t max_channels) {
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
    // 先把内存都写一遍，免得音频线程第一次写到某一页时才去要内存
    memset(ring->samples, 0, ring->capacity * sizeof(float));
    if (max_channels == 0) {
        max_channels = 1;
    }
    ring->max_channels = max_channels > ZR_MAX_MIX_CHANNELS ? ZR_MAX_MIX_CHANNELS : max_channels;
    atomic_init(&ring->write_index, 0);
    atomic_init(&ring->read_index, 0);
    atomic_init(&ring->dropped, 0);
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

size_t zr_ring_write_buffer_list(zr_ring *ring, const AudioBufferList *input) {
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
    (void)input_time;
    (void)output;
    (void)output_time;
    zr_ring_write_buffer_list((zr_ring *)client, input);
    return noErr;
}
