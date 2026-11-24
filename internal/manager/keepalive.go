package manager

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"colab-comfyui/internal/colab"
)

var errSessionPruned = errors.New("colab session pruned on server")

func (m *Manager) stopKeepaliveLocked() {
	if m.keepaliveCancel != nil {
		m.keepaliveCancel()
		m.keepaliveCancel = nil
	}
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

	if _, err := m.execInRuntime(tickCtx, colab.KeepalivePingCode, colab.ExecuteOptions{IdleTimeout: 30 * time.Second}); err != nil {
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errSessionPruned) {
			if m.finishStoppedIfRunning(ctx) {
				m.appendLog("[KEEPALIVE] 检测到 Colab 云端实例已被回收或停止，已更新为 STOPPED。")
			}
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
	res, err := m.execInRuntime(tickCtx, colab.RestartTunnelCode, colab.ExecuteOptions{IdleTimeout: 75 * time.Second})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		m.appendLog("[KEEPALIVE] 自动重建隧道失败: " + err.Error())
		return
	}
	newRaw := extractTunnelURL(res.Stdout)
	parsed, parseErr := url.Parse(newRaw)
	if newRaw == "" || parseErr != nil {
		m.appendLog("[KEEPALIVE] 重建隧道未返回有效的 Cloudflare 地址。")
		return
	}

	tunnelOK := false
	for i := 0; i < 3; i++ {
		if m.probeTunnel(tickCtx, parsed, curPW) {
			tunnelOK = true
			break
		}
		select {
		case <-tickCtx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}

	m.mu.Lock()
	if ctx.Err() == nil && m.state == StateRunning {
		m.tunnelURL = parsed
		m.tunnelHealthy = tunnelOK
	}
	m.mu.Unlock()

	if tunnelOK {
		*consecutiveTunnelFailures = 0
		m.appendLog("[KEEPALIVE] 隧道自愈成功，已验证连通并无缝热切换上游地址。")
	} else {
		m.appendLog("[KEEPALIVE] 隧道进程已重建，但探活暂未返回 200，将在下一周期继续检查。")
	}
}

func (m *Manager) execInRuntime(ctx context.Context, code string, opts colab.ExecuteOptions) (*colab.ExecuteResult, error) {
	m.execMu.Lock()
	defer m.execMu.Unlock()

	rt, err := m.runtimeClient(ctx)
	if err != nil {
		return nil, err
	}
	prevKernel := rt.KernelID
	res, execErr := rt.Execute(ctx, code, opts)
	if rt.KernelID != "" && rt.KernelID != prevKernel {
		newKID := rt.KernelID
		m.mu.Lock()
		ep := m.endpoint
		if ep != "" {
			m.kernelID = newKID
		}
		m.mu.Unlock()
		if ep != "" {
			_ = m.store.Update(func(st *PersistedState) {
				if st.Endpoint == ep {
					st.KernelID = newKID
				}
			})
		}
	}
	return res, execErr
}

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
