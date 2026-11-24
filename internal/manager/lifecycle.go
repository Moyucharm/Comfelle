package manager

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"colab-comfyui/internal/auth"
	"colab-comfyui/internal/colab"
)

func (m *Manager) isCurrentOp(opID uint64) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.opSeq == opID
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
		m.clearPersistedRuntime()
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
		m.appendLog("[INIT] 热接管探测失败（可点接管并继续重新挂载）: " + err.Error())
		return
	}

	rawURL := extractTunnelURL(res.Stdout)
	if rawURL == "" {
		m.appendLog("[INIT] 发现活跃 VM 但 ComfyUI 尚未启动，请在面板点击「接管并继续」复用该实例。")
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
	started := now
	if persisted.StartedAt != nil && !persisted.StartedAt.IsZero() {
		started = *persisted.StartedAt
	}
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
	m.startedAt = &started
	m.lastHeartbeatAt = &now
	m.tunnelHealthy = m.probeTunnel(ctx, parsed, pw)
	m.errorMessage = ""
	m.clearProgressLocked()
	m.mu.Unlock()

	_ = m.store.Update(func(st *PersistedState) {
		st.Endpoint = picked.Endpoint
		st.KernelID = rt.KernelID
		st.SessionID = rt.SessionID
		st.Accelerator = normalizeAccel(picked.Accelerator)
		if st.StartedAt == nil || st.StartedAt.IsZero() {
			st.StartedAt = &started
		}
	})

	m.startKeepalive()
	m.appendLog("[INIT] 热接管成功！已轮换上游认证并恢复反向代理与保活守护。")
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
		m.clearProgressLocked()
		if released {
			m.endpoint = ""
			m.proxyURL = ""
			m.proxyToken = ""
			m.proxyTokenExp = time.Time{}
			m.kernelID = ""
			m.sessionID = ""
			m.startedAt = nil
		}
		m.mu.Unlock()

		if released {
			_ = m.store.Update(func(st *PersistedState) {
				if st.Endpoint == allocatedEndpoint {
					st.Endpoint = ""
					st.KernelID = ""
					st.SessionID = ""
					st.StartedAt = nil
				}
			})
		}
	}

	m.setProgress(12, "分配或复用 Colab 实例", fmt.Sprintf("正在申请规格 %s", gpu))
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
		assigned, err = m.api.Assign(ctx, nbh, gpu, highMem)
		if err != nil {
			fail("分配 Colab 实例失败", err)
			return
		}
		m.appendLog(fmt.Sprintf("[1/4] 新实例已就绪: %s (%s)", assigned.Endpoint, normalizeAccel(assigned.Accelerator)))
	}
	allocatedEndpoint = assigned.Endpoint
	allocAt := time.Now()
	var instanceStarted time.Time
	_ = m.store.Update(func(st *PersistedState) {
		if st.Endpoint != assigned.Endpoint || st.StartedAt == nil || st.StartedAt.IsZero() {
			st.StartedAt = &allocAt
		}
		instanceStarted = *st.StartedAt
		st.Endpoint = assigned.Endpoint
		st.Accelerator = normalizeAccel(assigned.Accelerator)
	})

	m.mu.Lock()
	m.endpoint = assigned.Endpoint
	m.gpu = normalizeAccel(assigned.Accelerator)
	m.proxyURL = assigned.RuntimeProxyInfo.URL
	m.proxyToken = assigned.RuntimeProxyInfo.Token
	m.proxyTokenExp = time.Now().Add(time.Duration(assigned.RuntimeProxyInfo.TokenExpiresInSeconds) * time.Second)
	m.startedAt = &instanceStarted
	m.mu.Unlock()

	m.setProgress(18, "连接远端 Jupyter 内核", assigned.Endpoint)
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

	// 第 2 步：检查并挂载持久化存储（优先 rclone 免弹窗挂载，否则回落到 Colab 原生 Drive 挂载）
	m.setProgress(24, "检查并挂载持久化存储", "检查当前挂载状态")
	m.appendLog("[2/4] 检查持久化存储挂载状态...")
	chk, err := rt.Execute(ctx, colab.CheckDriveMountCode, colab.ExecuteOptions{IdleTimeout: 30 * time.Second})
	if err != nil {
		fail("检查存储挂载失败", err)
		return
	}
	if strings.TrimSpace(chk.Stdout) == "DRIVE_IS_MOUNTED" {
		m.appendLog("[2/4] 持久化存储已处于挂载状态，无需重复挂载。")
	} else if rcloneCfgPath := strings.TrimSpace(m.cfg.Colab.RcloneConfig); rcloneCfgPath != "" {
		if err := m.mountRclone(ctx, rt, rcloneCfgPath, m.cfg.Colab.RcloneRemote); err != nil {
			fail("rclone 挂载持久化存储失败", err)
			return
		}
		m.appendLog("[2/4] rclone 持久化存储挂载完成！")
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
	m.setProgress(34, "部署 ComfyUI 与加密隧道", "正在准备远端脚本")

	pw, err := auth.RandomHex(24)
	if err != nil {
		fail("生成上游临时密码失败", err)
		return
	}

	m.appendLog("[3/4] 正在部署 ComfyUI、配置鉴权并拉起加密隧道...")
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
	m.setProgress(96, "验证加密隧道连通性", "探测 ComfyUI 就绪接口")
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
	started := now
	if cur := m.store.Snapshot().StartedAt; cur != nil && !cur.IsZero() {
		started = *cur
	}
	m.mu.Lock()
	if m.opSeq != opID || ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	m.state = StateRunning
	m.tunnelURL = parsedURL
	m.comfyPassword = pw
	m.startedAt = &started
	m.lastHeartbeatAt = &now
	m.tunnelHealthy = true
	m.errorMessage = ""
	m.clearProgressLocked()
	m.mu.Unlock()

	m.invalidateCCUCache()
	m.startKeepalive()
	m.appendLog("[READY] ComfyUI 实例已就绪！自动反代、上游自动鉴权与保活守护均已生效。")
}

func (m *Manager) mountRclone(ctx context.Context, rt *colab.RuntimeClient, cfgPath, remote string) error {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("读取 rclone 配置文件 %q 失败: %w", cfgPath, err)
	}
	confText := strings.TrimSpace(string(raw))
	if confText == "" {
		return fmt.Errorf("rclone 配置文件 %q 为空", cfgPath)
	}
	remote = strings.TrimSpace(remote)
	if remote == "" {
		remote = "gdrive:Colab/ComfyUI"
	}
	m.setProgress(28, "通过 rclone 挂载持久化存储", remote)
	m.appendLog(fmt.Sprintf("[2/4] 使用 rclone 挂载远端存储 %s（免弹窗零重复授权）...", remote))
	_, err = rt.Execute(ctx, colab.BuildRcloneMountCode(confText+"\n", remote), colab.ExecuteOptions{
		IdleTimeout: 4 * time.Minute,
		OnOutput:    m.appendStreamOutput,
	})
	return err
}

func (m *Manager) mountDriveWithHook(ctx context.Context, rt *colab.RuntimeClient, endpoint string) error {
	m.setProgress(26, "挂载 Google Drive", "发起 Colab 交互式挂载授权")
	m.appendLog("[2/4] 未配置 rclone，发起 Colab 交互式 Google Drive 挂载授权...")

	_, err := rt.Execute(ctx, colab.MountDriveCode, colab.ExecuteOptions{
		AllowStdin:  true,
		IdleTimeout: 10 * time.Minute,
		OnOutput:    m.appendStreamOutput,
		OnDriveAuthRequest: func(reqCtx context.Context, colabMsgID string) error {
			dryRes, err := m.api.PropagateDriveCredentials(reqCtx, endpoint, true)
			if err != nil {
				return err
			}
			if !dryRes.Success {
				if dryRes.UnauthorizedRedirectURI == "" {
					return errors.New("Colab 返回 Drive 未授权，但未提供授权链接")
				}
				m.appendLog("[AUTH] 需要确认 Google Drive 授权，请在控制台点击授权链接并在完成后点击「我已授权，继续」。")
				confirmCh := make(chan struct{}, 1)
				m.mu.Lock()
				m.state = StateWaitingDriveAuth
				m.driveAuthURL = dryRes.UnauthorizedRedirectURI
				m.driveAuthConfirmCh = confirmCh
				m.progressPct = 28
				m.progressStage = "等待确认 Google Drive 授权"
				m.progressDetail = "请在新窗口完成授权并点击继续"
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
				m.progressPct = 30
				m.progressStage = "正在挂载 Google Drive"
				m.progressDetail = "等待远端挂载完成"
				m.mu.Unlock()
			}

			if _, err := m.api.PropagateDriveCredentials(reqCtx, endpoint, false); err != nil {
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

	// 若缺少 proxyURL/proxyToken，按精确 endpoint 补查令牌；若云端已不存在则直接收尾。
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
		m.setProgress(45, "停机前同步插件归档", "正在打包并上传 custom_nodes")
		m.appendLog("[STOP] 停机前正在同步 custom_nodes 插件到持久化存储...")
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

	m.setProgress(82, "释放 Colab 实例停止计费", endpoint)
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
			m.clearProgressLocked()
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
	m.clearProgressLocked()
}

func (m *Manager) clearPersistedRuntime() {
	_ = m.store.Update(func(st *PersistedState) {
		st.Endpoint = ""
		st.KernelID = ""
		st.SessionID = ""
		st.StartedAt = nil
	})
}
