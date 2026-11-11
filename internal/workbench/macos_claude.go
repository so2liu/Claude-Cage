package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const macOSWindowSocketCommand = `exec /usr/bin/nc -U "$HOME/Library/Caches/dev.cage.GuestClaudeWindow/window.sock"`

// The native trial has a dedicated standard-user SSH identity. The guest GUI
// agent accepts only Claude operations on its protected per-user Unix socket.
func (s *Server) registerMacOSClaudeRoutes(mux routeRegistrar) {
	mux.HandleFunc("POST /api/macos/claude/window", s.macOSClaudeWindow)
	mux.HandleFunc("GET /api/macos/claude/connection", s.macOSWindowConnection)
	mux.HandleFunc("GET /api/macos/claude/frames", s.macOSWindowFrames)
	mux.HandleFunc("POST /api/macos/chrome/window", s.macOSClaudeWindow)
	mux.HandleFunc("GET /api/macos/chrome/connection", s.macOSWindowConnection)
	mux.HandleFunc("GET /api/macos/chrome/frames", s.macOSWindowFrames)
}

type macOSWindowRequest struct {
	Chrome     bool               `json:"-"`
	Action     string             `json:"action"`
	URL        string             `json:"url,omitempty"`
	Project    string             `json:"project,omitempty"`
	Nonce      string             `json:"nonce,omitempty"`
	Width      int                `json:"width,omitempty"`
	Height     int                `json:"height,omitempty"`
	Scale      float64            `json:"scale,omitempty"`
	Geometry   string             `json:"geometry,omitempty"`
	Window     uint32             `json:"window,omitempty"`
	PID        int32              `json:"pid,omitempty"`
	X          *float64           `json:"x,omitempty"`
	Y          *float64           `json:"y,omitempty"`
	Delta      *float64           `json:"delta,omitempty"`
	DeltaX     *float64           `json:"deltaX,omitempty"`
	Text       string             `json:"text,omitempty"`
	Key        string             `json:"key,omitempty"`
	Code       string             `json:"code,omitempty"`
	Down       *bool              `json:"down,omitempty"`
	Phase      string             `json:"phase,omitempty"`
	Button     *int               `json:"button,omitempty"`
	Buttons    int                `json:"buttons,omitempty"`
	Clicks     int                `json:"clicks,omitempty"`
	Shift      bool               `json:"shift,omitempty"`
	Cmd        bool               `json:"command,omitempty"`
	Control    bool               `json:"control,omitempty"`
	Alt        bool               `json:"alt,omitempty"`
	Mode       string             `json:"mode,omitempty"`
	Context    string             `json:"context,omitempty"`
	Session    string             `json:"session,omitempty"`
	After      string             `json:"after,omitempty"`
	KeepAlive  bool               `json:"keepAlive,omitempty"`
	Refresh    bool               `json:"refresh,omitempty"`
	Wait       bool               `json:"wait,omitempty"`
	Binary     bool               `json:"binary,omitempty"`
	ID         string             `json:"id,omitempty"`
	IDs        []string           `json:"ids,omitempty"`
	Kind       string             `json:"kind,omitempty"`
	Transfer   string             `json:"transfer,omitempty"`
	Sequence   int                `json:"sequence"`
	Path       string             `json:"path,omitempty"`
	Error      string             `json:"error,omitempty"`
	Files      []string           `json:"files,omitempty"`
	Phone      *phoneSurfaceInput `json:"phone,omitempty"`
	PhoneToken string             `json:"phone_token,omitempty"`
}

func (s *Server) macOSClaudeWindow(w http.ResponseWriter, r *http.Request) {
	if s.macosGuest == nil {
		http.NotFound(w, r)
		return
	}
	var input macOSWindowRequest
	if !readJSON(w, r, &input) {
		return
	}
	if !validMacOSWindowRequest(input) {
		http.Error(w, "客机窗口操作或尺寸无效", http.StatusBadRequest)
		return
	}
	input.Chrome = strings.HasPrefix(r.URL.Path, "/api/macos/chrome/")
	s.macOSWindowRPC(w, r, macOSWindowClientRequest(input, false))
}

var (
	errMacWindowInput   = errors.New("单次窗口输入过长")
	errMacWindowConnect = errors.New("无法连接虚拟 Mac")
)

func (s *Server) macOSWindowRPC(w http.ResponseWriter, r *http.Request, input macOSWindowRequest) {
	var reply []byte
	var err error
	if input.Action == "status" && !input.Chrome {
		reply, err = s.desktopStatus.read(r.Context(), s.lifetimeContext(), func(ctx context.Context) ([]byte, error) { return s.macOSWindowCall(ctx, input) })
	} else {
		// Input and capture retain their own live guest ownership validation.
		if input.Action == "launch" {
			s.desktopStatus.invalidate()
			defer s.desktopStatus.invalidate()
		}
		reply, err = s.macOSWindowCall(r.Context(), input)
	}
	if err != nil {
		code := http.StatusServiceUnavailable
		if errors.Is(err, errMacWindowInput) {
			code = http.StatusBadRequest
		}
		if errors.Is(err, errMacWindowConnect) {
			code = http.StatusBadGateway
		}
		http.Error(w, err.Error(), code)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(reply)
}

func (s *Server) macOSWindowCall(parent context.Context, input macOSWindowRequest) ([]byte, error) {
	body, err := json.Marshal(input)
	limit := 8000
	if strings.HasPrefix(input.Action, "phone-") || input.Action == "text" || input.Action == "ime" {
		// An 8 KiB UTF-8 message can expand sixfold when JSON escapes controls.
		limit = 64 << 10
	}
	if err != nil || len(body) > limit {
		return nil, errMacWindowInput
	}

	timeout := 12 * time.Second
	socketWait := "9"
	if input.Action == "launch" || input.Action == "phone-open" {
		// On a cold guest disk macOS verifies the signed Electron bundle
		// before LaunchServices reports success.
		timeout = 55 * time.Second
		socketWait = "50"
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	command := nativeWindowSocketCommand(input.Chrome, socketWait)
	cmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", command)
	cmd.Stdin = bytes.NewReader(append(body, '\n'))
	reader, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		return nil, errMacWindowConnect
	}
	// The agent returns one PNG (base64) or a small JSON status. Do not relay
	// partial replies or log input, frame data, or guest application contents.
	const maxReply = 12 << 20
	reply, readErr := io.ReadAll(io.LimitReader(reader, maxReply+1))
	if readErr != nil || len(reply) > maxReply {
		cancel()
	}
	waitErr := cmd.Wait()
	if readErr != nil || waitErr != nil || len(reply) > maxReply || !json.Valid(reply) {
		return nil, errors.New("请先登录虚拟 Mac 的普通账户，并完成 Apple 设置助理。")
	}
	return reply, nil
}

// Targets are selected by registered routes, never by a client-provided path.
func nativeWindowSocketCommand(chrome bool, wait string) string {
	label := "dev.cage.GuestClaudeWindow"
	if chrome {
		label = "dev.cage.GuestChromeWindow"
	}
	return `exec /usr/bin/nc -w ` + wait + ` -U "$HOME/Library/Caches/` + label + `/window.sock"`
}
