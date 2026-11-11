// GUI LaunchAgent for the macOS guest. launchd owns one private per-user socket.
import AppKit
import ApplicationServices
import ScreenCaptureKit
import Security

private let appURL = URL(fileURLWithPath: nativeAppPath)
private let requirement = #"anchor apple generic and identifier "com.anthropic.claudefordesktop" and certificate leaf[subject.OU] = "Q6L2SF6YDW" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists"#

struct Request: Decodable {
    let action: String
    var window: UInt32?
    var pid: Int32?
    var x, y, delta: Double?
    var deltaX: Double?
    var key, text: String?
    var url: String?
    var code, phase, mode, context, session: String?
    var down, control, alt: Bool?
    var button, buttons, clicks: Int?
    var shift, command: Bool?
    var project, nonce, geometry: String?
    var width, height: Int?
    var scale: Double?
    var after: String?
    var keepAlive: Bool?
    var refresh: Bool?
    var wait, binary: Bool?
    var id, kind: String?
    var ids: [String]?
    var transfer: String?
    var sequence: Int?
    var path: String?
    var error: String?
    var files: [String]?
    var length: Int?
    var phone: MacPhoneDesktopInput?
    var phone_token: String?
}

func consoleUser() throws {
    var info = stat()
    guard stat("/dev/console", &info) == 0, info.st_uid >= 500, info.st_uid == getuid(),
          (CGSessionCopyCurrentDictionary() as? [String:Any])?["CGSSessionScreenIsLocked"] as? Bool != true else {
        throw Failure(4, "requires_gui_login")
    }
}

private func verifiedInstall() throws {
    var rule: SecRequirement?
    var code: SecStaticCode?
    guard SecRequirementCreateWithString((nativeChrome ? #"anchor apple generic and identifier "com.google.Chrome" and certificate leaf[subject.OU] = "EQHXZ8M8AV""# : requirement) as CFString, [], &rule) == errSecSuccess, let rule,
          SecStaticCodeCreateWithPath(appURL as CFURL, [], &code) == errSecSuccess, let code,
          SecStaticCodeCheckValidity(code, [], rule) == errSecSuccess else {
        throw Failure(5, "identity_error")
    }
}

// The system picker can authorize a single window without persistent guest
// screen-recording access. Never accept a display, another app, or a group.
@available(macOS 15.2, *)
@MainActor
private final class ClaudePicker: NSObject, SCContentSharingPickerObserver {
    static let shared = ClaudePicker()
    private(set) var selection: SCContentFilter?
    func choose() {
        let picker = SCContentSharingPicker.shared
        picker.remove(self)
        picker.add(self)
        var config = SCContentSharingPickerConfiguration()
        config.allowedPickerModes = [.singleWindow]
        config.allowsChangingSelectedContent = false
        picker.defaultConfiguration = config
        picker.isActive = true
        picker.present(using: .window)
    }
    nonisolated func contentSharingPicker(_ picker: SCContentSharingPicker, didUpdateWith filter: SCContentFilter, for stream: SCStream?) {
        Task { @MainActor in
            let window = filter.includedWindows.first
            guard filter.style == .window, filter.includedWindows.count == 1,
                  let owner = window?.owningApplication,
                  owner.bundleIdentifier == nativeAppBundle,
                  (try? claudePIDs().contains(owner.processID)) == true else { self.selection = nil; return }
            self.selection = filter
        }
    }
    nonisolated func contentSharingPicker(_ picker: SCContentSharingPicker, didCancelFor stream: SCStream?) {}
    nonisolated func contentSharingPickerStartDidFailWithError(_ error: Error) {}
}

@MainActor
func selectedWindow() async throws -> (SCWindow, SCContentFilter?) {
    if #available(macOS 15.2, *), let filter = ClaudePicker.shared.selection, let window = filter.includedWindows.first {
        let pids = try claudePIDs()
        guard let owner = window.owningApplication, pids.contains(owner.processID) else { throw Failure(7, "window_changed") }
        // Obtain current geometry by ID. This includes only window metadata.
        let list = CGWindowListCopyWindowInfo([.optionIncludingWindow], window.windowID) as? [[String:Any]]
        guard let exact = list?.first, exact[kCGWindowOwnerPID as String] as? Int32 == owner.processID,
              exact[kCGWindowIsOnscreen as String] as? Bool == true else { throw Failure(7, "window_changed") }
        return (window, filter)
    }
    return (try await claudeWindow(), nil)
}

@MainActor
func sendInput(_ input: Request, window: SCWindow, canvas: ClaudeCanvas? = nil, maxTextBytes: Int = 8192, scrollX: Double = 0, maxScroll: Double = 1200, inputSession: MacWindowInputSession? = nil) throws {
    let target = try checkedMacInputWindow(input, window: window)
    try MacWindowInputSession.requirePointerOwner(inputSession)
    let focused = target.focused, pid = target.pid
    let origin = target.origin, extent = target.extent
    let source = CGEventSource(stateID: .privateState)
    func keyPair(_ key: CGKeyCode, flags: CGEventFlags = [], text: String? = nil) throws {
        for down in [true, false] {
            guard let event = CGEvent(keyboardEventSource: source, virtualKey: key, keyDown: down) else { throw Failure(8, "input_failed") }
            event.flags = flags
            if let text {
                let chars = Array(text.utf16)
                chars.withUnsafeBufferPointer { event.keyboardSetUnicodeString(stringLength: chars.count, unicodeString: $0.baseAddress) }
            }
            event.postToPid(pid)
        }
    }
    switch input.action {
    case "text":
        guard let text = input.text, !text.isEmpty, text.utf8.count <= maxTextBytes else { throw Failure(8, "invalid_input") }
        for character in text { try keyPair(0, text: String(character)) }
    case "key":
        if input.code != nil {
            guard let inputSession else { throw Failure(8, "input_connection_required") }
            try inputSession.key(input, target: target, window: window)
            return
        }
        let keys: [String: CGKeyCode] = ["Enter":36, "Tab":48, "Backspace":51, "Escape":53, "Delete":117, "ArrowLeft":123, "ArrowRight":124, "ArrowDown":125, "ArrowUp":126, "a":0, "z":6]
        guard let key = input.key, let code = keys[key], (input.command == true) == ["a", "z"].contains(key) else { throw Failure(8, "invalid_input") }
        var flags: CGEventFlags = []
        if input.shift == true { flags.insert(.maskShift) }
        if input.command == true { flags.insert(.maskCommand) }
        try keyPair(code, flags: flags)
    case "pointer":
        guard let inputSession else { throw Failure(8, "input_connection_required") }
        try inputSession.pointer(input, target: target, window: window, canvas: canvas)
    case "click", "scroll", "project-click":
        guard let x = input.x, let y = input.y, (0..<1).contains(x), (0..<1).contains(y) else { throw Failure(8, "invalid_input") }
        let crop = canvas?.cropTop ?? 0
        let point = CGPoint(x: origin.x + extent.width * x, y: origin.y + crop + (extent.height-crop) * y)
        if input.action == "project-click" {
            // NSOpenPanel's remote view returns the hosting AXWindow from
            // point queries. Hit-test only its actual approval button, using
            // its live accessibility bounds and the user's frame coordinates.
            var visited = 0
            func attribute(_ node: AXUIElement, _ name: String) -> CFTypeRef? {
                var result: CFTypeRef?
                guard AXUIElementCopyAttributeValue(node, name as CFString, &result) == .success else { return nil }
                return result
            }
            func buttonAtPoint(_ node: AXUIElement, depth: Int) -> AXUIElement? {
                guard depth <= 14, visited < 2048 else { return nil }
                visited += 1
                if attribute(node, kAXRoleAttribute) as? String == kAXButtonRole,
                   attribute(node, kAXTitleAttribute) as? String == "Use Project",
                   let pos = attribute(node, kAXPositionAttribute), CFGetTypeID(pos) == AXValueGetTypeID(),
                   let dim = attribute(node, kAXSizeAttribute), CFGetTypeID(dim) == AXValueGetTypeID() {
                    var p = CGPoint.zero, s = CGSize.zero
                    if AXValueGetValue(pos as! AXValue, .cgPoint, &p), AXValueGetValue(dim as! AXValue, .cgSize, &s),
                       CGRect(origin: p, size: s).contains(point) { return node }
                }
                for child in attribute(node, kAXChildrenAttribute) as? [AXUIElement] ?? [] {
                    if let match = buttonAtPoint(child, depth: depth + 1) { return match }
                }
                return nil
            }
            guard let button = buttonAtPoint(focused, depth: 0),
                  AXUIElementPerformAction(button, kAXPressAction as CFString) == .success else {
                throw Failure(8, "project_button_not_hit")
            }
            return
        }
        // An Electron NSView may ignore a CGEvent addressed to its process
        // while accepting the accessibility press on the hit control. Never
        // act on another window, a nearby control, or an arbitrary AX node.
        if input.action == "click", let button = macPressTarget(target, point: point) {
            guard AXUIElementPerformAction(button, kAXPressAction as CFString) == .success else { throw Failure(8, "input_failed") }
            return
        }
        if nativeChrome && !nativeChromeOwnsPointer(point,pid:pid) { throw Failure(7,"window_changed") }
        let events: [CGEvent?]
        if input.action == "scroll" {
            let horizontal = input.deltaX ?? scrollX
            guard let delta = input.delta, delta.isFinite, horizontal.isFinite,
                  (-maxScroll...maxScroll).contains(delta), (-maxScroll...maxScroll).contains(horizontal) else { throw Failure(8, "invalid_input") }
            events = [CGEvent(scrollWheelEvent2Source: source, units: .pixel, wheelCount: horizontal == 0 ? 1 : 2, wheel1: Int32(-delta), wheel2: Int32(-horizontal), wheel3: 0)]
        } else if nativeChrome {
            events = [CGEvent(mouseEventSource:source,mouseType:.leftMouseDown,mouseCursorPosition:point,mouseButton:.left),
                      CGEvent(mouseEventSource:source,mouseType:.leftMouseUp,mouseCursorPosition:point,mouseButton:.left)]
        } else {
            // Address the NSEvent to this window using its unflipped local
            // coordinates. CGEvent's global position alone does not give an
            // Electron NSView a locationInWindow when posting to a process.
            let local = CGPoint(x: point.x-origin.x, y: extent.height-(point.y-origin.y))
            let types: [NSEvent.EventType] = [.leftMouseDown, .leftMouseUp]
            events = types.map { type in
                NSEvent.mouseEvent(with: type, location: local, modifierFlags: [],
                                   timestamp: ProcessInfo.processInfo.systemUptime, windowNumber: Int(window.windowID),
                                   context: nil, eventNumber: 0, clickCount: 1, pressure: type == .leftMouseDown ? 1 : 0)?.cgEvent
            }
        }
        for item in events {
            guard let event = item else { throw Failure(8, "input_failed") }
            event.location = point
            event.setIntegerValueField(.mouseEventWindowUnderMousePointer, value: Int64(window.windowID))
            event.setIntegerValueField(.mouseEventWindowUnderMousePointerThatCanHandleThisEvent, value: Int64(window.windowID))
            event.setIntegerValueField(.mouseEventClickState, value: 1)
            if nativeChrome { event.post(tap:.cghidEventTap) } else { event.postToPid(pid) }
        }
    default: throw Failure(8, "invalid_input")
    }
}

@MainActor
func launchClaudeWindow() async throws {
    try verifiedInstall()
    let configuration = NSWorkspace.OpenConfiguration()
    configuration.activates = true
    if nativeChrome {
        configuration.createsNewApplicationInstance = true
        configuration.arguments = ["--user-data-dir=" + nativeChromeProfile, "--no-first-run", "--restore-last-session"]
        try FileManager.default.createDirectory(atPath: nativeChromeProfile, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        if let pid = try? claudePIDs().first, let running = NSRunningApplication(processIdentifier: pid) {
            let element = AXUIElementCreateApplication(pid)
            if AXIsProcessTrusted() { _ = AXUIElementSetAttributeValue(element, "AXManualAccessibility" as CFString, kCFBooleanTrue) }
            running.activate()
            for _ in 0..<20 {
                if NSWorkspace.shared.frontmostApplication?.processIdentifier == pid { return }
                try await Task.sleep(nanoseconds: 50_000_000)
            }
            return
        }
    }
    let application = try await NSWorkspace.shared.openApplication(at: appURL, configuration: configuration)
    if AXIsProcessTrusted(), try claudePIDs().contains(application.processIdentifier) {
        let element = AXUIElementCreateApplication(application.processIdentifier)
        _ = AXUIElementSetAttributeValue(element, "AXManualAccessibility" as CFString, kCFBooleanTrue)
    }
}

@MainActor
private func chromeInputTargetMetadata() -> [String:Any] {
    guard nativeChrome else { return [:] }
    var result: [String:Any] = ["foreground_pid": NSWorkspace.shared.frontmostApplication?.processIdentifier ?? 0]
    for pid in (try? claudePIDs()) ?? [] {
        let app = AXUIElementCreateApplication(pid)
        var focused: CFTypeRef?
        let status = AXUIElementCopyAttributeValue(app, kAXFocusedWindowAttribute as CFString, &focused)
        result["pid"] = pid; result["focused_status"] = status.rawValue
        if let focused, CFGetTypeID(focused) == AXUIElementGetTypeID() {
            let window = focused as! AXUIElement
            var point: CFTypeRef?, size: CFTypeRef?
            var origin = CGPoint.zero, extent = CGSize.zero
            if AXUIElementCopyAttributeValue(window, kAXPositionAttribute as CFString, &point) == .success,
               AXUIElementCopyAttributeValue(window, kAXSizeAttribute as CFString, &size) == .success,
               let point, let size, CFGetTypeID(point) == AXValueGetTypeID(), CFGetTypeID(size) == AXValueGetTypeID(),
               AXValueGetValue(point as! AXValue, .cgPoint, &origin), AXValueGetValue(size as! AXValue, .cgSize, &extent) {
                result["focused_bounds"] = [origin.x,origin.y,extent.width,extent.height]
            }
        }
        let windows = (CGWindowListCopyWindowInfo([.optionOnScreenOnly,.excludeDesktopElements], kCGNullWindowID) as? [[String:Any]]) ?? []
        result["visible_windows"] = windows.filter { $0[kCGWindowOwnerPID as String] as? Int32 == pid }.map {
            ["id":$0[kCGWindowNumber as String] ?? 0,"bounds":$0[kCGWindowBounds as String] ?? [:]]
        }
        break
    }
    return result
}

@MainActor
private func handle(_ input: Request, inputSession: MacWindowInputSession) async throws -> [String: Any] {
    try requireGuest()
    if input.action.hasPrefix("phone-") {
        return try await MacPhoneDesktop.shared.handle(input)
    }
    try consoleUser()
    switch input.action {
    case "files-start":
        let (window, _) = try await selectedWindow()
        guard input.pid == window.owningApplication?.processID, input.window == window.windowID else { throw Failure(7, "window_changed") }
        return try MacDesktopFiles.shared.start(window: window, kind: input.kind ?? "")
    case "files-add":
        return try MacDesktopFiles.shared.add(input.id ?? "", files: input.files ?? [])
    case "files-validate":
        return try MacDesktopFiles.shared.validate(input.id ?? "")
    case "files-finish":
        return try await MacDesktopFiles.shared.finish(input.id ?? "")
    case "files-finish-image":
        return try await MacDesktopFiles.shared.finishImage(input.id ?? "")
    case "files-cancel":
        return await MacDesktopFiles.shared.cancel(input.id ?? "")
    case "files-poll":
        return try await MacDesktopFiles.shared.poll()
    case "files-download-open", "files-download-read", "files-download-close", "files-download-retry", "files-download-ack":
        return try MacDesktopDownloads.shared.read(input)
    case "files-open-claim", "files-open-complete":
        return try MacDesktopFiles.shared.openAction(input.id ?? "", complete: input.action == "files-open-complete")
    case "audio-status":
        return MacAudioCapture.status()
    case "setup-status":
        return ["screen": CGPreflightScreenCaptureAccess(), "input": AXIsProcessTrusted()]
    case "setup-password-focus":
        return try MacSetupPassword.probe()
    case "setup-screen", "setup-input", "setup-projects":
        let pane: String
        if input.action == "setup-screen" {
            if #available(macOS 15.2, *) { SCContentSharingPicker.shared.isActive = false }
            if !CGPreflightScreenCaptureAccess() { _ = CGRequestScreenCaptureAccess() }
            pane = "com.apple.preference.security?Privacy_ScreenCapture"
        } else if input.action == "setup-input" {
            let options = [kAXTrustedCheckOptionPrompt.takeUnretainedValue() as String: true] as CFDictionary
            _ = AXIsProcessTrustedWithOptions(options)
            pane = "com.apple.preference.security?Privacy_Accessibility"
        } else {
            let vendor = URL(fileURLWithPath: "/Applications/macFUSE.app")
            let configuration = NSWorkspace.OpenConfiguration()
            configuration.activates = false
            _ = try await NSWorkspace.shared.openApplication(at: vendor, configuration: configuration)
            pane = "com.apple.LoginItems-Settings.extension"
        }
        guard let url = URL(string: "x-apple.systempreferences:" + pane), NSWorkspace.shared.open(url) else { throw Failure(7, "settings_unavailable") }
        NSRunningApplication.runningApplications(withBundleIdentifier: "com.apple.systempreferences").first?.activate()
        return ["ok":true]
    case "status":
        let installed = FileManager.default.fileExists(atPath: appURL.path)
        let running = ((try? claudePIDs())?.isEmpty == false)
        var screen = CGPreflightScreenCaptureAccess()
        if #available(macOS 15.2, *) { screen = screen || ClaudePicker.shared.selection != nil }
        return ["version": (Bundle(url: appURL)?.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String) ?? "", "installed": installed, "running": running, "screen": screen, "input": AXIsProcessTrusted(), "input_protocol": 1, "persistent": true, "push_frames": true, "foreground": NSWorkspace.shared.frontmostApplication?.bundleIdentifier ?? "", "foreground_pid": NSWorkspace.shared.frontmostApplication?.processIdentifier ?? 0, "input_target":chromeInputTargetMetadata()]
    case "open-url":
        guard nativeChrome, let address = input.url, address.utf8.count <= 4096,
              let url = URL(string: address), ["http","https"].contains(url.scheme?.lowercased() ?? ""),
              url.host != nil, url.user == nil, url.password == nil else { throw Failure(8,"invalid_input") }
        try await launchClaudeWindow()
        let command = Process()
        command.executableURL = appURL.appendingPathComponent("Contents/MacOS/Google Chrome")
        command.arguments = ["--user-data-dir=" + nativeChromeProfile, url.absoluteString]
        command.standardOutput = FileHandle.nullDevice; command.standardError = FileHandle.nullDevice
        try command.run()
        return ["ok":true]
    case "launch":
        try await launchClaudeWindow()
        return ["ok": true]
    case "stop":
        for pid in try claudePIDs() {
            guard let app = NSRunningApplication(processIdentifier: pid), app.terminate() else { throw Failure(7, "window_changed") }
        }
        return ["ok": true]
    case "request-screen":
        if #available(macOS 15.2, *) { ClaudePicker.shared.choose() } else { _ = CGRequestScreenCaptureAccess() }
        return ["screen": CGPreflightScreenCaptureAccess()]
    case "request-input":
        let options = [kAXTrustedCheckOptionPrompt.takeUnretainedValue() as String: true] as CFDictionary
        return ["input": AXIsProcessTrustedWithOptions(options)]
    case "frame":
        if input.wait != true && input.refresh != true && MacWindowStream.shared.unchanged(after: input.after) {
            return ["unchanged": true, "sequence": input.after!]
        }
        let (window, filter) = try await selectedWindow()
        if input.width != nil || input.height != nil || input.scale != nil {
            guard let width=input.width, let height=input.height, let scale=input.scale else { throw Failure(8,"invalid_input") }
            let capture = filter ?? SCContentFilter(desktopIndependentWindow: window)
            let canvas = ClaudeCanvasSession.shared
            let layout = try await canvas.layout(window, filter:capture, width:width, height:height, scale:scale)
            // A window object from before AX resizing still describes the old
            // surface. Reacquire that exact window before the first new frame.
            var target = window, source = capture
            if abs(window.frame.width - layout.frame.width) >= 1 || abs(window.frame.height - layout.frame.height) >= 1 {
                let (fresh, selection) = try await selectedWindow()
                guard fresh.windowID == layout.id, fresh.owningApplication?.processID == layout.pid else {
                    throw Failure(7, "window_changed")
                }
                target = fresh
                source = selection ?? SCContentFilter(desktopIndependentWindow: fresh)
            }
            let reply = try await canvas.capture(target, filter:source, layout:layout, after:input.after, wait:input.wait == true, binary:input.binary == true)
            try consoleUser()
            return reply
        }
        let data = try await screenshot(window, filter: filter)
        guard data.count < 8 << 20, let owner = window.owningApplication, try claudePIDs().contains(owner.processID) else {
            throw Failure(7, "window_changed")
        }
        try consoleUser()
        return ["image": data.base64EncodedString(), "window": window.windowID, "pid": owner.processID]
    case "input-reset":
        inputSession.reset()
        return ["ok": true]
    case "ime":
        let (window, _) = try await selectedWindow()
        let canvas = try input.geometry.map { try ClaudeCanvasSession.shared.inputGeometry($0, window: window) }
        return try inputSession.ime(input, window: window, canvas: canvas) { value in
            if value.isEmpty { return }
            var text = Request(action: "text")
            text.pid = input.pid; text.window = input.window; text.geometry = input.geometry; text.text = value
            try sendInput(text, window: window, canvas: canvas, inputSession: inputSession)
        }
    case "text", "key", "click", "scroll", "pointer":
        let (window, _) = try await selectedWindow()
        let canvas = try input.geometry.map { try ClaudeCanvasSession.shared.inputGeometry($0, window:window) }
        try sendInput(input, window: window, canvas:canvas, inputSession: inputSession)
        return ["ok": true]
    case "project-frame", "project-click":
        guard let path = input.project else { throw Failure(7, "no_project_panel") }
        let panel = try pendingProjectPanel(path)
        let window = try await visibleProjectPanel(panel)
        if input.action == "project-click" {
            guard input.nonce == panel.nonce, try pendingProjectPanel(path) == panel else { throw Failure(7, "window_changed") }
            try sendInput(input, window: window)
            for _ in 0..<16 {
                try await Task.sleep(nanoseconds: 100_000_000)
                if (try? pendingProjectPanel(path)) != panel { return ["ok": true] }
            }
            throw Failure(8, "project_panel_still_open")
        }
        let data = try await screenshot(window)
        guard data.count < 8 << 20, try pendingProjectPanel(path) == panel else { throw Failure(7, "window_changed") }
        try consoleUser()
        return ["image": data.base64EncodedString(), "window": panel.windowNumber, "pid": panel.pid, "nonce": panel.nonce]
    default: throw Failure(8, "unknown_action")
    }
}

private func respond(_ fd: Int32) async {
    let inputSession = await MainActor.run { MacWindowInputSession() }
    await respondConnection(fd, inputSession: inputSession)
    await MainActor.run { inputSession.reset() }
}

private func respondConnection(_ fd: Int32, inputSession: MacWindowInputSession) async {
    let file = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
    var timeout = timeval(tv_sec: 6, tv_usec: 0)
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, socklen_t(MemoryLayout.size(ofValue: timeout)))
    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, socklen_t(MemoryLayout.size(ofValue: timeout)))
    var peer = uid_t(), group = gid_t()
    guard getpeereid(fd, &peer, &group) == 0, peer == getuid() else { return }
    var body = Data()
    do {
      repeat {
        while !body.contains(10) {
            var chunk = [UInt8](repeating: 0, count: (64 << 10) - body.count)
            let count = Darwin.read(fd, &chunk, chunk.count)
            if count < 0 && errno == EINTR { continue }
            guard count > 0 else { return }
            body.append(contentsOf: chunk.prefix(count))
            guard body.count < (64 << 10) || body.contains(10) else { return }
        }
        let line = body.prefix(while: { $0 != 10 })
        let request = try JSONDecoder().decode(Request.self, from: line)
        body.removeFirst(line.count + 1)
        if request.action == "audio-capture" {
            guard body.isEmpty else { return }
            await MacAudioCapture.respond(fd)
            return
        }
        var reply: [String:Any]
        do {
            if request.action == "paste-image" {
                guard let length = request.length, length > 0, length <= 20 * 1024 * 1024, body.count <= length else { throw Failure(8, "attachment_size_limit") }
                let (window, _) = try await selectedWindow()
                guard request.pid == window.owningApplication?.processID, request.window == window.windowID else { throw Failure(7, "window_changed") }
                var image = body
                body.removeAll()
                while image.count < length {
                    guard let chunk = try file.read(upToCount: min(65536, length - image.count)), !chunk.isEmpty else { throw Failure(8, "attachment_incomplete") }
                    image.append(chunk)
                }
                reply = try await MacDesktopFiles.shared.pasteImage(image, window: window)
            } else { reply = try await handle(request, inputSession: inputSession) }
        }
        catch {
            let reason = (error as? Failure)?.message.components(separatedBy: ":").first ?? "window_unavailable"
            GuestDiagnostic.record("window", reason)
            reply = ["error": reason]
        }
        if request.binary == true && request.action == "frame" {
            let pixels = reply.removeValue(forKey: "imageBytes") as? Data ?? Data()
            reply["imageLength"] = pixels.count
            let header = try JSONSerialization.data(withJSONObject: reply)
            var length = UInt32(header.count).bigEndian
            try file.write(contentsOf: withUnsafeBytes(of: &length) { Data($0) })
            try file.write(contentsOf: header)
            try file.write(contentsOf: pixels)
        } else {
            var data = try JSONSerialization.data(withJSONObject: reply)
            data.append(10)
            try file.write(contentsOf: data)
        }
        if request.keepAlive != true { return }
      } while true
    } catch {
        let reason = (error as? Failure)?.message.components(separatedBy: ":").first ?? "window_unavailable"
        GuestDiagnostic.record("window", reason)
        if let data = try? JSONSerialization.data(withJSONObject: ["error": reason]) { try? file.write(contentsOf: data) }
    }
}

@main
struct ClaudeWindowService {
    static func main() {
        let arguments = Array(CommandLine.arguments.dropFirst())
        if arguments == ["--setup-password-probe"] { MacSetupPassword.runStream() }
        if arguments.count == 2 && ["--maintenance-open", "--maintenance-probe", "--maintenance-password-probe"].contains(arguments[0]) {
            if arguments[0] == "--maintenance-password-probe" { MacMaintenance.runPasswordProbe(purpose: arguments[1]) }
            let result: [String: Any]
            do { result = try MacMaintenance.run(arguments[0] == "--maintenance-open" ? "open" : "probe", purpose: arguments[1]) }
            catch { result = ["error": (error as? Failure)?.message ?? "maintenance_unavailable"] }
            if let data = try? JSONSerialization.data(withJSONObject: result) {
                FileHandle.standardOutput.write(data)
                FileHandle.standardOutput.write(Data([10]))
            }
            return
        }
        if CommandLine.arguments.dropFirst() == ["--setup-password-self-test"] {
            do { try MacSetupPassword.selfTest(); return }
            catch { fputs("macOS setup password self-test failed\n", stderr); exit(1) }
        }
        if CommandLine.arguments.dropFirst() == ["--native-open-self-test"] {
            do { try MacNativeOpen.selfTest(); return }
            catch { fputs("macOS native open self-test failed (\((error as? Failure)?.message ?? "unknown error"))\n", stderr); exit(1) }
        }
        if CommandLine.arguments.dropFirst() == ["--phone-self-test"] {
            do { try MacPhoneDesktop.selfTest(); return }
            catch { fputs("macOS phone desktop self-test failed\n", stderr); exit(1) }
        }
        if CommandLine.arguments.dropFirst() == ["--audio-self-test"] {
            do { try MacAudioCapture.selfTest(); return }
            catch { fputs("macOS audio self-test failed\n", stderr); exit(1) }
        }
        guard (try? requireGuest()) != nil, (try? consoleUser()) != nil else { exit(2) }
        _ = signal(SIGPIPE, SIG_IGN)
        let storage = UnsafeMutablePointer<UnsafeMutablePointer<Int32>>.allocate(capacity: 1)
        var count: Int = 0
        guard launch_activate_socket("Window", storage, &count) == 0, count == 1 else { exit(3) }
        let sockets = storage.pointee
        storage.deallocate()
        let listener = sockets[0]
        free(sockets)
        GuestDiagnostic.record("window", "started")
        NSApplication.shared.setActivationPolicy(.accessory)
        if !nativeChrome { MacNativeOpen.shared.install() }
        Task { @MainActor in
            do { if !nativeChrome { try await MacNativeOpen.shared.prepareMetadata() } }
            catch { GuestDiagnostic.record("files", (error as? Failure)?.message ?? "file_open_metadata_failed") }
        }
        DispatchQueue.global(qos: .userInitiated).async {
            let connections = DispatchSemaphore(value: 16)
            while true {
                connections.wait()
                let fd = accept(listener, nil, nil)
                if fd < 0 { connections.signal(); if errno == EINTR { continue }; exit(4) }
                Task { await respond(fd); connections.signal() }
            }
        }
        NSApplication.shared.run()
    }
}
