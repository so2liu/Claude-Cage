package hostproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHostDownloadRedirectProgressAndAccept(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/octet-stream" {
			t.Error("GitHub asset Accept header lost")
		}
		if r.URL.Path == "/release" {
			http.Redirect(w, r, "/asset", http.StatusFound)
			return
		}
		io.WriteString(w, "verified by caller")
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "archive")
	var output bytes.Buffer
	if err := downloadOnce(t.Context(), server.Client(), server.URL+"/release", file, "macos-cli-uv.tar.gz", "application/octet-stream", &output, time.Second, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(file)
	if string(got) != "verified by caller" || !strings.Contains(output.String(), "CAGE_HOST_BYTES macos-cli-uv.tar.gz 18 18") {
		t.Fatalf("body=%q progress=%s", got, &output)
	}
}

func TestHostDownloadStallAndCancellation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	start := time.Now()
	err := downloadOnce(ctx, server.Client(), server.URL, filepath.Join(t.TempDir(), "archive"), "chrome.dmg", "", io.Discard, 30*time.Millisecond, 0)
	if err == nil || time.Since(start) >= time.Second {
		t.Fatalf("stalled body did not stop promptly: %v", err)
	}
	cancel()
	if err := Download(ctx, server.URL, filepath.Join(t.TempDir(), "archive"), "chrome.dmg", "", "", "", io.Discard); err != context.Canceled {
		t.Fatalf("cancellation was not preserved: %v", err)
	}
}

// Some response bodies unblock a pending Read with EOF when canceled.
func TestHostDownloadIdleEOFIsNotSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	go func() {
		<-ctx.Done()
		writer.Close()
	}()
	err := copyDownload(ctx, cancel, reader, io.Discard, 10*time.Millisecond, -1, func(int) {})
	if err != context.Canceled {
		t.Fatalf("idle stream returned %v, want cancellation", err)
	}
}

func TestHostDownloadRejectsPlainHTTP(t *testing.T) {
	if err := downloadOnce(t.Context(), http.DefaultClient, "http://example.invalid/archive", filepath.Join(t.TempDir(), "archive"), "chrome.dmg", "", io.Discard, time.Second, 0); err == nil {
		t.Fatal("unencrypted host download accepted")
	}
}

func TestPinnedHostDownloadResumesPartialAndVerifies(t *testing.T) {
	const content = "0123456789"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	for _, tt := range []struct {
		name, partial string
		status        int
		ranges        []string
	}{
		{"resumed", "012", 206, []string{"bytes=3-"}},
		{"range-ignored", "old", 200, []string{"bytes=3-"}},
		{"already-complete", content, 416, []string{"bytes=10-"}},
		{"malformed-range", "012", 206, []string{"bytes=3-", ""}},
		{"wrong-prefix", "xxx", 206, []string{"bytes=3-", ""}},
		{"oversized", "too long to be a valid partial", 416, []string{"bytes=30-", ""}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var ranges []string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				head := r.Header.Get("Range")
				ranges = append(ranges, head)
				if head == "" || tt.status == 200 {
					io.WriteString(w, content)
					return
				}
				if tt.status == 416 {
					w.WriteHeader(416)
					return
				}
				if tt.name == "malformed-range" {
					w.Header().Set("Content-Range", "bytes 0-6/10")
				} else {
					w.Header().Set("Content-Range", "bytes 3-9/10")
				}
				w.WriteHeader(206)
				io.WriteString(w, content[3:])
			}))
			defer server.Close()
			partial := filepath.Join(t.TempDir(), "partial")
			if err := os.WriteFile(partial, []byte(tt.partial), 0600); err != nil {
				t.Fatal(err)
			}
			var progress bytes.Buffer
			if err := downloadAttempts(t.Context(), server.Client(), server.URL, partial, "editor.tgz", "", digest, &progress); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(partial)
			if err != nil || string(got) != content || !slices.Equal(ranges, tt.ranges) {
				t.Fatalf("result=%q ranges=%v: %v", got, ranges, err)
			}
			if tt.name == "resumed" && !strings.Contains(progress.String(), "editor.tgz 3 10") {
				t.Fatal(progress.String())
			}
		})
	}
}

func TestInterruptedHostDownloadKeepsPartialForNextAttempt(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Length", "10")
			io.WriteString(w, "012")
			return
		}
		if got := r.Header.Get("Range"); got != "bytes=3-" {
			t.Errorf("next GET range %q", got)
		}
		w.Header().Set("Content-Range", "bytes 3-9/10")
		w.WriteHeader(206)
		io.WriteString(w, "3456789")
	}))
	defer server.Close()
	partial := filepath.Join(t.TempDir(), "partial")
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("0123456789")))
	err := downloadAttempts(t.Context(), server.Client(), server.URL, partial, "editor.tgz", "", digest, io.Discard)
	if err != nil || requests.Load() != 2 {
		t.Fatalf("interrupted download: %v (%d requests)", err, requests.Load())
	}
}
