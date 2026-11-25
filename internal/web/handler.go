package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"comfelle/internal/auth"
	"comfelle/internal/manager"
)

//go:embed assets/login.html assets/panel.html assets/dock.js
var assetsFS embed.FS

// Handler 负责 /panel/* 登录、控制面板页面、API 与 SSE 日志流。
type Handler struct {
	auth *auth.Authenticator
	mgr  *manager.Manager
	mux  *http.ServeMux
}

// New 创建控制面板路由处理器。
func New(a *auth.Authenticator, m *manager.Manager) *Handler {
	h := &Handler{
		auth: a,
		mgr:  m,
		mux:  http.NewServeMux(),
	}
	h.mux.HandleFunc("GET /panel/login", h.handleLoginPage)
	h.mux.HandleFunc("POST /panel/login", h.handleLoginSubmit)
	h.mux.HandleFunc("POST /panel/logout", h.handleLogout)
	h.mux.HandleFunc("GET /panel", h.handlePanelPage)
	h.mux.HandleFunc("GET /panel/{$}", h.handlePanelPage)
	h.mux.HandleFunc("GET /panel/dock.js", h.handleDockJS)

	h.mux.HandleFunc("GET /panel/api/status", h.handleStatus)
	h.mux.HandleFunc("GET /panel/api/logs", h.handleLogsSSE)
	h.mux.HandleFunc("POST /panel/api/start", h.handleStart)
	h.mux.HandleFunc("POST /panel/api/stop", h.handleStop)
	h.mux.HandleFunc("POST /panel/api/drive-confirm", h.handleDriveConfirm)
	h.mux.HandleFunc("POST /panel/api/restart-comfy", h.handleRestartComfy)
	h.mux.HandleFunc("POST /panel/api/sync-plugins", h.handleSyncPlugins)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if h.auth.VerifyRequest(r) {
		http.Redirect(w, r, "/panel", http.StatusFound)
		return
	}
	data, _ := assetsFS.ReadFile("assets/login.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (h *Handler) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求格式无效"})
		return
	}

	ok, retryAfter := h.auth.Authenticate(r, strings.TrimSpace(req.Username), req.Password)
	if retryAfter > 0 {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("失败次数过多，请在 %d 秒后重试", int(retryAfter.Seconds())+1),
		})
		return
	}
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "用户名或密码错误"})
		return
	}

	if err := h.auth.IssueSession(w, r); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "签发会话失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "redirect": "/panel"})
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireSameOriginCSRF(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "非法跨站请求"})
		return
	}
	h.auth.ClearSession(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handlePanelPage(w http.ResponseWriter, r *http.Request) {
	data, _ := assetsFS.ReadFile("assets/panel.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (h *Handler) handleDockJS(w http.ResponseWriter, r *http.Request) {
	data, _ := assetsFS.ReadFile("assets/dock.js")
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, h.mgr.Status(r.Context()))
}

func (h *Handler) handleLogsSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 告知前置 Nginx 立即刷新 SSE

	history, ch, unsub := h.mgr.SubscribeLogs()
	defer unsub()

	sendLine := func(line string) {
		payload, _ := json.Marshal(line)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		flusher.Flush()
	}

	for _, line := range history {
		sendLine(line)
	}

	pingTicker := time.NewTicker(20 * time.Second)
	defer pingTicker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case line, open := <-ch:
			if !open {
				return
			}
			sendLine(line)
		case <-pingTicker.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (h *Handler) handleStart(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireSameOriginCSRF(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "非法跨站请求"})
		return
	}
	var req struct {
		GPU     string `json:"gpu"`
		HighMem bool   `json:"high_mem"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	if err := h.mgr.Start(req.GPU, req.HighMem); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleStop(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireSameOriginCSRF(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "非法跨站请求"})
		return
	}
	var req struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	if err := h.mgr.Stop(req.Force); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleDriveConfirm(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireSameOriginCSRF(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "非法跨站请求"})
		return
	}
	if err := h.mgr.ConfirmDriveAuth(); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleRestartComfy(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireSameOriginCSRF(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "非法跨站请求"})
		return
	}
	if err := h.mgr.RestartComfy(); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleSyncPlugins(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireSameOriginCSRF(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "非法跨站请求"})
		return
	}
	if err := h.mgr.SyncPluginsNow(); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
