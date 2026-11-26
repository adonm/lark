# Terminal wire protocol

Generated from `lark/protospec/spec.go` by `go generate ./...`; do not edit.

## Envelope

Every message is an 8-byte little-endian envelope followed by the payload:

| Field | Type | Width |
| --- | --- | --- |
| magic | u8 | 1 |
| type | u8 | 1 |
| seq | u16 | 2 |
| length | u32 | 4 |

`magic` is `0x4C`; payloads above 320 KiB are rejected.

## Messages

### 0x01 HELLO (terminal → Lark)

Terminal identity, capabilities, panel geometry and string lengths.

Fixed size: 21 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | proto_version | u8 | 1 |  |
| 1 | kind | u8 | 1 |  |
| 2 | caps_mask | u32 | 4 |  |
| 6 | display_formats | u8 | 1 |  |
| 7 | frame_codecs | u8 | 1 |  |
| 8 | width | u16 | 2 |  |
| 10 | height | u16 | 2 |  |
| 12 | shape | u8 | 1 |  |
| 13 | flags | u8 | 1 |  |
| 14 | udp_port | u16 | 2 |  |
| 16 | id_len | str8 | 1 |  |
| 17 | comp_len | str8 | 1 |  |
| 18 | board_len | str8 | 1 |  |
| 19 | fw_len | str8 | 1 |  |
| 20 | tok_len | str8 | 1 |  |

Length-prefixed string fields follow the fixed prefix in table order.

### 0x02 RESUME (terminal → Lark)

Resume token and terminal id. The old sequence field stays in the layout.

Fixed size: 22 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | proto_version | u8 | 1 |  |
| 1 | resume_token | bytes | 16 |  |
| 17 | last_seq | u32 | 4 | retired snapshot sequence |
| 21 | id_len | str8 | 1 |  |

Length-prefixed string fields follow the fixed prefix in table order.

### 0x03 TELEMETRY (terminal → Lark)

Periodic device health snapshot.

Fixed size: 16 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | wifi_rssi | i16 | 2 |  |
| 2 | battery_pct | i16 | 2 |  |
| 4 | charging | u8 | 1 |  |
| 5 | cpu_pct | u8 | 1 |  |
| 6 | flags | u8 | 1 |  |
| 7 | pad | u8 | 1 |  |
| 8 | free_heap | u32 | 4 |  |
| 12 | uptime_s | u32 | 4 |  |

### 0x04 POINTER (terminal → Lark)

Touch or gesture event with the shown frame revision.

Fixed size: 8 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | kind | u8 | 1 |  |
| 1 | pad | u8 | 1 |  |
| 2 | x | u16 | 2 |  |
| 4 | y | u16 | 2 |  |
| 6 | rev_lo | u16 | 2 |  |

### 0x06 REC_START (terminal → Lark)

Terminal acknowledgement that recording started.

Fixed size: 3 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | session | u16 | 2 |  |
| 2 | target_len | str8 | 1 |  |

Length-prefixed string fields follow the fixed prefix in table order.

### 0x07 REC_END (terminal → Lark)

Terminal notification that recording ended.

Fixed size: 4 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | session | u16 | 2 |  |
| 2 | cancelled | u8 | 1 |  |
| 3 | target_len | str8 | 1 |  |

Length-prefixed string fields follow the fixed prefix in table order.

### 0x08 TTS_ACK (terminal → Lark)

Terminal acknowledgement that a TTS stream finished or aborted.

Fixed size: 9 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | aborted | u8 | 1 |  |
| 1 | audio_id | u64 | 8 |  |

### 0x0F PING (terminal → Lark)

TCP keepalive; the four payload bytes are echoed by PONG.

Fixed size: 4 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | payload | bytes | 4 |  |

### 0x10 PONG (terminal → Lark)

TCP keepalive reply.

Fixed size: 4 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | payload | bytes | 4 |  |

### 0x81 WELCOME (Lark → terminal)

Session accepted: UDP endpoint, SSRC, limits, resume token and ids.

Fixed size: 38 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | proto_version | u8 | 1 |  |
| 1 | flags | u8 | 1 |  |
| 2 | udp_port | u16 | 2 |  |
| 4 | ssrc | u32 | 4 |  |
| 8 | max_msg_bytes | u32 | 4 |  |
| 12 | resume_window_ms | u32 | 4 |  |
| 16 | seq | u32 | 4 | retired snapshot sequence |
| 20 | resume_token | bytes | 16 |  |
| 36 | id_len | str8 | 1 |  |
| 37 | comp_len | str8 | 1 |  |

Length-prefixed string fields follow the fixed prefix in table order.

### 0x82 REJECT (Lark → terminal)

Session rejected with a reason and human-readable message.

Fixed size: 2 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | reason | u8 | 1 |  |
| 1 | msg_len | str8 | 1 |  |

Length-prefixed string fields follow the fixed prefix in table order.

### 0x83 FRAME (Lark → terminal)

Display frame; the payload is owned by the frame codec in frame.h.

Variable-length payload described by its own codec.

### 0x86 REC_CTRL (Lark → terminal)

Start, stop or cancel recording, with target flags and session.

Fixed size: 5 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | op | u8 | 1 |  |
| 1 | flags | u8 | 1 |  |
| 2 | session | u16 | 2 |  |
| 4 | target_len | str8 | 1 |  |

Length-prefixed string fields follow the fixed prefix in table order.

### 0x87 TTS_CTRL (Lark → terminal)

Start, finish or stop a TTS stream identified by audio_id.

Fixed size: 9 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | op | u8 | 1 |  |
| 1 | audio_id | u64 | 8 |  |

### 0x88 TTSPCM (Lark → terminal)

Raw 16 kHz mono little-endian PCM for the active TTS stream.

Variable-length payload described by its own codec.

### 0x8A CHIME (Lark → terminal)

Play one named tone.

Fixed size: 1 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | kind | u8 | 1 |  |

### 0x8B POWER (Lark → terminal)

Set panel brightness, 0-100.

Fixed size: 1 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | level | u8 | 1 |  |

### 0x8C VOLUME (Lark → terminal)

Set output volume, optionally persisting. The auto-mode byte is retired.

Fixed size: 5 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | level | u8 | 1 |  |
| 1 | persist | u8 | 1 |  |
| 2 | auto_mode | u8 | 1 | retired auto-volume mode |
| 3 | id_len | str8 | 1 |  |
| 4 | comp_len | str8 | 1 |  |

Length-prefixed string fields follow the fixed prefix in table order.

### 0x8D STATUS (Lark → terminal)

Assistant state. Sequence, progress, kind and text stay in the layout.

Fixed size: 9 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | seq | u32 | 4 | retired state sequence |
| 4 | state | u8 | 1 |  |
| 5 | progress | u8 | 1 | retired progress percent |
| 6 | kind | u8 | 1 | retired prompt kind |
| 7 | text_len | str16 | 2 | retired detail text |

Length-prefixed string fields follow the fixed prefix in table order.

### 0x8E VOICE_OWNER (Lark → terminal)

Companion voice ownership broadcast.

Fixed size: 6 bytes.

| Offset | Field | Type | Width | Notes |
| --- | --- | --- | --- | --- |
| 0 | seq | u32 | 4 | retired state sequence |
| 4 | comp_len | str8 | 1 |  |
| 5 | term_len | str8 | 1 |  |

Length-prefixed string fields follow the fixed prefix in table order.

## Constants

| Go | C | Value |
| --- | --- | --- |
| `protoMagic` | `LARK_PROTO_MAGIC` | 76 |
| `protoVersion1` | `LARK_PROTO_VERSION` | 1 |
| `protoHeaderSize` | `LARK_PROTO_HDR_SIZE` | 8 |
| `protoMaxPayload` | `LARK_PROTO_MAX_PAYLOAD` | 327680 |
| `resumeTokenLen` | `LARK_RESUME_TOKEN_LEN` | 16 |
| `resumeWindowMS` | `LARK_RESUME_WINDOW_MS` | 30000 |
| `—` | `LARK_DEFAULT_DEVICE_PORT` | 8766 |
| `—` | `LARK_DEFAULT_HTTP_PORT` | 8765 |
| `termKindHardware` | `LARK_KIND_HARDWARE` | 0 |
| `dispRGB565` | `LARK_DISP_RGB565` | 1 << 0 |
| `codecBitRaw` | `LARK_CODEC_RAW` | 1 << 0 |
| `codecBitRLE565` | `LARK_CODEC_RLE565` | 1 << 1 |
| `codecBitLZ4` | `LARK_CODEC_LZ4` | 1 << 2 |
| `codecBitJPEG` | `LARK_CODEC_JPEG` | 1 << 3 |
| `codecBitAll` | `—` | 15 |
| `shapeRound` | `LARK_SHAPE_ROUND` | 0 |
| `shapeRect` | `LARK_SHAPE_RECT` | 1 |
| `helloFlagIfFree` | `—` | 1 |
| `welcomeFlagBridgeEP` | `LARK_WELCOME_FLAG_BRIDGE_EP` | 1 << 0 |
| `telemetryFlagSleeping` | `LARK_TELEMETRY_SLEEPING` | 1 << 0 |
| `capBattery` | `LARK_CAP_BATTERY` | 1 << 0 |
| `capDrag` | `LARK_CAP_DRAG` | 1 << 4 |
| `capPointerRev` | `LARK_CAP_POINTER_REV` | 1 << 5 |
| `capBridgeEndpoint` | `LARK_CAP_BRIDGE_ENDPOINT` | 1 << 6 |
| `capPanelSleep` | `LARK_CAP_PANEL_SLEEP` | 1 << 7 |
| `rejectBadToken` | `LARK_REJECT_BAD_TOKEN` | 1 |
| `rejectBadVersion` | `LARK_REJECT_BAD_VERSION` | 2 |
| `rejectUnknownTerminal` | `LARK_REJECT_UNKNOWN_TERMINAL` | 3 |
| `rejectFull` | `LARK_REJECT_FULL` | 4 |
| `rejectOccupied` | `—` | 5 |
| `rejectBadCaps` | `LARK_REJECT_BAD_CAPS` | 6 |
| `statusIdle` | `LARK_STATE_IDLE` | 0 |
| `statusListening` | `LARK_STATE_LISTENING` | 1 |
| `statusThinking` | `LARK_STATE_THINKING` | 2 |
| `statusSpeaking` | `LARK_STATE_SPEAKING` | 3 |
| `statusBlocked` | `—` | 4 |
| `statusDone` | `LARK_STATE_DONE` | 5 |
| `statusError` | `LARK_STATE_ERROR` | 6 |
| `ptrTap` | `LARK_PTR_TAP` | 0 |
| `ptrDrag` | `LARK_PTR_DRAG` | 1 |
| `ptrUp` | `LARK_PTR_UP` | 2 |
| `recOpStop` | `LARK_REC_STOP` | 0 |
| `recOpStart` | `LARK_REC_START` | 1 |
| `recOpCancel` | `LARK_REC_CANCEL` | 2 |
| `recTargetInter` | `LARK_REC_TARGET_INTERCOM` | 1 << 0 |
| `recTargetListen` | `LARK_REC_TARGET_LISTEN` | 1 << 6 |
| `recTargetBridgeE` | `LARK_REC_TARGET_BRIDGE_EP` | 1 << 7 |
| `ttsOpStop` | `LARK_TTS_STOP` | 0 |
| `ttsOpStart` | `LARK_TTS_START` | 1 |
| `ttsOpEnd` | `LARK_TTS_END` | 2 |
| `chimeTap` | `LARK_CHIME_TAP` | 0 |
| `chimeThink` | `LARK_CHIME_THINK` | 1 |
| `chimeCancel` | `LARK_CHIME_CANCEL` | 2 |
| `chimeIntercom` | `LARK_CHIME_INTERCOM` | 3 |
| `chimeRing` | `LARK_CHIME_RING` | 4 |
| `rtpVersionByte` | `LARK_RTP_VERSION_BYTE` | 128 |
| `rtpHeaderSize` | `LARK_RTP_HDR_SIZE` | 12 |
| `rtpSamples` | `LARK_RTP_SAMPLES` | 320 |
| `rtpPCMBytes` | `LARK_RTP_PCM_BYTES` | 640 |
| `rtpPacketBytes` | `LARK_RTP_PACKET_BYTES` | 652 |
| `rtpPTRecPCM` | `LARK_RTP_PT_REC_PCM` | 96 |
