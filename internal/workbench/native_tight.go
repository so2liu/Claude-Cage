package workbench

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
)

// Tight carries the guest's encoded image directly to noVNC. The host checks dimensions
// and placement without decompressing pixels or compressing them a second time.
func nativeTightRectangles(frame windowsWindowFrame, encoding int32) ([]byte, uint16, bool, error) {
	if len(frame.Overlays) != 0 {
		return nativeTightOverlayRectangles(frame, encoding)
	}
	if frame.Width < 1 || frame.Height < 1 || frame.Width > 8192 || frame.Height > 8192 ||
		int64(frame.Width)*int64(frame.Height) > 40_000_000 || len(frame.Target) == 0 || len(frame.Target) > 256 {
		return nil, 0, false, errors.New("应用画面尺寸无效")
	}
	panes := frame.Panes
	if len(panes) != 0 && (len(panes) != 2 || frame.JPEG != "" || frame.PNG != "") {
		return nil, 0, false, errors.New("应用画面分屏无效")
	}
	if len(panes) == 2 && (panes[0].X != 0 || panes[1].X != panes[0].Width ||
		panes[0].Width+panes[1].Width != frame.Width || panes[0].Y != 0 || panes[1].Y != 0 ||
		panes[0].Height != frame.Height || panes[1].Height != frame.Height) {
		return nil, 0, false, nil
	}
	if len(panes) == 0 {
		panes = []windowsWindowPane{{windowsWindowFrame: frame}}
	}
	if len(panes) > 2 {
		return nil, 0, false, errors.New("应用画面分屏无效")
	}
	var output []byte
	var previous image.Rectangle
	for index, pane := range panes {
		encoded, expected, control := pane.JPEG, "jpeg", byte(0x90)
		if pane.PNG != "" {
			if encoding != -260 {
				return nil, 0, false, nil
			}
			encoded, expected, control = pane.PNG, "png", 0xa0
		}
		if encoded == "" || len(pane.Panes) != 0 || len(pane.Overlays) != 0 {
			return nil, 0, false, nil
		}
		if len(encoded) > (8<<20)*4/3+4 {
			return nil, 0, false, errors.New("应用画面过大")
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, 0, false, err
		}
		if len(data) > 0x3fffff {
			return nil, 0, false, nil
		}
		config, format, err := image.DecodeConfig(bytes.NewReader(data))
		bounds := image.Rect(pane.X, pane.Y, pane.X+pane.Width, pane.Y+pane.Height)
		if err != nil || format != expected || config.Width != pane.Width || config.Height != pane.Height ||
			!bounds.In(image.Rect(0, 0, frame.Width, frame.Height)) || index > 0 && !previous.Intersect(bounds).Empty() {
			return nil, 0, false, errors.New("应用画面位置或编码无效")
		}
		previous = bounds
		header := windowRFBRect(pane.Width, pane.Height, encoding)
		binary.BigEndian.PutUint16(header[0:2], uint16(pane.X))
		binary.BigEndian.PutUint16(header[2:4], uint16(pane.Y))
		output = append(output, header...)
		output = append(output, control)
		size := len(data)
		for i := 0; i < 2 && size >= 128; i++ {
			output = append(output, byte(size&127)|128)
			size >>= 7
		}
		output = append(output, byte(size))
		output = append(output, data...)
	}
	return output, uint16(len(panes)), true, nil
}

// Redraw the complete base before its overlays, preserving alpha and stacking
// order without decoding and recompressing the entire desktop on the host.
func nativeTightOverlayRectangles(frame windowsWindowFrame, encoding int32) ([]byte, uint16, bool, error) {
	if len(frame.Overlays) > 16 {
		return nil, 0, false, errors.New("应用覆盖画面无效")
	}
	overlays := frame.Overlays
	frame.Overlays = nil
	output, count, supported, err := nativeTightRectangles(frame, encoding)
	if err != nil || !supported {
		return nil, 0, supported, err
	}
	for _, overlay := range overlays {
		if len(overlay.Panes) != 0 || len(overlay.Overlays) != 0 || overlay.Width > frame.Width || overlay.Height > frame.Height || overlay.X < -frame.Width || overlay.Y < -frame.Height || overlay.X > frame.Width || overlay.Y > frame.Height {
			return nil, 0, false, errors.New("应用覆盖位置无效")
		}
		overlay.Target = frame.Target
		part, n, ok, err := nativeTightRectangles(overlay.windowsWindowFrame, encoding)
		if err != nil || !ok {
			return nil, 0, ok, err
		}
		bounds := image.Rect(overlay.X, overlay.Y, overlay.X+overlay.Width, overlay.Y+overlay.Height)
		if !bounds.In(image.Rect(0, 0, frame.Width, frame.Height)) {
			// Keep the existing compositor for partially clipped overlays.
			return nil, 0, false, nil
		}
		binary.BigEndian.PutUint16(part[0:2], uint16(overlay.X))
		binary.BigEndian.PutUint16(part[2:4], uint16(overlay.Y))
		output = append(output, part...)
		count += n
	}
	return output, count, true, nil
}
