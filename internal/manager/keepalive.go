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
