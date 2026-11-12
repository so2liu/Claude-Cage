package workbench

import (
	"sync"
	"testing"
	"time"
)

func TestWindowsInstallationInteractionStages(t *testing.T) {
	for _, stage := range []string{"account", "oobe", "credentials", "media", "tools", "services"} {
		progress := parseSetupProgress("CAGE_GUEST_STAGE windows-11 " + stage)
		if progress == nil || progress.System != "windows-11" || progress.Operation != stage || progress.Label == "" {
			t.Fatalf("Windows installation stage was lost: %s %+v", stage, progress)
		}
	}
	for _, system := range []string{"macos", "debian-13", "ubuntu-22.04"} {
		if parseSetupProgress("CAGE_GUEST_STAGE "+system+" oobe") != nil {
			t.Fatalf("Windows interaction stage accepted for %s", system)
		}
	}
}

// Child pipes may split a progress line across writes. A fallback mirror may
// restart from zero, and an absent Content-Length must stay indeterminate.
func TestSetupProgressFromChildOutput(t *testing.T) {
	var m jobManager
	var op sync.Mutex
	var points []*SetupProgress
	_, err := m.submit(&op, "start", 0, nil, nil, func() error {
		e := m.execution()
		e.setSetupProgress(&SetupProgress{Phase: "image"})
		for _, part := range []string{"CAGE_IMAGE_BYTES 25", " 100\n", "CAGE_IMAGE_BYTES 0 -1\n", "CAGE_IMAGE_BYTES 123 5\n", "CAGE_IMAGE_READY\n", "Installing Claude Code...\n"} {
			_, _ = e.Write([]byte(part))
			points = append(points, m.snapshot().SetupProgress)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitJobIdle(t, &m)
	if len(points) != 6 || points[0].Received != nil || points[1].Received == nil || *points[1].Received != 25 || points[1].Total != 100 || points[2].Received == nil || *points[2].Received != 0 || points[2].Total != 0 || points[3] != points[2] || points[4].Received != nil || points[5].Phase != "tools" {
		t.Fatalf("bad setup progress sequence: %+v", points)
	}
}

func TestNativeInstallProgressKeepsSystemAndSeparatesVerification(t *testing.T) {
	var m jobManager
	var op sync.Mutex
	_, err := m.submit(&op, "guest-install", 0, nil, nil, func() error {
		e := m.execution()
		e.setSetupProgress(&SetupProgress{Phase: "image", System: "macos"})
		_, _ = e.Write([]byte("CAGE_GUEST_BYTES macos download 25"))
		if m.snapshot().SetupProgress.Received != nil {
			t.Error("partial line became progress")
		}
		_, _ = e.Write([]byte(" 100\n"))
		m.mu.Lock()
		e.progressUpdated = time.Now().Add(-2 * time.Second)
		m.mu.Unlock()
		_, _ = e.Write([]byte("CAGE_GUEST_BYTES macos download 75 100\n"))
		p := m.snapshot().SetupProgress
		if p.BytesPerSecond < 24 || p.BytesPerSecond > 26 || *p.Received != 75 {
			t.Errorf("wrong measured rate: %+v", p)
		}
		_, _ = e.Write([]byte("CAGE_GUEST_BYTES macos verify 0 100\n"))
		p = m.snapshot().SetupProgress
		if p.Operation != "verify" || p.BytesPerSecond != 0 || *p.Received != 0 {
			t.Errorf("download leaked into verification: %+v", p)
		}
		_, _ = e.Write([]byte("CAGE_GUEST_STAGE macos restore\n下载 editor.tgz\n"))
		p = m.snapshot().SetupProgress
		if p.System != "macos" || p.Phase != "image" || p.Received != nil || p.BytesPerSecond != 0 {
			t.Errorf("stage retained stale bytes or lost retry system: %+v", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitJobIdle(t, &m)
	for _, line := range []string{"CAGE_GUEST_BYTES unknown download 2 10", "CAGE_GUEST_BYTES macos download 11 10", "CAGE_GUEST_BYTES macos verify -1 10", "CAGE_GUEST_BYTES macos anything 1 10", "CAGE_GUEST_STAGE macos arbitrary"} {
		if p := parseSetupProgress(line); p != nil {
			t.Errorf("invalid progress accepted: %s", line)
		}
	}
	for _, system := range []string{"macos", "windows-11", "debian-12", "debian-13", "ubuntu-22.04", "ubuntu-24.04"} {
		p := parseSetupProgress("CAGE_GUEST_BYTES " + system + " download 1 10")
		if p == nil || p.Total != 10 || p.System != system {
			t.Errorf("missing shared progress for %s", system)
		}
	}
}

func TestToolProgressUsesOnlyKnownPackageNames(t *testing.T) {
	p := parseSetupProgress("CAGE_TOOL_BYTES claude-code-linux-x64.tgz 4980736")
	if p == nil || p.Phase != "tools" || p.Received == nil || *p.Received != 4980736 || p.Total != 0 {
		t.Fatalf("expected current bytes without a guessed total, got %+v", p)
	}
	if p := parseSetupProgress("CAGE_TOOL_BYTES untrusted 12"); p != nil {
		t.Fatalf("unexpected package name entered UI: %+v", p)
	}
	if p := parseSetupProgress("CAGE_TOOL_BYTES chromium-common.deb 5 10"); p == nil || p.Total != 10 || *p.Received != 5 {
		t.Fatalf("missing Chromium progress from signed index: %+v", p)
	}
	if p := parseSetupProgress("CAGE_TOOL_BYTES chromium.deb 12 10"); p != nil {
		t.Fatalf("excess bytes entered UI: %+v", p)
	}
}

func TestMacHostDownloadsReplacePreviousComponentProgress(t *testing.T) {
	for _, name := range []string{"chrome.dmg", "macos-cli-claude", "macos-cli-uv.tar.gz", "macos-cli-python.tar.gz", "macos-cli-git.tar.gz"} {
		p := parseSetupProgress("CAGE_HOST_BYTES " + name + " 25 100")
		if p == nil || p.System != "macos" || p.Total != 100 || *p.Received != 25 {
			t.Fatalf("missing host component progress: %s %+v", name, p)
		}
		if p := parseSetupProgress("下载 " + name); p == nil || p.Received != nil {
			t.Fatalf("missing component start: %s %+v", name, p)
		}
	}
	for _, line := range []string{"CAGE_HOST_BYTES unknown 1 2", "CAGE_HOST_BYTES chrome.dmg 3 2", "CAGE_HOST_BYTES chrome.dmg -1 2"} {
		if p := parseSetupProgress(line); p != nil {
			t.Fatalf("invalid host progress: %s", line)
		}
	}
}

func TestMacOSSetupProgressReportsHostRetry(t *testing.T) {
	p := parseSetupProgress("CAGE_HOST_RETRY editor.tgz 2")
	if p == nil || p.Label != "正在下载 macOS 网页编辑器（准备第 2/3 次尝试）" {
		t.Fatal(p)
	}
	for _, line := range []string{"CAGE_HOST_RETRY unknown 2", "CAGE_HOST_RETRY editor.tgz 0"} {
		if p := parseSetupProgress(line); p != nil {
			t.Fatal(p)
		}
	}
}
