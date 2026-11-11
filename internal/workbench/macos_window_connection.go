package workbench

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"time"
)

func validMacOSWindowRequest(input macOSWindowRequest) bool {
	switch input.Action {
	case "open-url", "status", "launch", "stop", "request-screen", "request-input", "frame", "text", "key", "click", "scroll", "pointer", "ime", "input-reset":
	default:
		return false
	}
	if input.Action == "open-url" {
		u, err := url.Parse(input.URL)
		if err != nil || len(input.URL) > 4096 || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return false
		}
	}
	if len(input.After) > 128 || len(input.Geometry) > 128 || input.Width < 0 || input.Width > 2560 || input.Height < 0 || input.Height > 1600 || input.Scale < 0 || input.Scale > 2 {
		return false
	}
	if len(input.Text) > 8192 || len(input.Key) > 64 || len(input.Code) > 64 {
		return false
	}
	if input.Action == "pointer" || input.Action == "ime" || (input.Action == "key" && input.Code != "") {
		if input.PID <= 0 || input.Window == 0 || input.Geometry == "" {
			return false
		}
	}
	if input.Action == "pointer" {
		if input.Phase != "down" && input.Phase != "move" && input.Phase != "up" {
			return false
		}
		if input.Buttons < 0 || input.Buttons > 7 || input.X == nil || input.Y == nil ||
			math.IsNaN(*input.X) || math.IsNaN(*input.Y) || *input.X < 0 || *input.X >= 1 || *input.Y < 0 || *input.Y >= 1 {
			return false
		}
		if input.Phase != "move" && (input.Button == nil || *input.Button < 0 || *input.Button > 2 || input.Clicks < 1 || input.Clicks > 3) {
			return false
		}
	}
	if input.Action == "key" && input.Code != "" && input.Down == nil {
		return false
	}
	if input.Action == "ime" {
		if input.Mode != "status" && input.Mode != "begin" && input.Mode != "update" && input.Mode != "commit" && input.Mode != "cancel" {
			return false
		}
		if input.Mode != "status" && (!validInputToken(input.Session) || input.Sequence < 0) {
			return false
		}
		if input.Mode == "begin" && !validInputToken(input.Context) {
			return false
		}
	}
	return (input.Width == 0 && input.Height == 0 && input.Scale == 0) ||
		(input.Action == "frame" && input.Width >= 320 && input.Height >= 240 && input.Scale >= 1)
}

func macOSWindowClientRequest(input macOSWindowRequest, keepAlive bool) macOSWindowRequest {
	input.Project, input.Nonce = "", ""
	input.Phone, input.PhoneToken = nil, ""
	input.Transfer, input.Path, input.Error = "", "", ""
	input.IDs = nil
	input.Wait, input.Binary, input.KeepAlive = false, false, keepAlive
	if input.Action != "ime" {
		input.Sequence = 0
	}
	return input
}

// One authenticated WebSocket owns one ordinary-user SSH channel. Requests are
// ordered on that channel, including input and capture geometry changes.
func (s *Server) macOSWindowConnection(w http.ResponseWriter, r *http.Request) {
	if s.macosGuest == nil {
		http.NotFound(w, r)
		return
	}
	ws, err := s.upgrade(w, r)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(64 << 10)
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
	if err = cmd.Start(); err != nil {
		return
	}
	defer func() { cancel(); input.Close(); output.Close(); _ = cmd.Wait() }()
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64<<10), 12<<20)
	for {
		var message struct {
			ID      uint64             `json:"id"`
			Request macOSWindowRequest `json:"request"`
		}
		if err = ws.ReadJSON(&message); err != nil {
			return
		}
		if !validMacOSWindowRequest(message.Request) {
			return
		}
		message.Request = macOSWindowClientRequest(message.Request, true)
		body, err := json.Marshal(message.Request)
		if err != nil || len(body) > 64<<10 {
			return
		}
		limit := 12 * time.Second
		if message.Request.Action == "launch" {
			limit = 55 * time.Second
		}
		timer := time.AfterFunc(limit, func() { cancel(); output.Close(); ws.Close() })
		_, err = input.Write(append(body, '\n'))
		if err != nil || !scanner.Scan() {
			timer.Stop()
			return
		}
		timer.Stop()
		data := append(json.RawMessage(nil), scanner.Bytes()...)
		if !json.Valid(data) {
			return
		}
		_ = ws.SetWriteDeadline(time.Now().Add(12 * time.Second))
		if ws.WriteJSON(struct {
			ID     uint64          `json:"id"`
			Result json.RawMessage `json:"result"`
		}{message.ID, data}) != nil {
			return
		}
	}
}
