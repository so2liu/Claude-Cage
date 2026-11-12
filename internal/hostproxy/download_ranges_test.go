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
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func rangeFixtureBytes() []byte {
	data := make([]byte, parallelDownloadMinimum+137)
	for i := range data {
		data[i] = byte((i/8191 + i) % 251)
	}
	return data
}

func TestParallelPackageDownloadAndFallback(t *testing.T) {
	data := rangeFixtureBytes()
	for _, mode := range []string{"parallel", "ignored", "wrong-range", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			var active, peak, bounded atomic.Int32
			ready := make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/release" {
					http.Redirect(w, r, "/asset", http.StatusFound)
					return
				}
				if r.Header.Get("Accept") != "application/octet-stream" {
					t.Error("Accept lost")
				}
				if strings.Contains(r.UserAgent(), "Cage") {
					t.Error("product identity sent")
				}
				head := r.Header.Get("Range")
				if head != "" && !strings.HasSuffix(head, "-") {
					now := active.Add(1)
					defer active.Add(-1)
					for old := peak.Load(); now > old && !peak.CompareAndSwap(old, now); old = peak.Load() {
					}
					if bounded.Add(1) == 4 {
						close(ready)
					}
					select {
					case <-ready:
					case <-r.Context().Done():
						return
					case <-time.After(3 * time.Second):
						t.Error("four ranges not concurrent")
						return
					}
					if mode == "ignored" {
						r.Header.Del("Range")
					}
					if mode == "wrong-range" {
						w.Header().Set("Content-Range", "bytes 0-1/2")
						w.WriteHeader(206)
						io.WriteString(w, "xx")
						return
					}
				}
				body := data
				if mode == "corrupt" {
					body = bytes.Repeat([]byte{'x'}, len(data))
				}
				http.ServeContent(w, r, "package", time.Time{}, bytes.NewReader(body))
			}))
			defer server.Close()
			target := filepath.Join(t.TempDir(), "partial")
			digest := fmt.Sprintf("%x", sha256.Sum256(data))
			// A single attempt exercises the production split/fallback path;
			// hash verification is covered through downloadAttempts below.
			err := downloadStream(t.Context(), server.Client(), server.URL+"/release", target, "editor.tgz", "application/octet-stream", io.Discard, time.Second, 0, true)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(target)
			if err != nil || (mode != "corrupt" && !bytes.Equal(got, data)) {
				t.Fatalf("download mismatch: %v", err)
			}
			if peak.Load() != 4 {
				t.Fatalf("connections=%d", peak.Load())
			}
			if mode == "corrupt" {
				err = downloadAttempts(t.Context(), server.Client(), server.URL, target, "editor.tgz", "application/octet-stream", digest, io.Discard)
				if err == nil {
					t.Fatal("bad hash accepted")
				}
			}
		})
	}
}

func TestParallelPackageCancellationAndResume(t *testing.T) {
	data := rangeFixtureBytes()
	target := filepath.Join(t.TempDir(), "partial")
	var interrupted atomic.Bool
	interrupted.Store(true)
	var calls atomic.Int32
	var resumed atomic.Bool
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.ServeContent(w, r, "package", time.Time{}, bytes.NewReader(data))
			return
		}
		if interrupted.Load() {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
			w.WriteHeader(206)
			w.Write(data[start : start+4096])
			w.(http.Flusher).Flush()
			if calls.Add(1) == 4 {
				// Let clients receive the prefix before signaling cancellation.
				time.AfterFunc(100*time.Millisecond, cancel)
			}
			<-r.Context().Done()
			return
		}
		if start%int64((len(data)+3)/4) == 4096 {
			resumed.Store(true)
		}
		http.ServeContent(w, r, "package", time.Time{}, bytes.NewReader(data))
	}))
	defer server.Close()
	err := downloadRanges(ctx, server.Client(), server.URL, target, "editor.tgz", "", io.Discard, time.Second, 0, int64(len(data)))
	if err != context.Canceled {
		t.Fatalf("cancel=%v", err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Size() != 4096 {
		t.Fatalf("prefix=%v %v", info, err)
	}
	interrupted.Store(false)
	err = downloadStream(t.Context(), server.Client(), server.URL, target, "editor.tgz", "", io.Discard, time.Second, info.Size(), true)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, data) || !resumed.Load() {
		t.Fatalf("resume=%v, contents=%v err=%v", resumed.Load(), bytes.Equal(got, data), err)
	}
}
