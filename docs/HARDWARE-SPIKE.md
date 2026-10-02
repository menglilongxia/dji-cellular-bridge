# 硬件 Spike 记录：DJI 4G 模块（QDC507）

> 日期：2026-09-30。结论：**模块本身可用且已注册运营商，但 macOS 下没有现成的 AT 串口**，
> 需要用户态 USB 驱动才能发 AT。本文是 todo #1 的答案。

> **2026-10-01 更新（重要）**：模块的 USB **组合模式已被改写成"全功能"**，
> USB ID 从出厂的 `2CA3:0000` 变成 `2C7C:0125`，并且**打开了 UAC（USB 音频）与 ADB**。
> 详见文末 §7。AT 口位置（IF3/IF2）不变，驱动只是多认了一个 VID。

## 1. 环境

- 主机：AMD Ryzen 7 4800H 的 Hackintosh，macOS 13.7 Ventura（22H123，Intel 内核）
- 模块：DJI 4G 模块（nano-SIM，实测已插中国移动卡）

## 2. USB 枚举结果

`system_profiler SPUSBDataType` / `ioreg`：

- 产品名 `Baiwang`，制造商 `BAIWANG`
- **`idVendor = 0x2CA3`，`idProduct = 0x0000`**（注意：**不是** CellBridge 参考里的 `2C7C:0125`）
- `bDeviceClass=0xEF / bDeviceSubClass=2 / bDeviceProtocol=1` → IAD 复合设备

完整的配置描述符（6 个接口，`wTotalLength=258`）：

| 接口 | 类别 | 端点 | 实测用途 |
|---|---|---|---|
| IF0 | 255/255/255 | bulk IN 0x81 / bulk OUT 0x01 | 未测（推测 DIAG/QCDM） |
| IF1 | 255/0/0 | intr IN 0x83 / bulk IN 0x82 / bulk OUT 0x02 | 发 AT 无响应（推测 NMEA/QCDM） |
| IF2 | 255/0/0 | intr IN 0x85 / bulk IN 0x84 / bulk OUT 0x03 | **URC 口**（`RDY`/`+CPIN`/`+CTZE`…） |
| IF3 | 255/0/0 | intr IN 0x87 / bulk IN 0x86 / bulk OUT 0x04 | **AT 命令口**（回显 + `OK`） |
| IF4 | 2/6/0 (CDC-ECM) | intr IN 0x89 | AppleUserECM |
| IF5 | 10/0/0 (CDC Data) | bulk IN 0x88 / bulk OUT 0x05 | → `en2` |

## 3. 关键结论（与立项书假设的差异）

1. **USB ID 与 CellBridge 参考不同**：`2CA3:0000`，不是 `2C7C:0125`。
2. **出厂即 ECM 复合模式**：模块自带 CDC-ECM 网卡，`en2` 拿到 `192.168.225.20`，网关 `192.168.225.1`。
   macOS 的 `AppleUserECM` 只绑定了 ECM 功能。
3. **AT 口没有 `/dev/tty.*`**：IF0–IF3 都是 class 255（厂商自定义），macOS 无对应驱动，
   所以**不会**出现 `/dev/tty.usbmodem*`。这正是立项书 §7 风险 #1 的实锤。
4. **AT 不走网络**：扫描 `192.168.225.1` 全部 1–10000 端口，只有 53(DNS) 开放，没有 Web/AT-over-TCP 服务。
5. **模块侧没有 USB Audio**：描述符里没有 class 1 (audio) 接口，语音通道形态仍未确认（见 §6）。

## 4. AT 通道验证

macOS 没有驱动 → 用 IOKit/IOUSBLib 写了一个用户态探测工具：`tools/atprobe.m`。

```bash
xcrun clang -fobjc-arc -framework IOKit -framework Foundation -o atprobe tools/atprobe.m
./atprobe            # 只列描述符
./atprobe 3 $'AT\r'  # 在接口 3 上发一条 AT
```

实测（接口 3）：

| 命令 | 返回 |
|---|---|
| `ATI` | `Baiwang` / `QDC507` / `Revision: QDC507GLEFM21` |
| `AT+CGMM` | `QDC507` |
| `AT+CGMI` | `Baiwang` |
| `AT+CSQ` | `+CSQ: 25,99`（信号良好） |
| `AT+CREG?` | `+CREG: 0,1`（已注册本地网） |
| `AT+COPS?` | `+COPS: 0,0,"CHINA MOBILE",7`（LTE） |
| `AT+CMGF?` | `+CMGF: 0`（PDU 模式，需切 1 用文本模式） |
| `AT+CPMS?` | `+CPMS: "SM",0,50,"ME",0,23,"ME",0,23`（SIM 50 槽） |

接口 2（URC 口）开机时观察到：`RDY` / `+CPIN: READY` / `+QUSIM: 1` / `+CFUN: 1` /
`+QIND: SMS DONE` / `+QIND: PB DONE` / `+CTZE: "+32",0,"2026/09/30,12:37:49"`（+32 刻钟 = UTC+8）。

**即：SIM 就绪、已注册 LTE、短信存储可用。**

## 5. 对网关实现的影响

- `internal/modem/serial.go` 在 macOS 上**不能**用 `os.Open("/dev/tty…")` 实现。
  需要一个 USB AT 传输层：
  - 命令：接口 3，bulk OUT `0x04` / bulk IN `0x86`
  - URC：接口 2，bulk IN `0x84`（另有中断 IN `0x85`）
- 落地方式二选一（推荐后者）：
  1. Go + cgo 直接调 IOKit（构建复杂、依赖 CLT）
  2. 独立 helper 进程（`atprobe` 演进版）暴露 Unix domain socket / 管道，Go 侧只做协议
- `atprobe` 里的关键实现细节（踩过的坑）：必须先 `USBDeviceOpen`，
  再 `USBInterfaceOpen` + `SetAlternateInterface(0)`，写入要用 `WritePipeTO` 带超时，
  否则 bulk OUT 会被无限 NAK 卡死。

## 6. 仍未验证

- **短信端到端**：`AT+CMGS` / `+CMTI` 只是接口可用，尚未在 macOS 链路上真正收发过。
- IF0 / IF1 的确切用途（DIAG vs NMEA）未逐一确认。

## 7. USB 组合改写（2026-10-01）：拿到 USB 音频

### 背景

上一版结论是"模块没有 USB Audio 接口"。CellBridge（同款模块、Linux 上端到端验证过）的
README 指出：**UAC 位在出厂配置里是关闭的**，需要改写 usbcfg 并重启模块才会出现声卡。

### 实测过程

```text
# 1. 读出厂配置（注意最后两位：ADB=0, UAC=0）
AT+QCFG="usbcfg"?   →  +QCFG: "usbcfg",0x2CA3,0x0,1,1,1,1,1,0,0

# 2. 解锁 adb（QADBKEY 挑战应答，密钥是公开的固定值 SH_adb_quectel）
AT+QADBKEY?         →  +QADBKEY: 72404070
openssl passwd -1 -salt 72404070 SH_adb_quectel   → 取 22 位哈希段
AT+QADBKEY="<22位>" →  OK（持久生效，只需一次）

# 3. 写入全功能组合并重启射频/USB
AT+QCFG="usbcfg",0x2C7C,0x0125,1,1,1,1,1,1,1   →  OK
AT+CFUN=1,1                                      →  模块重新枚举
```

回滚值（出厂）：`AT+QCFG="usbcfg",0x2CA3,0x0,1,1,1,1,1,0,0` + `AT+CFUN=1,1`

### 改写后的接口布局（实测）

| 接口 | 类型 | 用途 |
|---|---|---|
| IF0 | 255/255/255 | DIAG |
| IF1 / IF2 / IF3 | 255/0/0 | NMEA / **URC** / **AT**（位置不变） |
| IF4 / IF5 | CDC-ECM | 网卡（en2 仍在） |
| **IF6** | 255/66/1 | **ADB** |
| **IF7** | class 1/1 | UAC 音频控制 |
| **IF8** | class 1/2 alt1, iso **IN 0x8b** | 模块 → Mac（麦克风方向） |
| **IF9** | class 1/2 alt1, iso **OUT 0x07** | Mac → 模块（扬声器方向） |

macOS 把它们暴露成**两个音频设备**（不是同一个设备的两端）：

```
AC Interface   in=1 out=0     ← 捕获
AS Interface   in=0 out=1     ← 播放
```

厂商字段是 `BAIWANG`，设备名里没有——匹配时要同时看 manufacturer。

### 语音桥接（已实现）

`internal/voice/`（macOS CoreAudio，cgo）负责 RTP ↔ 模块声卡：

- 用 AUHAL，**客户端格式 8 kHz/mono/Int16**，由 CoreAudio 负责与设备 48 kHz 的转换
- 捕获必须用 `kAudioOutputUnitProperty_SetInputCallback` + `AudioUnitRender`
  主动拉数据（用 SetRenderCallback 会得到一个"能启动但永不回调"的 unit）
- 客户端格式要设在**捕获 bus 1 的 output scope** / 播放 bus 0 的 input scope，设错同样静默无数据
- 用 `gatewayd --coreaudio --loopback-rtp=false --audio-device=BAIWANG` 启用；
  否则网关仍是 RTP 回声模式

实测（无通话时）：`captured 24576 frames in 3.4s ≈ 7283 frames/s`，静音（peak 0）——通道是活的。

### 仍未确认

**通话时模块是否真的把蜂窝语音路由到这个 USB 声卡。** CellBridge 的做法是在模块侧
`insmod qdc507_voice.ko` 并运行 `mavo-pcm-bridge --voice-route-session`（VoLTE 路由），
这些是**厂商二进制，不在开源仓库里**。所以：

- 如果模块默认把语音送到 UAC → 现在就能听到声音
- 如果不行 → 需要在模块侧加载厂商组件（需要先拿到那些文件；ADB 已解锁，具备推送条件）

另外 macOS 版 adb（35.0.2）**认不出** `2C7C:0125`（其内置 VID 白名单不含该 VID），
所以暂时进不了模块系统；后续要么换 VID、要么用别的 ADB 客户端。

## 8. 来电去重（2026-10-01 修复）

真实模块在对方拨入期间会**每 4–6 秒重发一次 `RING`**（标准行为，直到接听）。
最初驱动把每个 RING 都当成新来电，导致一通来电触发 5 个 SIP INVITE、App 收到 5 通"来电"。
现在 `ringing` 状态会去重：第一个 RING 起一通电话，重复的忽略；
`NO CARRIER/BUSY/NO ANSWER`、`ATA`、`ATH`、`ATD` 或 90 秒看门狗都会清除该状态。

## 9. 模块侧语音运行时（2026-10-01 下午，参考 DJOneHub 后的发现）

### 三个厂商文件是公开可取的

DJOneHub（`ZenGeekLabs/DJOneHub`、以及带通话功能的 `rogerbush007-a11y/DJOneHub-mac-enhanced`）
把模块侧通话运行时**放在仓库外**，运行时从上游 `moluncn/mavo` 的固定 commit 下载并逐一校验 SHA-256：

```
https://raw.githubusercontent.com/moluncn/mavo/0443dfdaf8aec086fd76ba2ee9152fd908114524/Resources/ModuleVoice/
  qdc507_aprv3.ko        sha256 3d82d3de…   644
  qdc507_voice.ko        sha256 ed382168…   644
  mavo-pcm-bridge.armv7  sha256 88d47c15…   755
```

已下载并核对通过，存放在 `work/module-voice/`。

### 模块侧流程（从 DJOneHub 源码读出）

```text
adb push 三个文件 → /tmp/mavo-call
insmod /tmp/mavo-call/qdc507_aprv3.ko
insmod /tmp/mavo-call/qdc507_voice.ko
mavo-pcm-bridge.armv7 --voice-route-session      # VoLTE PCM → UAC
依赖 /dev/snd/controlC0、pcmC0D4p/c、pcmC0D5p、pcmC0D6c（内核 3.18.44）
路由 PID：/run/mavo-voice-route.pid
```

只在通话激活时启动路由（他们注释：媒体路由要等 `CLCC` 报告通话已接通再开）。

### 他们的其它关键点

- 初始化序列：`ATE0` / `AT+CMGF=0` / `AT+CNMI=2,1,0,0,0` / `AT+CLIP=1` / **`AT+QPCMV=1,2`**
  （最后一条是"开启 UAC 语音模式"）——**但本模块固件对 `AT+QPCMV` 返回 ERROR**，
  所以我们也只能走模块侧运行时那条路。
- 要求 VoLTE 保持开启（校验 `+QCFG: "volte/disable",0`）并 `AT+QCFG="ims",1`。
- USB 组合与我们已经设置的一致：`0x2C7C,0x125,1,1,1,1,1,1,1`。
- 他们用 **libusb**（不是 IOKit 旧接口）操作 USB，并自己实现了 ADB-over-USB（系统 adb 不认这个 VID）。

### 仍然卡住：进不去模块系统

push/insmod 全都要 ADB，而 **本模块的 adbd 不响应**：

- IOKit 旧接口实现：CNXN 发出、读全部超时
- libusb 实现（自己编译的 libusb 1.0.27）：同样 `CNXN write -> 0`，读全部 `LIBUSB_ERROR_TIMEOUT`
- **用他们自己的工具**（在 Intel 上从源码编译成功）也一样：进程阻塞在
  `libusb_bulk_transfer`，网页界面起不来

### 与"刷过 VoHive"的关系（重要背景）

这块模块**之前刷过 VoHive**。而 VoHive 是主机侧软件（跑在 Linux/VM 上），
其 Mac 教程明确是把大疆 4G 模块（**EG25-G**）"伪装成 Quectel EC25"后部署。
结合本模块的异常现象（出厂 ID `2ca3:0000` 而非 `2ca3:4006`、`AT+QPCMV` 不被支持、
adbd 不响应），很可能这块模块的**固件/配置已被改写**，因此与 DJOneHub/CellBridge
文档里假设的标准状态不一致。

**下一步的关键问题**：当初是用什么设备/什么方式驱动这块模块跑 VoHive 的
（Linux 主机？树莓派？虚拟机？串口？）——那条路径才是本模块真正的可用入口。

## 10. 突破：ADB 打通，模块侧语音运行时已装好（2026-10-01 下午，已验证）

### ADB 一直不响应的真正原因（两个坑，都不是模块的问题）

参照 CellBridge 自己的 macOS 实现（`tools/module-provisioner/qdc507-adb-readonly`，
`darwin && cgo` + libusb）逐项对比后，发现我们的 ADB 客户端缺了两样：

1. **ADB 消息头里的 payload 校验和不能填 0**。这个模块的 adbd 会校验，
   填 0 的 CNXN 被**静默丢弃**（发的出去、永远没有回应，表现就像"adbd 没在跑"）。
   头 24 字节：`cmd / arg0 / arg1 / len / sum(payload) / cmd^0xffffffff`。
2. **头与载荷必须分成两次 USB bulk 传输写**。一次性写（我和 DJOneHub 的做法）
   同样得不到任何回应。

另外确认：`adbVersion` 应为 `0x01000000`（不是 0x01000001），banner 用 `host::`。

实现见 `mac-gateway/cmd/qdc507-push`（`darwin && cgo`，libusb）。
ADB 会话没干净关闭时模块会卡住，用 CellBridge 工具的 `--reset-usb` 复位即可恢复。

### 文件怎么传进去的（绕开 ADB sync push）

`sync:` 写入路径调试成本高，改用**模块反向拉取/反向推送**：

- 模块能通过蜂窝网出去，但**到不了 Mac**（模块侧防火墙：`ping 192.168.225.28` 100% 丢包）
- 反过来 **Mac → 模块是通的**（同网段，`bridge0 = 192.168.225.1`）

所以让**模块监听、Mac 主动连**：

```bash
# 模块侧（经 ADB shell 启动监听）
cd /tmp/mavo-call && busybox nc -l -p 9101 > mavo-pcm-bridge.armv7
# Mac 侧
nc -w 8 192.168.225.1 9101 < mavo-pcm-bridge.armv7
```

三个文件 MD5 逐一核对一致（17860 / 36664 / 999236 字节）。

### 模块侧运行结果（实测）

```text
insmod qdc507_aprv3.ko   → rc=0
insmod qdc507_voice.ko   → rc=0
lsmod → qdc507_voice (857364) + qdc507_aprv3 (148080) 均 Live

声卡出现：mdm9607-tomtom-i2s-snd-card
/dev/snd：controlC0  pcmC0D0-4(c/p)  pcmC0D5p  pcmC0D6c   ← 与 CellBridge 要求的完全一致

mavo-pcm-bridge.armv7 --check
  → all required symbols are available in libql_lib_audio.so.1
mavo-pcm-bridge.armv7 --voice-route-session
  → pcm_open /dev/snd/pcmC0D4c  (id='VoLTE (*)')
  → pcm_open /dev/snd/pcmC0D4p
```

**即：模块侧语音运行时已安装、内核模块已加载、VoLTE PCM 已接到 UAC。**
剩下就是在真实通话中验证 Mac 端能否从 UAC 收到/送入音频（网关的 CoreAudio 桥接已就绪）。

### 模块系统信息（供参考）

```text
Linux mdm9607 3.18.44 #1 PREEMPT Thu Jun 20 11:08:23 CST 2024 armv7l
rootfs 只读（ubifs），可写区在 /tmp、/data 等
可用工具：busybox 1.23.2（curl / wget / nc / base64 / insmod / alsaucm_test …）
音频库：libql_lib_audio.so.1（已存在）
```

## 11. ADB 并进网关进程（2026-10-01 晚，已实现并实测）

### 为什么之前的自动 provision 一定失败

旧方案里 ADB 是**另一个进程**（`cmd/qdc507-push`，libusb）。实测这个组合根本不可能稳定工作：

```text
# 网关在跑时，任何第二个进程打开模块：
USBDeviceOpen -> 0xe00002c5 (kIOReturnExclusiveAccess)
```

libusb 内部用 `USBDeviceOpenSeize`，它会**把设备从网关手里抢走**——于是网关的 IOKit
AT/URC 管道立刻变成 `ReadPipeTO 0xe00002eb`（此后一直读失败、再也不恢复），而抢来的
ADB 会话又会把 adbd 弄卡。日志里那句 `{"status":"BLOCKED_USB_RESET"}`
就是"谁都动不了设备"的现场。

**结论：ADB 必须在网关进程内做**（它本来就独占这个 USB 设备）。

### 现在的实现

| 位置 | 作用 |
|---|---|
| `internal/adb/` | ADB 协议（纯 Go，无 cgo；可用假 transport 单测） |
| `internal/modem/usbat_darwin.c/h` | 同一 IOKit 句柄上多认一个 ADB 接口（class `0xff`/sub `0x42`/proto `0x01`，IF6） |
| `internal/modem/usbatmodem_darwin.go` | 「ADB transport」+ `ADBClient()` + `RecoverADB()` |
| `internal/voiceroute/` | 模块侧语音路由的完整 provision 流程 |
| `cmd/qdc507-adb` | 调试 CLI：`shell / check / start / stop / recover`（不再需要 libusb） |

`cmd/qdc507-push`（libusb 版）保留为**协议参考实现**，但已加 build tag，默认不编译：
`go build -tags qdc507libusb ./cmd/qdc507-push`。它和网关**不能同时跑**。

### 又踩到的三个坑（都是这次实测出来的）

1. **「命令没有输出」≠「服务被拒绝」。** adbd 接受服务时先发 `OKAY`，跑完再发 `CLSE`；
   服务被拒绝时**只有 `CLSE`**。早先的实现（包括本仓库第一版）把
   `mkdir`/`pkill`/`true` 这种无输出命令一律当成「adbd 卡死」，于是每通电话都误判、
   还触发了不必要的复位。判定必须看「有没有收到过 OKAY」。
2. **`pkill -f mavo-pcm-bridge` 会杀掉正在跑这条命令的 shell 自己**——因为那一整段
   脚本就是它的命令行。必须用 pid 文件（脚本里写 `$$` 到 `/tmp/cb-*.pid`）或
   `killall`（按进程名匹配，不会匹配 `sh`）。这个坑让 provision 在第一行 `pkill`
   处静默死掉，表现为「几毫秒就结束、什么都没做」。
3. **模块只有 43 MB 内存**（`MemTotal: 43460 kB`，空闲常常只有 1–2 MB）。每次
   `adb shell` 都要 fork 一个 `sh`，所以**每通电话的 provision 只用一条 shell 命令**
   （脚本内部自己等 FIFO、等校准），而不是十来条往返。文件推送只在文件缺失/校验不符时做
   （每次模块重启一次），用 netcat：**模块 listen、Mac dial**（`192.168.225.1`）。
   监听用 `busybox nc -w 25 -l -p <port>`，会自己超时退出，不会在模块上积垃圾进程。

### adbd 卡死与恢复

adbd 仍会卡：**CNXN 正常应答，但所有 `shell:` 打开都被立刻 `CLSE`**（shell 起不来）。
这不是 USB 问题，复位 USB 也没用；实测**重启模块**才恢复。

恢复阶梯（`USBATModem.RecoverADB`）：

1. 先发一条 `shell: true` 探活（很多情况其实没坏）
2. 坏了 → 走 ADB 的 **`reboot:` 服务**（它不 fork，所以在 `shell:` 全被拒时**照样能用**），
   等同重启模块
3. 连 `reboot:` 都发不出去 → 兜底 `USBDeviceReEnumerate`
4. 等设备回来（约 60 s），开新会话、重新 `AT+CLIP=1`，再验证 shell 可用

网关启动时会话是**持久的一条**，不会每次调用都重开会话——反复建会话本身就容易把
adbd 弄卡。

### 触发时机与实测耗时

- `RING`（来电）或 `ATD`（去电）**立刻**后台 provision；接通时如果上一次已成功就
  不再重跑（避免在用户刚接通的瞬间把路由拆掉重启）
- 纯 shell 命令一次往返，实测：文件都在时 **2.7 s** 完成；文件缺失时
  push 三个文件（约 1 MB）另加 **4 s**
- 成功判据：`/tmp/cal.log` 里出现 `SEC_AUX_PCM_RX_Voice Mixer VoLTE`，且
  `/tmp/mavo-route.log` 出现 `VoLTE route session active on hw:0,4`

### 顺带修掉的 bug：HTTP API 卡死

`USBATModem.SendSMS` 先 `cmdMu.Lock()`，又调用会再锁一次 `cmdMu` 的 `command()`——
Go 的 `sync.Mutex` 不可重入，**发一条短信就把整个 AT 通道永久锁死**，
而 `GET /api/v1/status` 又要经过 AT 通道取信号，于是 HTTP API 跟着一起卡死。
现已拆成 `commandLocked()`，并给 HTTP server 加上读写超时。

### 新增 HTTP 接口（便于不通话也能验证）

```bash
curl localhost:8787/api/v1/voice          # 最近一次 provision 结果
curl localhost:8787/api/v1/voice/check    # 模块现状：文件/模块/声卡/进程/校准标记
curl -XPOST localhost:8787/api/v1/voice/start   # 立刻 provision（约 3 s）
curl -XPOST localhost:8787/api/v1/voice/stop    # 拆掉路由
curl -XPOST localhost:8787/api/v1/voice/recover # 复位 USB / 重启模块救活 adbd
```

不通话时也可以直接跑 `cmd/qdc507-adb`（**需要先停网关**，因为设备只能有一个主人）：

```bash
launchctl bootout gui/$UID/com.cellbridge.gateway
go run ./cmd/qdc507-adb check
go run ./cmd/qdc507-adb start
go run ./cmd/qdc507-adb script        # 打印模块侧脚本原文；加 run 直接执行
```

## 12. "路由全绿但通话静音"的三个真根因（2026-10-01 深夜，逐个实测确认）

前三节把"provision 能不能成功"解决了，但真实通话**仍然没有声音**。
逐个排除后，静音来自三个互不相干、却都会表现为"一切正常但没声音"的原因。

### 12.1 麦克风权限：macOS 拒绝时给的是**静音**，不是报错

最初的表现是**每次网关启动都弹一次麦克风权限**。原因是 `gatewayd` 完全没有代码签名：
macOS 的 TCC 用"二进制指纹"识别未签名的程序，**每次 `go build` 都是一个全新的 App**，
于是重新弹窗；而在有人点"允许"之前，CoreAudio **不会报错，只会交回全 0 采样**——
和"模块没送音频"长得一模一样。

处置：

1. `tools/deploy-gateway.sh` 现在**构建 + 用固定身份签名 + 重启**，签名身份在本机
   `security find-identity -v -p codesigning` 里取（本项目用 Xcode 免费账号那个
   `Apple Development: …`），identifier 固定为 `com.cellbridge.gateway`。
   TCC 授权因此能跨重新编译保留。
2. 判定方法：`POST /api/v1/audio/probe {"device":"Built-in Microphone","seconds":10}`
   会走网关自己的采集代码路径，返回 `frames / peak / verdict`。**对着 Mac 说话时
   peak > 0 才说明网关有采集权限**（实测 30 s：1,441,792 帧、peak 21632 → 权限正常）。
   `voice.OpenCapture`（只开采集、不碰播放）就是为这个诊断加的。

### 12.2 IMS/VoLTE 没开：mixer 通路名对不上

`AT+QCFG="ims"` 是**两个字段**：`<IMS 配置>,<VoLTE 能力>`。实测本模块是 **`0,1`**
——网络支持 VoLTE，但模块里 IMS 是**关**的。

这对我们是致命的：`alsaucm` 设的那条通路叫
`SEC_AUX_PCM_RX_Voice Mixer **VoLTE**`，`mavo-pcm-bridge` 打开的也是 `hw:0,4`
「VoLTE (*)」。IMS 关闭时语音只能走 CS 回落，VoLTE 这条 mixer **根本不参与通话**，
于是：**路由全部 RUNNING、mavo 活着、audio_enable=1，送出来的却是全 0**。

两个参考实现对它的要求完全一致：

- CellBridge（同款模块，真机验证）：`ims` 必须读回 **`1,1`** 才算具备通话能力
  （`gateway/internal/modem/qdc507/capabilities.go` 里就是 `fields[1]=="1" && fields[2]=="1"`）
- DJOneHub：决策文档写明"可用的语音配置必须在重启后读回配置 1 与 VoLTE 能力 1"，
  并把"把任何非零 IMS 配置都当成已启用"列进 Rejected

处置（已做，可逆）：

```bash
AT+QCFG="ims",1      # 读回应变成 +QCFG: "ims",1,1
AT+CFUN=1,1          # 重启模块生效（约 15 s，/tmp 会被清空，下次 provision 会重推运行时）
# 回滚：AT+QCFG="ims",0 + AT+CFUN=1,1
```

### 12.3 模块重启后网关握着失效句柄，谁也恢复不了

模块一旦重启（`AT+CFUN=1,1`、崩溃、掉电），USB 重新枚举，网关手上的 IOKit 句柄
全部失效：AT 写不进去（`WritePipeTO 0xe00002c0` = 句柄已关）、ADB 会话也没了，而
**旧的实现不会自愈**——只能重启网关。

现在 `USBATModem.watchUSB()` 每 2 s 检查一次"最近有没有成功过 USB 传输"，
连续 6 s 没有就丢掉旧句柄、重新找设备、重挂 AT/ADB 并重新 `AT+CLIP=1`。
实测：`AT+CFUN=1,1` 后不碰网关，日志出现
`no USB traffic for 19s — re-attaching` → `re-attached to the module`，状态与 AT 立即恢复。

### 12.4 顺带：路由就绪判据升级为"实时三件套"

原来的判据（进程在 + 日志里有 mixer 行）**在路由其实已经死掉时照样成立**。现在与
CellBridge/DJOneHub 对齐，必须同时满足：

```
mavo 进程存在（按 /proc/PID/exe 匹配，不是命令行）
/sys/class/android_usb/f_audio/audio_enable = 1
/proc/asound/card0/pcm4p/sub0/status = RUNNING   (播放/上行)
/proc/asound/card0/pcm4c/sub0/status = RUNNING   (采集/下行)
```

并且**每通电话轮换一次**（UAC 捕获流被宿主碰过一次就死，只有新会话能救），
轮换用 `kill -9` + 零残留校验（残留会话会把 `audio_enable` 改回 0，之后全静音）。
起路由会让 USB gadget 复位、ADB 回复丢失，所以 provision 现在是"重连 → 验证 → 重试"，
最多 3 次。

### 12.5 通话中逐跳计量（定位静音用）

`audioBridge` 在通话期间每 3 s 打一行：

```
audio: <call> mic_frames=<模块→Mac 的采样数> mic_peak=<峰值> played_frames=<Mac→模块> rtp_in=<手机→网关包数> rtp_out=<网关→手机包数>
```

四个值一眼定位：

| 现象 | 结论 |
|---|---|
| `mic_peak` 长期 0，对方在说话 | 模块没把通话音频送到 UAC（IMS/mixer/路由问题，见 12.2） |
| `mic_peak` > 0 但 `rtp_out` 不涨 | 网关→手机方向的问题 |
| `rtp_in` 不涨 | 手机根本没在发 RTP（App/网络问题） |
| `rtp_in` 涨但 `played_frames` 不涨 | 网关→模块播放方向的问题 |

## 13. 手机侧 RTP 双向不通的真因：iOS 本地网络权限 × 后台（2026-10-01 深夜定论）

模块侧全部达标（§12）之后，通话仍然静音，而逐跳计量显示：

```
audio: <call> mic_frames=168448 mic_peak=32768 played_frames=0 rtp_in=0 rtp_out=987 raw_in=0 peer=192.168.1.122:50000
```

- `mic_frames` / `mic_peak` 正常 → 模块在往 Mac 送真实语音
- `rtp_out` 正常涨 → 网关在往手机发
- **`raw_in=0`，手机侧 `rtp stats: sent=444 received=0`** → 两个方向的 RTP 都没到

设备侧同一时刻：**SIP（UDP 5060）双向正常、App 到网关的 HTTP（TCP 8787）正常**，
只有 RTP 的两个端口双向静默。Mac 侧已排除：防火墙关闭、默认路由正常、
Clash Verge 的 TUN 未接管、Tailscale 只管 100.x。

### 定论（Apple TN3179《Understanding local network privacy》）

官方规则表：

| 操作 | 需要"本地网络"权限 |
|---|---|
| 发出 TCP 连接 | 需要 |
| **发送 UDP 单播** | **需要** |
| **连接 UDP socket** | **需要** |
| 接收进来的 UDP 单播 | 不需要 |

而且 iOS 有一条关键条款：

> **App 在后台时执行本地网络操作、而权限仍处于"未决定"状态，系统会直接拒绝且
> 不弹窗、不记录**；只有前台操作才会弹窗。

我们的媒体 socket 是**通话建立时才创建**的——那时 CallKit 占着屏幕，App 在后台，
于是被静默拒绝：`send` 全部成功、包一个都出不去，收也收不到。
而 SIP 的 socket 是用户**在前台点"连接"**时创建的，所以一直正常。

实测验证（不用打电话）：把 UDP 回声探测做成常驻（App 每 5 s 向网关 40099 打一轮），
**App 置于前台时**：

```
udp echo: 7 bytes from 192.168.1.122:64404 (CB-ECHO)   ×3
phone audio: udp echo: sent=3 replies=3
```

前台双向全通。结论：不是网络、不是权限没给，是**"后台创建 socket"这一条**。

### 处置

**把媒体 socket 也搬到前台创建，并跨通话复用**（和 SIP 完全同构）：

- App：`SIPClient.connect()` 里（前台）就创建 RTP socket，绑定固定本地端口 50000，
  连到网关固定媒体端口 40000；来电/去电都直接复用这个 socket，通话结束**不销毁**
- 网关：媒体端口固定为 40000（`pickRTPPortLocked`；只有并发通话才回退到 40002…），
  这样 App 在前台就能知道要连哪儿
- 诊断留档：`RTPSession` 上报 `rtp stats: sent/errors/received`，
  网关逐跳计量里带 `raw_in`（收到的原始数据报，含无法解析的）和 `peer`
