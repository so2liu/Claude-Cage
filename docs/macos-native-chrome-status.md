# macOS native Chrome development status

This branch contains installation, browser display, and native Chrome work based
on `v0.1.7-rc.1` (`26237de`). It is a development snapshot, not a finished release.

## Changes

- Route host-side macOS tool downloads through the configured Cage proxy and
  terminate downloader subprocesses when installation is cancelled.
- Resolve restore-image symlinks before handing their paths to Virtualization.framework.
- Apply guest timezone and language settings and clear stale negative DNS cache
  entries when network access opens.
- Validate browser bitmap dimensions during resize and provide an RFB cursor for
  the legacy browser/editor display path.
- Add a separate ordinary Google Chrome profile and signed guest helper. Capture
  the complete Chrome window through ScreenCaptureKit and deliver native macOS
  keyboard and pointer events through the existing authenticated window channels.
- Use the native Chrome view for macOS browser tabs. Retain the separate headless
  browser service for the editor and diagnostics.
- Support macOS authorization sheets and restore Chrome focus before viewer
  clicks and scrolling.

## Known unresolved issue

The user still reports that the entire Chrome picture in the deployed Cage app
cannot be clicked, including after the focus change. This issue remains open.

Direct native input tests on a local guest test page received trusted mouse moves,
clicks, and text. Isolated React fixtures also forwarded input after a main-window
to popup transition. These results do **not** establish that input works through
the user's actual Cage window. Continue diagnosis at that boundary before claiming
the browser or personal sign-in flow is working end to end.

## Relevant checks

- Go packages: `internal/guestos`, `internal/maclogin`, `internal/workbench`.
- Node tests: `ui/src/macos-window-input.test.mjs`,
  `ui/src/remote-display.test.mjs`, and
  `scripts/guest-os/tests/windows-browser.test.mjs`.
- UI fixtures: `ui/scripts/macos-chrome-display-smoke.mjs` and
  `ui/scripts/macos-chrome-push-smoke.mjs` (run with the Vite fixture server and a
  configured Playwright module/browser executable).

The browser integration tests require `CAGE_BROWSER_TEST_PLAYWRIGHT` and
`CAGE_BROWSER_TEST_CHROMIUM`; without them those integration cases are skipped.
Builds, VM state, browser profiles, screenshots, credentials, and local diagnostic
logs are not part of this source snapshot.
