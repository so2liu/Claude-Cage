// Capture one visible Claude Desktop window to stdout inside a Cage macOS VM.
// Build with swiftc -parse-as-library -target arm64-apple-macos14.0.
import AppKit
import ApplicationServices
import ImageIO
import ScreenCaptureKit
import Security
import UniformTypeIdentifiers

let nativeChrome = CommandLine.arguments.contains("--chrome")
let nativeChromeProfile = NSHomeDirectory() + "/Library/Application Support/Cage/NativeChrome"
let nativeAppPath = nativeChrome ? "/Applications/Google Chrome.app" : "/Applications/Claude.app"
let nativeAppBundle = nativeChrome ? "com.google.Chrome" : "com.anthropic.claudefordesktop"
private let bundleID = nativeAppBundle

struct Failure: Error {
    let code: Int32
    let message: String
    init(_ code: Int32, _ message: String) {
        self.code = code
        self.message = message
    }
}

func requireGuest() throws {
    var model = [CChar](repeating: 0, count: 256)
    var count = model.count
    guard sysctlbyname("hw.model", &model, &count, nil, 0) == 0,
          String(cString: model).range(of: #"^VirtualMac[0-9]+,[0-9]+$"#, options: .regularExpression) != nil else {
        throw Failure(2, "not_cage_guest: Run inside a Cage virtual Mac.")
    }
    let fd = open("/var/db/cage-guest-os", O_RDONLY | O_NOFOLLOW | O_NONBLOCK)
    guard fd >= 0 else { throw Failure(2, "not_cage_guest: Missing guest marker.") }
    let marker = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
    var metadata = stat()
    guard fstat(fd, &metadata) == 0, metadata.st_mode & S_IFMT == S_IFREG,
          metadata.st_uid == 0, metadata.st_mode & 0o022 == 0,
          (5...6).contains(metadata.st_size),
          let bytes = try marker.read(upToCount: 6), [Data("macos".utf8), Data("macos\n".utf8)].contains(bytes) else {
        throw Failure(2, "not_cage_guest: Invalid guest marker.")
    }
}

func claudeSigningRequirement() throws -> SecRequirement {
    let expression = #"anchor apple generic and identifier "com.anthropic.claudefordesktop" and certificate leaf[subject.OU] = "Q6L2SF6YDW" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists"#
    let selected = nativeChrome ? #"anchor apple generic and identifier "com.google.Chrome" and certificate leaf[subject.OU] = "EQHXZ8M8AV""# : expression
    var requirement: SecRequirement?
    guard SecRequirementCreateWithString(selected as CFString, [], &requirement) == errSecSuccess, let requirement else {
        throw Failure(5, "identity_error: Could not construct the Claude signing requirement.")
    }
    return requirement
}

func claudePIDs() throws -> Set<pid_t> {
    var console = stat()
    var attributes: SessionAttributeBits = []
    guard lstat("/dev/console", &console) == 0, console.st_uid >= 500, console.st_uid == geteuid(),
          SessionGetInfo(callerSecuritySession, nil, &attributes) == errSecSuccess,
          attributes.contains(.sessionHasGraphicAccess) else {
        throw Failure(4, "requires_gui_login: Run as the logged-in guest desktop user in their GUI session.")
    }
    let path = URL(fileURLWithPath: nativeAppPath).resolvingSymlinksInPath()
    let apps = NSRunningApplication.runningApplications(withBundleIdentifier: bundleID).filter {
        $0.bundleURL?.resolvingSymlinksInPath() == path && !$0.isTerminated
    }
    guard !apps.isEmpty else { throw Failure(3, "not_running: Launch /Applications/Claude.app in the guest.") }
    let requirement = try claudeSigningRequirement()
    let validPIDs = apps.compactMap { app -> pid_t? in
        if nativeChrome {
            // Never capture the separate editor/diagnostic automation process.
            let command = Process(); command.executableURL = URL(fileURLWithPath: "/bin/ps")
            command.arguments = ["-p", String(app.processIdentifier), "-o", "command="]
            let pipe = Pipe(); command.standardOutput = pipe; command.standardError = FileHandle.nullDevice
            guard (try? command.run()) != nil else { return nil }
            let bytes = pipe.fileHandleForReading.readDataToEndOfFile(); command.waitUntilExit()
            let args = String(data: bytes, encoding: .utf8) ?? ""
            guard args.contains("--user-data-dir=" + nativeChromeProfile), !args.contains("--remote-debugging") else { return nil }
        }
        let attributes = [kSecGuestAttributePid: app.processIdentifier] as CFDictionary
        var code: SecCode?
        guard SecCodeCopyGuestWithAttributes(nil, attributes, [], &code) == errSecSuccess, let code,
              SecCodeCheckValidity(code, [], requirement) == errSecSuccess else { return nil }
        return app.processIdentifier
    }
    guard !validPIDs.isEmpty else { throw Failure(5, "identity_error: The running app failed Anthropic signature verification.") }
    return Set(validPIDs)
}

func claudeWindow() async throws -> SCWindow {
    try requireGuest()
    let pids = try claudePIDs()
    guard CGPreflightScreenCaptureAccess() else {
        throw Failure(6, "screen_recording_permission: Authorize this capture tool in the guest's Privacy & Security settings.")
    }
    let content = try await SCShareableContent.excludingDesktopWindows(true, onScreenWindowsOnly: true)
    let windows = content.windows.filter { window in
        guard window.isOnScreen, window.windowLayer == 0, !window.frame.isEmpty,
              let owner = window.owningApplication else { return false }
        return owner.bundleIdentifier == bundleID && pids.contains(owner.processID)
    }
    if nativeChrome && AXIsProcessTrusted() {
        for pid in pids {
            let app = AXUIElementCreateApplication(pid)
            var focused: CFTypeRef?
            guard AXUIElementCopyAttributeValue(app, kAXFocusedWindowAttribute as CFString, &focused) == .success,
                  let focused, CFGetTypeID(focused) == AXUIElementGetTypeID() else { continue }
            let element = focused as! AXUIElement
            var point: CFTypeRef?, size: CFTypeRef?
            var origin = CGPoint.zero, extent = CGSize.zero
            guard AXUIElementCopyAttributeValue(element, kAXPositionAttribute as CFString, &point) == .success,
                  AXUIElementCopyAttributeValue(element, kAXSizeAttribute as CFString, &size) == .success,
                  let point, let size, CFGetTypeID(point) == AXValueGetTypeID(), CFGetTypeID(size) == AXValueGetTypeID(),
                  AXValueGetValue(point as! AXValue, .cgPoint, &origin), AXValueGetValue(size as! AXValue, .cgSize, &extent) else { continue }
            if let current = windows.first(where: { w in
                w.owningApplication?.processID == pid && abs(w.frame.minX-origin.x)<1 && abs(w.frame.minY-origin.y)<1 &&
                abs(w.frame.width-extent.width)<1 && abs(w.frame.height-extent.height)<1
            }) { return current }
        }
    }
    guard let window = windows.max(by: { $0.frame.width * $0.frame.height < $1.frame.width * $1.frame.height }) else {
        throw Failure(7, "no_visible_window: Open a Claude window on the guest desktop.")
    }
    return window
}

func screenshot(_ window: SCWindow, filter selected: SCContentFilter? = nil) async throws -> Data {
    let filter = selected ?? SCContentFilter(desktopIndependentWindow: window)
    let config = SCStreamConfiguration()
    config.width = max(1, Int((filter.contentRect.width * CGFloat(filter.pointPixelScale)).rounded()))
    config.height = max(1, Int((filter.contentRect.height * CGFloat(filter.pointPixelScale)).rounded()))
    config.ignoreShadowsSingleWindow = true
    config.showsCursor = false
    if #available(macOS 14.2, *) { config.includeChildWindows = nativeChrome }
    let image = try await SCScreenshotManager.captureImage(contentFilter: filter, configuration: config)
    return try pngData(image)
}

func pngData(_ image: CGImage) throws -> Data {
    try encodedImage(image, type: .png)
}

func encodedImage(_ image: CGImage, type: UTType, properties: CFDictionary? = nil) throws -> Data {
    let data = NSMutableData()
    guard let encoder = CGImageDestinationCreateWithData(data, type.identifier as CFString, 1, nil) else {
        throw Failure(8, "capture_failed: Could not create an image encoder.")
    }
    CGImageDestinationAddImage(encoder, image, properties)
    guard CGImageDestinationFinalize(encoder) else { throw Failure(8, "capture_failed: Could not encode the window.") }
    return data as Data
}

#if !GUEST_WINDOW_APP
@main
struct ClaudeWindow {
    static func main() async {
        do {
            let png = try await screenshot(claudeWindow())
            try FileHandle.standardOutput.write(contentsOf: png)
        } catch {
            let failure: Failure
            if let known = error as? Failure {
                failure = known
            } else if (error as NSError).domain == SCStreamErrorDomain &&
                        (error as NSError).code == SCStreamError.userDeclined.rawValue {
                failure = Failure(6, "screen_recording_permission: The guest user declined screen capture.")
            } else {
                failure = Failure(8, "capture_failed: \(error.localizedDescription)")
            }
            FileHandle.standardError.write(Data("\(failure.message)\n".utf8))
            exit(failure.code)
        }
    }
}

#endif
