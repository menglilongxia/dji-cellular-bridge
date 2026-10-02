# CellBridge：DJI 4G 模块 → Mac 网关 → iPhone

把大疆 4G 模块（DJI Cellular Dongle）插在 Mac 上，用它当一个"私人基站"：
Mac 跑一个 Go 网关，把这个号码的**短信**和**通话**实时转发到 iPhone，
来电直接走 **CallKit 原生全屏来电界面**——锁屏响铃、系统通话记录、
蓝牙耳机/车机免提，和普通电话没有区别。

> **English TL;DR** — Turn a DJI LTE dongle (QDC507) plugged into a Mac into a
> personal SMS/voice gateway for iPhone. `mac-gateway/` is a pure-Go SIP/RTP +
> AT-command daemon; `ios-app/` is a native SwiftUI SIP client using
> PushKit + CallKit. macOS only for now (no `/dev/tty.*` on macOS, so AT goes
> through a user-space IOKit USB driver). Docs are in Chinese.

```
┌─────────────┐    SIP 5060/UDP     ┌──────────────────────┐     USB      ┌────────────────┐
│   iPhone    │◄───────────────────►│        Mac           │◄────────────►│  DJI 4G 模块    │
│ SIP + RTP   │    RTP PCMU/8000    │  gatewayd (Go)       │  AT 口(IF3)  │  QDC507        │
│ PushKit +   │                     │  ├ SIP server        │  URC 口(IF2) │  MDM9607       │
│ CallKit     │◄─── APNs VoIP ──────│  ├ RTP ↔ CoreAudio   │  UAC 音频    │  nano-SIM      │
└─────────────┘                     │  └ ADB → 模块语音路由 │  ADB 口(IF6) │                │
                                    └──────────────────────┘              └───────┬────────┘
                                                                                │ 4G / VoLTE
                                                                                ▼
                                                                          运营商网络
```

## 它能做什么

| 功能 | 说明 |
|---|---|
| 📞 **来电** | 模块 `RING`/`+CLIP` → 网关发 SIP INVITE → iPhone 原生全屏来电界面，`ATA` 应答后双向语音 |
| 📱 **去电** | App 内拨号 → 网关 `ATD<号码>;` |
| 💬 **短信** | 双向收发，App 内按号码分线程；网关侧 `AT+CMGS` / `+CMTI`+`AT+CMGR` |
| 📇 **联系人** | 来电页面与通话记录显示通讯录姓名 |
| 🕘 **历史** | 通话记录（已接/未接/失败）+ 短信记录，持久化到磁盘 |
| 🔊 **语音桥** | RTP（G.711 μ-law 8 kHz）↔ 模块 USB 声卡（CoreAudio），采样率转换在网关内完成 |
| 🔌 **模块侧语音路由** | 网关内嵌 ADB 客户端，通话前自动在模块里 `insmod` + 起 `mavo-pcm-bridge`，实测约 2.7 s |
| 💸 **零苹果费用方案** | 没有付费 Apple 账号时，来电可以用模块 SIM 给你的手机发一条"有来电"短信提醒，再把 INVITE 重试窗口延长到约 60 s |
| 🧪 **假模块联调** | `--modem=fake` 不需要任何硬件，可以先把整条软件链路跑通 |

## 它目前**不能**做什么（诚实版）

这个项目的硬件部分只在一台机器、一块模块上验证过，请先看清楚边界：

- ❌ **没有端到端"稳定可日用"**：语音链路已经逐跳打通到"模块把真实语音送进 UAC、
  网关双向计量正常"，但**最后一步"通话中双方都能听清"仍需要你打一通真电话确认**。
- ❌ **免费 Apple ID 走后无法被推送唤醒**：免费签名拿不到 `aps-environment`，
  App 被系统彻底杀掉之后收不到 VoIP push。降级方案是短信提醒（见上）。
- ❌ **只在 macOS 上验证过**：而且是一台 Intel 平台、macOS 13.7 Ventura 的黑苹果。
  Apple Silicon 理论上可用，但没有实测。
- ❌ **模块侧的语音运行时不包含在本仓库里**：那是厂商二进制
  （`mavo-pcm-bridge.armv7` 等），仓库只提供获取方式与校验值。
- ❌ **APNs 只有接口没有实现**：`internal/push` 里是接口 + 控制台占位，
  接真实推送需要你自己填 `.p8` 密钥与 device token。
- ⚠️ **模块固件会被改写**：为了打开 USB 音频（UAC）与 ADB，需要写 `usbcfg`
  并重启模块。**这是可逆的**，回滚命令在
  [`docs/HARDWARE-SPIKE.md`](docs/HARDWARE-SPIKE.md) 里，但请自行评估风险。

## 需要什么

- **一块大疆 4G 模块**（一代 Cellular Dongle，模块内部型号 `QDC507`，
  白牌固件 `Baiwang`，主控 Qualcomm MDM9607）+ 一张已开通 **VoLTE** 的 nano-SIM
- **一台 Mac**（网关要常驻运行；本项目在 Intel + macOS 13.7 上验证）
- **一部 iPhone**（iOS 16+，CallKit/PushKit 不支持模拟器）
- 可选：**Tailscale**（Mac 和 iPhone 不在同一局域网时最省事的方案）
- 可选：**付费 Apple 开发者账号**（想用真正的 VoIP 推送才需要，¥688/年）

## 快速开始

```bash
git clone https://github.com/menglilongxia/dji-cellular-bridge.git
cd dji-cellular-bridge
```

### 0. 先把软件链路跑通（不需要硬件）

```bash
cd mac-gateway
go vet ./... && go build ./... && go test ./...

go run ./cmd/gatewayd --modem=fake --sip=:5060 --api=:8787 \
  --user=iphone --pass=changeme
```

另开一个终端模拟一条来电和一条短信：

```bash
curl localhost:8787/healthz
curl localhost:8787/api/v1/status
curl -XPOST localhost:8787/api/v1/fake/incoming-call -d '{"from":"13800000100"}'
curl -XPOST localhost:8787/api/v1/fake/incoming-sms  -d '{"from":"13800000100","body":"hello"}'
```

App 装好后，「状态」页填 Mac 的 IP（Tailscale 的 `100.x.x.x` 或局域网 IP），
点「连接」，看到「已注册」就能收到上面这两条。FakeModem 模式下语音是回声，
对着手机说话会听到自己。

### 1. 配置模块（这一步会改模块的 USB 配置）

出厂状态下模块的 USB 是 `2CA3:0000`、**没有 UAC 音频也没有可用的 ADB**，
macOS 也拿不到 `/dev/tty.*`（AT 口是厂商自定义 class，没有系统驱动）。
所以需要先写一次 `usbcfg`，把模块切成"全功能"组合：

```text
AT+QCFG="usbcfg"?                              → 读出出厂值，先记下来
AT+QADBKEY?                                    → 拿到挑战值，配合公开密钥 SH_adb_quectel 解锁 adb
AT+QCFG="usbcfg",0x2C7C,0x0125,1,1,1,1,1,1,1   → 写入全功能组合（开 UAC + ADB）
AT+CFUN=1,1                                    → 重启模块，重新枚举 USB

# 回滚到出厂：
# AT+QCFG="usbcfg",0x2CA3,0x0,1,1,1,1,1,0,0
# AT+CFUN=1,1
```

macOS 上怎么发 AT：用仓库里的用户态 USB 工具（需要 Xcode Command Line Tools）：

```bash
xcrun clang -fobjc-arc -framework IOKit -framework Foundation -o atprobe tools/atprobe.m
./atprobe 3 $'ATI\r'        # 接口 3 = AT 命令口
```

完整过程（接口布局、实测返回值、踩过的坑）见
[`docs/HARDWARE-SPIKE.md`](docs/HARDWARE-SPIKE.md)。

另外通话音频还要求 **IMS/VoLTE 打开**：

```text
AT+QCFG="ims",1        # 读回应为 +QCFG: "ims",1,1
AT+CFUN=1,1
```

### 2. 运行网关（接上真实模块）

```bash
cd mac-gateway
go build -o "$HOME/Library/Application Support/CellBridge/gatewayd" ./cmd/gatewayd

"$HOME/Library/Application Support/CellBridge/gatewayd" \
  --modem=usb --sip=:5060 --api=:8787 --user=iphone --pass=changeme \
  --coreaudio --loopback-rtp=false --audio-device=BAIWANG \
  --voice-adb --voice-files="$HOME/Library/Application Support/CellBridge/module-voice" \
  --module-ip=192.168.225.1
```

`--voice-files` 指向模块侧语音运行时（三个厂商文件）。它们不在本仓库里，
可以从上游项目按 commit 取回并核对 SHA-256，细节见
[`docs/HARDWARE-SPIKE.md` §9](docs/HARDWARE-SPIKE.md)。

**权限提醒**：网关要采集模块的 USB 声卡，macOS 会拦在麦克风权限后面；
未授权时 CoreAudio **不报错、只返回全 0**。用固定签名身份部署可以避免
每次重编译都要重新授权：

```bash
tools/deploy-gateway.sh        # 编译 + 用稳定的 Apple Development 证书签名 + 重启
curl -XPOST localhost:8787/api/v1/audio/probe -d '{"device":"Built-in Microphone","seconds":10}'
```

### 3. 编译安装 iPhone App

```bash
# 普通情况：用 Xcode 打开、选你的 Team、Run
open ios-app/CellBridgeApp.xcodeproj

# 免费 Apple ID + macOS Ventura 的完整踩坑方案 + 一键脚本：
#   docs/IOS-BUILD-ON-VENTURA.md
TEAM_ID=你的团队ID DEVICE_UDID=你的设备UDID tools/build-ios.sh
```

免费签名首次启动要在 **设置 → 通用 → VPN 与设备管理** 里信任证书；
免费签名 7 天过期，重新 Run 一次即可续。

## 网关参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `--modem` | `fake` | `fake` 用假模块联调；`usb` 走用户态 IOKit USB 驱动 |
| `--sip` / `--api` | `:5060` / `:8787` | SIP 与 HTTP 监听地址 |
| `--user` / `--pass` | `iphone` / `changeme` | SIP 单账号凭据，**请改掉** |
| `--api-token` | 空 | HTTP API 的 Bearer token；为空则免鉴权（仅开发用） |
| `--advertise-ip` | 自动 | SDP/Via 里公布的 IP，多网卡时手动指定 |
| `--coreaudio` | `false` | 把通话音频桥到模块 USB 声卡（需要 `loopback-rtp=false`） |
| `--audio-device` | `BAIWANG` | 匹配 CoreAudio 设备名的子串 |
| `--loopback-rtp` | `true` | 回声模式；接真机时设 `false` |
| `--voice-adb` | `false` | 用网关内嵌的 ADB 自动做模块侧语音路由（推荐） |
| `--voice-files` | 空 | 模块侧语音运行时目录 |
| `--voice-script` | 空 | 备选：外挂脚本做语音路由（`start`/`stop`） |
| `--module-ip` | `192.168.225.1` | 模块在 USB ECM 网卡上的地址 |
| `--alert-phone` | 空 | 免费方案：来电时用模块 SIM 发短信提醒这个号码 |
| `--data-dir` | `~/Library/Application Support/CellBridge` | 历史记录等持久化目录 |
| `--fake-call-after` / `--fake-sms-after` | `0` | 假模块：N 秒后自动来一通电话/一条短信（演示用） |

## HTTP API

`GET /healthz` · `GET /api/v1/status` · `GET /api/v1/messages` ·
`POST /api/v1/messages`（`{"to","body"}`）· `GET /api/v1/history{,/calls,/sms}` ·
`POST /api/v1/modem/at`（直接下发 AT）·
语音路由：`GET /api/v1/voice`、`GET /api/v1/voice/check`、
`POST /api/v1/voice/{start,stop,recover}`、`GET /api/v1/voice/diag` ·
音频诊断：`GET /api/v1/audio/devices`、`POST /api/v1/audio/probe` ·
假模块触发：`POST /api/v1/fake/incoming-{call,sms}`

协议细节（SIP 信令、RTP、推送载荷、AT 映射表）见
[`docs/PROTOCOL.md`](docs/PROTOCOL.md)。

## 目录结构

```
├── mac-gateway/                  Go 网关（纯 stdlib，无第三方依赖）
│   ├── cmd/gatewayd/             主程序
│   ├── cmd/atcli/                交互式 AT 终端
│   ├── cmd/qdc507-adb/           模块调试 CLI（shell/check/start/stop/recover）
│   ├── cmd/qdc507-push/          早期 libusb 版 ADB 实现（build tag，默认不编译）
│   ├── cmd/voiceprobe/           音频链路探针
│   └── internal/
│       ├── modem/                Modem 抽象 + AT 解析 + FakeModem + IOKit USB 驱动
│       ├── sip/                  SIP 服务端（REGISTER/digest/INVITE/BYE/CANCEL/MESSAGE）
│       ├── rtp/                  RTP 组帧 + G.711 μ-law
│       ├── voice/                CoreAudio ↔ 8 kHz PCM 桥接
│       ├── adb/                  纯 Go 的 ADB 协议（含 3 个非标准坑的适配）
│       ├── voiceroute/           模块侧语音路由的完整 provision 流程
│       ├── gateway/              控制器：来电编排、短信路由、音频桥接
│       ├── sms/ · push/ · history/ · api/
├── ios-app/                      原生 SwiftUI App（自研轻量 SIP 栈，不用 PJSIP）
│   └── CellBridgeApp/{SIP,Call,SMS,Contacts,History,UI}/
├── tools/                        atprobe.m · build-ios.sh · deploy-gateway.sh · tss-proxy.py
└── docs/                         PROTOCOL · BUILD · HARDWARE-SPIKE · IOS-BUILD-ON-VENTURA · DESIGN
```

## 踩坑记录（值得单独一读的部分）

这个项目最大的价值可能不在代码量，而在
[`docs/HARDWARE-SPIKE.md`](docs/HARDWARE-SPIKE.md) 里那一长串"看起来一切正常、
但就是没声音"的排查过程。几个比较有代表性的：

- macOS 上模块**没有 `/dev/tty.*`**，AT 必须走用户态 IOKit USB；
- 这个模块的 adbd **会校验 ADB 头的 payload checksum**，填 0 的 CNXN 被静默丢弃，
  表现就像"adbd 没在运行"；
- `SendSMS` 里不可重入的 `sync.Mutex` 把整条 AT 通道锁死，顺带把 HTTP API 也拖死；
- macOS 未授权麦克风时 CoreAudio **不报错、只给全 0**，和"模块没送音频"完全同形；
- `AT+QCFG="ims"` 是**两个字段**，IMS 关掉时 VoLTE mixer 根本不参与通话，
  于是"路由全绿但全静音"；
- iOS 的**后台创建 UDP socket 会被静默拒绝**（Apple TN3179），
  所以媒体 socket 必须在 App 前台就建好并跨通话复用。

## 参考项目

- [**mccding/CellBridge**](https://github.com/mccding/cellbridge) —— 同款模块、
  已在 Linux 真机端到端验证过，本项目的架构与模块侧解锁流程主要参考它。
- [moluncn/mavo](https://github.com/moluncn/mavo) —— 模块侧语音运行时（厂商二进制）的来源。
- [mattboston/sms-gateway](https://github.com/mattboston/sms-gateway) —— Go 自托管短信网关。
- [wdoekes/asterisk-chan-dongle](https://github.com/wdoekes/asterisk-chan-dongle) ——
  USB dongle 打电话的经典方案。

## 免责声明

本项目与 DJI / 大疆无任何关系，是一个非官方的个人折腾项目。
改写模块的 `usbcfg`、加载厂商内核模块、解锁 ADB 都可能让设备进入
需要重启甚至重刷才能恢复的状态；请自行评估风险，并自行确保使用方式
符合当地法律法规与运营商条款。仓库不包含任何厂商二进制文件。

## License

[MIT](LICENSE)

---

> 仓库：<https://github.com/menglilongxia/dji-cellular-bridge> ·
> 发布包：[Releases](https://github.com/menglilongxia/dji-cellular-bridge/releases)
