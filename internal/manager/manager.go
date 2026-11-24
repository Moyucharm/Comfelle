package manager

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"colab-comfyui/internal/auth"
	"colab-comfyui/internal/colab"
	"colab-comfyui/internal/config"
	"colab-comfyui/internal/proxy"
)

// State 描述 Colab ComfyUI 实例的生命周期状态。
type State string

const (
	StateStopped          State = "STOPPED"
	StateStarting         State = "STARTING"
	StateWaitingDriveAuth State = "WAITING_DRIVE_AUTH"
	StateDeploying        State = "DEPLOYING"
	StateRunning          State = "RUNNING"
	StateStopping         State = "STOPPING"
	StateError            State = "ERROR"
)

const (
	maxLogLines      = 300
	tokenRenewMargin = 5 * time.Minute
	comfyBasicUser   = "comfy"
)

var (
	tunnelURLRe = regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)
	redactURLRe = regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)
)

// ColabAPI 定义 Manager 依赖的 Colab 后端接口（便于测试解耦）。
type ColabAPI interface {
	ListAssignments(ctx context.Context) ([]colab.Assignment, error)
	Assign(ctx context.Context, notebookHash uuid.UUID, gpu string, highMem bool) (*colab.Assignment, error)
	Unassign(ctx context.Context, endpoint string) error
	GetCCUInfo(ctx context.Context) (*colab.CCUInfo, error)
	PropagateDriveCredentials(ctx context.Context, endpoint, fileID string, dryRun bool) (*colab.DrivePropagationResult, error)
}

// StatusSnapshot 对前端 /panel/api/status 暴露的安全状态视图（不含 Tunnel URL 与密码）。
type StatusSnapshot struct {
	State              State      `json:"state"`
	GPU                string     `json:"gpu"`
	DefaultGPU         string     `json:"default_gpu"`
	HighMem            bool       `json:"high_mem"`
	DriveNotebookSet   bool       `json:"drive_notebook_set"`
	DriveAuthURL       string     `json:"drive_auth_url,omitempty"`
	ErrorMessage       string     `json:"error_message,omitempty"`
	StartedAt          *time.Time `json:"started_at,omitempty"`
	LastHeartbeatAt    *time.Time `json:"last_heartbeat_at,omitempty"`
	KeepaliveIntervalS int        `json:"keepalive_interval_sec"`
	TunnelHealthy      bool       `json:"tunnel_healthy"`
	CCUBalance         *float64   `json:"ccu_balance,omitempty"`
	CCURateHourly      *float64   `json:"ccu_rate_hourly,omitempty"`
	ActiveAssignments  int        `json:"active_assignments"`
	HasActiveVM        bool       `json:"has_active_vm"`
}

// Manager 负责单实例状态机、热复用认领、保活自愈、停机同步插件与实时日志广播。
type Manager struct {
	cfg   *config.Config
	api   ColabAPI
	store *StateStore
	probe *http.Client

	mu                 sync.RWMutex
	state              State
	gpu                string
	endpoint           string
	proxyURL           string
	proxyToken         string
	proxyTokenExp      time.Time
	kernelID           string
	sessionID          string
	tunnelURL          *url.URL
	comfyPassword      string // 仅存内存，每次启动或热接管随机生成
	driveAuthURL       string
	driveAuthConfirmCh chan struct{}
	errorMessage       string
	startedAt          *time.Time
	lastHeartbeatAt    *time.Time
	tunnelHealthy      bool
	opSeq              uint64
	opCancel           context.CancelFunc
	keepaliveCancel    context.CancelFunc

	usageMu      sync.Mutex
	cachedCCU    *colab.CCUInfo
	cachedCCUAt  time.Time

	logMu       sync.Mutex
	logs        []string
	subscribers map[chan string]struct{}
}

// New 创建实例管理器。
func New(cfg *config.Config, api ColabAPI, store *StateStore) *Manager {
	return &Manager{
		cfg:   cfg,
		api:   api,
		store: store,
		probe: &http.Client{Timeout: 12 * time.Second},
		state: StateStopped,
		gpu:   cfg.Colab.DefaultGPU,
		subscribers: make(map[chan string]struct{}),
	}
}

// ProxyTarget 实现 proxy.TargetProvider，向反代层提供实时动态上游与内存中的临时密码。
func (m *Manager) ProxyTarget() proxy.Target {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.state != StateRunning || m.tunnelURL == nil || m.comfyPassword == "" {
		return proxy.Target{Ready: false}
	}
	uCopy := *m.tunnelURL
	return proxy.Target{
		URL:      &uCopy,
		Username: comfyBasicUser,
		Password: m.comfyPassword,
		Ready:    true,
	}
}

// Status 返回当前安全状态快照（附带缓存的 Compute Units 信息）。
func (m *Manager) Status(ctx context.Context) StatusSnapshot {
	m.mu.RLock()
	snap := StatusSnapshot{
		State:              m.state,
		GPU:                m.gpu,
		DefaultGPU:         m.cfg.Colab.DefaultGPU,
		HighMem:            m.cfg.Colab.HighMem,
		DriveNotebookSet:   strings.TrimSpace(m.cfg.Colab.DriveNotebookFileID) != "",
		DriveAuthURL:       m.driveAuthURL,
		ErrorMessage:       m.errorMessage,
		KeepaliveIntervalS: m.cfg.Colab.KeepaliveIntervalSec,
		TunnelHealthy:      m.tunnelHealthy,
		HasActiveVM:        m.endpoint != "" || m.store.Snapshot().Endpoint != "",
	}
	if m.startedAt != nil {
		t := *m.startedAt
		snap.StartedAt = &t
	}
	if m.lastHeartbeatAt != nil {
		t := *m.lastHeartbeatAt
		snap.LastHeartbeatAt = &t
	}
	m.mu.RUnlock()

	if ccu := m.getCCUInfoCached(ctx); ccu != nil {
		b := ccu.CurrentBalance
		r := ccu.ConsumptionRateHourly
		snap.CCUBalance = &b
		snap.CCURateHourly = &r
		snap.ActiveAssignments = ccu.AssignmentsCount
	}
	return snap
}

// Reconcile 在服务启动时执行一次对账：仅认领本系统记录过 Endpoint 的存活会话，绝不触碰非本系统创建的实例。
func (m *Manager) Reconcile(ctx context.Context) {
	persisted := m.store.Snapshot()
	if persisted.Endpoint == "" {
		m.appendLog("[INIT] 本系统当前无托管中的历史实例。")
		return
	}

	m.appendLog(fmt.Sprintf("[INIT] 正在检查本系统托管实例 (%s) 是否仍在云端存活...", persisted.Endpoint))
	assignments, err := m.api.ListAssignments(ctx)
	if err != nil {
		m.appendLog("[INIT] 检查云端会话失败（不影响后续手动启动）: " + err.Error())
		return
	}

	var picked *colab.Assignment
	for i := range assignments {
		if assignments[i].Endpoint == persisted.Endpoint {
			picked = &assignments[i]
			break
		}
	}
	if picked == nil {
		m.appendLog("[INIT] 本系统上次记录的实例已释放，状态已重置（未触碰账号下其他实例）。")
		_ = m.store.Update(func(st *PersistedState) {
			st.Endpoint = ""
			st.KernelID = ""
			st.SessionID = ""
		})
		return
	}

	m.appendLog(fmt.Sprintf("[INIT] 发现存活会话 (%s / %s)，尝试零中断热接管...", picked.Endpoint, picked.Accelerator))
	pw, err := auth.RandomHex(24)
	if err != nil {
		m.appendLog("[INIT] 生成临时密码失败: " + err.Error())
		return
	}

	rt := colab.NewRuntimeClient(picked.RuntimeProxyInfo.URL, picked.RuntimeProxyInfo.Token, persisted.KernelID, persisted.SessionID)
	res, err := rt.Execute(ctx, colab.BuildRotateAuthAndGetURLCode(pw), colab.ExecuteOptions{
		IdleTimeout: 90 * time.Second,
	})
	if err != nil {
		m.appendLog("[INIT] 热接管探测失败（可点启动按钮重新挂载接管）: " + err.Error())
		return
	}

	rawURL := extractTunnelURL(res.Stdout)
	if rawURL == "" {
		m.appendLog("[INIT] 发现活跃 VM 但 ComfyUI 尚未启动，请在面板点击「启动」复用该实例继续部署。")
		m.mu.Lock()
		m.endpoint = picked.Endpoint
		m.gpu = normalizeAccel(picked.Accelerator)
		m.proxyURL = picked.RuntimeProxyInfo.URL
		m.proxyToken = picked.RuntimeProxyInfo.Token
		m.proxyTokenExp = time.Now().Add(time.Duration(picked.RuntimeProxyInfo.TokenExpiresInSeconds) * time.Second)
		m.kernelID = rt.KernelID
		m.sessionID = rt.SessionID
		m.mu.Unlock()
		return
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		m.appendLog("[INIT] 解析隧道地址失败: " + err.Error())
		return
	}

	now := time.Now()
	m.mu.Lock()
	m.state = StateRunning
	m.gpu = normalizeAccel(picked.Accelerator)
	m.endpoint = picked.Endpoint
	m.proxyURL = picked.RuntimeProxyInfo.URL
	m.proxyToken = picked.RuntimeProxyInfo.Token
	m.proxyTokenExp = now.Add(time.Duration(picked.RuntimeProxyInfo.TokenExpiresInSeconds) * time.Second)
	m.kernelID = rt.KernelID
	m.sessionID = rt.SessionID
	m.tunnelURL = parsed
	m.comfyPassword = pw
	m.startedAt = &now
	m.lastHeartbeatAt = &now
	m.tunnelHealthy = m.probeTunnel(ctx, parsed, pw)
	m.errorMessage = ""
	m.mu.Unlock()

	_ = m.store.Update(func(st *PersistedState) {
		st.Endpoint = picked.Endpoint
		st.KernelID = rt.KernelID
		st.SessionID = rt.SessionID
		st.Accelerator = normalizeAccel(picked.Accelerator)
	})

	m.startKeepalive()
	m.appendLog("[INIT] 热接管成功！已轮换上游认证并恢复反向代理与保活守护。")
}

// Start 异步启动（或复用）Colab 实例、挂载 Drive 并部署 ComfyUI。
func (m *Manager) Start(gpu string, highMem bool) error {
	m.mu.Lock()
	if m.state != StateStopped && m.state != StateError {
		st := m.state
		m.mu.Unlock()
		return fmt.Errorf("当前状态为 %s，无法重复启动", st)
	}
	if strings.TrimSpace(gpu) == "" {
		gpu = m.cfg.Colab.DefaultGPU
	} else {
		gpu = config.NormalizeGPU(gpu)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if m.opCancel != nil {
		m.opCancel()
	}
	m.stopKeepaliveLocked()
	m.opSeq++
	opID := m.opSeq
	m.state = StateStarting
	m.gpu = gpu
	m.errorMessage = ""
	m.driveAuthURL = ""
	m.opCancel = cancel
	m.mu.Unlock()

	go m.runStartFlow(ctx, opID, gpu, highMem)
	return nil
}

// ConfirmDriveAuth 在等待 Drive 授权状态时，由前端点击「我已授权，继续」触发恢复挂载。
func (m *Manager) ConfirmDriveAuth() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != StateWaitingDriveAuth || m.driveAuthConfirmCh == nil {
		return errors.New("当前不处于等待 Drive 授权状态")
	}
	select {
	case m.driveAuthConfirmCh <- struct{}{}:
	default:
	}
	return nil
}

// Stop 停止保活、可选先同步插件到 Google Drive，再释放本系统托管的 Colab VM 停止计费。
func (m *Manager) Stop(force bool) error {
	m.mu.Lock()
	endpoint := m.endpoint
	if endpoint == "" {
		endpoint = m.store.Snapshot().Endpoint
	}
	if m.state == StateStopped && endpoint == "" {
		m.mu.Unlock()
		return nil
	}
	if m.state == StateStopping && !force {
		m.mu.Unlock()
		return errors.New("正在关闭中，请稍候；若卡住可点击「强制立即关机」")
	}

	if m.opCancel != nil {
		m.opCancel()
		m.opCancel = nil
	}
	m.stopKeepaliveLocked()

	pURL := m.proxyURL
	pTok := m.proxyToken
	kID := m.kernelID
	sID := m.sessionID

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	m.opSeq++
	opID := m.opSeq
	m.opCancel = cancel

	// 立即下线反代上游，避免请求打向正在销毁的实例
	m.state = StateStopping
	m.tunnelURL = nil
	m.comfyPassword = ""
	m.tunnelHealthy = false
	m.driveAuthURL = ""
	m.errorMessage = ""
	m.mu.Unlock()

	go m.runStopFlow(ctx, cancel, opID, force, endpoint, pURL, pTok, kID, sID)
	return nil
}

// RestartComfy 在不销毁 VM 的前提下仅重启 ComfyUI 进程（装完插件后常用）。
func (m *Manager) RestartComfy() error {
	m.mu.RLock()
	if m.state != StateRunning {
		m.mu.RUnlock()
		return errors.New("仅在实例运行中时可重启 ComfyUI")
	}
	m.mu.RUnlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		m.appendLog("[COMFY] 正在重启 ComfyUI 进程（保留 VM 与隧道）...")
		rt, err := m.runtimeClient(ctx)
		if err != nil {
			m.appendLog("[COMFY] 获取运行时失败: " + err.Error())
			return
		}
		_, err = rt.Execute(ctx, colab.RestartComfyCode, colab.ExecuteOptions{
			IdleTimeout: 3 * time.Minute,
			OnOutput:    m.appendStreamOutput,
		})
		if err != nil {
			m.appendLog("[COMFY] 重启 ComfyUI 失败: " + err.Error())
			return
		}
		m.appendLog("[COMFY] ComfyUI 重启完成，可刷新页面使用。")
	}()
	return nil
}

// SyncPluginsNow 手动立即将 custom_nodes 同步到 Google Drive。
func (m *Manager) SyncPluginsNow() error {
	m.mu.RLock()
	if m.state != StateRunning {
		m.mu.RUnlock()
		return errors.New("仅在实例运行中时可手动同步插件")
	}
	m.mu.RUnlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		m.appendLog("[SYNC] 开始手动同步 custom_nodes 到 Google Drive...")
		rt, err := m.runtimeClient(ctx)
		if err != nil {
			m.appendLog("[SYNC] 失败: " + err.Error())
			return
		}
		_, err = rt.Execute(ctx, colab.SyncPluginsCode, colab.ExecuteOptions{
			IdleTimeout: 90 * time.Second,
			OnOutput:    m.appendStreamOutput,
		})
		if err != nil {
			m.appendLog("[SYNC] 同步报错: " + err.Error())
			return
		}
		m.appendLog("[SYNC] 插件同步完成。")
	}()
	return nil
}

// SubscribeLogs 返回历史日志与实时日志订阅通道（供 SSE 使用）。
func (m *Manager) SubscribeLogs() ([]string, chan string, func()) {
	ch := make(chan string, 64)
	m.logMu.Lock()
	history := append([]string(nil), m.logs...)
	m.subscribers[ch] = struct{}{}
	m.logMu.Unlock()

	unsub := func() {
		m.logMu.Lock()
		if _, ok := m.subscribers[ch]; ok {
			delete(m.subscribers, ch)
			close(ch)
		}
		m.logMu.Unlock()
	}
	return history, ch, unsub
}

// Shutdown 用于服务退出时优雅清理后台协程（不会自动销毁远端 Colab VM，以便重启服务后热复用）。
func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.opSeq++
	if m.opCancel != nil {
		m.opCancel()
		m.opCancel = nil
	}
	m.stopKeepaliveLocked()
	m.mu.Unlock()
}

func (m *Manager) isCurrentOp(opID uint64) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.opSeq == opID
}

// unassignVerified 调用 Colab Unassign 接口；若接口报错（例如目标已提前释放返回 HTTP 404），
// 则通过 ListAssignments 向云端权威核验实例是否确实已不存在，避免误报失败或盲目吞掉错误。
func (m *Manager) unassignVerified(ctx context.Context, endpoint string) (bool, error) {
	err := m.api.Unassign(ctx, endpoint)
	if err == nil {
		return false, nil
	}
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return false, err
	}
	list, listErr := m.api.ListAssignments(ctx)
	if listErr == nil {
		for _, a := range list {
			if a.Endpoint == endpoint {
				return false, err
			}
		}
		return true, nil
	}
	return false, err
}

func (m *Manager) runStartFlow(ctx context.Context, opID uint64, gpu string, highMem bool) {
	var allocatedEndpoint string
	var activeRT *colab.RuntimeClient

	fail := func(msg string, err error) {
		if errors.Is(err, context.Canceled) || !m.isCurrentOp(opID) {
			return
		}
		full := msg
		if err != nil {
			full = msg + ": " + err.Error()
		}
		m.appendLog("[ERROR] " + full)

		// 启动/挂载/部署失败时自动释放已分配的 Colab 实例，防止后台持续计费。
		released := true
		if allocatedEndpoint != "" {
			m.appendLog(fmt.Sprintf("[CLEANUP] 启动失败，正在自动释放 Colab 实例 %s 停止计费...", allocatedEndpoint))
			cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
			if activeRT != nil {
				_ = activeRT.ShutdownKernel(cleanCtx)
			}
			if alreadyGone, uErr := m.unassignVerified(cleanCtx, allocatedEndpoint); uErr != nil {
				released = false
				m.appendLog("[CLEANUP] 自动释放失败，请点击面板「释放残留实例」重试: " + uErr.Error())
			} else if alreadyGone {
				m.appendLog("[CLEANUP] 云端核验确认 Colab 实例已释放，停止计费。")
			} else {
				m.appendLog("[CLEANUP] 已自动释放 Colab 实例，停止计费。")
			}
			cleanCancel()
			m.invalidateCCUCache()
		}

		m.mu.Lock()
		if m.opSeq != opID || m.state == StateStopping || m.state == StateStopped {
			m.mu.Unlock()
			return
		}
		m.stopKeepaliveLocked()
		m.state = StateError
		m.errorMessage = full
		m.driveAuthURL = ""
		m.tunnelURL = nil
		m.comfyPassword = ""
		m.tunnelHealthy = false
		if released {
			m.endpoint = ""
			m.proxyURL = ""
			m.proxyToken = ""
			m.proxyTokenExp = time.Time{}
			m.kernelID = ""
			m.sessionID = ""
		}
		m.mu.Unlock()

		if released {
			_ = m.store.Update(func(st *PersistedState) {
				if st.Endpoint == allocatedEndpoint {
					st.Endpoint = ""
					st.KernelID = ""
					st.SessionID = ""
				}
			})
		}
	}

	m.appendLog(fmt.Sprintf("[1/4] 检查或分配本系统专属 Colab 实例（规格: %s）...", gpu))
	var assigned *colab.Assignment

	// 严格隔离：仅当本地记录了本系统托管的 Endpoint 且它仍在云端存活时才复用，绝不劫持账号下其他训练/研究实例。
	persisted := m.store.Snapshot()
	if persisted.Endpoint != "" {
		if list, err := m.api.ListAssignments(ctx); err == nil {
			for i := range list {
				if list[i].Endpoint == persisted.Endpoint {
					cp := list[i]
					assigned = &cp
					m.appendLog(fmt.Sprintf("[1/4] 复用本系统已托管的 Colab 实例: %s (%s)", assigned.Endpoint, normalizeAccel(assigned.Accelerator)))
					break
				}
			}
		}
	}
	if assigned == nil {
		nbh, err := uuid.Parse(persisted.NotebookHash)
		if err != nil {
			nbh = uuid.New()
		}
		// Assign 使用本系统专属固定 notebook_hash：若该 notebook_hash 在云端已有实例会自动复用，否则独立分配新实例。
		assigned, err = m.api.Assign(ctx, nbh, gpu, highMem)
		if err != nil {
			fail("分配 Colab 实例失败", err)
			return
		}
		m.appendLog(fmt.Sprintf("[1/4] 新实例已就绪: %s (%s)", assigned.Endpoint, normalizeAccel(assigned.Accelerator)))
	}
	allocatedEndpoint = assigned.Endpoint
	// 立即持久化本系统托管的 Endpoint，确保任何异常中断都能精准识别归属、不误伤其他实例。
	_ = m.store.Update(func(st *PersistedState) {
		st.Endpoint = assigned.Endpoint
		st.Accelerator = normalizeAccel(assigned.Accelerator)
	})

	m.mu.Lock()
	m.endpoint = assigned.Endpoint
	m.gpu = normalizeAccel(assigned.Accelerator)
	m.proxyURL = assigned.RuntimeProxyInfo.URL
	m.proxyToken = assigned.RuntimeProxyInfo.Token
	m.proxyTokenExp = time.Now().Add(time.Duration(assigned.RuntimeProxyInfo.TokenExpiresInSeconds) * time.Second)
	m.mu.Unlock()

	rt := colab.NewRuntimeClient(assigned.RuntimeProxyInfo.URL, assigned.RuntimeProxyInfo.Token, m.store.Snapshot().KernelID, m.store.Snapshot().SessionID)
	activeRT = rt
	if _, err := rt.EnsureKernel(ctx); err != nil {
		fail("连接远端 Jupyter Kernel 失败", err)
		return
	}

	m.mu.Lock()
	m.kernelID = rt.KernelID
	m.sessionID = rt.SessionID
	m.mu.Unlock()
	_ = m.store.Update(func(st *PersistedState) {
		st.Endpoint = assigned.Endpoint
		st.KernelID = rt.KernelID
		st.SessionID = rt.SessionID
		st.Accelerator = normalizeAccel(assigned.Accelerator)
	})

	// 第 2 步：检查并挂载 Google Drive
	m.appendLog("[2/4] 检查 Google Drive 挂载状态...")
	chk, err := rt.Execute(ctx, colab.CheckDriveMountCode, colab.ExecuteOptions{IdleTimeout: 30 * time.Second})
	if err != nil {
		fail("检查 Drive 挂载失败", err)
		return
	}
	if strings.TrimSpace(chk.Stdout) == "DRIVE_IS_MOUNTED" {
		m.appendLog("[2/4] Google Drive 已处于挂载状态，无需重复授权。")
	} else {
		if err := m.mountDriveWithHook(ctx, rt, assigned.Endpoint); err != nil {
			fail("挂载 Google Drive 失败", err)
			return
		}
		m.appendLog("[2/4] Google Drive 挂载完成！")
	}

	// 第 3 步：部署或恢复 ComfyUI，并动态生成本次会话专属的随机 Basic Auth 密码
	m.mu.Lock()
	m.state = StateDeploying
	m.driveAuthURL = ""
	m.mu.Unlock()

	pw, err := auth.RandomHex(24)
	if err != nil {
		fail("生成上游临时密码失败", err)
		return
	}

	m.appendLog("[3/4] 正在部署 / 恢复 ComfyUI、配置 nginx 鉴权并拉起加密隧道...")
	deployCode := colab.BuildDeployCode(pw, m.cfg.Colab.HFToken, m.cfg.Colab.CivitaiToken)
	depRes, err := rt.Execute(ctx, deployCode, colab.ExecuteOptions{
		IdleTimeout: 15 * time.Minute,
		OnOutput:    m.appendStreamOutput,
	})
	if err != nil {
		fail("部署 ComfyUI 失败", err)
		return
	}

	rawURL := extractTunnelURL(depRes.Stdout)
	if rawURL == "" {
		fail("部署完成但未捕获到 Cloudflare Tunnel 地址", nil)
		return
	}
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		fail("解析 Tunnel 地址失败", err)
		return
	}

	// 第 4 步：带 Basic Auth 探测隧道连通性
	m.appendLog("[4/4] 正在通过自动鉴权验证隧道连通性...")
	healthy := false
	for i := 0; i < 15; i++ {
		if ctx.Err() != nil {
			return
		}
		if m.probeTunnel(ctx, parsedURL, pw) {
			healthy = true
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	if !healthy {
		fail("隧道已创建但连续探测 /system_stats 未返回 200，请检查部署日志", nil)
		return
	}

	now := time.Now()
	m.mu.Lock()
	if m.opSeq != opID || ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	m.state = StateRunning
	m.tunnelURL = parsedURL
	m.comfyPassword = pw
	m.startedAt = &now
	m.lastHeartbeatAt = &now
	m.tunnelHealthy = true
	m.errorMessage = ""
	m.mu.Unlock()

	m.invalidateCCUCache()
	m.startKeepalive()
	m.appendLog("[READY] ComfyUI 实例已就绪！自动反代、上游自动鉴权与保活守护均已生效。")
}

func (m *Manager) mountDriveWithHook(ctx context.Context, rt *colab.RuntimeClient, endpoint string) error {
	fileID := strings.TrimSpace(m.cfg.Colab.DriveNotebookFileID)
	if fileID != "" {
		m.appendLog("[2/4] 使用配置的持久化 Drive 笔记本 File ID 发起免重授挂载检查...")
	} else {
		m.appendLog("[2/4] 未配置 drive_notebook_file_id，发起交互式 Drive 挂载授权...")
	}

	_, err := rt.Execute(ctx, colab.MountDriveCode, colab.ExecuteOptions{
		AllowStdin:  true,
		IdleTimeout: 10 * time.Minute,
		OnOutput:    m.appendStreamOutput,
		OnDriveAuthRequest: func(reqCtx context.Context, colabMsgID string) error {
			dryRes, err := m.api.PropagateDriveCredentials(reqCtx, endpoint, fileID, true)
			if err != nil {
				return err
			}
			if !dryRes.Success {
				if dryRes.UnauthorizedRedirectURI == "" {
					return errors.New("Colab 返回 Drive 未授权，但未提供授权链接")
				}
				m.appendLog("[AUTH] 需要首次确认 Google Drive 授权，请在控制台点击授权链接并在完成后点击「我已授权，继续」。")
				confirmCh := make(chan struct{}, 1)
				m.mu.Lock()
				m.state = StateWaitingDriveAuth
				m.driveAuthURL = dryRes.UnauthorizedRedirectURI
				m.driveAuthConfirmCh = confirmCh
				m.mu.Unlock()

				timer := time.NewTimer(10 * time.Minute)
				defer timer.Stop()
				select {
				case <-reqCtx.Done():
					return reqCtx.Err()
				case <-timer.C:
					return errors.New("等待 Google Drive 授权超时（10 分钟）")
				case <-confirmCh:
					m.appendLog("[AUTH] 收到用户授权确认，正在将 Drive 凭据传播到 Colab VM...")
				}

				m.mu.Lock()
				m.state = StateStarting
				m.driveAuthURL = ""
				m.driveAuthConfirmCh = nil
				m.mu.Unlock()
			} else {
				m.appendLog("[AUTH] Google 后端已记住该笔记本授权，免人工确认自动挂载！")
			}

			if _, err := m.api.PropagateDriveCredentials(reqCtx, endpoint, fileID, false); err != nil {
				return fmt.Errorf("传播 Drive 凭据到 VM 失败: %w", err)
			}
			return nil
		},
	})
	return err
}

func (m *Manager) runStopFlow(ctx context.Context, cancel context.CancelFunc, opID uint64, force bool, endpoint, pURL, pTok, kID, sID string) {
	defer cancel()

	// 严格只关闭本系统记录过的托管实例，绝不回退抓取账号下其他非本系统实例。
	if endpoint == "" {
		m.appendLog("[STOP] 本系统当前没有托管中的实例（不会触碰账号下其他非本系统实例）。")
		m.finishStoppedIfCurrent(opID)
		return
	}

	// 若缺少 proxyURL/proxyToken（例如刚重启后点释放残留），按精确 endpoint 补查令牌；若云端已不存在则直接收尾。
	if !force && (pURL == "" || pTok == "") {
		if list, err := m.api.ListAssignments(ctx); err == nil {
			found := false
			for _, a := range list {
				if a.Endpoint == endpoint {
					pURL = a.RuntimeProxyInfo.URL
					pTok = a.RuntimeProxyInfo.Token
					found = true
					break
				}
			}
			if !found {
				m.appendLog(fmt.Sprintf("[STOP] 云端核验确认实例 %s 已不存在（可能已提前释放或被 Colab 回收）。", endpoint))
				if m.finishStoppedIfCurrent(opID) {
					m.invalidateCCUCache()
					m.appendLog("[STOP] Colab 实例已确认释放，状态已重置为已停止。")
				}
				return
			}
		}
	}

	if !force && pURL != "" && pTok != "" {
		m.appendLog("[STOP] 停机前正在同步 custom_nodes 插件到 Google Drive...")
		syncCtx, syncCancel := context.WithTimeout(ctx, 90*time.Second)
		rt := colab.NewRuntimeClient(pURL, pTok, kID, sID)
		if _, err := rt.Execute(syncCtx, colab.SyncPluginsCode, colab.ExecuteOptions{
			IdleTimeout: 80 * time.Second,
			OnOutput:    m.appendStreamOutput,
		}); err != nil {
			if !m.isCurrentOp(opID) || ctx.Err() != nil {
				syncCancel()
				return
			}
			m.appendLog("[STOP] 插件同步警告（将继续关机以防扣费）: " + err.Error())
		}
		_ = rt.ShutdownKernel(syncCtx)
		syncCancel()
	}

	if !m.isCurrentOp(opID) || ctx.Err() != nil {
		return
	}

	m.appendLog(fmt.Sprintf("[STOP] 正在释放 Colab 实例 %s 以停止计费...", endpoint))
	alreadyGone, err := m.unassignVerified(ctx, endpoint)
	if !m.isCurrentOp(opID) {
		return
	}
	if err != nil {
		m.appendLog("[STOP] 释放实例接口返回错误: " + err.Error())
		m.mu.Lock()
		if m.opSeq == opID {
			m.state = StateError
			m.errorMessage = "释放实例失败，请点击强制关机重试: " + err.Error()
		}
		m.mu.Unlock()
		return
	}

	if m.finishStoppedIfCurrent(opID) {
		m.invalidateCCUCache()
		if alreadyGone {
			m.appendLog(fmt.Sprintf("[STOP] 释放接口返回异常，但云端核验确认实例 %s 已释放，已停止计费。", endpoint))
		} else {
			m.appendLog("[STOP] Colab 实例已彻底关闭并释放资源，停止计费。")
		}
	}
}

func (m *Manager) finishStoppedIfRunning(ctx context.Context) bool {
	m.mu.Lock()
	if ctx.Err() != nil || m.state != StateRunning {
		m.mu.Unlock()
		return false
	}
	m.resetStoppedLocked()
	m.mu.Unlock()
	m.clearPersistedRuntime()
	return true
}

func (m *Manager) finishStoppedIfCurrent(opID uint64) bool {
	m.mu.Lock()
	if m.opSeq != opID {
		m.mu.Unlock()
		return false
	}
	m.resetStoppedLocked()
	m.opCancel = nil
	m.mu.Unlock()
	m.clearPersistedRuntime()
	return true
}

func (m *Manager) stopKeepaliveLocked() {
	if m.keepaliveCancel != nil {
		m.keepaliveCancel()
		m.keepaliveCancel = nil
	}
}

func (m *Manager) resetStoppedLocked() {
	m.stopKeepaliveLocked()
	m.state = StateStopped
	m.endpoint = ""
	m.proxyURL = ""
	m.proxyToken = ""
	m.proxyTokenExp = time.Time{}
	m.kernelID = ""
	m.sessionID = ""
	m.tunnelURL = nil
	m.comfyPassword = ""
	m.startedAt = nil
	m.lastHeartbeatAt = nil
	m.tunnelHealthy = false
	m.driveAuthURL = ""
	m.errorMessage = ""
}

func (m *Manager) clearPersistedRuntime() {
	_ = m.store.Update(func(st *PersistedState) {
		st.Endpoint = ""
		st.KernelID = ""
		st.SessionID = ""
	})
}

func (m *Manager) startKeepalive() {
	m.mu.Lock()
	if m.state != StateRunning {
		m.mu.Unlock()
		return
	}
	m.stopKeepaliveLocked()
	ctx, cancel := context.WithCancel(context.Background())
	m.keepaliveCancel = cancel
	interval := time.Duration(m.cfg.Colab.KeepaliveIntervalSec) * time.Second
	m.mu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		consecutiveTunnelFailures := 0

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.runKeepaliveTick(ctx, &consecutiveTunnelFailures)
			}
		}
	}()
}

func (m *Manager) runKeepaliveTick(ctx context.Context, consecutiveTunnelFailures *int) {
	if ctx.Err() != nil {
		return
	}
	m.mu.RLock()
	running := m.state == StateRunning
	m.mu.RUnlock()
	if !running {
		return
	}

	tickCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	rt, err := m.runtimeClient(tickCtx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errSessionPruned) {
			if m.finishStoppedIfRunning(ctx) {
				m.appendLog("[KEEPALIVE] 检测到 Colab 云端实例已被回收或停止，已更新为 STOPPED。")
			}
			return
		}
		m.appendLog("[KEEPALIVE] 刷新运行时令牌失败: " + err.Error())
		return
	}

	if _, err := rt.Execute(tickCtx, colab.KeepalivePingCode, colab.ExecuteOptions{IdleTimeout: 30 * time.Second}); err != nil {
		if ctx.Err() != nil {
			return
		}
		m.appendLog("[KEEPALIVE] 心跳执行异常: " + err.Error())
		return
	}

	now := time.Now()
	m.mu.RLock()
	curURL := m.tunnelURL
	curPW := m.comfyPassword
	stillRunning := m.state == StateRunning
	m.mu.RUnlock()
	if ctx.Err() != nil || !stillRunning {
		return
	}

	ok := curURL != nil && curPW != "" && m.probeTunnel(tickCtx, curURL, curPW)
	m.mu.Lock()
	if ctx.Err() != nil || m.state != StateRunning {
		m.mu.Unlock()
		return
	}
	m.lastHeartbeatAt = &now
	m.tunnelHealthy = ok
	m.mu.Unlock()

	if ok {
		*consecutiveTunnelFailures = 0
		m.appendLog("[KEEPALIVE] 心跳成功，上游隧道状态正常。")
		return
	}

	*consecutiveTunnelFailures++
	m.appendLog(fmt.Sprintf("[KEEPALIVE] 心跳成功，但隧道探活失败（连续第 %d 次），尝试自动重建 cloudflared...", *consecutiveTunnelFailures))
	res, err := rt.Execute(tickCtx, colab.RestartTunnelCode, colab.ExecuteOptions{IdleTimeout: 75 * time.Second})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		m.appendLog("[KEEPALIVE] 自动重建隧道失败: " + err.Error())
		return
	}
	if newRaw := extractTunnelURL(res.Stdout); newRaw != "" {
		if parsed, err := url.Parse(newRaw); err == nil {
			m.mu.Lock()
			if ctx.Err() == nil && m.state == StateRunning {
				m.tunnelURL = parsed
				m.tunnelHealthy = true
			}
			m.mu.Unlock()
			*consecutiveTunnelFailures = 0
			m.appendLog("[KEEPALIVE] 隧道自愈成功，已无缝热切换上游地址。")
		}
	}
}

var errSessionPruned = errors.New("colab session pruned on server")

func (m *Manager) runtimeClient(ctx context.Context) (*colab.RuntimeClient, error) {
	m.mu.RLock()
	ep := m.endpoint
	pURL := m.proxyURL
	pTok := m.proxyToken
	exp := m.proxyTokenExp
	kID := m.kernelID
	sID := m.sessionID
	m.mu.RUnlock()

	if ep == "" {
		return nil, errSessionPruned
	}

	// 若 proxy token 即将过期，则从 /tun/m/assignments 刷新
	if pURL == "" || pTok == "" || time.Until(exp) <= tokenRenewMargin {
		list, err := m.api.ListAssignments(ctx)
		if err != nil {
			return nil, err
		}
		found := false
		for _, a := range list {
			if a.Endpoint == ep {
				pURL = a.RuntimeProxyInfo.URL
				pTok = a.RuntimeProxyInfo.Token
				exp = time.Now().Add(time.Duration(a.RuntimeProxyInfo.TokenExpiresInSeconds) * time.Second)
				found = true
				break
			}
		}
		if !found {
			return nil, errSessionPruned
		}
		m.mu.Lock()
		m.proxyURL = pURL
		m.proxyToken = pTok
		m.proxyTokenExp = exp
		m.mu.Unlock()
	}

	return colab.NewRuntimeClient(pURL, pTok, kID, sID), nil
}

func (m *Manager) probeTunnel(ctx context.Context, target *url.URL, password string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(target.String(), "/")+"/system_stats", nil)
	if err != nil {
		return false
	}
	basic := base64.StdEncoding.EncodeToString([]byte(comfyBasicUser + ":" + password))
	req.Header.Set("Authorization", "Basic "+basic)
	resp, err := m.probe.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (m *Manager) getCCUInfoCached(ctx context.Context) *colab.CCUInfo {
	m.usageMu.Lock()
	if m.cachedCCU != nil && time.Since(m.cachedCCUAt) < 30*time.Second {
		cp := *m.cachedCCU
		m.usageMu.Unlock()
		return &cp
	}
	m.usageMu.Unlock()

	cCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	info, err := m.api.GetCCUInfo(cCtx)
	if err != nil || info == nil {
		m.usageMu.Lock()
		defer m.usageMu.Unlock()
		if m.cachedCCU != nil {
			cp := *m.cachedCCU
			return &cp
		}
		return nil
	}

	m.usageMu.Lock()
	m.cachedCCU = info
	m.cachedCCUAt = time.Now()
	cp := *info
	m.usageMu.Unlock()
	return &cp
}

func (m *Manager) invalidateCCUCache() {
	m.usageMu.Lock()
	m.cachedCCUAt = time.Time{}
	m.usageMu.Unlock()
}

func (m *Manager) appendStreamOutput(chunk string) {
	for _, line := range strings.Split(strings.ReplaceAll(chunk, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 对外输出日志脱敏，避免把原始 trycloudflare.com 域名暴露到前端日志框
		safe := redactURLRe.ReplaceAllString(line, "https://<hidden-tunnel>.trycloudflare.com")
		m.appendLog(safe)
	}
}

func (m *Manager) appendLog(line string) {
	stamped := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), line)
	m.logMu.Lock()
	m.logs = append(m.logs, stamped)
	if len(m.logs) > maxLogLines {
		m.logs = m.logs[len(m.logs)-maxLogLines:]
	}
	for ch := range m.subscribers {
		select {
		case ch <- stamped:
		default:
		}
	}
	m.logMu.Unlock()
}

func extractTunnelURL(output string) string {
	matches := tunnelURLRe.FindAllString(output, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}

func normalizeAccel(raw string) string {
	upper := strings.ToUpper(strings.TrimSpace(raw))
	if upper == "" || upper == "NONE" {
		return "CPU"
	}
	return upper
}
