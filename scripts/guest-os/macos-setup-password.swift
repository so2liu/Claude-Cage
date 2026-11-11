import AppKit
import IOKit
import Security

@MainActor
enum MacSetupPassword {
    private static func validateRequest(_ data: Data) throws {
        guard data.count <= 4096, let value = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              value.isEmpty || (value.count == 1 && value["action"] as? String == "probe") else {
            throw Failure(8, "setup_password_probe_request_invalid")
        }
    }

    nonisolated private static func nextLine() -> Data? {
        var data = Data()
        var oversized = false
        while true {
            let byte = fgetc(stdin)
            if byte == EOF { return data.isEmpty && !oversized ? nil : (oversized ? Data() : data) }
            if byte == 10 { return oversized ? Data() : data }
            if data.count < 4096 { data.append(UInt8(byte)) }
            else { oversized = true }
        }
    }

    static func runStream() -> Never {
        _ = signal(SIGPIPE, SIG_IGN)
        NSApplication.shared.setActivationPolicy(.prohibited)
        DispatchQueue.global(qos: .userInitiated).async {
            while let data = nextLine() {
                let finished = DispatchSemaphore(value: 0)
                DispatchQueue.main.async {
                    let response: [String: Any]
                    do {
                        try validateRequest(data)
                        response = try probe()
                    } catch { response = ["error": (error as? Failure)?.message ?? "setup_password_probe_failed"] }
                    do {
                        var encoded = try JSONSerialization.data(withJSONObject: response)
                        encoded.append(10)
                        try FileHandle.standardOutput.write(contentsOf: encoded)
                    } catch { exit(0) }
                    finished.signal()
                }
                finished.wait()
            }
            DispatchQueue.main.async { exit(0) }
        }
        NSApplication.shared.run()
        exit(0)
    }

    struct Window {
        let id: UInt32
        let pid: pid_t
        let bounds: CGRect
        let layer: Int
        var systemSurface = false
    }

    private static func signed(_ app: NSRunningApplication, bundle: String) -> Bool {
        guard app.bundleIdentifier == bundle else { return false }
        var code: SecCode?, requirement: SecRequirement?
        return SecRequirementCreateWithString("anchor apple and identifier \"\(bundle)\"" as CFString, [], &requirement) == errSecSuccess &&
            SecCodeCopyGuestWithAttributes(nil, [kSecGuestAttributePid as String: app.processIdentifier] as CFDictionary, [], &code) == errSecSuccess &&
            code != nil && requirement != nil && SecCodeCheckValidity(code!, [], requirement!) == errSecSuccess
    }

    private static func consoleSession() throws -> String {
        try consoleUser()
        let entry = IORegistryGetRootEntry(kIOMainPortDefault)
        guard entry != 0 else { throw Failure(4, "requires_gui_login") }
        defer { IOObjectRelease(entry) }
        guard let property = IORegistryEntryCreateCFProperty(entry, "IOConsoleUsers" as CFString, kCFAllocatorDefault, 0)?.takeRetainedValue(),
              let sessions = property as? [[String: Any]] else { throw Failure(4, "requires_gui_login") }
        let active = sessions.filter { $0["kCGSSessionOnConsoleKey"] as? Bool == true }
        guard active.count == 1, let current = active.first,
              current["kCGSessionLoginDoneKey"] as? Bool == true,
              current["CGSSessionScreenIsLocked"] as? Bool != true,
              current["kCGSSessionUserIDKey"] as? UInt32 == getuid(),
              current["kCGSSessionUserNameKey"] as? String == NSUserName(),
              let session = current["CGSSessionUniqueSessionUUID"] as? String,
              UUID(uuidString: session) != nil else { throw Failure(4, "requires_gui_login") }
        return session
    }

    static func select(windows: [Window], pid: pid_t, bundle: String, display: CGRect, accessibility: Bool) throws -> Window {
        guard ["com.apple.systempreferences", "com.apple.authorizationhost", "com.apple.SecurityAgent"].contains(bundle),
              let index = windows.firstIndex(where: { $0.pid == pid && ($0.layer == 0 || bundle == "com.apple.SecurityAgent" && $0.layer == 1000) && $0.bounds.width >= 80 && $0.bounds.height >= 40 }) else {
            throw Failure(7, "setup_password_window_unavailable")
        }
        let target = windows[index]
        guard display.contains(target.bounds), target.id != 0 else { throw Failure(7, "setup_password_window_unavailable") }
        if !accessibility && bundle == "com.apple.systempreferences" {
            let parent = windows.dropFirst(index + 1).contains { window in
                window.pid == pid && window.layer == 0 && window.bounds.contains(target.bounds) &&
                    window.bounds.width > target.bounds.width && window.bounds.height > target.bounds.height &&
                    target.bounds.width * target.bounds.height < window.bounds.width * window.bounds.height * 0.75
            }
            guard parent else { throw Failure(7, "setup_password_window_unavailable") }
        }
        for window in windows.prefix(index) where !window.systemSurface && window.bounds.intersects(target.bounds) {
            throw Failure(7, "setup_password_window_obscured")
        }
        return target
    }

    private static func passwordFocused(_ app: NSRunningApplication, window: CGRect) -> Bool {
        let application = AXUIElementCreateApplication(app.processIdentifier)
        AXUIElementSetMessagingTimeout(application, 0.25)
        func attribute(_ element: AXUIElement, _ key: String) -> CFTypeRef? {
            var value: CFTypeRef?
            return AXUIElementCopyAttributeValue(element, key as CFString, &value) == .success ? value : nil
        }
        guard let value = attribute(application, kAXFocusedUIElementAttribute), CFGetTypeID(value) == AXUIElementGetTypeID() else { return false }
        let field = value as! AXUIElement
        var pid: pid_t = 0
        guard AXUIElementGetPid(field, &pid) == .success, pid == app.processIdentifier,
              attribute(field, kAXSubroleAttribute) as? String == kAXSecureTextFieldSubrole,
              attribute(field, kAXEnabledAttribute) as? Bool == true,
              let owner = attribute(field, kAXWindowAttribute), CFGetTypeID(owner) == AXUIElementGetTypeID() else { return false }
        var origin = CGPoint.zero, size = CGSize.zero
        guard let position = attribute(owner as! AXUIElement, kAXPositionAttribute), let extent = attribute(owner as! AXUIElement, kAXSizeAttribute),
              CFGetTypeID(position) == AXValueGetTypeID(), CFGetTypeID(extent) == AXValueGetTypeID(),
              AXValueGetValue(position as! AXValue, .cgPoint, &origin), AXValueGetValue(extent as! AXValue, .cgSize, &size) else { return false }
        return abs(origin.x - window.minX) < 1 && abs(origin.y - window.minY) < 1 &&
            abs(size.width - window.width) < 1 && abs(size.height - window.height) < 1
    }

    static func probe() throws -> [String: Any] {
        try requireGuest()
        let session = try consoleSession()
        guard let front = MacMaintenance.targetApplication(), let bundle = front.bundleIdentifier,
              ["com.apple.systempreferences", "com.apple.authorizationhost", "com.apple.SecurityAgent"].contains(bundle), signed(front, bundle: bundle) else {
            throw Failure(7, "setup_password_foreground_changed")
        }
        guard let raw = CGWindowListCopyWindowInfo([.optionOnScreenOnly, .excludeDesktopElements], kCGNullWindowID) as? [[String: Any]] else {
            throw Failure(7, "setup_password_window_unavailable")
        }
        let display = CGDisplayBounds(CGMainDisplayID())
        var trustedSurfaces: [pid_t: Bool] = [:]
        let windows = raw.compactMap { item -> Window? in
            guard let id = item[kCGWindowNumber as String] as? UInt32, let pid = item[kCGWindowOwnerPID as String] as? Int32,
                  let rectangle = item[kCGWindowBounds as String] as? [String: Any], let bounds = CGRect(dictionaryRepresentation: rectangle as CFDictionary),
                  bounds.width > 0, bounds.height > 0, (item[kCGWindowAlpha as String] as? Double ?? 1) > 0,
                  let layer = item[kCGWindowLayer as String] as? Int else { return nil }
            var surface = MacMaintenance.cursorSurface(pid, bounds: bounds, layer: layer)
            if layer > 0, abs(bounds.minX - display.minX) < 1, abs(bounds.minY - display.minY) < 1,
               abs(bounds.width - display.width) < 1, abs(bounds.height - display.height) < 1 {
                if let cached = trustedSurfaces[pid] { surface = cached }
                else {
                    if let app = NSRunningApplication(processIdentifier: pid), let owner = app.bundleIdentifier,
                       ["com.apple.dock", "com.apple.notificationcenterui"].contains(owner) { surface = signed(app, bundle: owner) }
                    trustedSurfaces[pid] = surface
                }
            }
            return Window(id: id, pid: pid, bounds: bounds, layer: layer, systemSurface: surface)
        }
        let accessibility = AXIsProcessTrusted()
        let target = try select(windows: windows, pid: front.processIdentifier, bundle: bundle, display: display, accessibility: accessibility)
        let focused = accessibility && passwordFocused(front, window: target.bounds)
        guard MacMaintenance.targetIsCurrent(front),
              try consoleSession() == session else { throw Failure(7, "setup_password_foreground_changed") }
        return ["version": 1, "session": session, "user": NSUserName(), "pid": front.processIdentifier,
                "window": target.id, "bundle": bundle, "rect": [target.bounds.minX, target.bounds.minY, target.bounds.width, target.bounds.height],
                "display": [display.minX, display.minY, display.width, display.height], "accessibility": accessibility,
                "password_focused": focused, "selection_required": !accessibility]
    }

    static func selfTest() throws {
        try validateRequest(Data("{}".utf8))
        try validateRequest(Data("{\"action\":\"probe\"}".utf8))
        for request in ["", "[]", "{\"action\":\"other\"}", "{\"action\":\"probe\",\"text\":\"test-secret\"}"] {
            do { try validateRequest(Data(request.utf8)); throw Failure(1, "setup_password_test_request") }
            catch let error as Failure where error.message == "setup_password_probe_request_invalid" { }
        }
        let display = CGRect(x: 0, y: 0, width: 1920, height: 1200)
        let parent = Window(id: 38, pid: 2338, bounds: CGRect(x: 598, y: 258, width: 723, height: 625), layer: 0)
        let panel = Window(id: 49, pid: 2338, bounds: CGRect(x: 830, y: 376, width: 260, height: 389), layer: 0)
        let overlay = Window(id: 8, pid: 613, bounds: display, layer: 20, systemSurface: true)
        guard try select(windows: [overlay, panel, parent], pid: panel.pid, bundle: "com.apple.systempreferences", display: display, accessibility: false).id == panel.id else { throw Failure(1, "setup_password_test_selection") }
        let ordinary = Window(id: 9, pid: 999, bounds: panel.bounds, layer: 0)
        for windows in [[parent], [ordinary, panel, parent], [Window(id: 9, pid: 999, bounds: display, layer: 20), panel, parent], [panel]] {
            do { _ = try select(windows: windows, pid: panel.pid, bundle: "com.apple.systempreferences", display: display, accessibility: false); throw Failure(1, "setup_password_test_accepted_invalid") }
            catch let error as Failure where error.code == 7 { }
        }
        guard try select(windows: [panel], pid: panel.pid, bundle: "com.apple.authorizationhost", display: display, accessibility: false).id == panel.id,
              try select(windows: [parent], pid: panel.pid, bundle: "com.apple.systempreferences", display: display, accessibility: true).id == parent.id else { throw Failure(1, "setup_password_test_accessibility") }
        print("macOS setup password window selection, system surfaces, obscuration and first-permission cases passed")
    }
}
