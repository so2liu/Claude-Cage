package workbench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsBrowserRelayHelper(t *testing.T) {
	if os.Getenv("CAGE_BROWSER_RELAY_FIXTURE") != "1" {
		return
	}
	var uploaded []byte
	downloaded := bytes.Repeat([]byte{0xab}, 131089)
	direction := ""
	const transfer = "0123456789abcdef0123456789abcdef"
	read := bufio.NewScanner(os.Stdin)
	read.Buffer(make([]byte, 65536), 128<<10)
	for read.Scan() {
		var request struct {
			Path  string `json:"path"`
			Input struct {
				ID        string `json:"id"`
				Tab       string `json:"tab"`
				Context   string `json:"context"`
				Document  string `json:"document"`
				Direction string `json:"direction"`
				Data      string `json:"data"`
				Sequence  int    `json:"sequence"`
				After     uint64 `json:"after"`
			} `json:"input"`
		}
		if json.Unmarshal(read.Bytes(), &request) != nil {
			os.Exit(2)
		}
		var result any = map[string]any{"ok": true}
		switch request.Path {
		case "/frame":
			if code := os.Getenv("CAGE_BROWSER_FRAME_FAILURE"); code != "" {
				result = map[string]any{"error": code, "code": code}
				break
			}
			time.Sleep(time.Hour)
		case "/tabs":
			result = map[string]any{"active": "tab", "tabs": []any{}}
			if workspace := os.Getenv("CAGE_BROWSER_EDITOR_WORKSPACE"); workspace != "" {
				result = map[string]any{"active": "tab", "tabs": []any{map[string]any{"id": "tab", "kind": "editor", "workspace": workspace, "blocked": true}}}
			}
		case "/events":
			result = map[string]any{"sequence": 2, "events": []any{}}
			if request.Input.After == 2 {
				if acknowledgement := os.Getenv("CAGE_BROWSER_EVENT_ACK"); acknowledgement != "" {
					if err := os.WriteFile(acknowledgement, []byte("2"), 0600); err != nil {
						os.Exit(5)
					}
				}
			}
			if request.Input.After < 2 {
				result = map[string]any{"sequence": 2, "state": map[string]any{"active": "tab", "tabs": []any{}}, "events": []any{
					map[string]any{"sequence": 2, "type": "clipboard", "value": map[string]any{"id": "tab", "text": "复制🙂", "context": "0123456789abcdef0123456789abcdef", "document": "current-document"}},
				}}
			}
		case "/phone-browser/frame":
			result = map[string]any{"error": "fixture public-only denial", "code": "phone_browser_error", "status": 403}
		case "/file-open":
			direction = request.Input.Direction
			if request.Input.Direction == "upload" || request.Input.Direction == "extension" {
				uploaded = nil
				result = map[string]any{"transfer": transfer}
			}
			if request.Input.Direction == "download" {
				downloaded = bytes.Repeat([]byte{0xab}, 131089)
				metadata := map[string]any{"transfer": transfer, "size": len(downloaded), "name": "binary.bin"}
				if request.Input.Tab != "" {
					if request.Input.Tab != strings.Repeat("a", 32) || request.Input.Context != strings.Repeat("b", 32) || request.Input.Document != strings.Repeat("c", 32) {
						result = map[string]any{"error": "copy_changed", "code": "copy_changed"}
						break
					}
					metadata["name"], metadata["mime"] = "clipboard.txt", "text/plain"
					downloaded = []byte(strings.Repeat("中", (2<<20)/3) + "ab")
					if strings.HasPrefix(request.Input.ID, "11111111") {
						metadata["name"], metadata["mime"] = "clipboard.png", "image/png"
						downloaded, _ = base64.StdEncoding.DecodeString(testPNG(t))
					}
					metadata["size"] = len(downloaded)
				}
				result = metadata
			}
		case "/file-chunk":
			chunk, err := base64.StdEncoding.DecodeString(request.Input.Data)
			if err != nil || len(chunk) > 65536 {
				os.Exit(3)
			}
			uploaded = append(uploaded, chunk...)
			result = map[string]any{"ack": request.Input.Sequence + 1}
		case "/file-finish":
			if direction == "extension" {
				result = map[string]any{"session": "bf19a220-9ba0-4e69-a519-e23ad6bf0d71", "name": "Fixture extension", "version": "1.0", "permissions": []string{"storage"}, "hostPermissions": []string{}, "size": len(uploaded)}
			} else {
				result = map[string]any{"ok": bytes.Equal(uploaded, bytes.Repeat([]byte{0xab}, 131089)), "size": len(uploaded), "name": "binary.bin"}
			}
		case "/vault":
			result = map[string]any{"available": true, "origin": "https://mail.example.com", "document": "fixture-document", "candidates": []any{}, "entries": []any{}}
		case "/image-start", "/paste-start":
			uploaded = nil
			result = map[string]any{"transfer": transfer}
		case "/image-chunk", "/paste-chunk":
			chunk, err := base64.StdEncoding.DecodeString(request.Input.Data)
			if err != nil || len(chunk) > 65536 {
				os.Exit(3)
			}
			uploaded = append(uploaded, chunk...)
			result = map[string]any{"ack": request.Input.Sequence + 1}
		case "/image-finish", "/paste-finish":
			result = map[string]any{"ok": true, "size": len(uploaded), "clipboard": map[string]any{"revision": 1, "available": true}}
		case "/file-read":
			start := request.Input.Sequence * 65536
			remaining := len(downloaded) - start
			if remaining > 65536 {
				remaining = 65536
			}
			result = map[string]any{"data": base64.StdEncoding.EncodeToString(downloaded[start : start+remaining]), "sequence": request.Input.Sequence + 1, "eof": start+remaining == len(downloaded)}
		}
		if os.Getenv("CAGE_BROWSER_PERSISTENT_FIXTURE") == "1" {
			result.(map[string]any)["service"] = map[string]any{"version": 1, "persistent": true}
		}
		data, _ := json.Marshal(result)
		_, _ = os.Stdout.Write(append(data, '\n'))
	}
	os.Exit(0)
}

func fixtureWindowsBrowserClient(t *testing.T) *windowsBrowserClient {
	client := newWindowsBrowserClient(context.Background(), "unused-test-profile")
	client.start = func(ctx context.Context) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsBrowserRelayHelper$")
		cmd.Env = append(os.Environ(), "CAGE_BROWSER_RELAY_FIXTURE=1")
		return cmd, nil
	}
	t.Cleanup(client.close)
	return client
}

func TestWindowsBrowserRouteRejectsPathAndCredentialInjection(t *testing.T) {
	for _, path := range []string{"http://private.invalid/tabs", "/tabs?token=secret", "/../private", "/file-open/secret", "tabs"} {
		if validWindowsBrowserRoute("POST", path) {
			t.Fatalf("accepted route %q", path)
		}
	}
	if validWindowsBrowserRoute("GET", "/file-open") {
		t.Fatal("accepted file mutation using GET")
	}
	client := fixtureWindowsBrowserClient(t)
	if _, err := client.request(context.Background(), "POST", "/tabs?secret", nil); err == nil {
		t.Fatal("invalid route reached transport")
	}
}

func TestWindowsBrowserChunkedUploadAndDownloadPreserveBinaryBytes(t *testing.T) {
	client := fixtureWindowsBrowserClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	payload := bytes.Repeat([]byte{0xab}, 131089)
	data, err := client.upload(ctx, "bf19a220-9ba0-4e69-a519-e23ad6bf0d71", "binary.bin", bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		OK   bool `json:"ok"`
		Size int  `json:"size"`
	}
	if json.Unmarshal(data, &receipt) != nil || !receipt.OK || receipt.Size != len(payload) {
		t.Fatal("upload was not confirmed")
	}
	stream, size, name, err := client.download(ctx, "bf19a220-9ba0-4e69-a519-e23ad6bf0d71")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	got, err := io.ReadAll(stream)
	if err != nil || size != int64(len(payload)) || name != "binary.bin" || !bytes.Equal(got, payload) {
		t.Fatalf("download mismatch: size=%d err=%v", len(got), err)
	}
}

func TestWindowsBrowserCanceledFrameCannotBecomeNextViewerReply(t *testing.T) {
	client := fixtureWindowsBrowserClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := client.request(ctx, "GET", "/frame", nil); err != context.DeadlineExceeded {
		t.Fatalf("wanted cancellation, got %v", err)
	}
	ctx, cancelNext := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelNext()
	data, err := client.request(ctx, "GET", "/tabs", nil)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Active string `json:"active"`
	}
	if json.Unmarshal(data, &state) != nil || state.Active != "tab" {
		t.Fatal("stale frame replaced new tab response")
	}
}

func TestWindowsBrowserPreservesPopupFrameFailure(t *testing.T) {
	for _, code := range []string{"desktop_target_changed", "desktop_capture_busy", "desktop_empty_frame", "frame_unavailable"} {
		t.Run(code, func(t *testing.T) {
			client := fixtureWindowsBrowserClient(t)
			start := client.start
			client.start = func(ctx context.Context) (*exec.Cmd, error) {
				cmd, err := start(ctx)
				if err == nil {
					cmd.Env = append(cmd.Env, "CAGE_BROWSER_FRAME_FAILURE="+code)
				}
				return cmd, err
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			_, err := client.request(ctx, "GET", "/frame", nil)
			var failure windowsBrowserFailure
			if !errors.As(err, &failure) || failure.code != code {
				t.Fatalf("lost structured code %s: %v", code, err)
			}
		})
	}
}

func TestNativeBrowserEventsPreserveCopyContextAndSequence(t *testing.T) {
	client := fixtureWindowsBrowserClient(t)
	acknowledgement := filepath.Join(t.TempDir(), "event-cursor")
	start := client.start
	client.start = func(ctx context.Context) (*exec.Cmd, error) {
		command, err := start(ctx)
		if err == nil {
			command.Env = append(command.Env, "CAGE_BROWSER_EVENT_ACK="+acknowledgement)
		}
		return command, err
	}
	server := &Server{windowsBrowserControl: client}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream := server.subscribeNativeBrowser(ctx)
	defer stream.Close()
	type readResult struct {
		data []byte
		err  error
	}
	finished := make(chan readResult, 1)
	go func() { data, err := io.ReadAll(stream); finished <- readResult{data, err} }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(acknowledgement); err == nil && string(data) == "2" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("subscriber did not advance the acknowledged event cursor", ctx.Err())
		case result := <-finished:
			t.Fatal("event stream ended before cursor acknowledgement", result.err)
		case <-ticker.C:
		}
	}
	// End only after a second poll has proved the previous sequence was retained.
	// Cancellation may race that poll's reply; either EOF or Canceled is normal.
	cancel()
	var result readResult
	select {
	case result = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("event stream did not stop after cancellation")
	}
	data, err := result.data, result.err
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	wanted := "id: 2\nevent: clipboard\ndata: "
	if bytes.Count(data, []byte(wanted)) != 1 {
		t.Fatalf("copy event was lost or replayed: %s", data)
	}
	for _, value := range []string{"复制🙂", "0123456789abcdef0123456789abcdef", "current-document"} {
		if !bytes.Contains(data, []byte(value)) {
			t.Fatalf("copy event lost %q", value)
		}
	}
}

func TestNativeBrowserViewerOwnershipUsesServiceCapability(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprint(persistent), func(t *testing.T) {
			client := fixtureWindowsBrowserClient(t)
			client.ownsBrowser = true
			start := client.start
			client.start = func(ctx context.Context) (*exec.Cmd, error) {
				cmd, err := start(ctx)
				if persistent {
					cmd.Env = append(cmd.Env, "CAGE_BROWSER_PERSISTENT_FIXTURE=1")
				}
				return cmd, err
			}
			if _, err := client.request(t.Context(), "GET", "/tabs", nil); err != nil {
				t.Fatal(err)
			}
			viewer, release := client.viewer(t.Context())
			if (viewer != client) != persistent {
				t.Fatal("viewer ownership did not follow guest capability")
			}
			if _, err := viewer.request(t.Context(), "GET", "/tabs", nil); err != nil {
				t.Fatal(err)
			}
			release()
			if _, err := client.request(t.Context(), "GET", "/tabs", nil); err != nil {
				t.Fatalf("closing a viewer closed the owning connection: %v", err)
			}
		})
	}
}
