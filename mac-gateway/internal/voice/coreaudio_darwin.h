#ifndef CELLBRIDGE_COREAUDIO_H
#define CELLBRIDGE_COREAUDIO_H

#include <stdint.h>

/*
 * CoreAudio bridge between the QDC507 module's USB Audio (UAC) interface and
 * the gateway's RTP path.
 *
 * The module only exposes a USB Audio interface after its USB composition has
 * been switched on (AT+QCFG="usbcfg",0x2C7C,0x0125,1,1,1,1,1,1,1) — see
 * docs/HARDWARE-SPIKE.md. Both directions run at 8 kHz mono; CoreAudio does the
 * sample-rate conversion to/from the device's native 48 kHz.
 */

typedef struct voice_handle voice_handle;

/* Lists matching device names into buf (newline separated). Returns bytes
 * written, or -1. If substr is NULL every device is listed. */
int voice_list_devices(const char *substr, char *buf, int buflen);

/* Opens input+output on the device whose name contains `name_substr`.
 * Captured audio is delivered to the Go callback `cellbridgeVoiceFrame`
 * (declared in _cgo_export.h) with `ctx` passed straight through — callers use
 * it as an opaque integer handle so no Go pointer crosses the C boundary.
 * Returns NULL on failure (see voice_last_error). */
voice_handle *voice_open(const char *name_substr, intptr_t ctx);

/* Capture-only variant, for microphone-permission diagnostics: macOS answers a
 * denied capture with silence rather than an error, so the only way to tell
 * "denied" from "quiet room" is to open one input device and look at the
 * samples. Playback is not touched. */
voice_handle *voice_open_capture(const char *name_substr, intptr_t ctx);
voice_handle *voice_open_mode(const char *name_substr, intptr_t ctx, int with_playback);

/* Queues 8 kHz mono int16 PCM for playback. Returns 0 on success. */
int voice_play(voice_handle *h, const int16_t *pcm, int frames);

void voice_close(voice_handle *h);

const char *voice_last_error(void);

#endif /* CELLBRIDGE_COREAUDIO_H */
