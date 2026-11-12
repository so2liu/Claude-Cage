package workbench

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"testing"
)

func overlayTestFrame(t testing.TB, width, height int) windowsWindowFrame {
	t.Helper()
	base := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			base.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), uint8(x + y), 255})
		}
	}
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, base, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	popup := image.NewNRGBA(image.Rect(0, 0, 24, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 24; x++ {
			popup.SetNRGBA(x, y, color.NRGBA{200, 10, 40, uint8(x * 10)})
		}
	}
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, popup); err != nil {
		t.Fatal(err)
	}
	part := windowsWindowFrame{PNG: base64.StdEncoding.EncodeToString(pngData.Bytes()), Width: 24, Height: 16}
	return windowsWindowFrame{JPEG: base64.StdEncoding.EncodeToString(jpg.Bytes()), Width: width, Height: height, Target: "validated-target", Overlays: []windowsWindowPane{
		{windowsWindowFrame: part, X: 2, Y: 3}, {windowsWindowFrame: part, X: 12, Y: 9},
	}}
}

func TestNativeTightOverlaysPreservePixelsAndStacking(t *testing.T) {
	frame := overlayTestFrame(t, 64, 48)
	want, err := nativeFrameImage(frame)
	if err != nil {
		t.Fatal(err)
	}
	wire, count, ok, err := nativeTightRectangles(frame, -260)
	if err != nil || !ok || count != 3 {
		t.Fatalf("overlay passthrough: %d %v %v", count, ok, err)
	}
	got := image.NewRGBA(want.Bounds())
	r := bytes.NewReader(wire)
	for i := 0; i < int(count); i++ {
		header := make([]byte, 13)
		if _, err := io.ReadFull(r, header); err != nil {
			t.Fatal(err)
		}
		if int32(binary.BigEndian.Uint32(header[8:12])) != -260 {
			t.Fatal("wrong encoding")
		}
		if i == 0 && header[12] != 0x90 || i > 0 && header[12] != 0xa0 {
			t.Fatal("wrong image type")
		}
		size := 0
		for shift := 0; shift <= 14; shift += 7 {
			b, err := r.ReadByte()
			if err != nil {
				t.Fatal(err)
			}
			if shift == 14 {
				size |= int(b) << shift
				break
			}
			size |= int(b&127) << shift
			if b&128 == 0 {
				break
			}
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			original, _ := base64.StdEncoding.DecodeString(frame.JPEG)
			if !bytes.Equal(original, data) {
				t.Fatal("base image was re-encoded")
			}
		}
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		x, y := int(binary.BigEndian.Uint16(header)), int(binary.BigEndian.Uint16(header[2:]))
		draw.Draw(got, img.Bounds().Add(image.Pt(x, y)), img, img.Bounds().Min, draw.Over)
	}
	if r.Len() != 0 {
		t.Fatal("trailing payload")
	}
	for y := 0; y < frame.Height; y++ {
		for x := 0; x < frame.Width; x++ {
			if got.At(x, y) != want.At(x, y) {
				t.Fatalf("changed pixel at %d,%d", x, y)
			}
		}
	}
	// Every update includes the base, so removed/moved menus cannot leave trails.
	frame.Overlays = nil
	_, count, ok, err = nativeTightRectangles(frame, -260)
	if err != nil || !ok || count != 1 {
		t.Fatal("base redraw missing")
	}
}

func TestNativeTightOverlayLimits(t *testing.T) {
	t.Run("jpeg-only client falls back", func(t *testing.T) {
		_, _, ok, err := nativeTightRectangles(overlayTestFrame(t, 64, 48), 7)
		if ok || err != nil {
			t.Fatalf("unexpected: %v %v", ok, err)
		}
	})
	t.Run("clipped overlay falls back", func(t *testing.T) {
		frame := overlayTestFrame(t, 64, 48)
		frame.Overlays[0].X = -1
		_, _, ok, err := nativeTightRectangles(frame, -260)
		if ok || err != nil {
			t.Fatalf("unexpected: %v %v", ok, err)
		}
	})
	for _, name := range []string{"too many", "nested", "outside", "bad dimensions", "bad encoding"} {
		t.Run(name, func(t *testing.T) {
			frame := overlayTestFrame(t, 64, 48)
			switch name {
			case "too many":
				for len(frame.Overlays) < 17 {
					frame.Overlays = append(frame.Overlays, frame.Overlays[0])
				}
			case "nested":
				frame.Overlays[0].Overlays = frame.Overlays
			case "outside":
				frame.Overlays[0].X = 65
			case "bad dimensions":
				frame.Overlays[0].Width = 23
			case "bad encoding":
				frame.Overlays[0].PNG = "invalid"
			}
			_, _, ok, err := nativeTightRectangles(frame, -260)
			if ok || err == nil {
				t.Fatalf("invalid overlay accepted: %v %v", ok, err)
			}
		})
	}
}

func BenchmarkNativeOverlayTransport(b *testing.B) {
	frame := overlayTestFrame(b, 2700, 1770)
	b.Run("previous-full-composition", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			pixels, err := nativeFramePixels(frame)
			if err != nil {
				b.Fatal(err)
			}
			var output bytes.Buffer
			z, _ := zlib.NewWriterLevel(&output, zlib.BestSpeed)
			_, _ = z.Write(pixels)
			_ = z.Close()
		}
	})
	b.Run("layered-passthrough", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, _, ok, err := nativeTightRectangles(frame, -260); err != nil || !ok {
				b.Fatal(err)
			}
		}
	})
}
