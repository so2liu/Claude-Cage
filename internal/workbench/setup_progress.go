package workbench

import (
	"strconv"
	"strings"
	"time"
)

type SetupProgress struct {
	Phase          string  `json:"phase"`
	Label          string  `json:"label"`
	Received       *int64  `json:"received,omitempty"`
	Total          int64   `json:"total,omitempty"`
	System         string  `json:"system,omitempty"`
	Operation      string  `json:"operation,omitempty"`
	BytesPerSecond float64 `json:"bytes_per_second,omitempty"`
}

// Only fixed component names from the installer are displayed. Other output
// continues through the existing redacted operation log.
func parseSetupProgress(line string) *SetupProgress {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "CAGE_HOST_RETRY ") {
		fields := strings.Fields(line)
		if len(fields) != 3 || (fields[2] != "2" && fields[2] != "3") {
			return nil
		}
		label := macOSDownloadLabel(fields[1])
		if label == "" {
			return nil
		}
		return &SetupProgress{Phase: "image", Label: label + "（准备第 " + fields[2] + "/3 次尝试）", System: "macos", Operation: "download"}
	}
	if strings.HasPrefix(line, "CAGE_HOST_BYTES ") {
		fields := strings.Fields(line)
		if len(fields) != 4 {
			return nil
		}
		label := macOSDownloadLabel(fields[1])
		received, err := strconv.ParseInt(fields[2], 10, 64)
		total, te := strconv.ParseInt(fields[3], 10, 64)
		if label == "" || err != nil || te != nil || received < 0 || total < 0 || total > 0 && received > total {
			return nil
		}
		return &SetupProgress{Phase: "image", Label: label, System: "macos", Operation: "download", Received: &received, Total: total}
	}
	if strings.HasPrefix(line, "下载 ") {
		if label := macOSDownloadLabel(strings.TrimPrefix(line, "下载 ")); label != "" {
			return &SetupProgress{Phase: "image", Label: label, System: "macos"}
		}
	}
	if strings.HasPrefix(line, "CAGE_GUEST_BYTES ") {
		fields := strings.Fields(line)
		if len(fields) != 5 {
			return nil
		}
		name := guestProgressName(fields[1])
		received, err := strconv.ParseInt(fields[3], 10, 64)
		total, te := strconv.ParseInt(fields[4], 10, 64)
		if name == "" || err != nil || te != nil || received < 0 || total < 0 || (total > 0 && received > total) {
			return nil
		}
		label := "正在下载 " + name + " 系统镜像"
		switch fields[2] {
		case "download":
		case "verify":
			label = "正在校验 " + name + " 系统镜像"
		default:
			return nil
		}
		return &SetupProgress{Phase: "image", Label: label, Received: &received, Total: total, System: fields[1], Operation: fields[2]}
	}
	if strings.HasPrefix(line, "CAGE_GUEST_STAGE ") {
		fields := strings.Fields(line)
		if len(fields) != 3 || guestProgressName(fields[1]) == "" {
			return nil
		}
		p := &SetupProgress{System: fields[1]}
		if fields[1] == "windows-11" {
			p.Operation = fields[2]
		}
		switch fields[2] {
		case "restore":
			p.Phase, p.Label = "packages", "正在安装 "+guestProgressName(fields[1])+" 系统"
		case "account", "oobe", "credentials":
			if fields[1] != "windows-11" {
				return nil
			}
			p.Phase = "packages"
			p.Label = map[string]string{
				"account":     "请在安装窗口设置客机本地管理员",
				"oobe":        "请在虚拟机连接窗口完成 Windows 首次设置",
				"credentials": "请在安装窗口输入客机管理员账户",
			}[fields[2]]
		case "media":
			p.Phase, p.Label = "image", "正在下载并校验客机开发组件"
		case "tools":
			p.Phase, p.Label = "tools", "正在安装客机应用和开发工具"
		case "services":
			p.Phase, p.Label = "services", "正在完成客机配置"
		default:
			return nil
		}
		return p
	}
	if strings.HasPrefix(line, "CAGE_IMAGE_BYTES ") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil
		}
		received, err := strconv.ParseInt(fields[1], 10, 64)
		total, te := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || te != nil || received < 0 || total < -1 || (total > 0 && received > total) {
			return nil
		}
		return &SetupProgress{Phase: "image", Label: "正在下载 Debian 13 镜像", Received: &received, Total: max(total, 0)}
	}
	if strings.HasPrefix(line, "CAGE_TOOL_BYTES ") {
		fields := strings.Fields(line)
		if len(fields) != 3 && len(fields) != 4 {
			return nil
		}
		received, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || received < 0 {
			return nil
		}
		p := &SetupProgress{Phase: "tools", Received: &received}
		if len(fields) == 4 {
			total, err := strconv.ParseInt(fields[3], 10, 64)
			if err != nil || total <= 0 || received > total {
				return nil
			}
			p.Total = total
		}
		switch fields[1] {
		case "mise.tar.gz":
			p.Phase, p.Label = "packages", "正在下载 mise"
		case "code-server.deb":
			p.Label = "正在下载网页编辑器 code-server"
		case "chromium.deb":
			p.Phase, p.Label = "packages", "正在下载 Chromium 浏览器"
		case "chromium-common.deb":
			p.Phase, p.Label = "packages", "正在下载 Chromium 公共资源"
		case "claude-code-linux-x64.tgz", "claude-code-linux-arm64.tgz":
			p.Label = "正在下载 Claude Code"
		default:
			return nil
		}
		return p
	}
	switch line {
	case "CAGE_IMAGE_READY":
		return &SetupProgress{Phase: "image", Label: "正在启动 Linux 基础环境"}
	case "CAGE_IMAGE_VERIFY":
		return &SetupProgress{Phase: "image", Label: "正在校验 Debian 镜像"}
	case "Installing packages inside VM...", "Installing base packages...":
		return &SetupProgress{Phase: "packages", Label: "正在安装 Linux 基础软件包"}
	case "Installing Python 3...":
		return &SetupProgress{Phase: "packages", Label: "正在安装 Python"}
	case "Installing Chromium...":
		return &SetupProgress{Phase: "packages", Label: "正在安装 Chromium 浏览器"}
	case "Installing Node.js 24...":
		return &SetupProgress{Phase: "packages", Label: "正在安装 Node.js 24"}
	case "Installing mise...":
		return &SetupProgress{Phase: "packages", Label: "正在安装 mise"}
	case "Installing Claude Code...":
		return &SetupProgress{Phase: "tools", Label: "正在安装 Claude Code"}
	case "Installing code-server...":
		return &SetupProgress{Phase: "tools", Label: "正在安装网页编辑器 code-server"}
	}
	if strings.HasPrefix(line, "Running custom setup from ") {
		return &SetupProgress{Phase: "services", Label: "正在配置桌面服务"}
	}
	return nil
}

func macOSDownloadLabel(name string) string {
	component := map[string]string{
		"editor.tgz": "macOS 网页编辑器", "playwright.tgz": "浏览器控制组件", "node.tgz": "Node.js",
		"macfuse.dmg": "macFUSE 文件共享组件", "claude.dmg": "Claude Desktop", "chrome.dmg": "Chrome 浏览器",
		"macos-cli-claude": "Claude Code", "macos-cli-uv.tar.gz": "uv 开发工具",
		"macos-cli-python.tar.gz": "Python", "macos-cli-git.tar.gz": "Git", "macos-cli-gh.zip": "GitHub CLI",
	}[name]
	if component == "" {
		return ""
	}
	return "正在下载 " + component
}

func guestProgressName(system string) string {
	return map[string]string{"macos": "macOS", "windows-11": "Windows 11", "debian-12": "Debian 12", "debian-13": "Debian 13", "ubuntu-22.04": "Ubuntu 22.04", "ubuntu-24.04": "Ubuntu 24.04"}[system]
}

func (e *jobExecution) setSetupProgress(p *SetupProgress) {
	e.owner.mu.Lock()
	defer e.owner.mu.Unlock()
	if e.id != "" && e.id == e.owner.state.ID && e.owner.state.Running {
		e.owner.state.SetupProgress = p
		e.progressUpdated = time.Now()
	}
}
