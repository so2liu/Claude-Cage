package workbench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type macOSFrameHeader struct {
	ImageLength   int     `json:"imageLength"`
	MIME          string  `json:"mime,omitempty"`
	Sequence      string  `json:"sequence,omitempty"`
	Geometry      string  `json:"geometry,omitempty"`
	Window        uint32  `json:"window,omitempty"`
	PID           int32   `json:"pid,omitempty"`
	ContentWidth  float64 `json:"contentWidth,omitempty"`
	ContentHeight float64 `json:"contentHeight,omitempty"`
	PixelWidth    int     `json:"pixelWidth,omitempty"`
	PixelHeight   int     `json:"pixelHeight,omitempty"`
	Unchanged     bool    `json:"unchanged,omitempty"`
	Error         string  `json:"error,omitempty"`
	Revision      uint64  `json:"revision,omitempty"`
}

// Each record contains a big-endian JSON length, metadata, and the original
// encoded image. Validate dimensions without decompressing the image pixels.
func readMacOSFrame(reader io.Reader) (macOSFrameHeader, []byte, error) {
	var header macOSFrameHeader
	var size uint32
	if err := binary.Read(reader, binary.BigEndian, &size); err != nil {
		return header, nil, err
	}
	if size == 0 || size > 16<<10 {
		return header, nil, errors.New("窗口画面元数据过大")
	}
	metadata := make([]byte, size)
	if _, err := io.ReadFull(reader, metadata); err != nil {
		return header, nil, err
	}
	if err := json.Unmarshal(metadata, &header); err != nil {
		return header, nil, err
	}
	if header.ImageLength < 0 || header.ImageLength >= 8<<20 || len(header.Sequence) > 128 || len(header.Geometry) > 128 {
		return header, nil, errors.New("窗口画面元数据无效")
	}
	if header.Error != "" || header.Unchanged {
		if header.ImageLength != 0 {
			return header, nil, errors.New("窗口状态包含无效画面")
		}
		return header, nil, nil
	}
	if header.ImageLength == 0 || header.Sequence == "" || header.Geometry == "" || header.Window == 0 || header.PID <= 0 ||
		header.ContentWidth <= 0 || header.ContentHeight <= 0 || header.ContentWidth > 8192 || header.ContentHeight > 8192 ||
		header.PixelWidth < 1 || header.PixelHeight < 1 || header.PixelWidth > 8192 || header.PixelHeight > 8192 ||
		int64(header.PixelWidth)*int64(header.PixelHeight) > 40_000_000 {
		return header, nil, errors.New("窗口画面尺寸无效")
	}
	data := make([]byte, header.ImageLength)
	if _, err := io.ReadFull(reader, data); err != nil {
		return header, nil, err
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") || header.MIME != "image/"+format ||
		config.Width != header.PixelWidth || config.Height != header.PixelHeight {
		return header, nil, errors.New("窗口画面编码无效")
	}
	return header, data, nil
}

type macOSFrameSubscription struct {
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Scale    float64 `json:"scale"`
	Revision uint64  `json:"revision"`
	Ack      string  `json:"ack"`
}

// Frame waits have their own SSH channel so input never queues behind capture.
// Only one image is in flight until the browser decodes it or changes size.
func (s *Server) macOSWindowFrames(w http.ResponseWriter, r *http.Request) {
	if s.macosGuest == nil {
		http.NotFound(w, r)
		return
	}
	ws, err := s.upgrade(w, r)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(1024)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(s.lifetimeContext(), func() { cancel(); ws.Close() })
	defer stop()
	cmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", nativeWindowSocketCommand(r.URL.Path == "/api/macos/chrome/connection" || r.URL.Path == "/api/macos/chrome/frames", "30"))
	input, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	defer output.Close()
	if cmd.Start() != nil {
		return
	}
	defer func() { cancel(); input.Close(); output.Close(); _ = cmd.Wait() }()
	relayMacOSFrames(ctx, ws, input, output)
}

func relayMacOSFrames(parent context.Context, ws *websocket.Conn, input io.WriteCloser, output io.ReadCloser) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { input.Close(); output.Close(); ws.Close() })
	defer stop()
	var mutex sync.Mutex
	var latest macOSFrameSubscription
	changed := make(chan struct{}, 1)
	acknowledged := make(chan string, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer cancel()
		for {
			var next macOSFrameSubscription
			if ws.ReadJSON(&next) != nil {
				return
			}
			if next.Ack != "" {
				if len(next.Ack) > 128 {
					return
				}
				select {
				case acknowledged <- next.Ack:
				case <-ctx.Done():
					return
				}
				continue
			}
			if next.Revision == 0 || next.Width < 320 || next.Width > 2560 || next.Height < 240 || next.Height > 1600 || next.Scale < 1 || next.Scale > 2 {
				return
			}
			mutex.Lock()
			valid := next.Revision > latest.Revision
			if valid {
				latest = next
			}
			mutex.Unlock()
			if !valid {
				return
			}
			select {
			case changed <- struct{}{}:
			default:
			}
		}
	}()
	defer func() { cancel(); input.Close(); output.Close(); ws.Close(); <-readerDone }()
	select {
	case <-ctx.Done():
		return
	case <-changed:
	}
	reader := bufio.NewReaderSize(output, 64<<10)
	var revision uint64
	after := ""
	for ctx.Err() == nil {
		mutex.Lock()
		config := latest
		mutex.Unlock()
		if revision != config.Revision {
			after, revision = "", config.Revision
		}
		request := macOSWindowRequest{Action: "frame", Width: config.Width, Height: config.Height, Scale: config.Scale,
			After: after, KeepAlive: true, Wait: true, Binary: true, Refresh: true}
		body, _ := json.Marshal(request)
		timer := time.AfterFunc(12*time.Second, func() { cancel(); output.Close(); ws.Close() })
		_, err := input.Write(append(body, '\n'))
		if err != nil {
			timer.Stop()
			return
		}
		header, pixels, err := readMacOSFrame(reader)
		timer.Stop()
		if err != nil {
			return
		}
		mutex.Lock()
		stale := config.Revision != latest.Revision
		mutex.Unlock()
		if stale {
			continue
		}
		if header.Unchanged {
			if after == "" || header.Sequence != after {
				return
			}
			continue
		}
		header.Revision = config.Revision
		metadata, _ := json.Marshal(header)
		_ = ws.SetWriteDeadline(time.Now().Add(6 * time.Second))
		writer, err := ws.NextWriter(websocket.BinaryMessage)
		if err != nil {
			return
		}
		err = binary.Write(writer, binary.BigEndian, uint32(len(metadata)))
		if err == nil {
			_, err = writer.Write(metadata)
		}
		if err == nil {
			_, err = writer.Write(pixels)
		}
		closeErr := writer.Close()
		if err != nil || closeErr != nil || header.Error != "" {
			return
		}
		after = header.Sequence
		consumed := false
		for !consumed {
			select {
			case <-ctx.Done():
				return
			case ack := <-acknowledged:
				consumed = ack == after
			case <-changed:
				mutex.Lock()
				consumed = config.Revision != latest.Revision
				mutex.Unlock()
			}
		}
	}
}
