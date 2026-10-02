#include "coreaudio_darwin.h"

#include <AudioToolbox/AudioToolbox.h>
#include <CoreAudio/CoreAudio.h>
#include <string.h>
#include <stdlib.h>
#include <stdio.h>
#include <pthread.h>

/* Implemented on the Go side (see voice_darwin.go). */
extern void cellbridgeVoiceFrame(int16_t *pcm, int frames, void *ctx);

#define VOICE_RATE 8000
#define RING_FRAMES (VOICE_RATE * 2) /* 2 s of playback buffer */

struct voice_handle {
    AudioUnit inUnit;
    AudioUnit outUnit;
    AudioDeviceID inDev;
    AudioDeviceID outDev;
    int running;

    void *ctx;

    /* Playback ring buffer, written by the RTP side, read by CoreAudio. */
    pthread_mutex_t lock;
    int16_t ring[RING_FRAMES];
    int head;
    int tail;

    /* Scratch buffer for AUHAL capture: the input callback must pull the audio
     * itself with AudioUnitRender (a render callback is never called for the
     * capture bus). */
    AudioBufferList *inList;
    int16_t *inData;
    int inDataBytes;

    /* Nominal rates this open call changed, restored on close. */
    Float64 inRate;
    Float64 outRate;
    int inRateOverridden;
    int outRateOverridden;
    double inClientRate;    /* what AUHAL actually delivers, read back */
};

static char g_err[256];

static void seterr(const char *what, OSStatus st) {
    snprintf(g_err, sizeof(g_err), "%s (OSStatus %d)", what, (int)st);
}

const char *voice_last_error(void) { return g_err; }

static int cf_matches(AudioDeviceID dev, AudioObjectPropertySelector sel, const char *substr) {
    CFStringRef name = NULL;
    UInt32 size = sizeof(name);
    AudioObjectPropertyAddress addr = {
        sel,
        kAudioObjectPropertyScopeGlobal,
        kAudioObjectPropertyElementMain,
    };
    if (AudioObjectGetPropertyData(dev, &addr, 0, NULL, &size, &name) != noErr || !name) {
        return 0;
    }
    char buf[256] = {0};
    CFStringGetCString(name, buf, sizeof(buf), kCFStringEncodingUTF8);
    CFRelease(name);
    return strcasestr(buf, substr) != NULL;
}

/* The module's UAC shows up as two devices named "AC Interface" (capture) and
 * "AS Interface" (playback) whose *manufacturer* is BAIWANG, so match either
 * field. */
static int name_matches(AudioDeviceID dev, const char *substr) {
    if (!substr || !*substr) return 1;
    if (cf_matches(dev, kAudioObjectPropertyName, substr)) return 1;
    if (cf_matches(dev, kAudioObjectPropertyManufacturer, substr)) return 1;
    return 0;
}

static int device_has_streams(AudioDeviceID dev, int input) {
    AudioObjectPropertyAddress addr = {
        kAudioDevicePropertyStreamConfiguration,
        input ? kAudioObjectPropertyScopeInput : kAudioObjectPropertyScopeOutput,
        kAudioObjectPropertyElementMain,
    };
    UInt32 size = 0;
    if (AudioObjectGetPropertyDataSize(dev, &addr, 0, NULL, &size) != noErr || size == 0) {
        return 0;
    }
    AudioBufferList *list = (AudioBufferList *)malloc(size);
    if (!list) return 0;
    int ok = 0;
    if (AudioObjectGetPropertyData(dev, &addr, 0, NULL, &size, list) == noErr) {
        for (UInt32 i = 0; i < list->mNumberBuffers; i++) {
            if (list->mBuffers[i].mNumberChannels > 0) { ok = 1; break; }
        }
    }
    free(list);
    return ok;
}

static int find_device(const char *substr, int wantInput, AudioDeviceID *out) {
    AudioObjectPropertyAddress addr = {
        kAudioHardwarePropertyDevices,
        kAudioObjectPropertyScopeGlobal,
        kAudioObjectPropertyElementMain,
    };
    UInt32 size = 0;
    if (AudioObjectGetPropertyDataSize(kAudioObjectSystemObject, &addr, 0, NULL, &size) != noErr) {
        return 0;
    }
    UInt32 count = size / sizeof(AudioDeviceID);
    AudioDeviceID *devs = (AudioDeviceID *)malloc(size);
    if (!devs) return 0;
    if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &addr, 0, NULL, &size, devs) != noErr) {
        free(devs);
        return 0;
    }
    int found = 0;
    for (UInt32 i = 0; i < count; i++) {
        if (!name_matches(devs[i], substr)) continue;
        if (!device_has_streams(devs[i], wantInput)) continue;
        *out = devs[i];
        found = 1;
        break;
    }
    free(devs);
    return found;
}

int voice_list_devices(const char *substr, char *buf, int buflen) {
    AudioObjectPropertyAddress addr = {
        kAudioHardwarePropertyDevices,
        kAudioObjectPropertyScopeGlobal,
        kAudioObjectPropertyElementMain,
    };
    UInt32 size = 0;
    if (AudioObjectGetPropertyDataSize(kAudioObjectSystemObject, &addr, 0, NULL, &size) != noErr) {
        return -1;
    }
    UInt32 count = size / sizeof(AudioDeviceID);
    AudioDeviceID *devs = (AudioDeviceID *)malloc(size);
    if (!devs) return -1;
    if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &addr, 0, NULL, &size, devs) != noErr) {
        free(devs);
        return -1;
    }
    int used = 0;
    for (UInt32 i = 0; i < count; i++) {
        if (!name_matches(devs[i], substr)) continue;
        CFStringRef name = NULL;
        UInt32 nsize = sizeof(name);
        AudioObjectPropertyAddress naddr = {
            kAudioObjectPropertyName,
            kAudioObjectPropertyScopeGlobal,
            kAudioObjectPropertyElementMain,
        };
        if (AudioObjectGetPropertyData(devs[i], &naddr, 0, NULL, &nsize, &name) != noErr || !name) {
            continue;
        }
        char cname[256] = {0};
        CFStringGetCString(name, cname, sizeof(cname), kCFStringEncodingUTF8);
        CFRelease(name);
        int n = snprintf(buf + used, buflen - used, "%s in=%d out=%d\n", cname,
                         device_has_streams(devs[i], 1), device_has_streams(devs[i], 0));
        if (n > 0) used += n;
        if (used >= buflen - 1) break;
    }
    free(devs);
    return used;
}

/* AUHAL input callback: pull the captured audio with AudioUnitRender. */
static OSStatus input_callback(void *refCon, AudioUnitRenderActionFlags *flags,
                               const AudioTimeStamp *ts, UInt32 bus, UInt32 frames,
                               AudioBufferList *ioData) {
    (void)bus; (void)ioData;
    voice_handle *h = (voice_handle *)refCon;
    if (!h || !h->running || !h->inList || !h->inData) return noErr;
    if ((int)(frames * sizeof(int16_t)) > h->inDataBytes) return noErr;
    h->inList->mBuffers[0].mDataByteSize = frames * sizeof(int16_t);
    OSStatus st = AudioUnitRender(h->inUnit, flags, ts, 1, frames, h->inList);
    if (st != noErr) return st;
    cellbridgeVoiceFrame((int16_t *)h->inList->mBuffers[0].mData, (int)frames, h->ctx);
    return noErr;
}

static OSStatus output_callback(void *refCon, AudioUnitRenderActionFlags *flags,
                                const AudioTimeStamp *ts, UInt32 bus, UInt32 frames,
                                AudioBufferList *ioData) {
    (void)flags; (void)ts; (void)bus;
    voice_handle *h = (voice_handle *)refCon;
    if (!h || !ioData) return noErr;
    for (UInt32 b = 0; b < ioData->mNumberBuffers; b++) {
        AudioBuffer *ab = &ioData->mBuffers[b];
        int16_t *dst = (int16_t *)ab->mData;
        int want = (int)(ab->mDataByteSize / sizeof(int16_t));
        for (int i = 0; i < want; i++) {
            int16_t v = 0;
            pthread_mutex_lock(&h->lock);
            if (h->head != h->tail) {
                v = h->ring[h->tail];
                h->tail = (h->tail + 1) % RING_FRAMES;
            }
            pthread_mutex_unlock(&h->lock);
            dst[i] = v;
        }
    }
    return noErr;
}

/* AUHAL bus layout: bus 0 = playback, bus 1 = capture.
 * The client format of a capture unit lives on bus 1's *output* scope, and of
 * a playback unit on bus 0's *input* scope. Setting the wrong bus silently
 * produces an audio unit that runs but never calls back. */
static OSStatus set_client_format(AudioUnit unit, UInt32 bus, AudioUnitScope scope) {
    AudioStreamBasicDescription fmt;
    memset(&fmt, 0, sizeof(fmt));
    fmt.mSampleRate = VOICE_RATE;
    fmt.mFormatID = kAudioFormatLinearPCM;
    fmt.mFormatFlags = kAudioFormatFlagIsSignedInteger | kAudioFormatFlagIsPacked;
    fmt.mFramesPerPacket = 1;
    fmt.mChannelsPerFrame = 1;
    fmt.mBitsPerChannel = 16;
    fmt.mBytesPerFrame = 2;
    fmt.mBytesPerPacket = 2;
    return AudioUnitSetProperty(unit, kAudioUnitProperty_StreamFormat,
                                scope, bus, &fmt, sizeof(fmt));
}

/* The module's UAC device changes its nominal rate underneath us: idle it sits
 * at 8 kHz, but as soon as the call route comes up it can jump to 16/48 kHz.
 * If we keep treating its frames as 8 kHz the RTP stream carries six times the
 * audio it claims and the call turns into robotic, stuttering noise.
 *
 * MaVo/DJOneHub's verified fix is to ask the *device* for 8 kHz up front and
 * read it back, then restore the original rate when we are done. */
static int lock_nominal_rate(AudioDeviceID dev, Float64 *original, int *overridden) {
    AudioObjectPropertyAddress addr = {
        kAudioDevicePropertyNominalSampleRate,
        kAudioObjectPropertyScopeGlobal,
        kAudioObjectPropertyElementMain,
    };
    Float64 current = 0;
    UInt32 size = sizeof(current);
    if (AudioObjectGetPropertyData(dev, &addr, 0, NULL, &size, &current) != noErr) return 0;
    *original = current;
    *overridden = 0;
    if (fabs(current - 8000.0) < 0.5) return 1;
    Float64 want = 8000.0;
    if (AudioObjectSetPropertyData(dev, &addr, 0, NULL, sizeof(want), &want) != noErr) return 0;
    Float64 applied = 0;
    size = sizeof(applied);
    if (AudioObjectGetPropertyData(dev, &addr, 0, NULL, &size, &applied) != noErr) return 0;
    if (fabs(applied - 8000.0) >= 0.5) return 0;
    *overridden = 1;
    return 1;
}

static void restore_nominal_rate(AudioDeviceID dev, Float64 original, int overridden) {
    if (!overridden || original <= 0) return;
    AudioObjectPropertyAddress addr = {
        kAudioDevicePropertyNominalSampleRate,
        kAudioObjectPropertyScopeGlobal,
        kAudioObjectPropertyElementMain,
    };
    Float64 want = original;
    AudioObjectSetPropertyData(dev, &addr, 0, NULL, sizeof(want), &want);
}

/* Reads back the sample rate the AUHAL unit will actually hand us. */
static double client_sample_rate(AudioUnit unit, UInt32 bus, AudioUnitScope scope) {
    AudioStreamBasicDescription got = {0};
    UInt32 size = sizeof(got);
    if (AudioUnitGetProperty(unit, kAudioUnitProperty_StreamFormat, scope, bus, &got, &size) != noErr) {
        return 0;
    }
    return got.mSampleRate;
}

voice_handle *voice_open_capture(const char *name_substr, intptr_t ctx) {
    return voice_open_mode(name_substr, ctx, 0);
}

voice_handle *voice_open(const char *name_substr, intptr_t ctx) {
    return voice_open_mode(name_substr, ctx, 1);
}

/* voice_open_mode is voice_open with the playback half optional. Capture-only
 * exists for diagnostics: a denied microphone permission does not fail, it
 * delivers silence, so the only way to tell "denied" from "quiet" is to open
 * one device and look at the samples. */
voice_handle *voice_open_mode(const char *name_substr, intptr_t ctx, int with_playback) {
    voice_handle *h = calloc(1, sizeof(*h));
    if (!h) {
        snprintf(g_err, sizeof(g_err), "out of memory");
        return NULL;
    }
    h->ctx = (void *)ctx;
    pthread_mutex_init(&h->lock, NULL);

    /* Capture and playback are separate devices on the module's UAC, so look
     * them up independently. */
    if (!find_device(name_substr, 1, &h->inDev)) {
        snprintf(g_err, sizeof(g_err), "no capture device matching \"%s\"",
                 name_substr ? name_substr : "");
        voice_close(h);
        return NULL;
    }
    if (with_playback && !find_device(name_substr, 0, &h->outDev)) {
        snprintf(g_err, sizeof(g_err), "no playback device matching \"%s\"",
                 name_substr ? name_substr : "");
        voice_close(h);
        return NULL;
    }

    // Pin both halves of the UAC pair to the rate the RTP path speaks.
    if (!lock_nominal_rate(h->inDev, &h->inRate, &h->inRateOverridden)) {
        snprintf(g_err, sizeof(g_err), "module capture device will not run at 8 kHz");
        voice_close(h);
        return NULL;
    }
    if (with_playback && h->outDev != h->inDev &&
        !lock_nominal_rate(h->outDev, &h->outRate, &h->outRateOverridden)) {
        snprintf(g_err, sizeof(g_err), "module playback device will not run at 8 kHz");
        voice_close(h);
        return NULL;
    }
    if (h->outDev == h->inDev) {
        h->outRate = h->inRate;
        h->outRateOverridden = 0;   // released with the capture half
    }

    // --- capture ---
    AudioComponentDescription desc = {
        kAudioUnitType_Output, kAudioUnitSubType_HALOutput,
        kAudioUnitManufacturer_Apple, 0, 0,
    };
    AudioComponent comp = AudioComponentFindNext(NULL, &desc);
    OSStatus st = AudioComponentInstanceNew(comp, &h->inUnit);
    if (st != noErr) { seterr("create input unit", st); voice_close(h); return NULL; }

    UInt32 one = 1, zero = 0;
    AudioUnitSetProperty(h->inUnit, kAudioOutputUnitProperty_EnableIO,
                         kAudioUnitScope_Input, 1, &one, sizeof(one));
    AudioUnitSetProperty(h->inUnit, kAudioOutputUnitProperty_EnableIO,
                         kAudioUnitScope_Output, 0, &zero, sizeof(zero));
    AudioUnitSetProperty(h->inUnit, kAudioOutputUnitProperty_CurrentDevice,
                         kAudioUnitScope_Global, 0, &h->inDev, sizeof(h->inDev));
    st = set_client_format(h->inUnit, 1, kAudioUnitScope_Output);
    if (st != noErr) { seterr("set capture format", st); voice_close(h); return NULL; }
    /* Capture scratch buffer (the callback renders into this). */
    h->inDataBytes = 4096 * (int)sizeof(int16_t);
    h->inData = (int16_t *)calloc(1, h->inDataBytes);
    h->inList = (AudioBufferList *)calloc(1, sizeof(AudioBufferList));
    if (!h->inData || !h->inList) {
        snprintf(g_err, sizeof(g_err), "out of memory (capture buffer)");
        voice_close(h);
        return NULL;
    }
    h->inList->mNumberBuffers = 1;
    h->inList->mBuffers[0].mNumberChannels = 1;
    h->inList->mBuffers[0].mDataByteSize = 0;
    h->inList->mBuffers[0].mData = h->inData;

    AURenderCallbackStruct inCb = { input_callback, h };
    st = AudioUnitSetProperty(h->inUnit, kAudioOutputUnitProperty_SetInputCallback,
                              kAudioUnitScope_Global, 0, &inCb, sizeof(inCb));
    if (st != noErr) { seterr("set input callback", st); voice_close(h); return NULL; }
    st = AudioUnitInitialize(h->inUnit);
    if (st != noErr) { seterr("init input unit", st); voice_close(h); return NULL; }
    // Read back what AUHAL will really hand us: the device can renegotiate the
    // rate when the module's voice route comes up, and a silent mismatch here
    // is what turns a call into robotic noise.
    h->inClientRate = client_sample_rate(h->inUnit, 1, kAudioUnitScope_Output);
    if (h->inClientRate < 7900 || h->inClientRate > 8100) {
        snprintf(g_err, sizeof(g_err),
                 "module capture runs at %.0f Hz, not 8000 Hz", h->inClientRate);
        voice_close(h);
        return NULL;
    }

    // --- playback ---
    if (with_playback) {
    st = AudioComponentInstanceNew(comp, &h->outUnit);
    if (st != noErr) { seterr("create output unit", st); voice_close(h); return NULL; }
    AudioUnitSetProperty(h->outUnit, kAudioOutputUnitProperty_EnableIO,
                         kAudioUnitScope_Output, 0, &one, sizeof(one));
    AudioUnitSetProperty(h->outUnit, kAudioOutputUnitProperty_EnableIO,
                         kAudioUnitScope_Input, 1, &zero, sizeof(zero));
    AudioUnitSetProperty(h->outUnit, kAudioOutputUnitProperty_CurrentDevice,
                         kAudioUnitScope_Global, 0, &h->outDev, sizeof(h->outDev));
    st = set_client_format(h->outUnit, 0, kAudioUnitScope_Input);
    if (st != noErr) { seterr("set playback format", st); voice_close(h); return NULL; }
    AURenderCallbackStruct outCb = { output_callback, h };
    AudioUnitSetProperty(h->outUnit, kAudioUnitProperty_SetRenderCallback,
                         kAudioUnitScope_Input, 0, &outCb, sizeof(outCb));
    st = AudioUnitInitialize(h->outUnit);
    if (st != noErr) { seterr("init output unit", st); voice_close(h); return NULL; }
    double outClientRate = client_sample_rate(h->outUnit, 0, kAudioUnitScope_Input);
    if (outClientRate < 7900 || outClientRate > 8100) {
        snprintf(g_err, sizeof(g_err),
                 "module playback runs at %.0f Hz, not 8000 Hz", outClientRate);
        voice_close(h);
        return NULL;
    }
    }

    h->running = 1;
    if (AudioOutputUnitStart(h->inUnit) != noErr ||
        (with_playback && AudioOutputUnitStart(h->outUnit) != noErr)) {
        seterr("start audio units", -1);
        voice_close(h);
        return NULL;
    }
    return h;
}

int voice_play(voice_handle *h, const int16_t *pcm, int frames) {
    if (!h || !h->running) return -1;
    pthread_mutex_lock(&h->lock);
    for (int i = 0; i < frames; i++) {
        int next = (h->head + 1) % RING_FRAMES;
        if (next == h->tail) break; /* ring full: drop */
        h->ring[h->head] = pcm[i];
        h->head = next;
    }
    pthread_mutex_unlock(&h->lock);
    return 0;
}

void voice_close(voice_handle *h) {
    if (!h) return;
    h->running = 0;
    if (h->inUnit) {
        AudioOutputUnitStop(h->inUnit);
        AudioUnitUninitialize(h->inUnit);
        AudioComponentInstanceDispose(h->inUnit);
    }
    if (h->outUnit) {
        AudioOutputUnitStop(h->outUnit);
        AudioUnitUninitialize(h->outUnit);
        AudioComponentInstanceDispose(h->outUnit);
    }
    restore_nominal_rate(h->inDev, h->inRate, h->inRateOverridden);
    restore_nominal_rate(h->outDev, h->outRate, h->outRateOverridden);
    free(h->inList);
    free(h->inData);
    pthread_mutex_destroy(&h->lock);
    free(h);
}
