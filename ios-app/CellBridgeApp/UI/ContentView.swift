import SwiftUI

// MARK: - Root

struct ContentView: View {
    @EnvironmentObject var app: AppState

    var body: some View {
        TabView {
            PhoneTab()
                .tabItem { Label("通话", systemImage: "phone.fill") }
            MessagesTab()
                .tabItem { Label("短信", systemImage: "message.fill") }
            StatusTab()
                .tabItem { Label("状态", systemImage: "antenna.radiowaves.left.and.right") }
        }
        // Accent = system blue, just like Apple apps
        .tint(Theme.accent)
    }
}

// ============================================================
// MARK: - 通话 Tab
// ============================================================

struct PhoneTab: View {
    @EnvironmentObject var app: AppState
    @State private var number = ""
    @State private var showRecents = false

    var body: some View {
        NavigationStack {
            VStack(spacing: 0) {
                if let uuid = app.activeCallUUID {
                    ActiveCallView(uuid: uuid)
                } else {
                    DialerView(number: $number)
                }
            }
            .navigationTitle("通话")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .navigationBarTrailing) {
                    Button {
                        showRecents = true
                    } label: {
                        Image(systemName: "clock.arrow.circlepath")
                    }
                }
            }
            .sheet(isPresented: $showRecents) {
                RecentsSheet()
            }
        }
    }
}

// MARK: Dialer

struct DialerView: View {
    @EnvironmentObject var app: AppState
    @Binding var number: String

    private let rows: [[String]] = [
        ["1","2","3"],
        ["4","5","6"],
        ["7","8","9"],
        ["*","0","#"],
    ]

    var body: some View {
        VStack(spacing: 0) {
            // Display
            ZStack(alignment: .trailing) {
                Text(number.isEmpty ? " " : number)
                    .font(.system(size: 46, weight: .thin, design: .rounded))
                    .frame(maxWidth: .infinity)
                    .minimumScaleFactor(0.4)
                    .lineLimit(1)
                    .padding(.horizontal, 52)

                if !number.isEmpty {
                    Button {
                        number.removeLast()
                    } label: {
                        Image(systemName: "delete.backward")
                            .font(.title3)
                            .foregroundStyle(.secondary)
                    }
                    .padding(.trailing, 16)
                }
            }
            .padding(.top, 24)
            .padding(.bottom, 16)

            // Keys
            ForEach(rows, id: \.self) { row in
                HStack(spacing: 0) {
                    ForEach(row, id: \.self) { k in
                        KeypadButton(key: k) { number.append(k) }
                    }
                }
            }

            // Call button row
            HStack(spacing: 0) {
                Spacer().frame(maxWidth: .infinity)

                Button {
                    app.placeCall(number: number)
                    number = ""
                } label: {
                    Image(systemName: "phone.fill")
                        .font(.system(size: 26))
                        .foregroundStyle(.white)
                        .frame(width: 70, height: 70)
                        .background(app.registered && !number.isEmpty
                                    ? Theme.accent : Color.secondary.opacity(0.25),
                                    in: Circle())
                }
                .disabled(!app.registered || number.isEmpty)

                Spacer().frame(maxWidth: .infinity)
            }
            .padding(.top, 20)
            .padding(.bottom, 32)
        }
    }
}

struct KeypadButton: View {
    let key: String
    let action: () -> Void

    private let sub: [String: String] = [
        "2":"ABC","3":"DEF","4":"GHI","5":"JKL",
        "6":"MNO","7":"PQRS","8":"TUV","9":"WXYZ","0":"+"
    ]

    var body: some View {
        Button(action: action) {
            VStack(spacing: 2) {
                Text(key)
                    .font(.system(size: 32, weight: .light, design: .rounded))
                if let s = sub[key] {
                    Text(s)
                        .font(.system(size: 10, weight: .medium))
                        .kerning(1.2)
                        .foregroundStyle(.secondary)
                }
            }
            .frame(maxWidth: .infinity)
            .frame(height: 76)
            .contentShape(Rectangle())
        }
        .buttonStyle(KeypadStyle())
    }
}

struct KeypadStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .background(configuration.isPressed
                        ? Color(uiColor: .tertiarySystemFill)
                        : Color.clear)
            .animation(.easeOut(duration: 0.08), value: configuration.isPressed)
    }
}

// MARK: Active call

struct ActiveCallView: View {
    @EnvironmentObject var app: AppState
    let uuid: UUID

    var body: some View {
        VStack(spacing: 0) {
            Spacer()

            // Contact avatar
            let num = app.activeCallNumber ?? ""
            let name = app.contacts.displayName(for: num)

            ZStack {
                Circle()
                    .fill(Color(uiColor: .secondarySystemFill))
                    .frame(width: 96, height: 96)
                Text(name.isEmpty ? "#" : String(name.prefix(1)))
                    .font(.system(size: 40, weight: .semibold))
                    .foregroundStyle(.secondary)
            }
            .padding(.bottom, 16)

            Text(name)
                .font(.title2.weight(.semibold))
            if name != num {
                Text(num)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                    .padding(.top, 4)
            }

            StatusPill(text: "通话中", color: Theme.accent)
                .padding(.top, 12)

            Spacer()

            // End call
            Button {
                app.sip.hangup(uuid: uuid)
            } label: {
                Image(systemName: "phone.down.fill")
                    .font(.system(size: 28))
                    .foregroundStyle(.white)
                    .frame(width: 72, height: 72)
                    .background(Theme.danger, in: Circle())
            }

            Spacer().frame(height: 48)
        }
    }
}

// MARK: Recents sheet

struct RecentsSheet: View {
    @EnvironmentObject var app: AppState
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            List {
                if app.history.callRecords.isEmpty {
                    Text("暂无通话记录")
                        .foregroundStyle(.secondary)
                        .frame(maxWidth: .infinity, alignment: .center)
                        .listRowBackground(Color.clear)
                } else {
                    ForEach(app.history.callRecords.reversed()) { r in
                        RecentRow(record: r)
                    }
                }
            }
            .navigationTitle("通话记录")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .navigationBarLeading) {
                    Button("关闭") { dismiss() }
                }
            }
        }
    }
}

struct RecentRow: View {
    @EnvironmentObject var app: AppState
    let record: HistoryStore.CallRecord

    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: iconName)
                .foregroundStyle(iconColor)
                .frame(width: 20)

            VStack(alignment: .leading, spacing: 2) {
                let name = app.contacts.displayName(for: record.number)
                Text(name).font(.body)
                if name != record.number {
                    Text(record.number).font(.caption).foregroundStyle(.secondary)
                }
            }

            Spacer()

            VStack(alignment: .trailing, spacing: 2) {
                Text(record.formattedDate).font(.caption).foregroundStyle(.secondary)
                if !record.formattedDuration.isEmpty {
                    Text(record.formattedDuration).font(.caption2).foregroundStyle(.tertiary)
                }
            }
        }
        .padding(.vertical, 2)
    }

    private var iconName: String {
        record.direction == "out" ? "arrow.up.right" : "arrow.down.left"
    }
    private var iconColor: Color {
        guard record.direction == "in" else { return .secondary }
        return record.isMissed ? Theme.danger : Theme.accent
    }
}

// ============================================================
// MARK: - 短信 Tab
// ============================================================

struct MessagesTab: View {
    @EnvironmentObject var app: AppState
    @State private var showNew = false
    @State private var searchText = ""

    private var filtered: [HistoryStore.Conversation] {
        guard !searchText.isEmpty else { return app.history.conversations }
        return app.history.conversations.filter {
            app.contacts.displayName(for: $0.peer).localizedCaseInsensitiveContains(searchText)
            || $0.peer.contains(searchText)
        }
    }

    var body: some View {
        NavigationStack {
            List {
                ForEach(filtered) { conv in
                    NavigationLink(destination: ChatView(peer: conv.peer)) {
                        ConvRow(conv: conv)
                    }
                }
            }
            .listStyle(.plain)
            .overlay {
                if app.history.conversations.isEmpty {
                    VStack(spacing: 12) {
                        Image(systemName: "message")
                            .font(.system(size: 48))
                            .foregroundStyle(.secondary)
                        Text("暂无短信")
                            .font(.title3.weight(.semibold))
                            .foregroundStyle(.secondary)
                        Text(app.registered ? "收到的短信会出现在这里" : "请先在「状态」页连接网关")
                            .font(.subheadline)
                            .foregroundStyle(.tertiary)
                    }
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                }
            }
            .searchable(text: $searchText, prompt: "搜索")
            .navigationTitle("短信")
            .toolbar {
                ToolbarItem(placement: .navigationBarTrailing) {
                    Button {
                        showNew = true
                    } label: {
                        Image(systemName: "square.and.pencil")
                    }
                }
            }
            .sheet(isPresented: $showNew) {
                NewChatView()
            }
        }
    }
}

struct ConvRow: View {
    @EnvironmentObject var app: AppState
    let conv: HistoryStore.Conversation

    var body: some View {
        HStack(spacing: 12) {
            // Avatar
            ZStack {
                Circle()
                    .fill(Color(uiColor: .secondarySystemFill))
                    .frame(width: 50, height: 50)
                Text(initial)
                    .font(.system(size: 21, weight: .semibold))
                    .foregroundStyle(Theme.accent)
            }

            VStack(alignment: .leading, spacing: 3) {
                HStack(alignment: .firstTextBaseline) {
                    Text(app.contacts.displayName(for: conv.peer))
                        .font(.headline)
                    Spacer()
                    if let last = conv.lastMessage {
                        Text(last.formattedDate)
                            .font(.subheadline)
                            .foregroundStyle(.tertiary)
                    }
                }
                if let last = conv.lastMessage {
                    Text(last.body)
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                }
            }
        }
        .padding(.vertical, 4)
    }

    private var initial: String {
        let n = app.contacts.displayName(for: conv.peer)
        return n == conv.peer ? String(conv.peer.prefix(2)) : String(n.prefix(1))
    }
}


// MARK: - Status Pill

struct StatusPill: View {
    let text: String
    let color: Color

    var body: some View {
        Text(text)
            .font(.subheadline.weight(.medium))
            .foregroundStyle(.white)
            .padding(.horizontal, 16)
            .padding(.vertical, 6)
            .background(color, in: Capsule())
    }
}

// MARK: Chat view

struct ChatView: View {
    @EnvironmentObject var app: AppState
    let peer: String

    @State private var draft = ""
    @FocusState private var composerFocused: Bool

    private var messages: [HistoryStore.SMSRecord] {
        app.history.conversations.first(where: { $0.peer == peer })?.messages ?? []
    }

    var body: some View {
        VStack(spacing: 0) {
            // Messages
            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(spacing: 0) {
                        ForEach(messages) { msg in
                            BubbleView(record: msg)
                                .id(msg.id)
                        }
                    }
                    .padding(.horizontal, 8)
                    .padding(.top, 8)
                    .padding(.bottom, 8)
                }
                .dismissKeyboardOnTap()
                .onChange(of: messages.count) { _ in
                    if let last = messages.last {
                        withAnimation(.easeOut(duration: 0.2)) {
                            proxy.scrollTo(last.id, anchor: .bottom)
                        }
                    }
                }
                .onAppear {
                    DispatchQueue.main.asyncAfter(deadline: .now() + 0.05) {
                        if let last = messages.last {
                            proxy.scrollTo(last.id, anchor: .bottom)
                        }
                    }
                }
            }

            Divider()

            // Composer — iMessage-style
            HStack(alignment: .bottom, spacing: 8) {
                TextField("短信", text: $draft, axis: .vertical)
                    .font(.body)
                    .tint(Theme.accent)
                    .padding(.horizontal, 12)
                    .padding(.vertical, 7)
                    .background(
                        RoundedRectangle(cornerRadius: 18, style: .continuous)
                            .strokeBorder(Color(uiColor: .separator), lineWidth: 1)
                    )
                    .lineLimit(1...6)
                    .focused($composerFocused)

                // Send
                Button {
                    sendDraft()
                } label: {
                    Image(systemName: "arrow.up.circle.fill")
                        .font(.system(size: 30))
                        .foregroundStyle(canSend ? Theme.accent : Color.secondary.opacity(0.4))
                }
                .disabled(!canSend)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
            .background(Color(uiColor: .systemBackground))
        }
        .navigationTitle(app.contacts.displayName(for: peer))
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .navigationBarTrailing) {
                Button {
                    app.placeCall(number: peer)
                } label: {
                    Image(systemName: "phone")
                }
                .disabled(!app.registered)
            }
            ToolbarItem(placement: .keyboard) {
                HStack {
                    Spacer()
                    Button("完成") { composerFocused = false }
                }
            }
        }
    }

    private var canSend: Bool {
        app.registered && !draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    private func sendDraft() {
        let body = draft.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !body.isEmpty else { return }
        let target = peer.trimmingCharacters(in: .whitespaces)
        guard !target.isEmpty else { return }
        app.sendSMS(to: target, body: body)
        draft = ""
    }
}

struct BubbleView: View {
    let record: HistoryStore.SMSRecord

    var body: some View {
        HStack {
            if record.isOutgoing { Spacer(minLength: 60) }

            VStack(alignment: record.isOutgoing ? .trailing : .leading, spacing: 2) {
                Text(record.body)
                    .font(.body)
                    .padding(.horizontal, 12)
                    .padding(.vertical, 8)
                    .background(
                        record.isOutgoing
                            ? AnyShapeStyle(Theme.bubbleOut)
                            : AnyShapeStyle(Color(uiColor: .secondarySystemFill)),
                        in: RoundedRectangle(cornerRadius: 16, style: .continuous)
                    )
                    .foregroundStyle(record.isOutgoing ? .white : .primary)

                Text(record.formattedDate)
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
                    .padding(.horizontal, 4)
            }

            if !record.isOutgoing { Spacer(minLength: 60) }
        }
        .padding(.vertical, 2)
    }
}

// MARK: New chat sheet

struct NewChatView: View {
    @EnvironmentObject var app: AppState
    @Environment(\.dismiss) private var dismiss

    @State private var to = ""
    @State private var messageText = ""
    @FocusState private var activeField: Field?
    enum Field { case to, messageText }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    HStack {
                        Text("收件人")
                            .foregroundStyle(.secondary)
                        TextField("号码", text: $to)
                            .keyboardType(.phonePad)
                            .autocorrectionDisabled()
                            .focused($activeField, equals: .to)
                            .tint(Theme.accent)
                    }
                }

                Section {
                    TextField("短信内容", text: $messageText, axis: .vertical)
                        .lineLimit(4...10)
                        .focused($activeField, equals: .messageText)
                        .tint(Theme.accent)
                }
            }
            .navigationTitle("新建短信")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .navigationBarLeading) {
                    Button("取消") { dismiss() }
                }
                ToolbarItem(placement: .navigationBarTrailing) {
                    Button("发送") { send() }
                        .fontWeight(.semibold)
                        .disabled(!canSend)
                }
                ToolbarItem(placement: .keyboard) {
                    HStack {
                        Spacer()
                        Button("完成") { activeField = nil }
                    }
                }
            }
            .onAppear { activeField = .to }
        }
    }

    private var canSend: Bool {
        app.registered
        && !to.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
        && !messageText.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    private func send() {
        let t = to.trimmingCharacters(in: .whitespacesAndNewlines)
        let b = messageText.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !t.isEmpty, !b.isEmpty else { return }
        app.sendSMS(to: t, body: b)
        dismiss()
    }
}

// ============================================================
// MARK: - 状态 Tab
// ============================================================

struct StatusTab: View {
    @EnvironmentObject var app: AppState

    var body: some View {
        NavigationStack {
            Form {
                // Connection status
                Section {
                    HStack(spacing: 14) {
                        Image(systemName: app.registered ? "checkmark.circle.fill" : "circle.dashed")
                            .font(.title2)
                            .foregroundStyle(app.registered ? Theme.accent : .secondary)
                        VStack(alignment: .leading, spacing: 3) {
                            Text(app.registered ? "已连接" : "未连接")
                                .font(.headline)
                            Text(app.statusLine)
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                        Spacer()
                    }
                    .padding(.vertical, 4)

                    Button {
                        if app.registered { app.disconnect() } else { app.connect() }
                    } label: {
                        Text(app.registered ? "断开连接" : "立即连接")
                            .frame(maxWidth: .infinity, alignment: .center)
                            .foregroundStyle(app.registered ? Theme.danger : Theme.accent)
                    }
                    .disabled(app.serverHost.trimmingCharacters(in: .whitespaces).isEmpty)
                }

                // Server settings
                Section("服务器") {
                    LabeledContent("地址") {
                        TextField("192.168.x.x", text: $app.serverHost)
                            .keyboardType(.URL)
                            .autocorrectionDisabled()
                            .textInputAutocapitalization(.never)
                            .multilineTextAlignment(.trailing)
                            .tint(Theme.accent)
                    }
                    LabeledContent("SIP 端口") {
                        TextField("5060", text: Binding(
                            get: { "\(app.serverPort)" },
                            set: { if let n = Int($0) { app.serverPort = n } }
                        ))
                        .keyboardType(.numberPad)
                        .multilineTextAlignment(.trailing)
                        .tint(Theme.accent)
                    }
                    LabeledContent("HTTP 端口") {
                        TextField("8787", text: Binding(
                            get: { "\(app.apiPort)" },
                            set: { if let n = Int($0) { app.apiPort = n } }
                        ))
                        .keyboardType(.numberPad)
                        .multilineTextAlignment(.trailing)
                        .tint(Theme.accent)
                    }
                    LabeledContent("账号") {
                        TextField("iphone", text: $app.sipUser)
                            .autocorrectionDisabled()
                            .multilineTextAlignment(.trailing)
                            .tint(Theme.accent)
                    }
                    LabeledContent("密码") {
                        SecureField("", text: $app.sipPassword)
                            .multilineTextAlignment(.trailing)
                            .tint(Theme.accent)
                    }
                    Toggle("自动连接", isOn: $app.autoConnect)
                        .tint(Theme.accent)
                }

                // Modem / gateway
                Section("模块状态") {
                    if let s = app.gatewayStatus {
                        LabeledContent("型号", value: s.modem)
                        LabeledContent("蜂窝信号", value: s.signalText)
                        LabeledContent("客户端") {
                            Text(s.registered ? "已注册" : "未注册")
                                .foregroundStyle(s.registered ? Theme.accent : .secondary)
                        }
                        LabeledContent("活跃通话", value: "\(s.activeCalls)")
                    } else {
                        Label(
                            app.serverHost.isEmpty ? "填写服务器地址后显示" : "无法读取网关状态",
                            systemImage: "exclamationmark.triangle"
                        )
                        .foregroundStyle(.secondary)
                    }
                }

                // Network hint
                Section("网络") {
                    VStack(alignment: .leading, spacing: 6) {
                        Label("局域网：Mac 的内网 IP（192.168.x.x）", systemImage: "wifi")
                        Label("外网：安装 Tailscale 使用 100.x.x.x", systemImage: "network")
                    }
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                    .padding(.vertical, 2)
                }
            }
            .dismissKeyboardOnTap()
            .navigationTitle("状态")
            .toolbar {
                ToolbarItem(placement: .navigationBarTrailing) {
                    Button {
                        app.refreshGatewayStatus()
                    } label: {
                        Image(systemName: "arrow.clockwise")
                    }
                    .disabled(app.serverHost.isEmpty)
                }
                ToolbarItem(placement: .keyboard) {
                    HStack {
                        Spacer()
                        Button("完成") {
                            UIApplication.shared.sendAction(
                                #selector(UIResponder.resignFirstResponder),
                                to: nil, from: nil, for: nil)
                        }
                    }
                }
            }
        }
    }
}
