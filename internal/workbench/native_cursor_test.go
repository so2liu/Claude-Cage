package workbench

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func TestNativeRFBPublishesVisibleCursorWhenNegotiated(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	frame := browserFrameFixture(t, 64, 48)
	go func() {
		defer server.Close()
		_ = serveWindowsWindowRFB(server, frame, func(_ map[string]any, out any) error {
			if out != nil {
				*out.(*windowsWindowFrame) = frame
			}
			return nil
		})
	}()
	read := func(n int) []byte {
		t.Helper()
		b := make([]byte, n)
		if _, err := io.ReadFull(client, b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	write := func(b []byte) {
		t.Helper()
		if _, err := client.Write(b); err != nil {
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
	// Negotiate the same Cursor pseudo-encoding as the production noVNC client.
	write([]byte{2, 0, 0, 1, 255, 255, 255, 17})
	header := read(16)
	if header[0] != 0 || binary.BigEndian.Uint16(header[2:]) != 1 || int32(binary.BigEndian.Uint32(header[12:])) != -239 {
		t.Fatalf("invalid cursor update: %v", header)
	}
	w, h := int(binary.BigEndian.Uint16(header[8:])), int(binary.BigEndian.Uint16(header[10:]))
	if w < 1 || h < 1 || w > 64 || h > 64 {
		t.Fatalf("invalid pointer dimensions %dx%d", w, h)
	}
	data := read(w*h*4 + ((w+7)/8)*h)
	pixels, mask := data[:w*h*4], data[w*h*4:]
	black, white, transparent := 0, 0, 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if mask[y*((w+7)/8)+x/8]&(0x80>>uint(x%8)) == 0 {
				transparent++
				continue
			}
			i := (y*w + x) * 4
			if pixels[i] == 0 && pixels[i+1] == 0 && pixels[i+2] == 0 {
				black++
			}
			if pixels[i] == 255 && pixels[i+1] == 255 && pixels[i+2] == 255 {
				white++
			}
		}
	}
	if black == 0 || white == 0 || transparent == 0 {
		t.Fatalf("pointer lacks contrast/transparency: %d %d %d", black, white, transparent)
	}
	if output := os.Getenv("CAGE_CURSOR_PACKET_OUTPUT"); output != "" {
		if err := os.WriteFile(output, append(header, data...), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
