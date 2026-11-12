package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSessionArchiveContentionInBrowser(t *testing.T) {
	if os.Getenv("CAGE_TEST_ARCHIVE_BROWSER") == "" {
		t.Skip("set CAGE_TEST_ARCHIVE_BROWSER after building the UI to test with real archive handlers")
	}
	s, id, project := metadataFixture(t)
	record, _ := s.mobileCLI.record(s.profile, id)
	record.Title = "软件宣传视频制作"
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		t.Fatal(err)
	}
	s.op.Lock()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(s.op.Unlock) }
	defer release()
	var archives atomic.Int32
	handler := s.handler()
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fixture/release-operation" {
			release()
			jsonReply(w, map[string]bool{"ok": true})
			return
		}
		if r.URL.Path == "/api/cli/sessions/"+id+"/archive" {
			archives.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer host.Close()
	// Release a blocked request before httptest waits for its handler at cleanup.
	defer release()
	s.origin = host.URL
	input, _ := json.Marshal(map[string]string{
		"origin": host.URL, "cookie": s.cookieName() + "=" + s.session,
		"id": id, "workspace": project, "title": record.Title,
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	driver := exec.CommandContext(ctx, "node", "../../ui/scripts/archive-contention-smoke.mjs")
	driver.Env = append(os.Environ(), "CAGE_TEST_BROWSER="+os.Getenv("CAGE_TEST_ARCHIVE_BROWSER"))
	driver.Stdin = strings.NewReader(string(input))
	if output, err := driver.CombinedOutput(); err != nil {
		t.Fatalf("browser: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
	if archives.Load() != 1 {
		t.Fatalf("archive submissions: %d", archives.Load())
	}
	s.mobileCLI = mobileCLIStore{}
	restored, err := s.mobileCLI.record(s.profile, id)
	if err != nil || !restored.Archived || restored.Deleted {
		t.Fatal("UI archive was not durable", restored, err)
	}
}

func TestSessionArchiveWaitsForEnvironmentOperation(t *testing.T) {
	for _, route := range []string{"/api/cli/sessions/", "/api/mobile/cli/sessions/"} {
		t.Run(route, func(t *testing.T) {
			s, id, project := metadataFixture(t)
			// Network monitoring holds this lock even when the UI reports no job.
			s.op.Lock()
			locked := true
			defer func() {
				if locked {
					s.op.Unlock()
				}
			}()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- request(s, "POST", route+id+"/archive", s.origin, `{}`, true) }()
			select {
			case response := <-done:
				t.Fatalf("archive rejected during background operation: %d %s", response.Code, response.Body.String())
			case <-time.After(100 * time.Millisecond):
			}
			s.op.Unlock()
			locked = false
			select {
			case response := <-done:
				if response.Code != http.StatusOK {
					t.Fatal(response.Code, response.Body.String())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("archive did not finish after background operation released the lock")
			}
			if len(s.terminals.snapshot()) != 0 {
				t.Fatal("archive left its terminal visible")
			}
			s.mobileCLI = mobileCLIStore{}
			active, err := s.mobileCLI.summaries(s.profile, map[string]bool{project: true}, false)
			if err != nil || len(active) != 0 {
				t.Fatal("archived session remained active after reload", active, err)
			}
			archived, err := s.mobileCLI.summaries(s.profile, map[string]bool{project: true}, true)
			if err != nil || len(archived) != 1 || archived[0].ID != id {
				t.Fatal("archived history was not retained", archived, err)
			}
		})
	}
}

func TestSessionArchiveDoesNotApplyCancelledRequest(t *testing.T) {
	s, id, _ := metadataFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.op.Lock()
	done := make(chan error, 1)
	go func() { done <- s.setSessionArchived(id, true, ctx) }()
	select {
	case err := <-done:
		s.op.Unlock()
		t.Fatalf("archive did not wait for the operation: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	s.op.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled archive: %v", err)
	}
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil || record.Archived || len(s.terminals.snapshot()) != 1 {
		t.Fatal("cancelled archive changed the session", record, err)
	}
}

func metadataFixture(t *testing.T) (*Server, string, string) {
	t.Helper()
	s := testServer(t)
	project, _ := filepath.EvalSymlinks(t.TempDir())
	s.preferences.Workspace = project
	id := uuid.NewString()
	if _, err := s.mobileCLI.create(s.profile, project, id); err != nil {
		t.Fatal(err)
	}
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	record.Terminal, record.TerminalID = true, "fixture-terminal"
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		t.Fatal(err)
	}
	terminal := newTerminalSession(nil)
	terminal.id, terminal.sharedID, terminal.workspace, terminal.kind = record.TerminalID, id, project, "claude"
	terminal.closed = true
	close(terminal.done)
	s.terminals.sessions = map[string]*terminalSession{terminal.id: terminal}
	return s, id, project
}

func TestSessionMetadataBidirectionalAndDurable(t *testing.T) {
	s, id, project := metadataFixture(t)
	path := "/api/mobile/cli/sessions/" + id
	if w := request(s, "POST", path+"/metadata", s.origin, `{"title":"手机命名","pinned":true}`, true); w.Code != 200 {
		t.Fatalf("mobile metadata: %d %s", w.Code, w.Body.String())
	}
	w := request(s, "GET", "/api/terminals", "", "", true)
	var terminals []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &terminals); err != nil {
		t.Fatal(err)
	}
	if len(terminals) != 1 || terminals[0]["title"] != "手机命名" || terminals[0]["pinned"] != true {
		t.Fatalf("desktop did not reflect mobile: %s", w.Body.String())
	}
	if w := request(s, "POST", "/api/terminals/fixture-terminal/metadata", s.origin, `{"title":"电脑命名","pinned":false}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Guest discovery may replace the CLI-generated title; the user's name must survive.
	record, _ := s.mobileCLI.record(s.profile, id)
	record.Title = "CLI 自动摘要"
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		t.Fatal(err)
	}
	s.mobileCLI = mobileCLIStore{}
	summaries, err := s.mobileCLI.summaries(s.profile, map[string]bool{project: true}, false)
	if err != nil || len(summaries) != 1 || summaries[0].Title != "电脑命名" || summaries[0].Pinned {
		t.Fatalf("mobile metadata after restart: %#v %v", summaries, err)
	}
	for _, body := range []string{`{"title":" "}`, `{"title":"` + strings.Repeat("名", 101) + `"}`, `{}`} {
		if w := request(s, "POST", path+"/metadata", s.origin, body, true); w.Code == 200 {
			t.Fatal("invalid metadata accepted", body)
		}
	}
	if w := request(s, "POST", path+"/metadata", "https://other.invalid", `{"pinned":true}`, true); w.Code != http.StatusForbidden {
		t.Fatal("cross-origin write", w.Code)
	}
	s.account.lock()
	for _, action := range []string{"metadata", "delete"} {
		if w := request(s, "POST", path+"/"+action, s.origin, `{}`, true); w.Code != http.StatusLocked {
			t.Fatal("locked mutation", w.Code)
		}
	}
}

func TestSessionArchiveClosesBothListsAndRestores(t *testing.T) {
	for _, fromPhone := range []bool{false, true} {
		t.Run(map[bool]string{true: "phone", false: "desktop"}[fromPhone], func(t *testing.T) {
			s, id, project := metadataFixture(t)
			method, path, body := "DELETE", "/api/terminals/fixture-terminal", ""
			if fromPhone {
				method, path, body = "POST", "/api/mobile/cli/sessions/"+id+"/delete", `{}`
			}
			if w := request(s, method, path, s.origin, body, true); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			if len(s.terminals.snapshot()) != 0 {
				t.Fatal("desktop terminal survived removal")
			}
			s.mobileCLI = mobileCLIStore{}
			list, err := s.mobileCLI.summaries(s.profile, map[string]bool{project: true}, false)
			if err != nil || len(list) != 0 {
				t.Fatal("deleted entry reappeared after restart", list, err)
			}
			record, err := s.mobileCLI.record(s.profile, id)
			if err != nil || !record.Archived || record.Deleted || record.CLIID == "" {
				t.Fatal("history identity tombstone lost", err)
			}
			if w := request(s, "GET", "/api/mobile/cli/sessions/"+id, "", "", true); w.Code != 410 {
				t.Fatal("removed entry accessible", w.Code)
			}
			if _, err := s.mobileCLI.create(s.profile, project, id); err == nil {
				t.Fatal("stale create resurrected deleted entry")
			}
			archived, err := s.mobileCLI.summaries(s.profile, map[string]bool{project: true}, true)
			if err != nil || len(archived) != 1 || archived[0].ID != id {
				t.Fatal("archive lost its history entry", archived, err)
			}
			if w := request(s, "POST", "/api/cli/sessions/"+id+"/restore", s.origin, `{}`, true); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			for _, route := range []string{"/api/cli/sessions", "/api/mobile/cli/sessions"} {
				w := request(s, "GET", route, "", "", true)
				if w.Code != 200 || !strings.Contains(w.Body.String(), id) {
					t.Fatal("restored history missing from a client", route, w.Body.String())
				}
			}
		})
	}
}

func TestLegacyDeletedSessionMigratesToArchive(t *testing.T) {
	s, id, project := metadataFixture(t)
	record, _ := s.mobileCLI.record(s.profile, id)
	record.Deleted = true
	record.Turns = []mobileTurn{{ID: uuid.NewString(), Prompt: "retained prompt", Text: "retained reply", Status: "completed"}}
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		t.Fatal(err)
	}
	s.mobileCLI = mobileCLIStore{}
	list, err := s.mobileCLI.summaries(s.profile, map[string]bool{project: true}, true)
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	migrated, err := s.mobileCLI.record(s.profile, id)
	if err != nil || migrated.Deleted || !migrated.Archived || migrated.Turns[0].Text != "retained reply" {
		t.Fatal("legacy history lost", err)
	}
	data, _ := os.ReadFile(mobileRecordPath(s.profile, id))
	var persisted mobileRecord
	if json.Unmarshal(data, &persisted) != nil || !persisted.Archived || persisted.Deleted {
		t.Fatal("archive migration not persisted")
	}
}

func TestPurgeFailureKeepsDurablePendingDeletion(t *testing.T) {
	s, id, project := metadataFixture(t)
	path := "/api/cli/sessions/" + id
	if w := request(s, "POST", path+"/purge", s.origin, `{}`, true); w.Code != 400 {
		t.Fatal("purge skipped confirmation", w.Code)
	}
	if w := request(s, "POST", path+"/purge", s.origin, `{"confirm":true}`, true); w.Code == 200 {
		t.Fatal("unarchived purge accepted")
	}
	if w := request(s, "POST", path+"/archive", s.origin, `{}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(s, "POST", path+"/purge", s.origin, `{"confirm":true}`, true); w.Code == 200 {
		t.Fatal("offline guest claimed deletion")
	}
	s.mobileCLI = mobileCLIStore{}
	list, err := s.mobileCLI.summaries(s.profile, map[string]bool{project: true}, true)
	if err != nil || len(list) != 1 || !list[0].Deleting {
		t.Fatal("deletion intent was lost", list, err)
	}
	if w := request(s, "POST", path+"/restore", s.origin, `{}`, true); w.Code == 200 {
		t.Fatal("partly deleted history restored")
	}
}
