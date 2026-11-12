package workbench

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

var (
	validateProxyCore = managedproxy.Validate
	startProxyCore    = managedproxy.Start
	fetchProxySource  = managedproxy.Fetch
)

type proxyInput struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	URL          string   `json:"url"`
	Content      string   `json:"content"`
	Interval     int      `json:"interval_hours"`
	HostDownload bool     `json:"host_download"`
	NodeID       string   `json:"node_id"`
	NodeIDs      []string `json:"node_ids"`
	Port         int      `json:"port"`
}

func (s *Server) proxyStatus(w http.ResponseWriter, r *http.Request) {
	p, err := managedproxy.Load(s.profile)
	if err != nil {
		requestError(w, err)
		return
	}
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		requestError(w, err)
		return
	}
	sources := []any{}
	for _, src := range p.Sources {
		nodes := []any{}
		for _, n := range src.Nodes {
			nodes = append(nodes, map[string]any{"id": n.ID, "name": n.Name, "type": n.Type})
		}
		host := ""
		if u, e := url.Parse(src.URL); e == nil {
			host = u.Hostname()
		}
		sources = append(sources, map[string]any{"id": src.ID, "name": src.Name, "remote": src.URL != "", "host": host, "interval_hours": src.Interval, "updated_at": src.Updated, "error": src.Error, "usage": src.Usage, "nodes": nodes, "skipped": src.Skipped})
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	_, appliedErr := managedproxy.Applied(s.profile, cfg.ProxyURL, cfg.ProxyRevision)
	active := appliedErr == nil
	running := managedproxy.Running(ctx, s.profile, p)
	state, label := "unconfigured", "尚未配置"
	_, selected := p.Find(p.Selected)
	if selected {
		state, label = "unapplied", "尚未应用"
	}
	if active {
		state, label = "stopped", "代理已停止"
		if running {
			state, label = "prepared", "待启动环境"
			if s.vmRunning() {
				state, label = "paused", "外网未放行"
				if client, e := s.gateClient(); e == nil {
					if gate, e := client.Status(ctx); e == nil {
						if gate.Mode == cage.GateChecking {
							state, label = "checking", "正在检查"
						} else if gate.Mode == cage.GateOpen && gate.Report != nil && gate.Report.Authorizes(gate.RunID, cfg.Fingerprint(), time.Now()) == nil && time.Now().Before(gate.Until) {
							state, label = "ready", "已联网"
						}
					}
				}
			}
		}
	}
	var version struct {
		Version string `json:"version"`
	}
	if running {
		_ = managedproxy.API(ctx, s.profile, "GET", "/version", nil, &version)
	}
	_, coreErr := managedproxy.CorePath()
	route, routeErr := p.Route()
	if route == nil {
		route = append([]string{}, p.Chain...)
	}
	routeError := ""
	if routeErr != nil && selected {
		routeError = routeErr.Error()
	}
	jsonReply(w, map[string]any{"route": route, "route_error": routeError, "sources": sources, "selected": p.Selected, "port": p.Port, "running": running, "active": active, "state": state, "state_label": label, "network_open": state == "ready", "core_available": coreErr == nil, "version": version.Version, "revision": p.Revision, "bootstrap_dns": strings.Join(managedproxy.BootstrapDNSURLs(), "、"), "udp_blocked": true, "ipv6_blocked": true})
}
func (s *Server) proxyMutation(w http.ResponseWriter, r *http.Request) {
	var input proxyInput
	if !readJSON(w, r, &input, 12<<20) {
		return
	}
	action := r.PathValue("action")
	allowed := map[string]bool{"import": true, "refresh": true, "edit": true, "delete": true, "select": true, "restart": true, "stop": true, "route": true}
	if !allowed[action] {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	pauseVersion := s.safety.pauseVersion
	s.mu.Unlock()
	s.launch(w, "proxy", func() error { return s.changeProxy(action, input, &pauseVersion) })
}
func (s *Server) subscriptionClient(host bool) (*http.Client, error) {
	if host {
		return managedproxy.BootstrapClient(), nil
	}
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		return nil, err
	}
	if err = cfg.RequireManagedProxy(s.profile); err != nil {
		return nil, err
	}
	e, err := cage.NewEgress(cfg)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: e.DialContext}, Timeout: 30 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 3 || r.URL.Scheme != "https" || r.URL.User != nil {
			return errors.New("订阅重定向被拒绝")
		}
		return nil
	}}, nil
}
func (s *Server) changeProxy(action string, in proxyInput, resumeVersion *uint64) error {
	p, err := managedproxy.Load(s.profile)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		return err
	}
	ctx := s.operationContext()
	if action == "stop" {
		if err = s.closeProxyGate("内置代理已停止，外网保持关闭"); err != nil {
			return err
		}
		return managedproxy.Stop(s.profile)
	}
	if action == "select" || action == "restart" || action == "route" {
		if action == "route" {
			if len(in.NodeIDs) == 0 {
				return errors.New("请至少选择一个最终出口")
			}
			p.Chain = append([]string(nil), in.NodeIDs...)
			p.Selected = p.Chain[len(p.Chain)-1]
			if _, err := p.Route(); err != nil {
				return err
			}
		}
		if action == "select" {
			if _, ok := p.Find(in.NodeID); !ok {
				return errors.New("所选节点已不存在，请刷新列表")
			}
			p.Selected = in.NodeID
			if len(p.Chain) > 0 {
				p.Chain[len(p.Chain)-1] = in.NodeID
			}
		}
		if in.Port != 0 {
			if in.Port < 1024 || in.Port > 65535 {
				return errors.New("代理端口应为 1024–65535")
			}
			p.Port = in.Port
		}
		return s.applyProxy(p, cfg, resumeVersion, action != "restart")
	}
	index := -1
	for i, src := range p.Sources {
		if src.ID == in.ID {
			index = i
			break
		}
	}
	if action == "delete" {
		if index < 0 {
			return errors.New("订阅来源不存在")
		}
		if len(p.Chain) > 1 {
			for _, id := range p.Chain {
				for _, n := range p.Sources[index].Nodes {
					if n.ID == id {
						return errors.New("该来源包含当前代理链中的节点，请先选择并应用其他来源的节点")
					}
				}
			}
		}
		p.Sources = append(p.Sources[:index], p.Sources[index+1:]...)
		if _, ok := p.Find(p.Selected); !ok {
			p.Selected = ""
			p.Chain = nil
		}
	} else if action == "edit" {
		if index < 0 {
			return errors.New("订阅来源不存在")
		}
		if strings.TrimSpace(in.Name) == "" || len(in.Name) > 100 || in.Interval < 0 || in.Interval > 168 {
			return errors.New("名称或更新间隔无效")
		}
		p.Sources[index].Name = strings.TrimSpace(in.Name)
		p.Sources[index].Interval = in.Interval
		if in.URL != "" {
			u, e := url.Parse(in.URL)
			if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
				return errors.New("订阅需要完整的 HTTPS 网址")
			}
			p.Sources[index].URL = in.URL
		}
		return p.Save(s.profile)
	} else {
		var data []byte
		usage := ""
		if action == "refresh" {
			if index < 0 || p.Sources[index].URL == "" {
				return errors.New("该来源没有订阅链接")
			}
			src := p.Sources[index]
			in.Name, in.URL, in.Interval = src.Name, src.URL, src.Interval
		} else {
			data = []byte(in.Content)
			in.ID = ""
		}
		if in.URL != "" {
			s.setMessage("正在下载节点订阅…")
			client, e := s.subscriptionClient(in.HostDownload)
			if e != nil {
				s.proxySourceFailure(in.ID, e)
				return e
			}
			defer client.CloseIdleConnections()
			data, usage, err = fetchProxySource(ctx, client, in.URL)
			if err != nil {
				if index >= 0 {
					p.Sources[index].Error = err.Error()
					p.Sources[index].Attempted = time.Now()
					_ = p.Save(s.profile)
				}
				return err
			}
		}
		if err = p.Import(in.Name, in.URL, data, in.ID, in.Interval, usage); err != nil {
			s.proxySourceFailure(in.ID, err)
			return err
		}
		candidate := p
		if candidate.Selected == "" {
			candidate.Selected = candidate.Sources[0].Nodes[0].ID
		}
		if _, ok := candidate.Find(candidate.Selected); !ok {
			candidate.Selected = candidate.Sources[0].Nodes[0].ID
		}
		if err = validateProxyCore(ctx, s.profile, candidate); err != nil {
			s.proxySourceFailure(in.ID, err)
			return err
		}
		if _, ok := p.Find(p.Selected); !ok {
			p.Selected = ""
		}
	}
	if cfg.ProxyRevision != "" && cfg.ProxyURL == p.Endpoint() {
		if p.Selected == "" {
			if err = s.closeProxyGate("当前节点已被移除，请选择新节点"); err != nil {
				return err
			}
			if err = managedproxy.Stop(s.profile); err != nil {
				return err
			}
			return p.Save(s.profile)
		}
		return s.applyProxy(p, cfg, resumeVersion, false)
	}
	return p.Save(s.profile)
}

func (s *Server) proxySourceFailure(id string, failure error) {
	if id == "" {
		return
	}
	p, err := managedproxy.Load(s.profile)
	if err != nil {
		return
	}
	for i := range p.Sources {
		if p.Sources[i].ID == id {
			p.Sources[i].Error = failure.Error()
			p.Sources[i].Attempted = time.Now()
			_ = p.Save(s.profile)
			return
		}
	}
}
func (s *Server) closeProxyGate(reason string) error {
	if !s.vmRunning() {
		return nil
	}
	return s.revokeNetworkNow(reason, false)
}

func (s *Server) activateProxy(p managedproxy.Store, old cage.Config) (returned error) {
	// The explicit start operation already requested networking. A pause made
	// after that request must survive any core recovery within the start.
	return s.applyProxy(p, old, nil, false)
}

func (s *Server) applyProxy(p managedproxy.Store, old cage.Config, resumeVersion *uint64, selectExit bool) (returned error) {
	ctx := s.operationContext()
	previous, err := managedproxy.Load(s.profile)
	if err != nil {
		return err
	}
	s.setMessage("正在验证节点配置…")
	if err := validateProxyCore(ctx, s.profile, p); err != nil {
		return err
	}
	// Changing the endpoint requires an environment restart; node-only switches
	// keep its SSH sessions and VM alive and revoke the gate synchronously.
	restartVM := s.vmRunning() && old.ProxyURL != p.Endpoint()
	if err := s.closeProxyGate("代理节点切换中，旧连接及检测授权已关闭"); err != nil {
		return err
	}
	if restartVM {
		if err := s.stopEnvironment(); err != nil {
			return err
		}
	}
	s.setMessage("正在应用内置代理节点…")
	if err := managedproxy.Stop(s.profile); err != nil {
		return err
	}
	committed := false
	defer func() {
		if returned != nil && !committed {
			_ = previous.Save(s.profile)
			_ = writePrivate(s.configPath, old)
		}
	}()
	p.Revision = managedproxy.Token()
	cfg := old
	cfg.ExitPolicy = selectProxyExitPolicy(&p, previous, old.ExitPolicy, selectExit)
	if err := p.Save(s.profile); err != nil {
		return err
	}
	cfg.ProxyURL = p.Endpoint()
	cfg.ProxyRevision = p.Revision
	if err := writePrivate(s.configPath, cfg); err != nil {
		return err
	}
	if err := startProxyCore(ctx, s.profile, p); err != nil {
		return err
	}
	committed = true
	if resumeVersion != nil {
		if err := s.resumeNetworkIntentAt(*resumeVersion); err != nil {
			return err
		}
	}
	s.mu.Lock()
	// Only a user request may resume networking, and a later pause wins even
	// when it arrived while subscription download or core startup was pending.
	paused := s.networkPaused
	s.exitProbeError = ""
	s.locationError = ""
	s.location = nil
	s.safety.Report = nil
	s.mu.Unlock()
	if s.vmRunning() {
		client, err := s.gateClient()
		if err != nil {
			return err
		}
		state, err := client.SyncProxyRevision(ctx)
		if err != nil {
			return err
		}
		s.publishGate(state)
		if paused {
			return nil
		}
		s.setMessage("节点已应用，正在重新检查环境出口…")
		return s.runSafetyCheck()
	}
	if restartVM {
		return s.startEnvironment(false)
	}
	s.setMessage("节点已应用；启动 Cage 环境后完成联网检查")
	return nil
}

func (s *Server) proxyDelay(w http.ResponseWriter, r *http.Request) {
	var in proxyInput
	if !readJSON(w, r, &in) {
		return
	}
	p, err := managedproxy.Load(s.profile)
	if err != nil {
		requestError(w, err)
		return
	}
	if _, ok := p.Find(in.NodeID); !ok {
		requestError(w, errors.New("节点不存在"))
		return
	}
	var v struct {
		Delay int `json:"delay"`
	}
	err = managedproxy.API(r.Context(), s.profile, "GET", "/proxies/"+url.PathEscape(in.NodeID)+"/delay?timeout=5000&url="+url.QueryEscape("https://www.gstatic.com/generate_204"), nil, &v)
	if err != nil {
		requestError(w, errors.New("节点未载入核心或测速失败；请先应用节点以载入当前列表"))
		return
	}
	jsonReply(w, v)
}
func (s *Server) proxyConnections(w http.ResponseWriter, r *http.Request) {
	var response struct {
		Upload      int64 `json:"uploadTotal"`
		Download    int64 `json:"downloadTotal"`
		Connections []struct {
			ID       string `json:"id"`
			Upload   int64  `json:"upload"`
			Download int64  `json:"download"`
			Metadata struct {
				Network string `json:"network"`
				Host    string `json:"host"`
				IP      string `json:"destinationIP"`
				Port    string `json:"destinationPort"`
			} `json:"metadata"`
		} `json:"connections"`
	}
	if err := managedproxy.API(r.Context(), s.profile, "GET", "/connections", nil, &response); err != nil {
		jsonReply(w, map[string]any{"connections": []any{}, "uploadTotal": 0, "downloadTotal": 0})
		return
	}
	items := []any{}
	for i, c := range response.Connections {
		if i >= 100 {
			break
		}
		host := c.Metadata.Host
		if host == "" {
			host = c.Metadata.IP
		}
		items = append(items, map[string]any{"id": c.ID, "host": host, "port": c.Metadata.Port, "network": c.Metadata.Network, "upload": c.Upload, "download": c.Download})
	}
	jsonReply(w, map[string]any{"connections": items, "uploadTotal": response.Upload, "downloadTotal": response.Download})
}
func (s *Server) proxyLogs(w http.ResponseWriter, r *http.Request) {
	p, err := managedproxy.Load(s.profile)
	if err != nil {
		requestError(w, err)
		return
	}
	// Whitelisted lifecycle messages avoid publishing upstream raw log lines,
	// which can contain subscription secrets, credentials and host paths.
	lines := []string{"客机公网 UDP、IPv6、ICMP：阻断", "业务 DNS：经选定节点转发", "代理基础设施 DNS：" + strings.Join(managedproxy.BootstrapDNSURLs(), "、") + "（主机网络，并行查询）"}
	if managedproxy.Running(r.Context(), s.profile, p) {
		lines = append(lines, "内置核心运行中；已读回当前节点选择")
	} else {
		lines = append(lines, "内置核心未就绪；不会直连回退")
	}
	for _, src := range p.Sources {
		lines = append(lines, src.Updated.Format("2006-01-02 15:04")+" · "+src.Name+" · "+src.Error)
	}
	s.mu.Lock()
	if issue := s.currentNetworkErrorLocked(); issue != "" {
		lines = append(lines, issue)
	}
	s.mu.Unlock()
	jsonReply(w, map[string]any{"lines": lines})
}
func (s *Server) refreshProxySubscriptions() {
	if s.busy() {
		return
	}
	p, err := managedproxy.Load(s.profile)
	if err != nil {
		return
	}
	for _, src := range p.Sources {
		if src.URL != "" && src.Interval > 0 && time.Since(src.Updated) >= time.Duration(src.Interval)*time.Hour && time.Since(src.Attempted) > 15*time.Minute {
			// Automatic refresh never opts into a host-network fallback.
			_, _ = s.submitJob("proxy-update", func() error { return s.changeProxy("refresh", proxyInput{ID: src.ID}, nil) })
			return
		}
	}
}

func (s *Server) checkProxyHealth() {
	if s.busy() {
		return
	}
	cfg, err := loadConfig(s.configPath)
	if !s.vmRunning() {
		return
	}
	if err != nil {
		_ = s.closeProxyGate("代理配置无法读取，外网保持关闭")
		return
	}
	p, err := managedproxy.Load(s.profile)
	ctx, cancel := context.WithTimeout(s.lifetimeContext(), 2*time.Second)
	defer cancel()
	if err == nil && cfg.RequireManagedProxy(s.profile) == nil && managedproxy.Running(ctx, s.profile, p) {
		return
	}
	_ = s.closeProxyGate("内置代理已中断或节点状态变化，请重试应用节点")
	s.mu.Lock()
	s.safety.monitorError = "内置代理未就绪；外网保持关闭"
	s.mu.Unlock()
}
