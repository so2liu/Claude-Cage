//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/guestos"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	flags := flag.NewFlagSet("Cage.HyperVManager", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	binding := flags.String("binding", "", "")
	vm := flags.String("vm-id", "", "")
	hostSID := flags.String("host-sid", "", "")
	profile := flags.String("profile", "", "")
	state := flags.String("state", "", "")
	installation := flags.String("installation-id", "", "")
	pipe := flags.String("pipe", "", "")
	parentPID := flags.Uint("parent-pid", 0, "")
	if flags.Parse(os.Args[2:]) != nil || flags.NArg() != 0 {
		os.Exit(2)
	}
	var err error
	enrollmentFlags := *profile != "" || *state != "" || *installation != "" || *pipe != "" || *parentPID != 0
	switch os.Args[1] {
	case "service":
		if *binding == "" || *vm != "" || *hostSID != "" || enrollmentFlags {
			os.Exit(2)
		}
		err = guestos.RunWindowsHostManagerService(*binding)
	case "prepare", "binding":
		id, parseErr := uuid.Parse(*vm)
		if parseErr != nil || id == uuid.Nil || *hostSID == "" || *binding != "" || enrollmentFlags {
			os.Exit(2)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var record guestos.WindowsHostManagerBinding
		if os.Args[1] == "binding" {
			record, err = guestos.LoadWindowsHostManagerBinding(guestos.WindowsHostManagerBindingPath(id))
			if err == nil && (record.VMID != id || record.HostSID != *hostSID) {
				err = fmt.Errorf("binding_mismatch")
			}
		} else {
			record, err = guestos.PrepareWindowsHostManagerBinding(ctx, id, *hostSID)
		}
		if err == nil {
			err = json.NewEncoder(os.Stdout).Encode(record)
		}
	case "user-enrollment":
		id, idErr := uuid.Parse(*vm)
		install, installErr := uuid.Parse(*installation)
		if idErr != nil || installErr != nil || id == uuid.Nil || install == uuid.Nil || *hostSID == "" || *profile == "" || *state == "" || *pipe == "" || *parentPID == 0 || *parentPID > 0xffffffff || *binding != "" {
			os.Exit(2)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		err = guestos.RunWindowsHostEnrollment(ctx, guestos.WindowsHostEnrollmentOptions{VMID: id, InstallationID: install, HostSID: *hostSID, Profile: *profile, State: *state, Pipe: *pipe, ParentPID: uint32(*parentPID)})
	default:
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Windows host manager action did not complete.")
		var pipeError *guestos.WindowsHostEnrollmentPipeError
		if errors.As(err, &pipeError) {
			fmt.Fprintln(os.Stderr, pipeError)
		}
		os.Exit(1)
	}
}
