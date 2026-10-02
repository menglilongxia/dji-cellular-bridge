# 构建与联调说明

## 1. Mac 网关（Go）

要求：Go 1.24+。

```bash
cd mac-gateway
export PATH=$HOME/sdk/go/bin:$PATH   # 或按你的 Go 安装路径
go vet ./... && go build ./... && go test ./...
```

运行（fake 模式，先不接硬件）：

```bash
go run ./cmd/gatewayd --modem=fake --sip=:5060 --api=:8787 \
  --user=iphone --pass=changeme

# 如果没有付费 Apple 开发者账号（免费账号无 APNs），加 --alert-phone
# 来电时网关会用模块 SIM 发短信到该号码，提醒用户打开 App：
go run ./cmd/gatewayd --modem=usb --sip=:5060 --api=:8787 \
  --user=iphone --pass=changeme --coreaudio --voice-adb \
  --voice-files=~/Library/Application\ Support/CellBridge/module-voice \
  --alert-phone=+86xxxxxxxxxx
```

冒烟测试（另开终端）：

```bash
curl localhost:8787/healthz
curl localhost:8787/api/v1/status
# 模拟来电（App 应响铃）
curl -XPOST localhost:8787/api/v1/fake/incoming-call -d '{"from":"13800000000"}'
# 模拟来短信
curl -XPOST localhost:8787/api/v1/fake/incoming-sms -d '{"from":"13800000000","body":"hello"}'
```

## 2. iPhone App（Xcode）

要求：macOS + Xcode 16+，iOS 16+ 真机（CallKit/PushKit 不支持模拟器）。

### 不用付费开发者账号也能装（免费 Apple ID 签名）

1. iPhone 开启开发者模式：**设置 → 隐私与安全 → 开发者模式**。
2. USB 连 Mac，手机上点"信任此电脑"。
3. Xcode → Settings → Accounts，登录普通 Apple ID。
4. 工程 Signing & Capabilities 里 Team 选你的 Apple ID（Personal Team），
   Bundle ID 改成唯一的（如 `com.你的名字.cellbridge`）。
5. 顶部设备选你的 iPhone，点 Run。
6. 首次启动手机会拦截：**设置 → 通用 → VPN 与设备管理** 里信任该 Apple ID。

限制（Apple 定的，所有免费签名都一样）：

- 签名 **7 天过期**，过期后 App 打不开，连 Mac 重新 Run 一次即可续
  （也可用 AltStore/SideStore 在同一 Wi-Fi 下自动续签）；
- 同一 Apple ID 最多装 3 个自签名 App；
- **推送不可用**：免费签名拿不到 `aps-environment`  entitlement，
  APNs（VoIP push / 普通推送）需要付费 Apple Developer Program（¥688/年）。
  因此免费安装可测：SIP 注册、前台/后台来电响铃、去电、短信；
  **App 被彻底杀掉后的来电唤醒**必须等付费账号 + 真实 APNs 接入后才能用。

### App 内配置

1. Mac 与 iPhone 接入同一网络（推荐 Tailscale，手机和 Mac 都在线即可直连）。
2. App「状态」页填入 Mac 的 IP（如 Tailscale 的 100.x 地址），点「连接」。
3. 看到「已注册」后即可：
   - 用 `POST /api/v1/fake/incoming-call` 触发模拟来电 → 手机原生来电界面响铃；
   - 「短信」页收发短信。

## 3. 联调清单（fake 模式验收标准）

- [ ] App 显示「已注册」；网关 `/api/v1/status` 显示 1 个注册客户端
- [ ] 模拟来电 → 手机 CallKit 响铃 → 接听 → 通话（fake 模式为回声）
- [ ] 拒接/挂断 → 双方状态一致
- [ ] App 去电 → 网关日志显示 FakeModem 拨号 → 接通
- [ ] 双向短信收发正常
- [ ] 杀掉 App 后模拟来电 → VoIP push 唤醒 → 仍能响铃（需真实 APNs）

## 4. 转入硬件阶段（软件验收通过后）

按"先软件、后硬件"的顺序，**软件主体完成并通过上面清单后**，再接入 DJI 4G 模块：

1. macOS 下确认模块 AT 串口枚举形态（`/dev/tty.usb*` / `cu.*`）。
2. 实现 `internal/modem/serial.go`（AT 收发 + URC 事件）。
3. 确认 USB Audio 设备，做 CoreAudio ↔ RTP 桥接（替代 fake loopback）。
4. 按 CellBridge 的 AT 探测/解锁流程处理模块侧语音路由（QDC507 相关风险见立项书）。
