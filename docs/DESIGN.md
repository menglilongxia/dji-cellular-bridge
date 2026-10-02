# 设计文档：DJI 4G 模块 → Mac 网关 → iPhone（CallKit 原生体验）

> 本文写于立项当天，保留最初的调研结论与架构选型，可以对照"当初怎么想的"。
> **文中标注"待办"的条目很多后来已经完成**——最新的实现与实测状态请看
> [`HARDWARE-SPIKE.md`](HARDWARE-SPIKE.md)，以及仓库根目录的
> [`README.md`](../README.md)。
>
> 日期：2026-09-30（立项）

## 1. 项目目标

把 DJI 4G 模块（nano-SIM）插到 Mac 上，Mac 跑一个常驻服务端，实时把该号码的**短信**和**通话**转发到 iPhone：

- 短信：收发双向，手机端实时收到推送
- 通话：手机端以来电形式响铃，接听体验走 **CallKit 原生 UI**（锁屏来电、系统通话记录、蓝牙/车载兼容），和普通电话无异

一句话架构：**模块负责蜂窝侧（VoLTE/CS 通话 + 短信），Mac 负责协议转换（AT 信令 + 音频桥接），iPhone 负责原生体验（SIP + PushKit + CallKit）**。

## 2. GitHub 相关项目调研结论

| 项目 | 相关度 | 结论 / 可借鉴点 |
|---|---|---|
| [mccding/CellBridge](https://github.com/mccding/cellbridge/blob/HEAD/README.en.md) | ★★★★★ 核心参考 | 针对的**正是 DJI 一代 4G 模块（QDC507）**，已在真机上验证端到端打通：VoLTE 语音、短信收发、来电推送。架构可直接复用，只是把它的 NAS 网关换成 Mac。必读它的 README（AT 端口探测、QADBKEY 解锁、语音路由、排障方法论） |
| [mattboston/sms-gateway](https://github.com/mattboston/sms-gateway) | ★★★★ | Go 写的自托管短信网关，**原生支持 macOS ARM64**，USB 4G dongle + AT 指令收发短信 + REST API。Mac 端短信引擎的代码结构可直接参考 |
| [wdoekes/asterisk-chan-dongle](https://github.com/wdoekes/asterisk-chan-dongle/blob/HEAD/README.md) | ★★★★ | USB dongle 打电话的经典方案：AT 信令控制通话（拨号/接听/挂断/来电显示）+ dongle 音频通道传语音。通话控制状态机可参考 |
| [mfairley/expo-callkit-telecom](https://github.com/mfairley/expo-callkit-telecom) | ★★★ | CallKit + PushKit VoIP 推送的完整实践：VoIP push 载荷格式、token 注册流程 |
| [nisfeb/talon iOS VoIP 文档](https://github.com/nisfeb/talon/blob/HEAD/docs/ios-voip.md) | ★★★ | iOS 端 PushKit + CallKit 落地的实战记录，含 Apple 合规要点 |
| [adiled/hum gsm-modem](https://github.com/adiled/hum/blob/HEAD/hives/gsm-modem/README.md) 等 | ★★ | AT 指令收发短信的极简实现，辅助参考 |

### 关键技术事实（来自 CellBridge 实测）

- DJI 一代 4G 模块 = **QDC507（Baiwang 固件），USB ID `2C7C:0125`，Qualcomm MDM9607**，nano-SIM 卡槽
- USB 枚举出 **4 个串口**：ttyUSB0=Diag、ttyUSB1=NMEA、ttyUSB2=AT（拨号/短信/配置）、ttyUSB3=Modem，以及一个 **USB Audio 设备**（语音 PCM 走这里）
- 出厂 adb 被 `AT+QADBKEY` 挑战锁住，但挑战-响应算法公开（固定密钥 `SH_adb_quectel`），可解锁并持久化
- 语音走 **VoLTE**，需 SIM 卡开通 VoLTE；短信走 `AT+CMGS`（发送）/ `+CMTI` URC（接收）
- iPhone 侧本质是一个 **SIP 客户端**：信令 SIP、语音 RTP（PCMU/8000），来电靠 **APNs VoIP 推送** 唤醒，Apple 硬性要求收到 VoIP push 后 **~5 秒内必须上报 CallKit**，否则会被收回推送权限

## 3. 系统架构（拟采用）

```
┌─────────────┐      Tailscale       ┌──────────────────────┐      USB       ┌──────────────────┐
│   iPhone    │◄────────────────────►│      Mac 服务端       │◄──────────────►│  DJI 4G 模块      │
│ SIP(pjsip)  │   SIP 5060/UDP       │  SIP server          │  AT 串口        │  QDC507 / MDM9607 │
│ PushKit+    │   RTP PCMU/8000      │  RTP↔USB音频桥接      │  USB Audio      │  nano-SIM        │
│ CallKit     │   APNs VoIP push     │  SMS 引擎(AT+CMGS)    │                │                  │
└─────────────┘                      └──────────────────────┘                └────────┬─────────┘
                                                                                    │ 4G/VoLTE
                                                                                    ▼
                                                                              运营商网络
```

- **呼叫流程（来电）**：模块 `+CLIP` 上报来电 → Mac 通过 APNs VoIP push 唤醒 iPhone → App 5 秒内 `reportNewIncomingCall` → 用户在原生来电 UI 接听 → Mac `ATA` 应答模块 → RTP 与 USB 音频双向桥接
- **呼叫流程（去电）**：App 内拨号（CallKit 去电 UI）→ SIP INVITE → Mac `ATD<号码>;` → 接通后同上桥接音频
- **短信流程（收）**：模块 `+CMTI` → Mac `AT+CMGR` 读短信 → APNs 普通推送到 App
- **短信流程（发）**：App → Mac（SIP MESSAGE 或 REST）→ `AT+CMGS` 发送

## 4. 技术选型（建议）

| 模块 | 选型 | 理由 |
|---|---|---|
| Mac 服务端 | **Swift**（备选 Go） | CoreAudio 接 USB 音频、Network 框架做 SIP/RTP 都是一等公民；Go 则胜在跨平台、可参考 sms-gateway |
| USB 串口 AT 控制 | SwiftSerial / ORSSerialPort（Swift）或 go-serial | macOS 下设备形如 `/dev/tty.usbmodem*`，需实测枚举形态 |
| iPhone App | **PJSIP + PushKit + CallKit**（原生 Swift） | 最成熟的 SIP 方案，CallKit 原生体验；备选 baresip（CellBridge 的 YakPhone 即用 baresip） |
| 内网穿透 | **Tailscale** | Mac 与手机直连、无 NAT，CellBridge 已验证；备选：Mac 公网 IP / 内网 Bonjour（仅同一 Wi-Fi） |
| 推送 | APNs（VoIP push + 普通 push） | 需 Apple Developer 账号、VoIP 推送证书/entitlement |

## 5. 功能范围

**MVP**：插上即用（自动识别 AT 串口）、短信收发双向推送、通话双向（来电 CallKit 响铃/去电）、通话中音频双向、挂断/未接来电记录
**二期**：通讯录同步、短信/通话历史持久化、多设备、USSD（如查话费）、信号状态展示、模块断线重连

## 6. 里程碑（顺序为"先软件、后硬件"）

1. **软件主体（FakeModem 联调基线）** —— ✅ 已完成（2026-09-30）：
   Go 网关（SIP/RTP/SMS/推送 stub/HTTP API/AT 抽象）+ iOS App（SIP 客户端/CallKit/PushKit/SwiftUI/xcodeproj），
   `go vet/build/test` 全过，HTTP 冒烟测试通过。详见 §11。
2. **真机联调** —— 待办：在 Xcode 真机编译 App，按 `docs/BUILD.md` §3 清单验收（注册/来电响铃/通话/短信/杀进程唤醒）。
3. **APNs 真实接入** —— 待办：VoIP 推送证书 + `internal/push` 真实实现（当前为控制台占位）。
4. **硬件 Spike** —— 软件验收通过后：模块插 Mac，确认 USB 串口 + 音频设备枚举（`2C7C:0125`？），`ATI`/`AT+CSQ` 调通。
5. **硬件接入** —— 实现 `internal/modem/serial.go`；CoreAudio ↔ RTP 音频桥接；模块侧语音路由按 CellBridge 流程处理。
6. **打磨**：重连、异常处理、Mac 菜单栏常驻形态、TestFlight 分发。

## 7. 风险与待验证项

1. **macOS 下模块的 USB 枚举形态未知**（CellBridge 在 Linux 上验证）。DJI 模块可能默认是 MBIM/RNDIS 网卡模式而非 AT 串口模式，需 `usb_modeswitch` 或 `AT+QCFG` 切换 —— Spike 阶段第一优先级验证
2. **确认手头的模块是哪一代**：一代 QDC507 可 hack；二代（Cellular Dongle 2）资料较少，若走官方模式可能无 AT 口
3. VoLTE 音频路由：CellBridge 用了模块侧 `mavo-pcm-bridge`（厂商二进制，非开源）；简化方案是直接用 USB UAC 音频 + `ATD/ATA`，需实测通话音频是否从 USB 音频口出
4. Apple 合规：VoIP push 必须 5 秒内上报 CallKit；App 若上架需合规的 VoIP 用途说明（自用 TestFlight/企业分发可规避）
5. 需要 Apple Developer 账号（APNs 推送）

## 9. 部署平台差异（2026-09-30 补充）

CellBridge 已确认支持 CallKit：来电流程为模块 `RING/+CLIP` → 网关发 SIP INVITE + 并行推送 `type=voip` → YakPhone 被 PushKit 唤醒后 **CallKit 接管响铃**（即使 App 被杀掉也能响）。注意：它的推送走 Yak 官方中转服务 `push.yakteam.com`；自研 App 则需自己搭建 APNs VoIP 推送。

网关在不同平台的部署差距主要来自 **USB 透传** 与 **音频子系统**：

| 平台 | 可行性 | 说明 |
|---|---|---|
| Linux 原生 | ✅ 官方验证 | `/dev/ttyUSB*`、ALSA（`arecord/aplay`）、adb、systemd 全套可用；CellBridge 的目标环境 |
| Linux + Docker | ✅ 可行 | `--device=/dev/ttyUSB2 --device=/dev/snd` 透传即可，仓库自带 Dockerfile |
| **macOS 原生** | ✅ 推荐（本项目） | 串口为 `/dev/tty.usbmodem*`（CDC-ACM 一般免驱，待 spike 验证）；**ALSA 代码需移植为 CoreAudio**（`internal/voice/alsa.go` 是唯一平台相关大块）；`insmod`/mavo 等操作经 adb 在模块侧执行，不受 macOS 影响 |
| macOS + Docker | ❌ 不可行 | Docker Desktop 跑在轻量虚拟机里，**不支持 USB 设备透传**，容器内看不到模块。只能网关跑宿主机原生 |
| Windows 原生 | ⚠️ 可行但摩擦多 | 串口为 COM 口（如 `COM13`），需安装 Quectel USB 驱动；音频走 WASAPI（可用 portaudio 封装）；电源管理可能挂起 USB，需配置 |
| Windows + Docker | ❌ 不可行 | 同 macOS，Docker Desktop 无 USB 透传（WSL2 可用 usbipd 折腾，但不推荐生产用） |

**结论**：本项目场景（模块插 Mac）→ 网关以 **Mac 原生常驻程序**（菜单栏 App / LaunchDaemon）形态交付，不考虑 Docker。跨平台抽象时把"串口 AT"和"音频采集/播放"做成接口，Linux/macOS/Windows 各实现一套。

## 10. 下一步

- [x] 参考 GitHub 相关项目（CellBridge 等）—— 已完成，见 §2
- [x] 建 monorepo（`mac-gateway/` + `ios-app/` + `docs/`）—— 已完成
- [x] 服务端语言决策：**Go**（跨平台、可复用 sms-gateway 思路；Mac 常驻形态后续再定）
- [x] iPhone App 决策：**原生 Swift，自研轻量 SIP 栈**（不用 PJSIP/baresip，协议子集自研可控）
- [x] 软件主体开发（网关 + App，FakeModem 基线）—— 已完成
- [ ] Xcode 真机编译 + fake 模式联调验收（`docs/BUILD.md` §3 清单）
- [ ] APNs 真实接入（VoIP 推送证书 + `internal/push` 实现）
- [ ] 软件验收通过后：模块插 Mac 做硬件 Spike（`system_profiler SPUSBDataType` 看是否为 `2C7C:0125`）
- [ ] 实现 `internal/modem/serial.go` + CoreAudio 音频桥接

## 11. 开发进展（2026-09-30，软件主体完成）

### mac-gateway（Go）

| 模块 | 文件 | 状态 |
|---|---|---|
| Modem 抽象 + AT 解析 + FakeModem | `internal/modem/` | ✅ 单测通过；`serial.go` 为硬件阶段占位 |
| SIP 服务端（REGISTER/digest/INVITE/BYE/CANCEL/MESSAGE/OPTIONS） | `internal/sip/` | ✅ 单测通过；INVITE 去重、200 OK 重发 |
| RTP + G.711 μ-law | `internal/rtp/` | ✅ 单测通过；fake 模式 loopback |
| SMS 引擎 | `internal/sms/` | ✅ |
| 推送（APNs 接口 + 未配置占位） | `internal/push/` | ⚠️ 待真实 APNs 实现 |
| 来电编排（push + INVITE 重试 5 次） | `internal/gateway/` | ✅ |
| HTTP API（status/messages/fake 触发器） | `internal/api/` | ✅ 冒烟测试通过 |
| 主程序 | `cmd/gatewayd` | ✅ `go vet/build/test` 全过 |

### ios-app（Swift）

| 模块 | 文件 | 状态 |
|---|---|---|
| SIP 消息/SDP 编解码 | `SIP/SIPMessage.swift` | ✅ |
| SIP 客户端（注册/来去电/短信/RTP 接入） | `SIP/SIPClient.swift` | ✅（待 Xcode 编译验证） |
| RTP/G.711 | `SIP/RTP.swift` | ✅（待 Xcode 编译验证） |
| CallKit（来电上报/接听/挂断/去电） | `Call/CallManager.swift` | ✅（待 Xcode 编译验证） |
| 音频桥（麦克风/扬声器 ↔ 8kHz PCM） | `Call/AudioBridge.swift` | ✅（待 Xcode 编译验证） |
| PushKit（VoIP 唤醒 → 重连 SIP） | `AppDelegate.swift` | ✅（待真机验证；5s 规则靠网关 INVITE 重试保障） |
| 状态协调/SMS 存储/SwiftUI 三页 | `AppState.swift` 等 | ✅（待 Xcode 编译验证） |
| Xcode 工程 | `CellBridgeApp.xcodeproj` | ✅ 引用完整性已脚本校验 |

### 文档

- `docs/PROTOCOL.md`：SIP/RTP/推送/HTTP API/Modem 抽象协议基线
- `docs/BUILD.md`：构建、fake 联调步骤、转硬件阶段 checklist

### 明确未完成（不宣称已打通）

真机编译、真实 APNs、真实模块 AT 串口、USB Audio/CoreAudio 桥接、模块侧语音路由（`qdc507` 相关）均未验证。
