import AppKit
import IOKit
import Security

// Invoked directly by the installation identity after verifying this bundle.
// Only Apple system settings and its authorization UI can supply a target.
@MainActor
enum MacMaintenance {
    private static let applications: Set<String> = ["com.apple.systempreferences", "com.apple.authorizationhost", "com.apple.SecurityAgent"]
    private static let panes = [
        "projects": "x-apple.systempreferences:com.apple.ExtensionsPreferences?extensionPointIdentifier=com.apple.fskit.fsmodule",
        "screen": "x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture",
        "input": "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility",
    ]

    static func settingsURL(for purpose: String) -> URL? {
        panes[purpose].flatMap { URL(string: $0) }
    }

    static func runPasswordProbe(purpose: String) -> Never {
        guard panes[purpose] != nil else { exit(2) }
        MacSetupPassword.runStream()
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

    private static func verified(_ app: NSRunningApplication) -> Bool {
        guard let bundle = app.bundleIdentifier, applications.contains(bundle) else { return false }
        var code: SecCode?, requirement: SecRequirement?
        return SecRequirementCreateWithString("anchor apple and identifier \"\(bundle)\"" as CFString, [], &requirement) == errSecSuccess &&
            SecCodeCopyGuestWithAttributes(nil, [kSecGuestAttributePid as String: app.processIdentifier] as CFDictionary, [], &code) == errSecSuccess &&
            code != nil && requirement != nil && SecCodeCheckValidity(code!, [], requirement!) == errSecSuccess
    }

    // macOS 26 can host the front System Settings authorization sheet in
    // Apple's separate SecurityAgent process. Select only its top visible
    // system-level panel, with a verified Apple signature.
    static func targetApplication() -> NSRunningApplication? {
        guard let front = NSWorkspace.shared.frontmostApplication, verified(front) else { return nil }
        if front.bundleIdentifier == "com.apple.systempreferences",
           let agent = NSRunningApplication.runningApplications(withBundleIdentifier: "com.apple.SecurityAgent").first,
           verified(agent),
           let windows = CGWindowListCopyWindowInfo([.optionOnScreenOnly,.excludeDesktopElements], kCGNullWindowID) as? [[String:Any]],
           windows.contains(where: { $0[kCGWindowOwnerPID as String] as? Int32 == agent.processIdentifier && $0[kCGWindowLayer as String] as? Int == 1000 }) { return agent }
        return front
    }
    static func targetIsCurrent(_ app: NSRunningApplication) -> Bool {
        targetApplication()?.processIdentifier == app.processIdentifier
    }
    static func cursorSurface(_ pid: Int32, bounds: CGRect, layer: Int) -> Bool {
        guard layer == CGWindowLevelForKey(.cursorWindow), bounds.width <= 64, bounds.height <= 64 else { return false }
        var code: SecCode?, rule: SecRequirement?
        return SecRequirementCreateWithString(#"anchor apple and identifier "com.apple.WindowServer""# as CFString, [], &rule) == errSecSuccess &&
            SecCodeCopyGuestWithAttributes(nil, [kSecGuestAttributePid as String:pid] as CFDictionary, [], &code) == errSecSuccess &&
            code != nil && rule != nil && SecCodeCheckValidity(code!, [], rule!) == errSecSuccess
    }
    static func backgroundSystemSurface(_ pid: Int32, bounds: CGRect, display: CGRect) -> Bool {
        guard bounds == display, let app = NSRunningApplication(processIdentifier: pid),
              let bundle = app.bundleIdentifier, ["com.apple.dock","com.apple.notificationcenterui"].contains(bundle) else { return false }
        var code: SecCode?, rule: SecRequirement?
        return SecRequirementCreateWithString("anchor apple and identifier \"\(bundle)\"" as CFString, [], &rule) == errSecSuccess &&
            SecCodeCopyGuestWithAttributes(nil, [kSecGuestAttributePid as String:pid] as CFDictionary, [], &code) == errSecSuccess &&
            code != nil && rule != nil && SecCodeCheckValidity(code!, [], rule!) == errSecSuccess
    }

    private static func passwordFocused(_ app: NSRunningApplication, window: CGRect) -> Bool {
        guard let bundle = app.bundleIdentifier, applications.contains(bundle), AXIsProcessTrusted() else { return false }
        let application = AXUIElementCreateApplication(app.processIdentifier)
        AXUIElementSetMessagingTimeout(application, 0.5)
        func attribute(_ element: AXUIElement, _ name: String) -> CFTypeRef? {
            var value: CFTypeRef?
            return AXUIElementCopyAttributeValue(element, name as CFString, &value) == .success ? value : nil
        }
        guard let value = attribute(application, kAXFocusedUIElementAttribute), CFGetTypeID(value) == AXUIElementGetTypeID() else { return false }
        let field = value as! AXUIElement
        var pid: pid_t = 0
        guard AXUIElementGetPid(field, &pid) == .success, pid == app.processIdentifier,
              attribute(field, kAXSubroleAttribute) as? String == kAXSecureTextFieldSubrole,
              attribute(field, kAXEnabledAttribute) as? Bool == true,
              let rawWindow = attribute(field, kAXWindowAttribute), CFGetTypeID(rawWindow) == AXUIElementGetTypeID() else { return false }
        let owner = rawWindow as! AXUIElement
        var origin = CGPoint.zero, size = CGSize.zero
        guard let position = attribute(owner, kAXPositionAttribute), let extent = attribute(owner, kAXSizeAttribute),
              CFGetTypeID(position) == AXValueGetTypeID(), CFGetTypeID(extent) == AXValueGetTypeID(),
              AXValueGetValue(position as! AXValue, .cgPoint, &origin), AXValueGetValue(extent as! AXValue, .cgSize, &size) else { return false }
        return abs(origin.x - window.minX) < 1 && abs(origin.y - window.minY) < 1 &&
            abs(size.width - window.width) < 1 && abs(size.height - window.height) < 1
    }

    static func run(_ operation: String, purpose: String) throws -> [String: Any] {
        try requireGuest()
        let session = try consoleSession()
        guard let url = settingsURL(for: purpose) else { throw Failure(8, "maintenance_purpose_invalid") }
        if operation == "open" {
            if purpose == "projects" {
                let configuration = NSWorkspace.OpenConfiguration()
                configuration.activates = false
                NSWorkspace.shared.openApplication(at: URL(fileURLWithPath: "/Applications/macFUSE.app"), configuration: configuration)
            }
            guard NSWorkspace.shared.open(url) else { throw Failure(8, "maintenance_open_failed") }
            return ["opened": true]
        }
        guard operation == "probe", let front = targetApplication() else {
            throw Failure(7, "maintenance_foreground_changed")
        }
        if !verified(front) {
            return ["error": "maintenance_foreground_changed", "blocking_pid": front.processIdentifier,
                    "blocking_bundle": front.bundleIdentifier ?? ""]
        }
        guard let windows = CGWindowListCopyWindowInfo([.optionOnScreenOnly, .excludeDesktopElements], kCGNullWindowID) as? [[String: Any]] else {
            throw Failure(7, "maintenance_window_unavailable")
        }
        let display = CGDisplayBounds(CGMainDisplayID())
        var covering: [(bounds: CGRect, pid: Int32?, window: UInt32?)] = []
        for window in windows {
            guard let raw = window[kCGWindowBounds as String] as? [String: Any],
                  let bounds = CGRect(dictionaryRepresentation: raw as CFDictionary), bounds.width > 0, bounds.height > 0,
                  (window[kCGWindowAlpha as String] as? Double ?? 1) > 0 else { continue }
            let owner = window[kCGWindowOwnerPID as String] as? Int32
            if cursorSurface(owner ?? -1, bounds: bounds, layer: window[kCGWindowLayer as String] as? Int ?? 0) { continue }
            if owner == front.processIdentifier && bounds.width >= 100 && bounds.height >= 60 {
                let region = bounds.intersection(display)
                guard !region.isNull, region.width >= 80, region.height >= 40,
                      let id = window[kCGWindowNumber as String] as? UInt32 else {
                    throw Failure(7, "maintenance_window_unavailable")
                }
                guard try consoleSession() == session else { throw Failure(4, "requires_gui_login") }
                guard targetIsCurrent(front) else {
                    throw Failure(7, "maintenance_foreground_changed")
                }
                if let blocker = covering.first(where: { $0.bounds.intersects(region) }) {
                    let bundle = blocker.pid.flatMap { NSRunningApplication(processIdentifier: $0)?.bundleIdentifier }
                    return ["error": "maintenance_window_obscured", "blocking_pid": blocker.pid ?? 0,
                            "blocking_window": blocker.window ?? 0, "blocking_bundle": bundle ?? ""]
                }
                return ["version": 1, "session": session, "user": NSUserName(), "pid": front.processIdentifier,
                    "window": id, "bundle": front.bundleIdentifier!,
                    "password_focused": MacSetupPassword.passwordFocused(front, window: bounds),
                    "rect": [region.minX, region.minY, region.width, region.height],
                    "display": [display.minX, display.minY, display.width, display.height]]
            }
            if owner != front.processIdentifier, !backgroundSystemSurface(owner ?? -1, bounds: bounds, display: display) {
                covering.append((bounds, owner, window[kCGWindowNumber as String] as? UInt32))
            }
        }
        throw Failure(7, "maintenance_window_unavailable")
    }
}
