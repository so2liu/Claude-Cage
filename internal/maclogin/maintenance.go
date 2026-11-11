package maclogin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"time"

	"github.com/google/uuid"
)

type maintenanceWindow struct {
	Version         int        `json:"version"`
	Session         string     `json:"session"`
	User            string     `json:"user"`
	PID             int32      `json:"pid"`
	Window          uint32     `json:"window"`
	Bundle          string     `json:"bundle"`
	Rect            [4]float64 `json:"rect"`
	Display         [4]float64 `json:"display"`
	Error           string     `json:"error"`
	PasswordFocused bool       `json:"password_focused"`
}

func (w maintenanceWindow) valid(user string) bool {
	if w.Version != 1 || w.Error != "" || w.User != user || w.PID <= 0 || w.Window == 0 ||
		(w.Bundle != "com.apple.systempreferences" && w.Bundle != "com.apple.authorizationhost" && w.Bundle != "com.apple.SecurityAgent") {
		return false
	}
	id, err := uuid.Parse(w.Session)
	if err != nil || id == uuid.Nil {
		return false
	}
	for _, value := range append(w.Rect[:], w.Display[:]...) {
		if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > 32768 {
			return false
		}
	}
	return w.Display[2] > 0 && w.Display[3] > 0 && math.Abs(w.Display[2]/w.Display[3]-1.6) < 0.001 && w.Rect[2] >= 80 && w.Rect[3] >= 40 &&
		w.Rect[0] >= w.Display[0] && w.Rect[1] >= w.Display[1] &&
		w.Rect[0]+w.Rect[2] <= w.Display[0]+w.Display[2] && w.Rect[1]+w.Rect[3] <= w.Display[1]+w.Display[3]
}

func (w maintenanceWindow) geometry() string {
	w.PasswordFocused = false
	data, _ := json.Marshal(w)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:16])
}

// Round inward so the image and accepted coordinates remain inside the window.
func (w maintenanceWindow) region() [4]int {
	x := int(math.Ceil((w.Rect[0] - w.Display[0]) / w.Display[2] * 1024))
	y := int(math.Ceil((w.Rect[1] - w.Display[1]) / w.Display[3] * 640))
	right := int(math.Floor((w.Rect[0] + w.Rect[2] - w.Display[0]) / w.Display[2] * 1024))
	bottom := int(math.Floor((w.Rect[1] + w.Rect[3] - w.Display[1]) / w.Display[3] * 640))
	return [4]int{x, y, right - x, bottom - y}
}

func (w maintenanceWindow) contains(x, y int) bool {
	r := w.region()
	return x >= r[0] && y >= r[1] && x < r[0]+r[2] && y < r[1]+r[3]
}

func (selection *passwordSelection) selectMaintenance(window maintenanceWindow, target passwordTarget, user string, x, y int, now time.Time) {
	*selection = passwordSelection{}
	if target.valid(user) && target.SelectionRequired && window.geometry() == target.geometry() && target.contains(x, y) {
		*selection = passwordSelection{Geometry: target.geometry(), X: x, Y: y, Clicked: now}
	}
}

func (selection *passwordSelection) consumeMaintenance(window maintenanceWindow, target passwordTarget, user string, now time.Time) (passwordSelection, bool) {
	selected := *selection
	*selection = passwordSelection{}
	return selected, window.permitsPassword(target, selected, user, now)
}

func (window maintenanceWindow) permitsPassword(target passwordTarget, selection passwordSelection, user string, now time.Time) bool {
	return window.geometry() == target.geometry() && selection.permits(target, user, now)
}
