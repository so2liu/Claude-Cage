// A geometry token describes only the signed Claude window actually captured.
// All coordinates used for presentation and input are measured in guest points.
import AppKit
import ScreenCaptureKit
import ImageIO
import UniformTypeIdentifiers

struct ClaudeCanvas {
    let id: CGWindowID
    let pid: pid_t
    let frame: CGRect
    let cropTop: CGFloat
    let scale: CGFloat
    let resizable: Bool
    var content: CGSize { CGSize(width: frame.width, height: frame.height - cropTop) }

    func matches(_ other: ClaudeCanvas) -> Bool {
        id == other.id && pid == other.pid && frame == other.frame && cropTop == other.cropTop && scale == other.scale
    }
}

private func axAttribute(_ element: AXUIElement, _ attribute: String) -> CFTypeRef? {
    var value: CFTypeRef?
    guard AXUIElementCopyAttributeValue(element, attribute as CFString, &value) == .success else { return nil }
    return value
}

private func axRect(_ element: AXUIElement) -> CGRect? {
    guard let p = axAttribute(element, kAXPositionAttribute), CFGetTypeID(p) == AXValueGetTypeID(),
          let s = axAttribute(element, kAXSizeAttribute), CFGetTypeID(s) == AXValueGetTypeID() else { return nil }
    var pos = CGPoint.zero, size = CGSize.zero
    guard AXValueGetValue(p as! AXValue, .cgPoint, &pos), AXValueGetValue(s as! AXValue, .cgSize, &size) else { return nil }
    return CGRect(origin: pos, size: size)
}

private func liveBounds(_ id: CGWindowID, _ pid: pid_t) throws -> CGRect {
    let windows = CGWindowListCopyWindowInfo(.optionIncludingWindow, id) as? [[String:Any]]
    guard let info = windows?.first, info[kCGWindowOwnerPID as String] as? Int32 == pid,
          info[kCGWindowIsOnscreen as String] as? Bool == true,
          let box = info[kCGWindowBounds as String] as? NSDictionary,
          let rect = CGRect(dictionaryRepresentation: box), !rect.isEmpty else { throw Failure(7, "window_changed") }
    return rect
}

private func owningAXWindow(_ pid: pid_t, _ frame: CGRect) -> AXUIElement? {
    guard AXIsProcessTrusted(), NSWorkspace.shared.frontmostApplication?.processIdentifier == pid,
          let raw = axAttribute(AXUIElementCreateApplication(pid), kAXFocusedWindowAttribute),
          CFGetTypeID(raw) == AXUIElementGetTypeID() else { return nil }
    let window = raw as! AXUIElement
    guard let actual = axRect(window), abs(actual.minX-frame.minX)<1, abs(actual.minY-frame.minY)<1,
          abs(actual.width-frame.width)<1, abs(actual.height-frame.height)<1 else { return nil }
    return window
}

private func canSet(_ window: AXUIElement, _ name: String) -> Bool {
    var result = DarwinBoolean(false)
    return AXUIElementIsAttributeSettable(window, name as CFString, &result) == .success && result.boolValue
}

private func titleBarHeight(_ window: AXUIElement, _ frame: CGRect) -> CGFloat {
    // Chrome's tabs and address bar belong to the captured surface. Capture
    // and input geometry must use the same uncropped native window bounds.
    if nativeChrome { return 0 }
    var bottom: CGFloat = 0
    for name in [kAXCloseButtonAttribute, kAXMinimizeButtonAttribute, kAXZoomButtonAttribute] {
        guard let raw = axAttribute(window, name), CFGetTypeID(raw) == AXUIElementGetTypeID(),
              let button = axRect(raw as! AXUIElement), button.width > 2, button.height > 2,
              frame.contains(button), button.minX-frame.minX < 100, button.maxY-frame.minY < 70 else { continue }
        let margin = min(8, max(2, button.minY - frame.minY))
        bottom = max(bottom, ceil(button.maxY - frame.minY + margin))
    }
    return bottom < frame.height / 3 ? bottom : 0
}

@MainActor
final class ClaudeCanvasSession {
    static let shared = ClaudeCanvasSession()
    private var last: ClaudeCanvas?
    private var token = ""
    private var requested: (id: CGWindowID, pid: pid_t, size: CGSize, observed: CGSize, time: Date)?

    func layout(_ window: SCWindow, filter: SCContentFilter, width: Int, height: Int, scale: Double) async throws -> ClaudeCanvas {
        guard let pid = window.owningApplication?.processID,
              (320...2560).contains(width), (240...1600).contains(height),
              scale.isFinite, (1...2).contains(scale) else { throw Failure(8, "invalid_input") }
        var bounds = try liveBounds(window.windowID, pid)
        var crop: CGFloat = 0, resized = false
        if let ax = owningAXWindow(pid, bounds) {
            crop = titleBarHeight(ax, bounds)
            if canSet(ax, kAXSizeAttribute) {
                let display = CGDisplayBounds(CGMainDisplayID())
                let wanted = CGSize(width: min(CGFloat(width), display.width),
                                    height: min(CGFloat(height) + crop, display.height))
                let close = abs(bounds.width - wanted.width) < 2 && abs(bounds.height - wanted.height) < 2
                let recent = requested.map { $0.id == window.windowID && $0.pid == pid && $0.size == wanted &&
                    $0.observed == bounds.size && Date().timeIntervalSince($0.time) < 3 } ?? false
                if !close && !recent {
                    var size = wanted
                    if let value = AXValueCreate(.cgSize, &size),
                       AXUIElementSetAttributeValue(ax, kAXSizeAttribute as CFString, value) == .success {
                        try await Task.sleep(nanoseconds: 70_000_000)
                        bounds = try liveBounds(window.windowID, pid)
                        // Position only an enlarged guest window that would extend off its display.
                        var origin = CGPoint(x: max(display.minX, min(bounds.minX, display.maxX-bounds.width)),
                                             y: max(display.minY, min(bounds.minY, display.maxY-bounds.height)))
                        if origin != bounds.origin, canSet(ax, kAXPositionAttribute),
                           let point = AXValueCreate(.cgPoint, &origin) {
                            _ = AXUIElementSetAttributeValue(ax, kAXPositionAttribute as CFString, point)
                            bounds = try liveBounds(window.windowID, pid)
                        }
                    }
                    requested = (window.windowID, pid, wanted, bounds.size, Date())
                }
                resized = abs(bounds.width-wanted.width) < 2 && abs(bounds.height-wanted.height) < 2
                crop = titleBarHeight(ax, bounds)
            }
        }
        return ClaudeCanvas(id: window.windowID, pid: pid, frame: bounds, cropTop: crop,
                            scale: min(max(1,CGFloat(filter.pointPixelScale)), CGFloat(scale)), resizable: resized)
    }

    func record(_ value: ClaudeCanvas) -> String {
        if last?.matches(value) != true { token = UUID().uuidString; last = value }
        return token
    }

    func inputGeometry(_ requested: String, window: SCWindow) throws -> ClaudeCanvas {
        guard let last, token == requested, last.id == window.windowID, last.pid == window.owningApplication?.processID,
              try liveBounds(last.id, last.pid) == last.frame,
              let ax = owningAXWindow(last.pid, last.frame), titleBarHeight(ax,last.frame) == last.cropTop else {
            throw Failure(7, "window_changed")
        }
        return last
    }

    func capture(_ window: SCWindow, filter: SCContentFilter, layout: ClaudeCanvas, after: String? = nil, wait: Bool = false, binary: Bool = false, phoneJPEG: Bool = false) async throws -> [String:Any] {
        let config = SCStreamConfiguration()
        config.width = max(1, Int((layout.frame.width*layout.scale).rounded()))
        config.height = max(1, Int((layout.frame.height*layout.scale).rounded()))
        config.captureResolution = .best
        config.ignoreShadowsSingleWindow = true; config.showsCursor = false
        if #available(macOS 14.2, *) { config.includeChildWindows = nativeChrome }
        let geometry = record(layout)
        let (updated, sequence) = try await MacWindowStream.shared.frame(filter: filter, configuration: config, identity: geometry, after: after, wait: wait)
        guard try liveBounds(layout.id, layout.pid) == layout.frame,
              try claudePIDs().contains(layout.pid) else { throw Failure(7, "window_changed") }
        guard let image = updated else { return ["unchanged": true, "sequence": sequence, "geometry": geometry] }
        let top = Int((layout.cropTop * CGFloat(image.height) / layout.frame.height).rounded())
        guard top >= 0, top < image.height,
              let content = image.cropping(to: CGRect(x:0,y:top,width:image.width,height:image.height-top)),
              try liveBounds(layout.id, layout.pid) == layout.frame,
              try claudePIDs().contains(layout.pid) else { throw Failure(7, "window_changed") }
        // Encoding a full frame must not block the GUI actor that delivers input.
        let (data, mime) = try await Task.detached(priority: .userInitiated) {
            var data = phoneJPEG ? try phoneDesktopJPEG(content) : try pngData(content)
            var mime = phoneJPEG ? "image/jpeg" : "image/png"
            if data.count >= 8<<20 {
                mime = "image/jpeg"
                for quality in [0.93, 0.84] {
                    data = try encodedImage(content, type:.jpeg, properties:[kCGImageDestinationLossyCompressionQuality: quality] as CFDictionary)
                    if data.count < 8<<20 { break }
                }
            }
            guard data.count < 8<<20 else { throw Failure(8, "frame_limit") }
            return (data, mime)
        }.value
        // The actor may have handled another window while the encoder ran.
        guard token == geometry, try liveBounds(layout.id, layout.pid) == layout.frame,
              try claudePIDs().contains(layout.pid) else { throw Failure(7, "window_changed") }
        var reply: [String:Any] = ["window":layout.id,"pid":layout.pid,"mime":mime,"sequence":sequence,
                "contentWidth":layout.content.width,"contentHeight":layout.content.height,
                "pixelWidth":content.width,"pixelHeight":content.height,"cropTop":layout.cropTop,
                "resizable":layout.resizable, "geometry":geometry]
        if binary { reply["imageBytes"] = data }
        else { reply["image"] = data.base64EncodedString() }
        return reply
    }
}
