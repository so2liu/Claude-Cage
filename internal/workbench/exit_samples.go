package workbench

import (
	"fmt"
	"strings"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/usererror"
)

// All guest platforms return the same observation contract. Do not infer a
// transport failure from an unavailable sample or expose raw guest output.
func incompleteExitSamples(hostIP string, hostErr error, terminal networkObservation, terminalErr error, browser networkObservation, browserErr error) string {
	var missing []string
	for _, sample := range []struct {
		name        string
		observation networkObservation
		err         error
	}{
		{"宿主", networkObservation{Status: 200, IP: hostIP}, hostErr},
		{"虚拟机终端", terminal, terminalErr},
		{"浏览器", browser, browserErr},
	} {
		var reason string
		switch {
		case sample.err != nil:
			reason = diagnostics.Redact(usererror.Display("读取采样", sample.err, "请重新检测。").Error())
		case sample.observation.Status == 0:
			reason = "没有收到检测通道的响应"
		case sample.observation.Status != 200:
			reason = fmt.Sprintf("检测通道返回 HTTP %d，未取得出口地址", sample.observation.Status)
		case sample.observation.IP == "":
			reason = "检测通道没有返回出口地址"
		}
		if reason != "" {
			missing = append(missing, sample.name+"："+reason)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "出口采样不完整，无法确认配置范围（" + strings.Join(missing, "；") + "）"
}
