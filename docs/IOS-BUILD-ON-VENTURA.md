# 在 macOS Ventura 上编译 / 安装 iOS App（目标机是 iOS 26）

> 日期：2026-10-01。本文记录在这台机器（macOS 13.7 Ventura Hackintosh，
> 无法升级 macOS）上为 iPhone（iOS 26.x）构建 App 的完整方案、
> 踩过的坑，以及目前卡在哪一步。

## 1. 版本约束（实测数据）

| Xcode | 最低 macOS（App 本体 `LSMinimumSystemVersion`） | iOS SDK |
|---|---|---|
| 15.0 / 15.0.1 / 15.1 / 15.2 | **13.5** | 17.0 – 17.2 |
| 15.3 / 15.4 | **14.0** | 17.4 / 17.5 |
| 26.0 – 26.3 | **15.6** | 26.0 – 26.2 |
| 27.0 – 27.2 | 26.6 | 27.0 / 27.2 |

两个结论：

1. **编 iOS 26 只需要 macOS 15.6**（Sequoia），不需要 macOS 26。所以真正的门槛
   是"升级 macOS"，而这台机器不能升级。
2. Ventura 上**唯一官方支持的版本是 Xcode 15.2**。我们下载的 15.4 的 App 本体
   要求 macOS 14.0，只能靠下面的补丁方式跑。

## 2. 实际采用的方案

分工：

- **Xcode 15.4**（打了补丁）负责**编译**和**用免费 Apple ID 签名**
- **pymobiledevice3** 负责**设备通信与安装**，绕开 Xcode 对设备系统版本的检查
- **tools/tss-proxy.py** 负责让被杀毒式屏蔽的 Apple TSS 域名可达

### 2.1 安装 Xcode 15.4

```bash
# 1. 从 developer.apple.com/download/all/ 下载 Xcode_15.4.xip（需登录）
xip --expand Xcode_15.4.xip && mv Xcode.app /Applications/
sudo xcodebuild -license accept          # 写 /Library/Preferences，必须 sudo
xcodebuild -runFirstLaunch               # 实测不需要 sudo
```

**坑：GUI 启动被 LaunchServices 拒绝**
`/Applications/Xcode.app/Contents/Info.plist` 里 `LSMinimumSystemVersion = 14.0`，
双击会报"此版本的 macOS 不能与 Xcode 配合使用"。而且 `/Applications` 下的 bundle
受 **App Management** 保护、改不了（`Operation not permitted`）。

解法：复制一份到用户目录再改（补丁只加在副本上，原版不动）：

```bash
cp -cR /Applications/Xcode.app ~/Applications/Xcode154.app
plutil -replace LSMinimumSystemVersion -string 13.0 \
       ~/Applications/Xcode154.app/Contents/Info.plist
open ~/Applications/Xcode154.app        # 之后可正常启动，出现在 Dock/⌘Tab
```

命令行工具（`xcodebuild`）不受影响，始终可用；需要 GUI（例如登录 Apple ID）
时才用这个副本。

### 2.2 必须补装 iOS 平台组件

只有 SDK 不够：`xcodebuild -showdestinations` 会报
`iOS 17.5 is not installed. To use with Xcode, first download and install the platform`，
连 "Any iOS Device" 占位目标都不可用（于是 `-scheme` 构建直接失败）。

```bash
xcodebuild -downloadPlatform iOS      # 7.34 GB，装完目的地解析才正常
```

装完后 `-showdestinations` 能列出真机：

```
{ platform:iOS, arch:arm64, id:00008150-0000000000000000, name:My iPhone }
```

### 2.3 免费 Apple ID 与 entitlements

工程的 `CellBridgeApp.entitlements` 申请了 `aps-environment`。
免费 **Personal Team 不支持 Push Notifications 能力**，签名会直接失败：

```
Cannot create a iOS App Development provisioning profile for "com.example.cellbridge".
Personal development teams ... do not support the Push Notifications capability.
```

两点注意：

1. 传 `CODE_SIGN_ENTITLEMENTS=`（空串）**会被 xcodebuild 忽略**，仍读工程里的文件；
   必须传一个真实存在的空 entitlements 路径。→ `CellBridgeApp/CellBridgeApp-free.entitlements`
2. 光去掉 entitlements 还不够：iOS 会**杀掉**没有该 entitlement 却注册 VoIP 推送的
   App。所以免费构建同时用 `SWIFT_ACTIVE_COMPILATION_CONDITIONS=... CELLBRIDGE_NO_PUSH`
   把 PushKit 那段编译掉（见 `AppDelegate.swift`）。

免费签名的能力边界：SIP 注册、来电/去电、CallKit 原生界面、短信都可用；
**"App 被杀后靠 VoIP 推送唤醒"不可用**（与 `HANDOFF.md` §1.3 的结论一致）。

### 2.4 Apple TSS 被 hosts 屏蔽 → tools/tss-proxy.py

本机 `/etc/hosts` 有一段 Hackintosh 防升级规则（`# macOS OS 屏蔽更新 by hkmac`），
把 `gs.apple.com`（TSS 签名服务）等解析到 `127.0.0.1`。
个性化 DeveloperDiskImage 必须访问它，否则 pymobiledevice3 报
`Failed to establish a new connection: [Errno 61] Connection refused`。

路由器（192.168.1.1）上的 Clash 代理要认证（407），不能用。

解法：`tools/tss-proxy.py` —— 一个只对 Apple TSS 域名生效的本地 HTTP 代理，
用 DoH 拿真实 IP 从而绕开 `/etc/hosts`，其它域名仍按系统原有方式连接
（保持路由器的 fake-ip 代理正常工作）。**不需要 sudo、不改 hosts。**

```bash
python3 tools/tss-proxy.py &                                  # 127.0.0.1:8899
HTTP_PROXY=http://127.0.0.1:8899 HTTPS_PROXY=http://127.0.0.1:8899 \
  pymobiledevice3 mounter auto-mount
```

验证过 `http://gs.apple.com/TSS/controller?action=2` 从"连接被拒"变成 **200**。

### 2.5 pymobiledevice3 的安装坑（Intel + Python 3.9）

```bash
python3 -m pip install --user --upgrade pip
python3 -m pip install --user --only-binary=:all: cryptography==48.0.1
python3 -m pip install --user "cryptography==48.0.1" pymobiledevice3
```

- 直接装最新版会去**源码编译 cryptography**（Rust/maturin），失败 ——
  2026 年上游基本不再发 Intel macOS 的 wheel，必须锁到还有
  `cp39-abi3-macosx_...universal2` 的版本。
- 若用 `--only-binary=:all:` 装 pymobiledevice3，解析器会一路回退到 2021 年的
  `1.0.0`（不支持 iOS 26），只对 cryptography 加这个限制。
- `~/.pymobiledevice3` 是本机 6 月 7 日某个 root 工具留下的，**没有写权限**，
  且 pymobiledevice3 检测到该目录存在就优先使用它。改名也被拒，于是用隔离 HOME：
  ```bash
  HOME=$HOME/.pmd3home PYTHONPATH=$HOME/Library/Python/3.9/lib/python/site-packages \
    pymobiledevice3 ...
  ```

### 2.6 一键脚本

```bash
tools/build-ios.sh            # 挂 DDI + 编译 + 签名 + 打包 ipa + 安装 + 启动
tools/build-ios.sh build      # 只编译签名，产出 build/CellBridgeApp.ipa
tools/build-ios.sh install    # 只安装
WITH_PUSH=1 tools/build-ios.sh   # 付费账号：带 aps-environment 与 PushKit
```

## 3. 首次编译发现并修复的问题

```  
CallManager.swift : 用了 AVAudioSession 却没 import → 两个 CallKit 音频代理方法匹配不上协议
CallManager.swift : CXProviderConfiguration(localizedName:) 已废弃且属性只读，改普通 init
ContentView.swift : @State private var body 与 SwiftUI 的 var body 重名 → 改名 draft
SIP/RTP.swift     : G.711 µ-law 编码写成 C 风格 ^ 取反 → 改为 ~
```

`** BUILD SUCCEEDED **`（iPhoneOS 17.5 SDK，arm64）。产物 `CellBridgeApp.app` 版本 0.1.0。

## 3.5 首次真机运行发现的问题（编译期发现不了的）

代码能编过不等于能跑。第一次在真机上拨号，连着崩了两次，都是运行期才暴露的：

| 现象 | 根因 | 修复 |
|---|---|---|
| 一拨号/一接听**立刻闪退** | `AudioBridge.start()` 用 8 kHz 格式对 `inputNode` 调 `installTap`，而真机麦克风是 48 kHz。`AVAudioEngine` 直接抛 **ObjC 异常**（Swift 的 `try?` 拦不住）→ SIGABRT | 用输入节点自己的格式装 tap，再用 `AVAudioConverter` 转 8 kHz/Int16 |
| 修复后**仍然闪退**，栈在 `RTP.packet` | `UInt8(timestamp >> 16)` / `>> 8`（ssrc 同）**没有加 `& 0xFF` 掩码**：`timestamp >> 16` 最大 65535，`UInt8(...)` 一转换就触发断言 → SIGTRAP。作者给低位加了多余的 `& 0xFF`，却漏了移位那几行 | 四个移位转换全部补 `& 0xFF` |
| （加固）远端 SDP 里的端口 | `UInt16(sdp.port)`，port 来自对端文本，异常值会崩 | 改 `UInt16(clamping:)` |

定位手法（很有用）：`pymobiledevice3 crash ls` / `crash pull <dir> --match CellBridgeApp`
拿到 `.ips`，再用 `atos -o <app二进制> -arch arm64 -l <image base> <地址>` 还原到**源码行号**。
Debug 构建没有内联，行号非常准 —— 上面第二个 bug 就是这样直接定位到 `RTP.swift:11` 的。

## 4. 卡点的解法（2026-10-01 上午，已验证）

曾经卡在 **把设备的 UDID 注册进团队**：

```
error: Communication with Apple failed: Your team has no devices from which to
generate a provisioning profile. Connect a device to use or manually add device
IDs in Certificates, Identifiers & Profiles.
```

原因：Xcode 15.4 的 CoreDevice 连不上这台 iOS 26.5.1 设备，三种 destination 写法
（UDID / CoreDevice id / name）都返回 `Device is busy (Waiting to reconnect)`；
destination 不成立，`-allowProvisioningDeviceRegistration` 就不会注册设备。

**解法（不需要手动去 portal 注册）：先把 DDI 挂上去。** 挂载成功后
`devicectl` 的设备状态从 `connected (no DDI)` 变成 `connected`，
`Device is busy` 消失，`-destination` 立刻可用，Xcode 随即自动完成了
设备注册 → 证书 → 描述文件 → 签名：

```bash
tools/build-ios.sh ddi      # 经 tss-proxy 挂载 iOS 26 的个性化 DDI
tools/build-ios.sh build    # -> BUILD SUCCEEDED，profile 里含本机 UDID
```

产物验证：

```
Identifier=com.example.cellbridge.sip
Authority=Apple Development: <你的 Apple ID 邮箱> (XXXXXXXXXX)
TeamIdentifier=XXXXXXXXXX
embedded.mobileprovision -> ProvisionedDevices: 00008150-0000000000000000
```

安装也已跑通（`pymobiledevice3 apps install` → `Installation succeed.`）。

**唯一需要人工的一步**：免费签名的 App 首次要在手机上信任证书 ——
设置 → 通用 → VPN 与设备管理 → 开发者 App → 信任（输入锁屏密码）。
不信任时程序化启动会报
`Failed to launch process with bundle identifier 'com.example.cellbridge.sip'`。

## 6. 关键参数备忘

| 项 | 值 |
|---|---|
| Team ID | `XXXXXXXXXX`（你的 Personal Team，免费） |
| Bundle ID | `com.example.cellbridge.sip`（免费账号必须先在 portal 上可用） |
| 设备 UDID | `00008150-0000000000000000`（你的 iPhone，iOS 26.x） |
| 免费构建 entitlements | `CellBridgeApp/CellBridgeApp-free.entitlements`（空） |
| 免费构建编译条件 | `CELLBRIDGE_NO_PUSH` |
