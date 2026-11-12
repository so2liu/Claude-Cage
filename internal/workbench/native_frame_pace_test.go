package workbench

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestNativeFramePaceBacksOffUntilChangeOrInput(t *testing.T) {
	start := time.Unix(1000, 0)
	var pace nativeFramePace
	pace.requested(start)
	if pace.next().Sub(start) != nativeFrameInterval {
		t.Fatal("a changing window was not requested at the full rate")
	}
	var waits []time.Duration
	for i := 0; i < 6; i++ {
		pace.answered(false)
		waits = append(waits, pace.next().Sub(start))
	}
	for i, factor := range []time.Duration{2, 4, 8, 8, 8, 8} {
		if waits[i] != nativeFrameInterval*factor {
			t.Fatalf("pause %d after unchanged frames is %v", i, waits[i])
		}
	}
	pace.input(start)
	if pace.next().Sub(start) != nativeFrameInterval {
		t.Fatal("input did not restore the full rate")
	}
	pace.answered(false)
	pace.answered(false)
	pace.answered(true)
	if pace.next().Sub(start) != nativeFrameInterval {
		t.Fatal("a changed frame did not restore the full rate")
	}
}

func TestNativeFramePaceIsFasterOnlyWhileASmoothDisplayIsOperated(t *testing.T) {
	start := time.Unix(1000, 0)
	pace := nativeFramePace{smooth: true}
	pace.requested(start)
	if pace.next().Sub(start) != nativeFrameInterval {
		t.Fatal("an unattended display was requested at the operating rate")
	}
	pace.input(start)
	if pace.next().Sub(start) != nativeOperatingInterval {
		t.Fatal("an operated display kept the usual rate")
	}
	// Unchanged frames still double the pause.
	pace.answered(false)
	if pace.next().Sub(start) != 2*nativeOperatingInterval {
		t.Fatal("an unchanged operated display was not requested less often")
	}
	pace.answered(true)
	later := start.Add(nativeOperatingHold)
	pace.requested(later)
	if pace.next().Sub(later) != nativeFrameInterval {
		t.Fatal("the operating rate outlived the input")
	}
	desktop := nativeFramePace{}
	desktop.requested(start)
	desktop.input(start)
	if desktop.next().Sub(start) != nativeFrameInterval {
		t.Fatal("a display that is not smooth was requested at the operating rate")
	}
}

// An unchanged window is asked for rarely, while input still reaches the
// guest at once and is followed by a prompt frame request.
func TestNativeRFBUnchangedWindowIsPolledSlowlyAndInputIsNotDelayed(t *testing.T) {
	frame := windowTestFrame(t, 2, 1)
	frame.Sequence = "first"
	server, client := net.Pipe()
	defer client.Close()
	if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var frames, keys []time.Time
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		done <- serveWindowsWindowRFB(server, frame, func(input map[string]any, output any) error {
			mu.Lock()
			defer mu.Unlock()
			switch input["action"] {
			case "frame":
				frames = append(frames, time.Now())
				if input["after"] == "first" {
					*output.(*windowsWindowFrame) = windowsWindowFrame{Sequence: "first", Unchanged: true}
				} else {
					*output.(*windowsWindowFrame) = frame
				}
			case "key":
				keys = append(keys, time.Now())
			}
			return nil
		})
	}()
	read := func(n int) []byte {
		t.Helper()
		data := make([]byte, n)
		if _, err := io.ReadFull(client, data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	write := func(data []byte) {
		t.Helper()
		if _, err := client.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	read(12)
	write([]byte("RFB 003.008\n"))
	read(2)
	write([]byte{1})
	read(4)
	write([]byte{1})
	init := read(24)
	read(int(binary.BigEndian.Uint32(init[20:])))
	write([]byte{2, 0, 0, 1, 0, 0, 0, 0})
	write([]byte{3, 0, 0, 0, 0, 0, 0, 2, 0, 1})
	read(16 + 2*1*4)
	// The viewer answers every update with its next incremental request.
	updates := make(chan struct{}, 64)
	go func() {
		for {
			if _, err := client.Write([]byte{3, 1, 0, 0, 0, 0, 0, 2, 0, 1}); err != nil {
				return
			}
			empty := make([]byte, 4)
			if _, err := io.ReadFull(client, empty); err != nil {
				return
			}
			updates <- struct{}{}
		}
	}()
	time.Sleep(1200 * time.Millisecond)
	mu.Lock()
	polled := len(frames) - 1
	mu.Unlock()
	// 36 requests at the fixed rate; 7 with the pauses of an unchanged window.
	if polled < 4 || polled > 10 {
		t.Fatalf("an unchanged window was requested %d times in 1.2 seconds", polled)
	}
	// Send a key early in a long pause.
	for len(updates) > 0 {
		<-updates
	}
	<-updates
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	before := len(frames)
	mu.Unlock()
	sent := time.Now()
	write([]byte{4, 1, 0, 0, 0, 0, 0, 'a'})
	<-updates
	mu.Lock()
	if len(keys) != 1 || keys[0].Sub(sent) > 150*time.Millisecond {
		t.Fatalf("key waited for the pending frame request: %v", keys)
	}
	if len(frames) != before+1 || frames[before].Sub(sent) > 150*time.Millisecond {
		t.Fatalf("frame after input was delayed by %v", frames[len(frames)-1].Sub(sent))
	}
	if frames[before].Before(keys[0]) {
		t.Fatal("frame was requested before the key reached the guest")
	}
	mu.Unlock()
	client.Close()
	<-done
}
