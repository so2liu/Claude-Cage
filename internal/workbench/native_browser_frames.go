package workbench

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"time"
)

// Chromium may publish the previous compositor surface immediately after a
// viewport resize. Never send that bitmap under the new RFB dimensions. Keep
// the authenticated viewer attached while waiting briefly for a matching frame.
func readNativeBrowserFrame(ctx context.Context, read func() ([]byte, error)) (windowsWindowFrame, error) {
	until := time.Now().Add(2 * time.Second)
	for {
		data, err := read()
		if err != nil {
			return windowsWindowFrame{}, err
		}
		var frame windowsWindowFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			return frame, err
		}
		if frame.Unchanged || !nativeBrowserFrameHasStaleSize(frame) {
			return frame, nil
		}
		if time.Now().After(until) {
			return windowsWindowFrame{}, errors.New("浏览器画面尺寸调整未完成，请稍后重新连接")
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return windowsWindowFrame{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func nativeBrowserFrameHasStaleSize(frame windowsWindowFrame) bool {
	panes := frame.Panes
	if len(panes) == 0 {
		panes = []windowsWindowPane{{windowsWindowFrame: frame}}
	}
	for _, pane := range panes {
		encoded := pane.JPEG
		if pane.PNG != "" {
			encoded = pane.PNG
		}
		// Invalid encodings and layouts still go through the existing strict
		// RFB validator. Only a valid bitmap with stale dimensions is retried.
		if encoded == "" || len(encoded) > (8<<20)*4/3+4 {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}
		config, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err == nil && (config.Width != pane.Width || config.Height != pane.Height) {
			return true
		}
	}
	return false
}
