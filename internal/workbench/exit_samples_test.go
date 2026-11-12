package workbench

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestIncompleteExitSamplesIdentifyRoutesAndRedactPrivateErrors(t *testing.T) {
	good := networkObservation{Status: 200, IP: "8.8.8.8"}
	message := incompleteExitSamples("", context.DeadlineExceeded, good, nil, networkObservation{}, errors.New("浏览器请求失败 https://user:password@private.example/secret token=private-token /Users/alice/project"))
	for _, want := range []string{"宿主", "超时", "浏览器", "出口采样不完整"} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing diagnostic %q: %s", want, message)
		}
	}
	for _, unwanted := range []string{"虚拟机终端", "private.example", "private-token", "alice", "password"} {
		if strings.Contains(message, unwanted) {
			t.Fatalf("successful route or private data included: %s", message)
		}
	}
	if message := incompleteExitSamples("8.8.8.8", nil, good, nil, good, nil); message != "" {
		t.Fatal("complete samples reported missing", message)
	}
	for _, tc := range []struct {
		observation networkObservation
		want        string
	}{
		{networkObservation{}, "没有收到检测通道的响应"},
		{networkObservation{Status: 503}, "HTTP 503"},
		{networkObservation{Status: 200}, "没有返回出口地址"},
	} {
		message := incompleteExitSamples("8.8.8.8", nil, tc.observation, nil, good, nil)
		if !strings.Contains(message, "虚拟机终端：") || !strings.Contains(message, tc.want) || strings.Contains(message, "宿主：") || strings.Contains(message, "浏览器：") {
			t.Fatalf("wrong route or failure category: %s", message)
		}
	}
}
