package workbench

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"testing"
	"time"
)

func browserFrameFixture(t *testing.T, w, h int) windowsWindowFrame {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return windowsWindowFrame{Width: w, Height: h, JPEG: base64.StdEncoding.EncodeToString(b.Bytes()), Target: "viewport", Sequence: "valid"}
}
func TestNativeBrowserResizeKeepsViewerUntilMatchingBitmap(t *testing.T) {
	good := browserFrameFixture(t, 64, 48)
	stale := browserFrameFixture(t, 64, 40)
	stale.Height = 48
	stale.Sequence = "stale"
	frames := []windowsWindowFrame{stale, stale, good}
	calls := 0
	got, err := readNativeBrowserFrame(t.Context(), func() ([]byte, error) { f := frames[calls]; calls++; return json.Marshal(f) })
	if err != nil || got.Sequence != "valid" || calls != 3 {
		t.Fatalf("frame=%+v calls=%d error=%v", got, calls, err)
	}
	if _, err := nativeFrameImage(got); err != nil {
		t.Fatal(err)
	}
	if _, _, supported, err := nativeTightRectangles(got, 7); err != nil || !supported {
		t.Fatalf("valid frame rejected: %v", err)
	}
}
func TestNativeBrowserStaleFrameCancellation(t *testing.T) {
	stale := browserFrameFixture(t, 64, 40)
	stale.Height = 48
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	_, err := readNativeBrowserFrame(ctx, func() ([]byte, error) { return json.Marshal(stale) })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation lost: %v", err)
	}
}
func TestNativeBrowserRetainsStrictFrameValidation(t *testing.T) {
	frame := browserFrameFixture(t, 64, 48)
	frame.JPEG = "invalid base64"
	got, err := readNativeBrowserFrame(t.Context(), func() ([]byte, error) { return json.Marshal(frame) })
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := nativeTightRectangles(got, 7); err == nil {
		t.Fatal("corrupt bitmap accepted")
	}
	unchanged := windowsWindowFrame{Unchanged: true, Sequence: "receipt"}
	got, err = readNativeBrowserFrame(t.Context(), func() ([]byte, error) { return json.Marshal(unchanged) })
	if err != nil || !got.Unchanged || got.Sequence != "receipt" {
		t.Fatalf("unchanged receipt lost: %v", err)
	}
	split := windowsWindowFrame{Width: 128, Height: 48, Panes: []windowsWindowPane{{windowsWindowFrame: browserFrameFixture(t, 64, 48)}, {windowsWindowFrame: browserFrameFixture(t, 64, 40), X: 64}}}
	split.Panes[1].Height = 48
	if !nativeBrowserFrameHasStaleSize(split) {
		t.Fatal("split viewport transition ignored")
	}
}
