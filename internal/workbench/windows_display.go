package workbench

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/guestos"
)

type windowsWindowFrame struct {
	Sequence  string              `json:"sequence,omitempty"`
	Unchanged bool                `json:"unchanged,omitempty"`
	ID        string              `json:"id"`
	Document  string              `json:"document"`
	PNG       string              `json:"png"`
	JPEG      string              `json:"jpeg"`
	Width     int                 `json:"width"`
	Height    int                 `json:"height"`
	Target    string              `json:"target"`
	Density   float64             `json:"density"`
	Panes     []windowsWindowPane `json:"panes,omitempty"`
	Overlays  []windowsWindowPane `json:"overlays,omitempty"`
	Wheel     bool                `json:"pixelWheel,omitempty"` // the guest scrolls by a pointer event's distance
}

type windowsWindowPane struct {
	windowsWindowFrame
	X int `json:"x"`
	Y int `json:"y"`
}

type windowsWindowFailure struct{ code string }

func (e windowsWindowFailure) Error() string {
	return "Windows 应用操作未完成，请确认客机已解锁且 Claude 窗口仍在运行"
}

// Retry only validated frame reads during short window/popup transitions.
// Session, identity, permission and transport failures are never suppressed.
func transientWindowFrame(code string) bool {
	switch code {
	case "browser_target_changed", "target_changed", "desktop_target_changed", "desktop_capture_busy", "desktop_empty_frame":
		return true
	}
	return false
}

func requestDesktopFrame(ctx context.Context, request func(any, any) error, input map[string]any, output any) error {
	for attempt := 0; ; attempt++ {
		err := request(input, output)
		var failure windowsWindowFailure
		if attempt >= 5 || !errors.As(err, &failure) || !transientWindowFrame(failure.code) {
			return err
		}
		timer := time.NewTimer(80 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// release ends the guest connection that owns this viewer's held keys and
// buttons and opens another.
func desktopViewerRequest(ctx context.Context, request func(any, any) error, release func() error) func(map[string]any, any) error {
	inputSuspended := false
	return func(input map[string]any, output any) error {
		if input["action"] == "frame" {
			if inputSuspended {
				delete(input, "after")
			}
			err := requestDesktopFrame(ctx, request, input, output)
			if err == nil && !output.(*windowsWindowFrame).Unchanged {
				inputSuspended = false
			}
			return err
		}
		isInput := input["action"] == "pointer" || input["action"] == "key" || input["action"] == "text"
		if isInput && inputSuspended {
			return nil
		}
		err := request(input, output)
		var failure windowsWindowFailure
		if isInput && errors.As(err, &failure) && failure.code == "desktop_target_changed" {
			// The guest keeps keys and buttons held when it rejects an event:
			// a dropped key-up would leave its key down. Release them, then
			// drop the old event and queued input; never replay it on a popup.
			if err := release(); err != nil {
				return err
			}
			inputSuspended = true
			return nil
		}
		return err
	}
}

// A viewer's guest connection owns the keys and buttons it holds down, and
// the guest releases them when that connection ends.
type desktopViewerConnection struct {
	mu      sync.Mutex
	current *nativeWindowConnection
	closed  bool
}

func (c *desktopViewerConnection) request(input, output any) error {
	c.mu.Lock()
	current := c.current
	c.mu.Unlock()
	return current.request(input, output)
}

func (c *desktopViewerConnection) replace(open func() (*nativeWindowConnection, error)) error {
	next, err := open()
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		next.Close()
		return net.ErrClosed
	}
	previous := c.current
	c.current = next
	c.mu.Unlock()
	previous.Close()
	return nil
}

func (c *desktopViewerConnection) Close() {
	c.mu.Lock()
	c.closed = true
	current := c.current
	c.mu.Unlock()
	current.Close()
}

type nativeWindowConnection struct {
	cmd     *exec.Cmd
	input   io.WriteCloser
	output  io.ReadCloser
	scanner *bufio.Scanner
	cancel  context.CancelFunc
	once    sync.Once
}

func (s *Server) windowsWindowConnection(ctx context.Context) (*nativeWindowConnection, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd, err := guestos.WindowsRelayCommand(ctx, s.profile, "desktop")
	if err != nil {
		cancel()
		return nil, err
	}
	return startWindowConnection(cmd, cancel)
}

func startWindowConnection(cmd *exec.Cmd, cancel context.CancelFunc) (*nativeWindowConnection, error) {
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		cancel()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		input.Close()
		output.Close()
		cancel()
		return nil, err
	}
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64<<10), clipboardWireLimit)
	return &nativeWindowConnection{cmd: cmd, input: input, output: output, scanner: scanner, cancel: cancel}, nil
}

func (c *nativeWindowConnection) Close() {
	c.once.Do(func() { c.cancel(); c.input.Close(); c.output.Close(); _ = c.cmd.Wait() })
}

func (c *nativeWindowConnection) request(input any, output any) error {
	if err := json.NewEncoder(c.input).Encode(input); err != nil {
		return err
	}
	if !c.scanner.Scan() {
		return errors.New("客机应用连接中断")
	}
	var status struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(c.scanner.Bytes(), &status) != nil {
		return errors.New("客机应用返回了无效响应")
	}
	if status.Error != "" {
		return windowsWindowFailure{code: status.Error}
	}
	if output == nil {
		return nil
	}
	return json.Unmarshal(c.scanner.Bytes(), output)
}

func (s *Server) windowsDesktopRequest(ctx context.Context, action string, input map[string]any, output any) error {
	connection, err := s.windowsWindowConnection(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	if input == nil {
		input = map[string]any{}
	}
	input["action"] = action
	return connection.request(input, output)
}

// Windows renders the same RFB view used by Linux. The guest publishes only its
// application client area; the controller never requests a desktop screenshot.
func (s *Server) windowsDesktopVNC(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	first, err := s.windowsWindowConnection(ctx)
	if err != nil {
		http.Error(w, "Windows 应用画面尚未连接", 502)
		return
	}
	connection := &desktopViewerConnection{current: first}
	defer connection.Close()
	var frame windowsWindowFrame
	if err = requestDesktopFrame(ctx, connection.request, map[string]any{"action": "frame"}, &frame); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	local, remote := net.Pipe()
	defer local.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer remote.Close()
		request := desktopViewerRequest(ctx, connection.request, func() error {
			return connection.replace(func() (*nativeWindowConnection, error) { return s.windowsWindowConnection(ctx) })
		})
		_ = serveWindowsWindowRFB(remote, frame, func(input map[string]any, output any) error {
			s.mu.Lock()
			keyboard := s.preferences.Keyboard
			s.mu.Unlock()
			if input["action"] == "key" {
				if key, ok := input["keysym"].(uint32); ok {
					input["keysym"] = windowsGuestKeysym(key, keyboard)
				}
			}
			return request(input, output)
		})
	}()
	s.streamVNC(w, r, local, local, func() { cancel(); local.Close(); remote.Close(); connection.Close() })
	<-done
}

func windowsGuestKeysym(key uint32, keyboard string) uint32 {
	if keyboard == "mac" && (key == 0xffe7 || key == 0xffe8 || key == 0xffeb || key == 0xffec) {
		return 0xffe3
	}
	return key
}

func nativeFrameImage(frame windowsWindowFrame) (image.Image, error) {
	if frame.Width < 1 || frame.Height < 1 || frame.Width > 8192 || frame.Height > 8192 || int64(frame.Width)*int64(frame.Height) > 40_000_000 || len(frame.Target) == 0 || len(frame.Target) > 256 {
		return nil, errors.New("应用画面无效")
	}
	if len(frame.Overlays) > 0 {
		if len(frame.Overlays) > 16 {
			return nil, errors.New("应用覆盖画面无效")
		}
		overlays := frame.Overlays
		frame.Overlays = nil
		base, err := nativeFrameImage(frame)
		if err != nil {
			return nil, err
		}
		canvas := image.NewRGBA(base.Bounds())
		draw.Draw(canvas, canvas.Bounds(), base, base.Bounds().Min, draw.Src)
		for _, overlay := range overlays {
			if len(overlay.Panes) != 0 || len(overlay.Overlays) != 0 || overlay.Width > frame.Width || overlay.Height > frame.Height || overlay.X < -frame.Width || overlay.Y < -frame.Height || overlay.X > frame.Width || overlay.Y > frame.Height {
				return nil, errors.New("应用覆盖位置无效")
			}
			overlay.Target = frame.Target
			img, err := nativeFrameImage(overlay.windowsWindowFrame)
			if err != nil {
				return nil, err
			}
			area := image.Rect(overlay.X, overlay.Y, overlay.X+overlay.Width, overlay.Y+overlay.Height)
			draw.Draw(canvas, area, img, img.Bounds().Min, draw.Over)
		}
		return canvas, nil
	}
	if len(frame.Panes) > 0 {
		if len(frame.Panes) != 2 || frame.PNG != "" || frame.JPEG != "" {
			return nil, errors.New("分屏画面无效")
		}
		img := image.NewRGBA(image.Rect(0, 0, frame.Width, frame.Height))
		draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
		var previous image.Rectangle
		for index, pane := range frame.Panes {
			bounds := image.Rect(pane.X, pane.Y, pane.X+pane.Width, pane.Y+pane.Height)
			if pane.X < 0 || pane.Y < 0 || len(pane.Panes) != 0 || !bounds.In(img.Bounds()) || index > 0 && !previous.Intersect(bounds).Empty() {
				return nil, errors.New("分屏画面位置无效")
			}
			pane.Target = frame.Target
			part, err := nativeFrameImage(pane.windowsWindowFrame)
			if err != nil {
				return nil, err
			}
			draw.Draw(img, bounds, part, part.Bounds().Min, draw.Src)
			previous = bounds
		}
		return img, nil
	}
	encoded := frame.PNG
	if encoded == "" {
		encoded = frame.JPEG
	}
	if len(encoded) > (20<<20)*4/3+4 {
		return nil, errors.New("应用画面无效")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width != frame.Width || config.Height != frame.Height {
		return nil, errors.New("应用画面尺寸无效")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

func nativeFramePixels(frame windowsWindowFrame) ([]byte, error) {
	decoded, err := nativeFrameImage(frame)
	if err != nil {
		return nil, err
	}
	rgba, ok := decoded.(*image.RGBA)
	if !ok {
		rgba = image.NewRGBA(image.Rect(0, 0, frame.Width, frame.Height))
		draw.Draw(rgba, rgba.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	}
	pixels := make([]byte, frame.Width*frame.Height*4)
	for y := 0; y < frame.Height; y++ {
		row := rgba.Pix[y*rgba.Stride : y*rgba.Stride+frame.Width*4]
		for x := 0; x < frame.Width; x++ {
			i := (y*frame.Width + x) * 4
			pixels[i] = row[x*4+2]
			pixels[i+1] = row[x*4+1]
			pixels[i+2] = row[x*4]
		}
	}
	return pixels, nil
}

func serveWindowsWindowRFB(stream io.ReadWriter, initial windowsWindowFrame, request func(map[string]any, any) error) error {
	if _, err := nativeFrameImage(initial); err != nil {
		return err
	}
	write := func(data []byte) error { _, err := stream.Write(data); return err }
	read := func(n int) ([]byte, error) {
		data := make([]byte, n)
		_, err := io.ReadFull(stream, data)
		return data, err
	}
	if err := write([]byte("RFB 003.008\n")); err != nil {
		return err
	}
	version, err := read(12)
	if err != nil || string(version) != "RFB 003.008\n" {
		return errors.New("画面协议版本无效")
	}
	// HTTP/WS origin, account and controller authentication precede this stream.
	if err = write([]byte{1, 1}); err != nil {
		return err
	}
	security, err := read(1)
	if err != nil || security[0] != 1 {
		return errors.New("画面认证协议无效")
	}
	if err = write([]byte{0, 0, 0, 0}); err != nil {
		return err
	}
	if _, err = read(1); err != nil {
		return err
	}
	init := make([]byte, 24)
	binary.BigEndian.PutUint16(init, uint16(initial.Width))
	binary.BigEndian.PutUint16(init[2:], uint16(initial.Height))
	copy(init[4:], []byte{32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0})
	name := []byte("Cage application")
	binary.BigEndian.PutUint32(init[20:], uint32(len(name)))
	init = append(init, name...)
	if err = write(init); err != nil {
		return err
	}
	frame := initial
	sized := false
	supportsResize := false
	supportsFence := false
	pixelWheel := false
	rgbOrder := false
	useZlib := false
	useTight := false
	useTightPNG := false
	var compressed bytes.Buffer
	var compressor *zlib.Writer
	defer func() {
		if compressor != nil {
			_ = compressor.Close()
		}
	}()
	// Reading ahead of the guest keeps input moving while a frame request
	// waits for its turn. Messages are still handled one at a time, in order.
	messages := make(chan []byte)
	stopped := make(chan struct{})
	defer close(stopped)
	var readErr error
	go func() {
		defer close(messages)
		for {
			message, err := readRFBClientMessage(stream)
			if err != nil {
				readErr = err
				return
			}
			select {
			case messages <- message:
			case <-stopped:
				return
			}
		}
	}()
	var pace nativeFramePace
	// The pending update request, and whether it accepts an unchanged frame.
	pending, incremental := false, true
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	schedule := func() {
		timer.Stop()
		select {
		case <-timer.C:
		default:
		}
		timer.Reset(time.Until(pace.next()))
	}
	sendFrame := func() error {
		pending = false
		pace.requested(time.Now())
		input := map[string]any{"action": "frame"}
		if incremental {
			input["after"] = frame.Sequence
		}
		wasIncremental := incremental
		incremental = true
		var next windowsWindowFrame
		if err := request(input, &next); err != nil {
			return err
		}
		pace.answered(!next.Unchanged)
		if next.Unchanged {
			if !wasIncremental || frame.Sequence == "" || next.Sequence != frame.Sequence {
				return errors.New("应用画面更新标识无效")
			}
			return write([]byte{0, 0, 0, 0})
		}
		if useTight || useTightPNG {
			encoding := int32(7)
			if useTightPNG {
				encoding = -260
			}
			rectangles, count, supported, err := nativeTightRectangles(next, encoding)
			if err != nil {
				return err
			}
			if supported {
				changed := next.Width != frame.Width || next.Height != frame.Height
				if changed && !supportsResize {
					return errors.New("画面尺寸已变化，请重新连接")
				}
				if changed {
					count++
				}
				header := []byte{0, 0, byte(count >> 8), byte(count)}
				if changed {
					header = append(header, windowRFBRect(next.Width, next.Height, -223)...)
				}
				if err = write(append(header, rectangles...)); err != nil {
					return err
				}
				frame = next
				sized = true
				return nil
			}
		}
		pixels, err := nativeFramePixels(next)
		if err != nil {
			return err
		}
		if rgbOrder {
			for i := 0; i < len(pixels); i += 4 {
				pixels[i], pixels[i+2] = pixels[i+2], pixels[i]
			}
		}
		encoding := int32(0)
		payload := pixels
		if useZlib {
			if compressor == nil {
				compressor, err = zlib.NewWriterLevel(&compressed, zlib.BestSpeed)
				if err != nil {
					return err
				}
			}
			compressed.Reset()
			if _, err = compressor.Write(pixels); err != nil {
				return err
			}
			if err = compressor.Flush(); err != nil {
				return err
			}
			payload = make([]byte, 4+compressed.Len())
			binary.BigEndian.PutUint32(payload, uint32(compressed.Len()))
			copy(payload[4:], compressed.Bytes())
			encoding = 6
		}
		changed := next.Width != frame.Width || next.Height != frame.Height
		if changed && !supportsResize {
			return errors.New("画面尺寸已变化，请重新连接")
		}
		count := uint16(1)
		if changed {
			count++
		}
		header := make([]byte, 4)
		binary.BigEndian.PutUint16(header[2:], count)
		if changed {
			header = append(header, windowRFBRect(next.Width, next.Height, -223)...)
		}
		header = append(header, windowRFBRect(next.Width, next.Height, encoding)...)
		if err = write(header); err != nil {
			return err
		}
		if err = write(payload); err != nil {
			return err
		}
		frame = next
		sized = true
		return nil
	}
	for {
		var due <-chan time.Time
		if pending {
			due = timer.C
		}
		var message []byte
		select {
		case <-due:
			if err := sendFrame(); err != nil {
				return err
			}
			continue
		case received, open := <-messages:
			if !open {
				return readErr
			}
			message = received
		}
		body := message[1:]
		switch message[0] {
		case 0:
			base := []byte{32, 24, 0, 1, 0, 255, 0, 255, 0, 255}
			if !bytes.Equal(body[3:13], base) || (!bytes.Equal(body[13:16], []byte{16, 8, 0}) && !bytes.Equal(body[13:16], []byte{0, 8, 16})) {
				return errors.New("画面像素格式无效")
			}
			rgbOrder = body[13] == 0
		case 2:
			encodings := body[3:]
			useZlib = false
			useTight = false
			useTightPNG = false
			fenceRequested, wheelRequested := false, false
			for i := 0; i < len(encodings); i += 4 {
				switch int32(binary.BigEndian.Uint32(encodings[i:])) {
				case cageWheelEncoding:
					wheelRequested = true
				case -312:
					fenceRequested = true
				case -223:
					supportsResize = true
				case 6:
					useZlib = true
				case 7:
					useTight = true
				case -260:
					useTightPNG = true
				}
			}
			if fenceRequested && !supportsFence {
				// noVNC enables ordered input after the server's first Fence request.
				if err := write([]byte{248, 0, 0, 0, 0x80, 0, 0, 0, 0}); err != nil {
					return err
				}
			}
			supportsFence = fenceRequested
			// An older guest would scroll one fixed step for every distance.
			wheelRequested = wheelRequested && initial.Wheel
			if fenceRequested && wheelRequested && !pixelWheel {
				// The viewer sends distances only after this confirmation.
				confirmation := append([]byte{248, 0, 0, 0, 0x80, 0, 0, 0, byte(len(cageWheelCapability))}, cageWheelCapability...)
				if err := write(confirmation); err != nil {
					return err
				}
			}
			pixelWheel = fenceRequested && wheelRequested
			// Only the browser display offers the wheel. Its guest streams
			// frames as it composes them, so scrolling can be shown at 60 Hz.
			pace.smooth = pixelWheel
		case 3:
			if body[0] == 0 {
				incremental = false
			}
			if !pending {
				pending = true
				schedule()
			}
		case 4:
			if !sized {
				continue
			}
			if err := request(map[string]any{"action": "key", "target": frame.Target, "keysym": binary.BigEndian.Uint32(body[3:]), "down": body[0] != 0}, nil); err != nil {
				return err
			}
		case 5:
			if !sized {
				continue
			}
			if err := request(map[string]any{"action": "pointer", "target": frame.Target, "mask": body[0], "x": binary.BigEndian.Uint16(body[1:]), "y": binary.BigEndian.Uint16(body[3:])}, nil); err != nil {
				return err
			}
		case cageWheelMessage:
			dx, dy := int32(binary.BigEndian.Uint32(body[5:])), int32(binary.BigEndian.Uint32(body[9:]))
			if !pixelWheel || body[0] != 0 || dx < -cageWheelLimit || dx > cageWheelLimit || dy < -cageWheelLimit || dy > cageWheelLimit {
				return errors.New("画面滚动消息无效")
			}
			if !sized || dx == 0 && dy == 0 {
				continue
			}
			// The direction bits let a guest without distances scroll one step.
			mask := 0
			if dx < 0 {
				mask |= 32
			} else if dx > 0 {
				mask |= 64
			}
			if dy < 0 {
				mask |= 8
			} else if dy > 0 {
				mask |= 16
			}
			if err := request(map[string]any{"action": "pointer", "target": frame.Target, "mask": mask,
				"x": binary.BigEndian.Uint16(body[1:]), "y": binary.BigEndian.Uint16(body[3:]), "deltaX": dx, "deltaY": dy}, nil); err != nil {
				return err
			}
		case 6:
			// Native shared clipboard owns clipboard data; committed text takes the
			// fixed Unicode input path without changing the OS clipboard.
			if err := request(map[string]any{"action": "text", "target": frame.Target, "text": string(body[7:])}, nil); err != nil {
				return err
			}
		case 248:
			if !supportsFence {
				return errors.New("画面输入同步消息无效")
			}
			flags := binary.BigEndian.Uint32(body[3:7])
			if flags&(1<<31) != 0 {
				// Guest input and stream writes finish in order on this goroutine,
				// satisfying BlockBefore and BlockAfter. SyncNext is unsupported.
				reply := append([]byte{248}, body...)
				binary.BigEndian.PutUint32(reply[4:8], flags&3)
				if err := write(reply); err != nil {
					return err
				}
			}
		}
		if message[0] == 4 || message[0] == 5 || message[0] == 6 || message[0] == cageWheelMessage {
			// The window answers input at once: the next frame is not delayed
			// by the pauses of an unchanged screen.
			pace.input(time.Now())
			if pending {
				schedule()
			}
		}
	}
}

// nativeFrameInterval is the shortest time between two frame requests, and
// nativeOperatingInterval the one a browser display has while it is operated.
const (
	nativeFrameInterval     = time.Second / 30
	nativeOperatingInterval = time.Second / 60
	nativeOperatingHold     = time.Second
)

// The Cage browser display sends the scrolled distance in one message. It
// offers the encoding, and uses the message once a fence names the capability.
const (
	cageWheelEncoding   = -57345
	cageWheelMessage    = 180
	cageWheelCapability = "cage-wheel-v1"
	cageWheelLimit      = 2048
)

// nativeFramePace spaces frame requests to a native guest. Each one is a
// round trip into the guest, also when its window has not changed, so the
// pause doubles with every unchanged answer, up to about a quarter second.
// A changed frame or the viewer's input restores the shortest pause. Every
// frame is a complete image: a smooth display has the shorter pause only for
// a moment after input, and plays unattended animation at the usual one.
type nativeFramePace struct {
	last, acted time.Time
	unchanged   int
	smooth      bool
}

func (p *nativeFramePace) next() time.Time {
	interval := nativeFrameInterval
	if p.smooth && !p.acted.IsZero() && p.last.Sub(p.acted) < nativeOperatingHold {
		interval = nativeOperatingInterval
	}
	return p.last.Add(interval << min(p.unchanged, 3))
}

func (p *nativeFramePace) requested(now time.Time) { p.last = now }

func (p *nativeFramePace) answered(changed bool) {
	if changed {
		p.unchanged = 0
	} else {
		p.unchanged++
	}
}

func (p *nativeFramePace) input(now time.Time) { p.unchanged, p.acted = 0, now }

// readRFBClientMessage reads one complete client message, type byte first.
func readRFBClientMessage(stream io.Reader) ([]byte, error) {
	message := make([]byte, 1, 32)
	if _, err := io.ReadFull(stream, message); err != nil {
		return nil, err
	}
	extend := func(n int) error {
		size := len(message)
		message = append(message, make([]byte, n)...)
		_, err := io.ReadFull(stream, message[size:])
		return err
	}
	var err error
	switch message[0] {
	case 0:
		err = extend(19)
	case 2:
		if err = extend(3); err == nil {
			count := int(binary.BigEndian.Uint16(message[2:]))
			if count > 256 {
				return nil, errors.New("画面编码列表无效")
			}
			err = extend(count * 4)
		}
	case 3:
		err = extend(9)
	case 4:
		err = extend(7)
	case 5:
		err = extend(5)
	case 6:
		if err = extend(7); err == nil {
			size := binary.BigEndian.Uint32(message[4:])
			if size > 2<<20 {
				return nil, errors.New("粘贴文字超过限制")
			}
			err = extend(int(size))
		}
	case cageWheelMessage:
		err = extend(13)
	case 248:
		if err = extend(8); err == nil {
			if message[8] > 64 {
				return nil, errors.New("画面输入同步消息无效")
			}
			err = extend(int(message[8]))
		}
	default:
		return nil, errors.New("画面输入协议无效")
	}
	return message, err
}

func windowRFBRect(width, height int, encoding int32) []byte {
	header := make([]byte, 12)
	binary.BigEndian.PutUint16(header[4:], uint16(width))
	binary.BigEndian.PutUint16(header[6:], uint16(height))
	binary.BigEndian.PutUint32(header[8:], uint32(encoding))
	return header
}

func (s *Server) windowsDesktopCredentials(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var state claudeDesktopState
	if err := s.windowsDesktopRequest(ctx, "status", nil, &state); err != nil || !state.Running {
		http.Error(w, "Claude Desktop 尚未启动", 409)
		return
	}
	density := state.DisplayScale
	if density < 1 || density > 8 {
		http.Error(w, "Claude Desktop 返回了无效的显示比例", 502)
		return
	}
	jsonReply(w, map[string]any{"password": "", "display_scale": density})
}
