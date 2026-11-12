package workbench

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func TestLinuxTimezoneRejectsStaleSystemFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux probe uses POSIX TZ semantics; native Windows settings are tested separately")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	directory := t.TempDir()
	localtime := filepath.Join(directory, "localtime")
	timezone := filepath.Join(directory, "timezone")
	if err := os.WriteFile(timezone, []byte("Asia/Shanghai\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Redirect only file paths to isolated fixtures. Keep the shipped probe's
	// time calculations, OS readlink behavior and Go decoder unchanged.
	script := strings.ReplaceAll(guestSafetyProbe, "/etc/localtime", filepath.ToSlash(localtime))
	script = strings.ReplaceAll(script, "/etc/timezone", filepath.ToSlash(timezone))
	zoneDirectory := filepath.Join(directory, "zoneinfo", "Asia")
	if err := os.MkdirAll(zoneDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	shanghai := fixedOffsetTZif(28800)
	if err := os.WriteFile(filepath.Join(zoneDirectory, "Shanghai"), shanghai, 0600); err != nil {
		t.Fatal(err)
	}
	zonePath, _ := json.Marshal([]string{filepath.Dir(zoneDirectory)})
	script = "import zoneinfo; zoneinfo.reset_tzpath(" + string(zonePath) + ")\n" + script
	input, _ := json.Marshal(map[string]any{"stage": "environment", "timezone": "Asia/Shanghai"})
	config := cage.DefaultConfig()
	config.Timezone = "Asia/Shanghai"
	config.Locale = "C"
	for _, tc := range []struct {
		name string
		data []byte
		want bool
	}{
		{"stale UTC file", fixedOffsetTZif(0), false},
		{"matching regular file", shanghai, true},
		{"unreadable zone data", []byte("invalid tzif"), false},
		{"missing file", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.data == nil {
				if err := os.Remove(localtime); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(localtime, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(python, "-I", "-c", script, base64.StdEncoding.EncodeToString(input))
			command.Env = append(os.Environ(), "TZ=Asia/Shanghai", "LANG=C", "LC_ALL=C")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("probe failed: %v %s", err, output)
			}
			var observation guestObservation
			if err := json.Unmarshal(output, &observation); err != nil {
				t.Fatal(err)
			}
			if got := guestSettingsMatch(observation, config); got != tc.want {
				t.Fatalf("settings match=%t want=%t: %s", got, tc.want, output)
			}
		})
	}
}

// A single fixed-offset TZif v1 record keeps fixtures independent of host tzdata.
func fixedOffsetTZif(offset int32) []byte {
	data := make([]byte, 54)
	copy(data, "TZif")
	binary.BigEndian.PutUint32(data[36:40], 1) // one local-time type
	binary.BigEndian.PutUint32(data[40:44], 4) // abbreviation including NUL
	binary.BigEndian.PutUint32(data[44:48], uint32(offset))
	copy(data[50:], "FIX\x00")
	return data
}
