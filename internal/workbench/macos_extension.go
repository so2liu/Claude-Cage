package workbench

import (
	"context"
	_ "embed"
	"errors"
	"strings"
	"time"
)

//go:embed macos_extension_probe.pl
var macOSProbeFSKit string

func (s *Server) checkMacOSSetupProjects(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	cmd := s.macOSSSHCommand(ctx, "-T", s.sshTarget(), "--", "exec /usr/bin/perl -")
	cmd.Stdin = strings.NewReader(macOSProbeFSKit)
	err := cmd.Run()
	s.mu.Lock()
	if err != nil {
		s.macSetupProjectsChecked = time.Time{}
	} else {
		s.macSetupProjectsChecked = time.Now()
	}
	s.mu.Unlock()
	if err != nil {
		return errors.New("文件共享权限尚未通过。请点击「文件系统扩展」重新打开授权面板，确认 macFUSE 和 macFUSE (local) 两个开关均已打开，Cage 会自动检查")
	}
	return nil
}
