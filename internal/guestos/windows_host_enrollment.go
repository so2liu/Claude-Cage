package guestos

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/google/uuid"
)

// WindowsHostEnrollmentPipeError exposes only a fixed operation and an OS code.
// The underlying PathError can contain the private pipe name.
type WindowsHostEnrollmentPipeError struct {
	Operation string
	Win32     syscall.Errno
}

func (err *WindowsHostEnrollmentPipeError) Error() string {
	return fmt.Sprintf("enrollment_pipe_%s_failed (win32=%d)", err.Operation, err.Win32)
}

func enrollmentPipeError(operation string, err error) error {
	var code syscall.Errno
	errors.As(err, &code)
	return &WindowsHostEnrollmentPipeError{Operation: operation, Win32: code}
}

// Options are captured by the original, unprivileged setup process. A pipe
// client cannot replace these paths, identities, or installation choices.
type WindowsHostEnrollmentOptions struct {
	VMID           uuid.UUID
	InstallationID uuid.UUID
	HostSID        string
	Profile        string
	State          string
	Pipe           string
	ParentPID      uint32
}

type windowsHostEnrollmentRequest struct {
	Version       int       `json:"version"`
	Challenge     string    `json:"challenge"`
	Action        string    `json:"action"`
	VMID          uuid.UUID `json:"vm_id"`
	GUIUserSID    string    `json:"gui_user_sid,omitempty"`
	PID           uint32    `json:"pid,omitempty"`
	ProcessHandle uint64    `json:"process_handle,omitempty"`
}

type windowsHostEnrollmentReply struct {
	Version    int    `json:"version"`
	OK         bool   `json:"ok"`
	Challenge  string `json:"challenge,omitempty"`
	Authorized bool   `json:"authorized,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (request windowsHostEnrollmentRequest) validate(vm uuid.UUID, owner bool, guestSID string) error {
	if request.Version != 1 || request.VMID != vm {
		return errors.New("enrollment_request_invalid")
	}
	if owner {
		switch request.Action {
		case "ready", "stop":
			if request.PID != 0 || request.ProcessHandle != 0 || request.GUIUserSID != "" {
				return errors.New("enrollment_request_invalid")
			}
		case "allow-admin":
			if request.PID == 0 || request.ProcessHandle == 0 || request.GUIUserSID != "" {
				return errors.New("enrollment_request_invalid")
			}
		default:
			return errors.New("enrollment_action_invalid")
		}
	} else {
		if request.PID != 0 || request.ProcessHandle != 0 || !windowsGUISID.MatchString(request.GUIUserSID) || guestSID != "" && request.GUIUserSID != guestSID {
			return errors.New("enrollment_request_invalid")
		}
		switch request.Action {
		case "deliver-gui", "deliver-packet":
		case "finalize":
			if guestSID == "" {
				return errors.New("enrollment_request_invalid")
			}
		default:
			return errors.New("enrollment_action_invalid")
		}
	}
	return nil
}
