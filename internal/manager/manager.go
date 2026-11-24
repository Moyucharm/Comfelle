package manager

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

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
	tunnelURLRe  = regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)
	redactURLRe  = regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)
	stageEventRe = regexp.MustCompile(`^\[STAGE\s+(\d{1,3})\]\s+(.+)$`)
)

// ColabAPI 定义 Manager 依赖的 Colab 后端接口（便于测试解耦）。
type ColabAPI interface {
	ListAssignments(ctx context.Context) ([]colab.Assignment, error)
	Assign(ctx context.Context, notebookHash uuid.UUID, gpu string, highMem bool) (*colab.Assignment, error)
	Unassign(ctx context.Context, endpoint string) error
	GetCCUInfo(ctx context.Context) (*colab.CCUInfo, error)
	PropagateDriveCredentials(ctx context.Context, endpoint string, dryRun bool) (*colab.DrivePropagationResult, error)
}

// StatusSnapshot 对前端 /panel/api/status 暴露的安全状态视图（不含 Tunnel URL 与密码）。
type StatusSnapshot struct {
	State              State      `json:"state"`
	GPU                string     `json:"gpu"`
	DefaultGPU         string     `json:"default_gpu"`
	HighMem            bool       `json:"high_mem"`
	RcloneMode         bool       `json:"rclone_mode"`
	DriveAuthURL       string     `json:"drive_auth_url,omitempty"`
	ErrorMessage       string     `json:"error_message,omitempty"`
	ProgressPct        int        `json:"progress_pct,omitempty"`
	ProgressStage      string     `json:"progress_stage,omitempty"`
	ProgressDetail     string     `json:"progress_detail,omitempty"`
	BusySince          *time.Time `json:"busy_since,omitempty"`
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
	progressPct        int
	progressStage      string
	progressDetail     string
	busySince          *time.Time
	startedAt          *time.Time
	lastHeartbeatAt    *time.Time
	tunnelHealthy      bool
	opSeq              uint64
	opCancel           context.CancelFunc
	keepaliveCancel    context.CancelFunc

	usageMu     sync.Mutex
	cachedCCU   *colab.CCUInfo
	cachedCCUAt time.Time

	logMu       sync.Mutex
	logs        []string
	subscribers map[chan string]struct{}
}

// New 创建实例管理器。
func New(cfg *config.Config, api ColabAPI, store *StateStore) *Manager {
	return &Manager{
		cfg:         cfg,
		api:         api,
		store:       store,
		probe:       &http.Client{Timeout: 12 * time.Second},
		state:       StateStopped,
		gpu:         cfg.Colab.DefaultGPU,
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

// Status 返回当前安全状态快照（附带缓存的 Compute Units 信息与结构化进度）。
func (m *Manager) Status(ctx context.Context) StatusSnapshot {
	m.mu.RLock()
	snap := StatusSnapshot{
		State:              m.state,
		GPU:                m.gpu,
		DefaultGPU:         m.cfg.Colab.DefaultGPU,
		HighMem:            m.cfg.Colab.HighMem,
		RcloneMode:         strings.TrimSpace(m.cfg.Colab.RcloneConfig) != "",
		DriveAuthURL:       m.driveAuthURL,
		ErrorMessage:       m.errorMessage,
		ProgressPct:        m.progressPct,
		ProgressStage:      m.progressStage,
		ProgressDetail:     m.progressDetail,
		KeepaliveIntervalS: m.cfg.Colab.KeepaliveIntervalSec,
		TunnelHealthy:      m.tunnelHealthy,
		HasActiveVM:        m.endpoint != "" || m.store.Snapshot().Endpoint != "",
	}
	if m.busySince != nil {
		t := *m.busySince
		snap.BusySince = &t
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

// Start 异步启动（或复用）Colab 实例、挂载持久化存储并部署 ComfyUI。
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
	now := time.Now()
	m.state = StateStarting
	m.gpu = gpu
	m.errorMessage = ""
	m.driveAuthURL = ""
	m.busySince = &now
	m.progressPct = 10
	m.progressStage = "准备启动实例"
	m.progressDetail = fmt.Sprintf("目标规格 %s", gpu)
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

// Stop 停止保活、可选先同步插件到持久化存储，再释放本系统托管的 Colab VM 停止计费。
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
		return errors.New("正在关闭中，请稍候；若卡住可点击「强制关机」")
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

	now := time.Now()
	m.state = StateStopping
	m.tunnelURL = nil
	m.comfyPassword = ""
	m.tunnelHealthy = false
	m.driveAuthURL = ""
	m.errorMessage = ""
	m.busySince = &now
	m.progressPct = 20
	m.progressStage = "正在关闭实例"
	m.progressDetail = "准备释放资源并停止计费"
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

// SyncPluginsNow 手动立即将 custom_nodes 打包同步到持久化存储。
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
		m.appendLog("[SYNC] 开始手动同步 custom_nodes 到持久化存储...")
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

func (m *Manager) setProgress(pct int, stage, detail string) {
	m.mu.Lock()
	if pct > 0 {
		m.progressPct = pct
	}
	if stage != "" {
		m.progressStage = stage
	}
	if detail != "" {
		m.progressDetail = detail
	}
	m.mu.Unlock()
}

func (m *Manager) clearProgressLocked() {
	m.progressPct = 0
	m.progressStage = ""
	m.progressDetail = ""
	m.busySince = nil
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
		safe := redactURLRe.ReplaceAllString(line, "https://<hidden-tunnel>.trycloudflare.com")
		if sm := stageEventRe.FindStringSubmatch(safe); len(sm) == 3 {
			pct, _ := strconv.Atoi(sm[1])
			m.setProgress(pct, sm[2], sm[2])
			m.appendLog("[STAGE] " + sm[2])
			continue
		}
		m.setProgress(0, "", safe)
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
