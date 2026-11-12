package workbench

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"
)

type sessionMetadataInput struct {
	Title  *string `json:"title"`
	Pinned *bool   `json:"pinned"`
}

func (s *Server) updateSessionMetadata(id string, input sessionMetadataInput, requestContext context.Context) (mobileSession, error) {
	if !s.op.TryLock() {
		return mobileSession{}, errTerminalBusy
	}
	defer s.op.Unlock()
	if input.Title == nil && input.Pinned == nil {
		return mobileSession{}, errors.New("请选择要修改的会话信息")
	}
	if input.Title != nil {
		title := strings.TrimSpace(*input.Title)
		if title == "" || !utf8.ValidString(title) || utf8.RuneCountInString(title) > 100 {
			return mobileSession{}, errors.New("会话名称需要 1 至 100 个字符")
		}
		input.Title = &title
	}
	s.conversationMu.Lock()
	defer s.conversationMu.Unlock()
	if requestContext != nil {
		s.account.mu.Lock()
		defer s.account.mu.Unlock()
		if !s.mobileAuthorized(requestContext) {
			return mobileSession{}, errMobileLocked
		}
	}
	m := &s.mobileCLI
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.record(s.profile, id)
	if err != nil {
		return mobileSession{}, err
	}
	if record.Deleted {
		return mobileSession{}, os.ErrNotExist
	}
	previous := *record
	if input.Title != nil {
		record.CustomTitle = *input.Title
	}
	if input.Pinned != nil {
		record.Pinned = *input.Pinned
	}
	record.MetadataSet = true
	record.changed()
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		*record = previous
		return mobileSession{}, err
	}
	if m.catalog != nil {
		m.catalog[id] = record.summary()
	}
	return record.snapshot(), nil
}

// Archive state belongs to the shared catalog and survives terminal exits.
func (s *Server) setSessionArchived(id string, archived bool, requestContext context.Context) error {
	if requestContext != nil && requestContext.Err() != nil {
		return requestContext.Err()
	}
	// Serialize with terminal creation and teardown, but let an explicit archive
	// wait for background network probes instead of rejecting the user's click.
	s.op.Lock()
	defer s.op.Unlock()
	if requestContext != nil && requestContext.Err() != nil {
		return requestContext.Err()
	}
	s.conversationMu.Lock()
	defer s.conversationMu.Unlock()
	m := &s.mobileCLI
	if requestContext != nil {
		s.account.mu.Lock()
		if !s.mobileAuthorized(requestContext) {
			s.account.mu.Unlock()
			return errMobileLocked
		}
	}
	m.mu.Lock()
	record, err := m.record(s.profile, id)
	if err != nil || record.Deleted {
		m.mu.Unlock()
		if requestContext != nil {
			s.account.mu.Unlock()
		}
		if err != nil {
			return err
		}
		return os.ErrNotExist
	}
	previous := *record
	if record.Deleting && !archived {
		m.mu.Unlock()
		if requestContext != nil {
			s.account.mu.Unlock()
		}
		return errors.New("永久删除尚未完成，请先重试删除")
	}
	record.Archived = archived
	record.changed()
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		*record = previous
		m.mu.Unlock()
		if requestContext != nil {
			s.account.mu.Unlock()
		}
		return err
	}
	if m.catalog != nil {
		m.catalog[id] = record.summary()
	}
	var done chan struct{}
	if run := m.active[id]; archived && run != nil && run.cancel != nil {
		run.cancel()
		done = run.done
	}
	terminalID := record.TerminalID
	m.mu.Unlock()
	if requestContext != nil {
		s.account.mu.Unlock()
	}
	if terminal := s.terminals.get(terminalID); archived && terminal != nil {
		if terminal.summary()["cli_session_id"] == id {
			s.terminals.remove(terminalID)
		}
	}
	if done != nil {
		<-done
	}
	return nil
}

func (s *Server) mobileSessionMetadata(w http.ResponseWriter, r *http.Request) {
	view, _, ok := s.authorizedMobileSessionRecord(w, r)
	if !ok {
		return
	}
	var input sessionMetadataInput
	if !readJSON(w, r, &input) {
		return
	}
	updated, err := s.updateSessionMetadata(view.ID, input, r.Context())
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, updated)
}

func (s *Server) mobileArchiveSession(w http.ResponseWriter, r *http.Request) {
	s.changeSessionArchive(w, r, true)
}

func (s *Server) mobileRestoreSession(w http.ResponseWriter, r *http.Request) {
	s.changeSessionArchive(w, r, false)
}

func (s *Server) changeSessionArchive(w http.ResponseWriter, r *http.Request, archived bool) {
	view, _, ok := s.authorizedMobileSessionRecord(w, r)
	if !ok {
		return
	}
	var input struct{}
	if !readJSON(w, r, &input) {
		return
	}
	if err := s.setSessionArchived(view.ID, archived, r.Context()); err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, map[string]bool{"ok": true})
}

func (s *Server) mobilePurgeSession(w http.ResponseWriter, r *http.Request) {
	// A separate endpoint keeps old clients' "delete" requests reversible.
	var input struct {
		Confirm bool `json:"confirm"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !input.Confirm {
		http.Error(w, "需要确认永久删除该会话及 Claude Code 原始历史", 400)
		return
	}
	if err := s.purgeSession(r.Context(), r.PathValue("id")); err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, map[string]bool{"ok": true})
}

func (s *Server) purgeSession(ctx context.Context, id string) error {
	if !s.op.TryLock() {
		return errTerminalBusy
	}
	defer s.op.Unlock()
	s.conversationMu.Lock()
	defer s.conversationMu.Unlock()
	s.account.mu.Lock()
	authorized := s.mobileAuthorized(ctx)
	s.account.mu.Unlock()
	if !authorized {
		return errMobileLocked
	}
	m := &s.mobileCLI
	m.mu.Lock()
	record, err := m.record(s.profile, id)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if _, err := s.projectWorkspace(record.Workspace); err != nil {
		return err
	}
	if record.Purged {
		return nil
	}
	if s.windowsGuest != nil && !record.Terminal && (record.Resume || len(record.Turns) > 0) {
		return errWindowsLegacyMobile
	}
	if !record.Archived {
		return errors.New("请先归档会话，再从已归档会话中永久删除")
	}
	// Once deletion is admitted, a retry must finish it before restoration can
	// reopen a transcript that may already be partly removed from the guest.
	record.Deleting = true
	record.changed()
	m.mu.Lock()
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		m.mu.Unlock()
		return err
	}
	if m.catalog != nil {
		m.catalog[id] = record.summary()
	}
	m.mu.Unlock()
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	executions := append([]claudeExecution{}, record.Executions...)
	executions = append(executions, claudeExecution{record.CLIID, record.Account, record.GuestConfig})
	for _, execution := range executions {
		input := map[string]any{
			"cli_id": execution.CLIID, "config": execution.Config,
			"account": execution.Account.GuestID, "workspace": guestPath(record.Workspace),
			"expect_history": execution.CLIID != record.CLIID || record.Resume,
		}
		if err := s.windowsConversationInput(record, input); err != nil {
			return err
		}
		if err := s.runCLIChannel(ctx, "purge", input, &result); err != nil {
			return err
		}
		if !result.OK {
			return errors.New(result.Error)
		}
	}
	// Keep only the identity needed to make retries and discovery idempotent.
	// If this write fails, the archived record can safely retry guest cleanup.
	tombstone := &mobileRecord{mobileSession: mobileSession{mobileSummary: mobileSummary{
		ID: record.ID, Workspace: record.Workspace, Deleted: true, Purged: true,
		Revision: record.Revision, Generation: record.Generation,
	}, Turns: []mobileTurn{}}, CLIID: record.CLIID}
	for _, execution := range record.Executions {
		tombstone.Executions = append(tombstone.Executions, claudeExecution{CLIID: execution.CLIID})
	}
	tombstone.changed()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := writePrivate(mobileRecordPath(s.profile, id), tombstone); err != nil {
		return err
	}
	delete(m.active, id)
	if m.catalog != nil {
		m.catalog[id] = tombstone.summary()
	}
	return nil
}

func (s *Server) terminalSessionMetadata(w http.ResponseWriter, r *http.Request) {
	terminal := s.terminals.get(r.PathValue("id"))
	if terminal == nil {
		http.NotFound(w, r)
		return
	}
	id, _ := terminal.summary()["cli_session_id"].(string)
	if id == "" {
		http.Error(w, "此终端没有共享 Claude Code 会话", 409)
		return
	}
	var input sessionMetadataInput
	if !readJSON(w, r, &input) {
		return
	}
	view, err := s.updateSessionMetadata(id, input, nil)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, view)
}

func (s *Server) terminalSummaries() []map[string]any {
	list := s.terminals.snapshot()
	m := &s.mobileCLI
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range list {
		id, _ := item["cli_session_id"].(string)
		if id == "" {
			continue
		}
		record, err := m.record(s.profile, id)
		if err != nil {
			continue
		}
		item["custom_title"], item["pinned"], item["metadata_set"] = record.CustomTitle, record.Pinned, record.MetadataSet
		item["title"] = record.snapshot().Title
		if s.macOSLegacyWorkspace(record) != "" {
			item["workspace_notice"] = macOSLegacyWorkspaceNotice
		}
	}
	return list
}
