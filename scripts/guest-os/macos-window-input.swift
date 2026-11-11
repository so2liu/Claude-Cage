import AppKit
import ApplicationServices
import ScreenCaptureKit

// HID delivery lets WindowServer perform Chrome's normal pointer routing.
// Only the verified foreground Chrome surface may receive a new pointer event.
@MainActor
func nativeChromeOwnsPointer(_ point: CGPoint, pid: pid_t) -> Bool {
    guard NSWorkspace.shared.frontmostApplication?.processIdentifier == pid,
          let windows = CGWindowListCopyWindowInfo([.optionOnScreenOnly,.excludeDesktopElements], kCGNullWindowID) as? [[String:Any]] else { return false }
    let display = CGDisplayBounds(CGMainDisplayID())
    for item in windows {
        guard let raw = item[kCGWindowBounds as String] as? [String:Any], let bounds = CGRect(dictionaryRepresentation: raw as CFDictionary),
              bounds.contains(point), (item[kCGWindowAlpha as String] as? Double ?? 1)>0,
              let owner = item[kCGWindowOwnerPID as String] as? Int32 else { continue }
        let layer = item[kCGWindowLayer as String] as? Int ?? 0
        if MacMaintenance.cursorSurface(owner,bounds:bounds,layer:layer) || MacMaintenance.backgroundSystemSurface(owner,bounds:bounds,display:display) { continue }
        if owner == pid { return true }
        if layer >= 0 { return false }
    }
    return false
}

struct MacInputWindow {
    let app: AXUIElement
    let focused: AXUIElement
    let pid: pid_t
    let origin: CGPoint
    let extent: CGSize
}

@MainActor
func checkedMacInputWindow(_ input: Request, window: SCWindow) throws -> MacInputWindow {
    guard AXIsProcessTrusted() else { throw Failure(9, "accessibility_permission") }
    guard let pid = window.owningApplication?.processID, input.pid == pid, input.window == window.windowID,
          NSWorkspace.shared.frontmostApplication?.processIdentifier == pid else { throw Failure(7, "window_changed") }
    let app = AXUIElementCreateApplication(pid)
    AXUIElementSetMessagingTimeout(app, 1)
    var value: CFTypeRef?
    guard AXUIElementCopyAttributeValue(app, kAXFocusedWindowAttribute as CFString, &value) == .success,
          let value, CFGetTypeID(value) == AXUIElementGetTypeID() else { throw Failure(7, "window_changed") }
    let focused = value as! AXUIElement
    var position: CFTypeRef?, size: CFTypeRef?
    var origin = CGPoint.zero, extent = CGSize.zero
    guard AXUIElementCopyAttributeValue(focused, kAXPositionAttribute as CFString, &position) == .success,
          AXUIElementCopyAttributeValue(focused, kAXSizeAttribute as CFString, &size) == .success,
          let position, let size, CFGetTypeID(position) == AXValueGetTypeID(), CFGetTypeID(size) == AXValueGetTypeID(),
          AXValueGetValue(position as! AXValue, .cgPoint, &origin), AXValueGetValue(size as! AXValue, .cgSize, &extent) else {
        throw Failure(7, "window_changed")
    }
    let tolerance: CGFloat = input.action == "project-click" ? 24 : 1
    guard abs(origin.x - window.frame.minX) < tolerance, abs(origin.y - window.frame.minY) < tolerance,
          abs(extent.width - window.frame.width) < tolerance, abs(extent.height - window.frame.height) < tolerance else {
        throw Failure(7, "window_changed")
    }
    return MacInputWindow(app: app, focused: focused, pid: pid, origin: origin, extent: extent)
}

@MainActor
func macPressTarget(_ target: MacInputWindow, point: CGPoint) -> AXUIElement? {
    var hit: AXUIElement?
    guard AXUIElementCopyElementAtPosition(target.app, Float(point.x), Float(point.y), &hit) == .success else { return nil }
    var node = hit
    for _ in 0..<8 {
        guard let current = node, !CFEqual(current, target.focused) else { break }
        func attribute(_ name: String) -> CFTypeRef? {
            var value: CFTypeRef?
            return AXUIElementCopyAttributeValue(current, name as CFString, &value) == .success ? value : nil
        }
        var pid: pid_t = 0
        guard AXUIElementGetPid(current, &pid) == .success, pid == target.pid,
              let owner = attribute(kAXWindowAttribute), CFEqual(owner, target.focused) else { break }
        if let role = attribute(kAXRoleAttribute) as? String, [kAXButtonRole, "AXLink"].contains(role) {
            var actions: CFArray?
            if AXUIElementCopyActionNames(current, &actions) == .success,
               (actions as? [String])?.contains(kAXPressAction) == true { return current }
        }
        guard let parent = attribute(kAXParentAttribute), CFGetTypeID(parent) == AXUIElementGetTypeID() else { break }
        node = (parent as! AXUIElement)
    }
    return nil
}

// Each socket owns its held input and composition. Disconnect releases only that socket.
@MainActor
final class MacWindowInputSession {
    private static var pointerOwner: UUID?
    private static var keyOwners: [UInt16: UUID] = [:]
    private let id = UUID()
    private let source = CGEventSource(stateID: .privateState)
    private var keys: [UInt16: (pid_t, CGWindowID)] = [:]
    private var pressedCodes: [String: UInt16] = [:]
    private var buttons: [Int: (pid_t, CGWindowID, CGPoint, Int)] = [:]
    private var press: (element: AXUIElement, down: CGEvent, point: CGPoint, pid: pid_t, window: CGWindowID)?
    private var focusedElement: AXUIElement?
    private var focusedTarget = ""
    private var focusContext = ""
    private var composition: (id: String, context: String, sequence: Int, updated: Date)?

    static func requirePointerOwner(_ session: MacWindowInputSession?) throws {
        if let owner = pointerOwner, owner != session?.id { throw Failure(8, "input_owned_by_another_view") }
    }

    private func flags(_ input: Request) -> CGEventFlags {
        var value: CGEventFlags = []
        if input.shift == true { value.insert(.maskShift) }
        if input.control == true { value.insert(.maskControl) }
        if input.alt == true { value.insert(.maskAlternate) }
        if input.command == true { value.insert(.maskCommand) }
        return value
    }

    func key(_ input: Request, target: MacInputWindow, window: SCWindow) throws {
        try Self.requirePointerOwner(self)
        if let pending = press {
            guard pending.pid == target.pid, pending.window == window.windowID else { throw Failure(7, "window_changed") }
            pending.down.postToPid(target.pid)
            press = nil
        }
        guard let down = input.down, let name = input.code, let key = input.key else { throw Failure(8, "invalid_input") }
        let mapped = MacKeyboard.virtualKey(code: name, key: key, shortcut: input.command == true || input.control == true || input.alt == true)
        guard let code = pressedCodes[name] ?? (down ? mapped : nil) else {
            if down { throw Failure(8, "invalid_input") }
            return
        }
        guard let event = CGEvent(keyboardEventSource: source, virtualKey: code, keyDown: down) else { throw Failure(8, "input_failed") }
        if let owner = Self.keyOwners[code], owner != id { throw Failure(8, "input_owned_by_another_view") }
        if !down && keys[code] == nil { return }
        event.flags = flags(input)
        event.setIntegerValueField(.keyboardEventAutorepeat, value: down && keys[code] != nil ? 1 : 0)
        if down && input.command != true && input.control != true && key.count == 1 {
            let text = Array(key.utf16)
            text.withUnsafeBufferPointer { event.keyboardSetUnicodeString(stringLength: text.count, unicodeString: $0.baseAddress) }
        }
        if down { keys[code] = (target.pid, window.windowID); pressedCodes[name] = code; Self.keyOwners[code] = id }
        else { keys.removeValue(forKey: code); pressedCodes.removeValue(forKey: name); Self.keyOwners.removeValue(forKey: code) }
        event.postToPid(target.pid)
    }

    func pointer(_ input: Request, target: MacInputWindow, window: SCWindow, canvas: ClaudeCanvas?) throws {
        try Self.requirePointerOwner(self)
        guard let phase = input.phase, ["down", "move", "up"].contains(phase),
              let x = input.x, let y = input.y, x.isFinite, y.isFinite, (0..<1).contains(x), (0..<1).contains(y) else { throw Failure(8, "invalid_input") }
        let crop = canvas?.cropTop ?? 0
        let point = CGPoint(x: target.origin.x + target.extent.width * x, y: target.origin.y + crop + (target.extent.height - crop) * y)
        let button = phase == "move" ? buttons.keys.sorted().first : input.button
        if nativeChrome {
            if phase != "move" && (button == nil || !(0...2).contains(button!) || !(1...3).contains(input.clicks ?? 0)) { throw Failure(8,"invalid_input") }
            if phase == "up", let button, buttons[button] == nil { return }
            guard nativeChromeOwnsPointer(point,pid:target.pid) else { throw Failure(7,"window_changed") }
            let types: [CGEventType] = phase == "move" ? [.leftMouseDragged,.otherMouseDragged,.rightMouseDragged] :
                phase == "down" ? [.leftMouseDown,.otherMouseDown,.rightMouseDown] : [.leftMouseUp,.otherMouseUp,.rightMouseUp]
            let type = button.map { types[$0] } ?? .mouseMoved
            let mouse: CGMouseButton = button == 2 ? .right : button == 1 ? .center : .left
            guard let event = CGEvent(mouseEventSource:source,mouseType:type,mouseCursorPosition:point,mouseButton:mouse) else { throw Failure(8,"input_failed") }
            let count = input.clicks ?? button.flatMap { buttons[$0]?.3 } ?? 0
            event.flags = flags(input); event.setIntegerValueField(.mouseEventClickState,value:Int64(count))
            if phase == "down", let button { buttons[button]=(target.pid,window.windowID,point,count); Self.pointerOwner=id }
            if phase == "up", let button { buttons.removeValue(forKey:button); if buttons.isEmpty { Self.pointerOwner=nil } }
            if phase == "move" { for held in Array(buttons.keys) { buttons[held]=(target.pid,window.windowID,point,count) } }
            event.post(tap:.cghidEventTap)
            return
        }
        let types: [NSEvent.EventType]
        if phase == "move" { types = [.leftMouseDragged, .otherMouseDragged, .rightMouseDragged] }
        else if phase == "down" { types = [.leftMouseDown, .otherMouseDown, .rightMouseDown] }
        else { types = [.leftMouseUp, .otherMouseUp, .rightMouseUp] }
        if phase != "move" && (button == nil || !(0...2).contains(button!) || !(1...3).contains(input.clicks ?? 0)) { throw Failure(8, "invalid_input") }
        if phase == "up", let button, buttons[button] == nil { return }
        if let pending = press {
            guard pending.pid == target.pid, pending.window == window.windowID else { throw Failure(7, "window_changed") }
            if phase == "move" && abs(point.x - pending.point.x) < 4 && abs(point.y - pending.point.y) < 4 { return }
            press = nil
            if phase == "up", button == 0, flags(input).isEmpty,
               let hit = macPressTarget(target, point: point), CFEqual(hit, pending.element) {
                buttons.removeValue(forKey: 0)
                if buttons.isEmpty { Self.pointerOwner = nil }
                guard AXUIElementPerformAction(hit, kAXPressAction as CFString) == .success else { throw Failure(8, "input_failed") }
                return
            }
            pending.down.postToPid(target.pid)
        }
        let type = button.map { types[$0] } ?? .mouseMoved
        let count = input.clicks ?? button.flatMap { buttons[$0]?.3 } ?? 0
        let local = CGPoint(x: point.x - target.origin.x, y: target.extent.height - (point.y - target.origin.y))
        guard let event = NSEvent.mouseEvent(with: type, location: local, modifierFlags: NSEvent.ModifierFlags(rawValue: UInt(flags(input).rawValue)),
            timestamp: ProcessInfo.processInfo.systemUptime, windowNumber: Int(window.windowID), context: nil,
            eventNumber: 0, clickCount: count, pressure: phase == "up" ? 0 : 1)?.cgEvent else { throw Failure(8, "input_failed") }
        event.location = point
        event.setIntegerValueField(.mouseEventWindowUnderMousePointer, value: Int64(window.windowID))
        event.setIntegerValueField(.mouseEventWindowUnderMousePointerThatCanHandleThisEvent, value: Int64(window.windowID))
        if phase == "down", let button { buttons[button] = (target.pid, window.windowID, point, count); Self.pointerOwner = id }
        if phase == "up", let button { buttons.removeValue(forKey: button); if buttons.isEmpty { Self.pointerOwner = nil } }
        if phase == "move" { for button in Array(buttons.keys) { buttons[button] = (target.pid, window.windowID, point, count) } }
        // Preserve the existing AX activation for Electron buttons. A real
        // drag flushes the deferred down before its first dragged event.
        if phase == "down", button == 0, count == 1, buttons.count == 1, flags(input).isEmpty,
           let element = macPressTarget(target, point: point) {
            press = (element, event, point, target.pid, window.windowID)
            return
        }
        event.postToPid(target.pid)
    }

    private func stillOwnsWindow(_ pid: pid_t, _ window: CGWindowID) -> Bool {
        guard (try? consoleUser()) != nil else { return false }
        let values = CGWindowListCopyWindowInfo(.optionIncludingWindow, window) as? [[String: Any]]
        return values?.first?[kCGWindowOwnerPID as String] as? Int32 == pid
    }

    func reset() {
        for (code, target) in keys {
            if stillOwnsWindow(target.0, target.1) { CGEvent(keyboardEventSource: source, virtualKey: code, keyDown: false)?.postToPid(target.0) }
            if Self.keyOwners[code] == id { Self.keyOwners.removeValue(forKey: code) }
        }
        for (button, target) in buttons where !(button == 0 && press != nil) && (nativeChrome || stillOwnsWindow(target.0, target.1)) {
            let type: CGEventType = button == 0 ? .leftMouseUp : button == 2 ? .rightMouseUp : .otherMouseUp
            let mouse: CGMouseButton = button == 0 ? .left : button == 2 ? .right : .center
            if let event = CGEvent(mouseEventSource: source, mouseType: type, mouseCursorPosition: target.2, mouseButton: mouse) {
                event.setIntegerValueField(.mouseEventWindowUnderMousePointer, value: Int64(target.1))
                event.setIntegerValueField(.mouseEventWindowUnderMousePointerThatCanHandleThisEvent, value: Int64(target.1))
                if nativeChrome { event.post(tap:.cghidEventTap) } else { event.postToPid(target.0) }
            }
        }
        keys.removeAll(); pressedCodes.removeAll(); buttons.removeAll(); press = nil; composition = nil
        if Self.pointerOwner == id { Self.pointerOwner = nil }
    }

    func ime(_ input: Request, window: SCWindow, canvas: ClaudeCanvas?, commit: (String) throws -> Void) throws -> [String: Any] {
        let mode = input.mode ?? ""
        guard ["status", "begin", "update", "commit", "cancel"].contains(mode) else { throw Failure(8, "input_mode_invalid") }
        let token = input.session ?? "", sequence = input.sequence ?? 0
        if mode != "status" && (token.count != 32 || token.range(of: "^[a-f0-9]{32}$", options: .regularExpression) == nil || sequence < 0) { throw Failure(8, "input_session_invalid") }
        if mode == "cancel" {
            if composition?.id == token && sequence > composition!.sequence { composition = nil }
            return ["available": false]
        }
        let target = try checkedMacInputWindow(input, window: window)
        func attribute(_ element: AXUIElement, _ name: String) -> CFTypeRef? {
            var value: CFTypeRef?
            return AXUIElementCopyAttributeValue(element, name as CFString, &value) == .success ? value : nil
        }
        guard let raw = attribute(target.app, kAXFocusedUIElementAttribute), CFGetTypeID(raw) == AXUIElementGetTypeID() else {
            composition = nil; focusedElement = nil; return ["available": false]
        }
        let element = raw as! AXUIElement
        var pid: pid_t = 0
        var valueWritable = DarwinBoolean(false), selectionWritable = DarwinBoolean(false)
        _ = AXUIElementIsAttributeSettable(element, kAXValueAttribute as CFString, &valueWritable)
        _ = AXUIElementIsAttributeSettable(element, kAXSelectedTextAttribute as CFString, &selectionWritable)
        guard AXUIElementGetPid(element, &pid) == .success, pid == target.pid,
              [kAXTextFieldRole, kAXTextAreaRole, kAXComboBoxRole].contains(attribute(element, kAXRoleAttribute) as? String ?? ""),
              valueWritable.boolValue || selectionWritable.boolValue,
              let owner = attribute(element, kAXWindowAttribute), CFEqual(owner, target.focused),
              (attribute(element, kAXEnabledAttribute) as? Bool) != false else { composition = nil; return ["available": false] }
        let identity = "\(window.windowID):\(target.pid):\(input.geometry ?? "")"
        if focusedElement == nil || !CFEqual(focusedElement!, element) || focusedTarget != identity {
            focusedElement = element; focusedTarget = identity; focusContext = UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased(); composition = nil
        }
        var rect = CGRect.zero
        var exact = false
        if let range = attribute(element, kAXSelectedTextRangeAttribute), CFGetTypeID(range) == AXValueGetTypeID() {
            var selected = CFRange()
            if AXValueGetValue(range as! AXValue, .cfRange, &selected) {
                selected.location += selected.length; selected.length = 0
                var bounds: CFTypeRef?
                if let parameter = AXValueCreate(.cfRange, &selected),
                   AXUIElementCopyParameterizedAttributeValue(element, kAXBoundsForRangeParameterizedAttribute as CFString, parameter, &bounds) == .success,
                   let bounds, CFGetTypeID(bounds) == AXValueGetTypeID() { exact = AXValueGetValue(bounds as! AXValue, .cgRect, &rect) }
            }
        }
        if !exact || rect.height <= 0 {
            var point = CGPoint.zero, size = CGSize.zero
            guard let position = attribute(element, kAXPositionAttribute), let extent = attribute(element, kAXSizeAttribute),
                  CFGetTypeID(position) == AXValueGetTypeID(), CFGetTypeID(extent) == AXValueGetTypeID(),
                  AXValueGetValue(position as! AXValue, .cgPoint, &point), AXValueGetValue(extent as! AXValue, .cgSize, &size) else { return ["available": false] }
            rect = CGRect(origin: point, size: CGSize(width: 1, height: min(24, size.height)))
        }
        if let current = composition, current.context != focusContext || Date().timeIntervalSince(current.updated) > 30 { composition = nil }
        let result: [String: Any] = ["available": true, "context": focusContext, "preedit_supported": false,
            "password": attribute(element, kAXSubroleAttribute) as? String == kAXSecureTextFieldSubrole,
            "rect": [rect.minX - target.origin.x, rect.minY - target.origin.y - (canvas?.cropTop ?? 0), max(1, rect.width), max(1, rect.height)],
            "width": target.extent.width, "height": target.extent.height - (canvas?.cropTop ?? 0), "caret_exact": exact]
        if mode == "status" {
            if composition?.id == token { composition?.updated = Date() }
            return result
        }
        if mode == "begin" {
            guard input.context == focusContext else { return ["available": false] }
            composition = (token, focusContext, sequence, Date()); return result
        }
        guard let current = composition, current.id == token, sequence > current.sequence else { return ["available": false] }
        guard (input.text ?? "").utf8.count <= 8192 else { throw Failure(8, "input_text_limit") }
        if mode == "commit" { try commit(input.text ?? ""); composition = nil }
        else { composition = (token, focusContext, sequence, Date()) }
        return result
    }
}
