package workbench

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/sylvinus/agent-vm/internal/usererror"
)

const failureHelp = "在设置中导出诊断包，并附上出错时正在执行的操作。"

// Only recognize known diagnostics. Unrecognized process output stays in the
// existing operation log and never becomes an arbitrary user-facing message.
func executionFailure(action string, err error, output string) error {
	lower := strings.ToLower(output)
	for _, rule := range []struct {
		patterns     []string
		reason, next string
	}{
		{[]string{"base vm not found"}, "基础模板缺失，无法创建或重置环境", "请先完成环境安装；已有环境的磁盘应保留，不要删除或重置个人数据。"},
		{[]string{"base vm setup did not complete"}, "基础模板尚未安装完成", "请继续环境安装后重试；已有环境的磁盘应保留，不要删除或重置个人数据。"},
		{[]string{"cage_windows_maintenance_authorization_canceled"}, "已取消 Windows 管理员授权", "请重新发起原操作，并在 Windows 系统授权窗口中确认。"},
		{[]string{"cage_windows_maintenance_authorization"}, "Windows 管理员授权窗口未能打开", "请从本机 Cage 重新发起操作，并使用可完成管理员授权的 Windows 账户。"},
		{[]string{"cage_windows_maintenance_validation"}, "Windows 环境绑定或组件清单未通过校验", "请重新发起原操作，组件包仍无法核验时重新安装完整 Cage 应用。"},
		{[]string{"cage_windows_maintenance_host_manager"}, "Windows 系统管理助手安装未完成", "请重试修复系统助手，仍失败时检查系统服务或安全软件记录并导出诊断包。"},
		{[]string{"cage_windows_maintenance_guest_connection"}, "Windows 客机连接未完成", "请启动并登录已绑定客机，使用该客机的管理员账户重试。"},
		{[]string{"cage_windows_maintenance_guest_components"}, "Windows 客机组件安装未完成", "请保持原客机运行，重试同步环境组件，仍失败时导出诊断包。"},
		{[]string{"cage_windows_maintenance_host_relay"}, "Windows 主机连接组件更新未完成", "请关闭占用连接组件的程序，再重试同步环境组件。"},
		{[]string{"cage_windows_maintenance_package_preparation"}, "Windows 组件包准备未完成", "请检查应用目录和系统盘空间后重试，更新工具时同时检查已选代理。"},
		{[]string{"cage_windows_maintenance_failed"}, "Windows 维护未完成", "请从 Cage 重新发起原操作，仍失败时导出诊断包。"},
		{[]string{"desktop file service state=failed"}, "Claude Desktop 的文件服务已经退出", "请从 Cage 重新打开 Claude Desktop；若应用反复退出，请在设置中查看资源用量并导出诊断包。"},
		{[]string{"curl: (7)", "connection refused"}, "目标服务没有接受连接", "请确认环境及所选代理已启动；文件传递时请重新打开接收文件的应用，再重试。"},
		{[]string{"no space left on device", "not enough space on the disk"}, "磁盘空间不足，无法继续写入", "请释放环境所在磁盘的空间后继续；不要删除 Cage 的个人数据目录。"},
		{[]string{"could not get lock", "unable to acquire the dpkg", "another process using it"}, "环境中已有另一个软件安装或更新进程", "请等待当前安装结束后再继续；不要手动删除安装锁文件。"},
		{[]string{"temporary failure resolving", "could not resolve host", "curl: (6)"}, "下载服务器的域名解析失败", "请在网络与代理设置中检查所选节点和 DNS，再继续安装。"},
		{[]string{"curl: (28)", "connection timed out", "operation timed out"}, "下载或环境连接等待超时", "请检查所选代理是否可用，再继续；已保存的下载会尝试续传。"},
		{[]string{"curl: (60)", "certificate verify failed", "certificate verification failed"}, "下载服务器的证书验证未通过", "请检查电脑时间和所选代理；仍失败时导出诊断包，不要关闭证书验证。"},
		{[]string{"407 proxy authentication", "proxy authentication required"}, "代理服务器拒绝了当前登录凭据", "请在网络与代理设置中更新代理账号或订阅，再应用节点。"},
		{[]string{"permission denied", "access is denied"}, "系统拒绝访问安装所需的文件或程序", "请检查应用及环境目录的权限；若安全软件拦截了 Cage，请先核实其拦截记录。"},
		{[]string{"tried to start delayed item"}, "环境的软件包下载程序反复重试，未能完成安装", "请使用包含 APT 修复的 Cage 构建后继续安装；仍失败时导出诊断包。"},
	} {
		for _, pattern := range rule.patterns {
			if strings.Contains(lower, pattern) {
				return usererror.New(action, rule.reason, rule.next, err)
			}
		}
	}
	return usererror.Wrap(action, err, "打开“操作详情”查看最后的错误；仍无法判断时，"+failureHelp)
}

func serviceHTTPFailure(action string, status int) error {
	reason, next := fmt.Sprintf("服务返回 HTTP %d，尚未取得可识别的具体原因", status), failureHelp
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		reason, next = "服务拒绝了这次连接的授权", "请重新连接当前环境；若仍出现，更新环境组件后再试。"
	case http.StatusNotFound, http.StatusGone:
		reason, next = "目标窗口、文件或服务接口已不存在", "请重新打开目标窗口或重新选择文件；已有环境较旧时，先更新环境组件。"
	case http.StatusConflict:
		reason, next = "目标状态已变化，或另一个操作尚未结束", "请确认当前页面与目标文件，再重新发起这次操作。"
	case http.StatusRequestEntityTooLarge:
		reason, next = "文件或请求内容超过服务允许的大小", "请缩小文件或分批处理，再重新发起这次操作。"
	case http.StatusTooManyRequests:
		reason, next = "服务暂时限制了请求次数", "请等待片刻后重试，避免连续点击。"
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		reason, next = "环境内的服务尚未就绪或已停止响应", "请先确认环境已启动；等待后仍无法连接时，查看环境设置中的组件状态。"
	case http.StatusGatewayTimeout:
		reason, next = "等待环境内服务响应超时", "请检查环境负载并稍后再试；持续超时则导出诊断包。"
	}
	return usererror.New(action, reason, next, nil)
}

func fileServiceFailure(status int, data []byte) error {
	known := map[string]string{
		"上传窗口已关闭，请重新选择文件":         "上传窗口已关闭。请重新点击应用内的上传按钮，再选择文件。",
		"原文件窗口已关闭":                "原文件窗口已关闭。请在接收文件的应用中重新打开文件窗口。",
		"请先完成已打开的文件窗口":            "另一个文件窗口尚未处理。请先完成或关闭该窗口，再重新选择文件。",
		"每个文件不能超过 2 GB":           "单个文件超过 2 GB 上限。请缩小文件后重新上传。",
		"项目文件夹尚未连接，请重新打开项目":       "项目文件夹尚未连接。请先在 Cage 中重新打开该项目，再选择目录。",
		"目标没有接收文件，请点击支持附件的输入框后重试": "目标没有接收文件。请先点击应用内支持附件的输入框，再重新选择文件。",
		"上传已取消":       "上传已被取消。需要继续时，请重新选择文件。",
		"当前窗口没有请求文件夹": "当前窗口需要选择文件，不能接收文件夹。请按应用的窗口提示重新选择。",
		"文件夹正在交给应用":   "文件夹仍在传给应用。请等待当前操作结束后再选择。",
		"文件位置无法读取":    "接收文件的应用无法读取该位置。请确认项目目录已连接，并重新选择。",
		"下载服务不可用":     "环境内的下载服务尚未就绪。请重新打开应用；旧环境请先更新环境组件。",
		"文件请求过大":      "这次文件请求超过服务允许的大小。请减少一次传递的文件数量。",
		"请选择项目文件夹":    "当前操作需要一个已连接的项目文件夹。请先在 Cage 中打开项目，再重新选择目录。",
	}
	if reason := known[strings.TrimSpace(string(data))]; reason != "" {
		return fmt.Errorf("传递文件失败：%s", reason)
	}
	return serviceHTTPFailure("传递文件", status)
}

func browserOperation(path string) string {
	switch path {
	case "/navigate":
		return "打开网页"
	case "/reload":
		return "刷新网页"
	case "/audit":
		return "读取浏览器检测结果"
	case "/tabs":
		return "读取浏览器标签页"
	case "/paste-image":
		return "粘贴图片"
	case "/editor-restart":
		return "打开编辑器"
	case "/extensions":
		return "读取浏览器扩展"
	default:
		return "执行浏览器操作"
	}
}

type failureOutput struct {
	mu   sync.Mutex
	tail []byte
}

func (b *failureOutput) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(data)
	if len(data) > 8192 {
		data = data[len(data)-8192:]
	}
	b.tail = append(b.tail, data...)
	if len(b.tail) > 8192 {
		b.tail = b.tail[len(b.tail)-8192:]
	}
	return n, nil
}
func (b *failureOutput) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.tail) }
func (b *failureOutput) writer(dst io.Writer) io.Writer {
	if dst == nil {
		return b
	}
	return io.MultiWriter(dst, b)
}
