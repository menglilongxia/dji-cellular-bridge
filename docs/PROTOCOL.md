# 协议说明（v0.1，FakeModem 联调基线）

网关（Mac，`gatewayd`）与 iPhone App 之间的全部交互协议。真实模块接入后
modem 侧实现会替换，但**这份协议不变**。

```
iPhone ── SIP 5060/UDP ──► Mac gatewayd ◄── AT 串口 ── DJI 4G 模块
   │        RTP 动态端口        │  ◄── USB Audio ── (语音 PCM)
   └─ APNs VoIP/普通推送 ◄─────┘
```

## 1. SIP（信令）

- 传输：UDP，网关监听 `:5060`，App 用固定本地端口（`SIPClient.localPort`）并在
  Contact 中公布真实端口，网关按 Contact 回呼。
- 账号：单账号，用户名 `iphone`，密码与网关 `--pass` 一致。
- 注册：`REGISTER` + Digest MD5 鉴权（`WWW-Authenticate` 401 挑战），`expires=600`，
  App 提前 60s 续注册。REGISTER 携带自定义头：
  - `X-Push-Token: <hex>` — APNs VoIP token，网关保存用于来电唤醒。
- 来电（模块 → 手机）：网关先发 VoIP push，再发 `INVITE sip:iphone@…`；
  App 回 `180 Ringing`，接听回 `200 OK`（带 SDP），网关回 `ACK`。
  INVITE 无应答时网关重试（见 §4）。
- 去电（手机 → 模块）：App 发 `INVITE sip:<号码>@网关`（To 头带目标号码）；
  网关回 `180` 后转 `ATD<号码>;`，接通回 `200 OK`。
- 挂断：任一方发 `BYE`；对方回 `200 OK`。来电未接听前挂断用 `CANCEL`。
- 短信：`SIP MESSAGE`，`Content-Type: text/plain; charset=utf-8`，
  - 上行：App → 网关，Request-URI 为 `sip:<收件人>@网关`，网关转 `AT+CMGS`；
  - 下行：网关 → App（已注册客户端），From 为发件人号码。
- SDP：仅 `PCMU/8000`（payload type 0），`m=audio <port> RTP/AVP 0`。
- 事务可靠性：网关对 INVITE 做重传去重；200 OK 按 RFC3261 重发直到 ACK。

## 2. RTP（语音）

- 每个通话动态分配 UDP 端口（网关侧 `RtpPortBase` 起，App 侧本地随机端口）。
- 编码：G.711 μ-law，8kHz mono，每帧 20ms = 160 采样 = 160 字节载荷。
- SSRC 随机，sequence/timestamp 单调递增。
- FakeModem 模式：网关把收到的 RTP 原样 loopback（回声测试）。
- 真实模块模式：网关在 RTP 与 USB Audio（CoreAudio）之间做 μ-law ↔ PCM 桥接。

## 3. 推送（APNs）

当前网关内只有接口 + 未配置占位（`internal/push`），生产需接入：

- **VoIP push**（来电唤醒）：`push-type: voip`，topic 为 `<bundle-id>.voip`，
  载荷示例：`{"caller": "13800000000"}`。
  App 收到后必须 **~5 秒内**调用 `reportNewIncomingCall`（Apple 硬性要求，
  否则收回 VoIP 推送权限）。
  - 实现要点：push 只负责**唤醒**；App 醒来立即重连 SIP/重新 REGISTER；
    网关在"无已注册客户端"时按 2s 间隔重试 INVITE（最多 5 次），
    因此 INVITE 驱动的 CallKit 上报通常在 push 后 2–3s 内发生。
- **普通推送**（短信/未接来电）：标准 `alert` 推送，App 在前台时也可走 SIP MESSAGE 直达。

entitlements：`aps-environment`（development/production 按构建类型切换），
Info.plist `UIBackgroundModes` 含 `voip`，并开启 Push Notifications + Background Modes 能力。

## 4. 来电时序（App 被杀死的最坏情况）

```
t=0    模块 RING / +CLIP → 网关
t=0    网关发 VoIP push；INVITE → 无注册客户端 → 2s 后重试
t≈0.5s App 被 push 唤醒 → SIP 重连 → REGISTER（带 X-Push-Token）
t=2s   网关重试 INVITE → 200 OK → ACK → CallKit 响铃（< 5s ✓）
```

## 5. HTTP API（调试 / 非 SIP 兜底）

网关 `--api=:8787`。`--api-token` 为空时免鉴权（仅开发环境）。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | `{"ok": true}` |
| GET | `/api/v1/status` | modem 类型/信号强度/注册客户端/活跃通话 |
| GET | `/api/v1/messages` | 短信记录（收发） |
| POST | `/api/v1/messages` | `{"to","body"}` → 经 modem/AT+CMGS 发送 |
| POST | `/api/v1/fake/incoming-call` | `{"from"}` 模拟来电（仅 fake 模式） |
| POST | `/api/v1/fake/incoming-sms` | `{"from","body"}` 模拟来短信（仅 fake 模式） |

## 6. Modem 抽象（硬件阶段映射表）

`internal/modem.Modem` 接口保持不变；`serial.go` 接入真实模块时实现：

| 接口方法 | AT 映射 |
|---|---|
| `Dial(number)` | `ATD<号码>;`，等待 `OK` / `+CIEV` 接通 |
| `Answer()` | `ATA` |
| `Hangup()` | `ATH` |
| `SendSMS(to, body)` | `AT+CMGS="<to>"` → `>` → 正文 + Ctrl-Z |
| 事件 `OnRing/OnSMS` | URC：`RING` / `+CLIP: "<号码>"` / `+CMTI: "SM",<idx>` → `AT+CMGR=<idx>` |
| `Signal()` | `AT+CSQ` |
| 音频 | USB Audio 设备 ↔ RTP（CoreAudio，macOS 移植自 CellBridge 的 ALSA 路径） |
