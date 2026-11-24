package colab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// RuntimeClient 通过 Colab Runtime Proxy 与远端 Jupyter Kernel 交互（HTTP + WebSocket）。
type RuntimeClient struct {
	BaseURL    string
	ProxyToken string
	KernelID   string
	SessionID  string
	HTTPClient *http.Client
}

// NewRuntimeClient 构造针对特定 Colab VM 的 Jupyter 运行时客户端。
func NewRuntimeClient(baseURL, proxyToken, kernelID, sessionID string) *RuntimeClient {
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	return &RuntimeClient{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		ProxyToken: proxyToken,
		KernelID:   kernelID,
		SessionID:  sessionID,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// ExecuteOptions 控制单次远端 Python 执行的行为与回调。
type ExecuteOptions struct {
	AllowStdin         bool
	IdleTimeout        time.Duration
	OnOutput           func(text string)
	OnDriveAuthRequest func(ctx context.Context, colabMsgID string) error
}

// ExecuteResult 记录一次代码执行的聚合输出。
type ExecuteResult struct {
	Stdout string
	Stderr string
	Error  string
}

type jupyterHeader struct {
	MsgID    string `json:"msg_id"`
	MsgType  string `json:"msg_type"`
	Username string `json:"username"`
	Session  string `json:"session"`
	Date     string `json:"date"`
	Version  string `json:"version"`
}

type jupyterMessage struct {
	Header       jupyterHeader  `json:"header"`
	ParentHeader map[string]any `json:"parent_header"`
	Metadata     map[string]any `json:"metadata"`
	Content      map[string]any `json:"content"`
	Buffers      []any          `json:"buffers"`
	Channel      string         `json:"channel"`
	MsgType      string         `json:"msg_type,omitempty"`
}

// EnsureKernel 复用 VM 上已有的 Python 内核，若无则创建一个新内核。
func (r *RuntimeClient) EnsureKernel(ctx context.Context) (string, error) {
	if r.KernelID != "" {
		if ok, err := r.kernelExists(ctx, r.KernelID); err == nil && ok {
			return r.KernelID, nil
		}
		r.KernelID = ""
	}

	kernels, err := r.listKernels(ctx)
	if err == nil && len(kernels) > 0 {
		r.KernelID = kernels[0]
		return r.KernelID, nil
	}

	payload, _ := json.Marshal(map[string]string{"name": "python3"})
	var raw []byte
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := r.newRequest(ctx, http.MethodPost, "/api/kernels", bytes.NewReader(payload))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := r.HTTPClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("创建远端 Jupyter Kernel 失败: %w", err)
		} else {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				raw = body
				lastErr = nil
				break
			}
			lastErr = fmt.Errorf("创建远端 Kernel 返回 HTTP %d: %s", resp.StatusCode, string(body))
			if resp.StatusCode < 500 {
				return "", lastErr
			}
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}
	}
	if lastErr != nil {
		return "", lastErr
	}

	var model struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &model); err != nil || model.ID == "" {
		return "", fmt.Errorf("解析 Kernel 创建响应失败: %s", string(raw))
	}
	r.KernelID = model.ID
	return r.KernelID, nil
}

// ShutdownKernel 请求停止当前 Jupyter Kernel（通常在释放 VM 前调用）。
func (r *RuntimeClient) ShutdownKernel(ctx context.Context) error {
	if r.KernelID == "" {
		return nil
	}
	req, err := r.newRequest(ctx, http.MethodDelete, "/api/kernels/"+url.PathEscape(r.KernelID), nil)
	if err != nil {
		return err
	}
	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	r.KernelID = ""
	return nil
}

// Execute 在远端 Jupyter Kernel 中执行 Python 代码，实时回调输出并可拦截 Drive 授权请求。
func (r *RuntimeClient) Execute(ctx context.Context, code string, opts ExecuteOptions) (*ExecuteResult, error) {
	if _, err := r.EnsureKernel(ctx); err != nil {
		return nil, err
	}

	wsURL, err := r.channelsWSURL()
	if err != nil {
		return nil, err
	}

	headers := http.Header{
		"X-Colab-Runtime-Proxy-Token": {r.ProxyToken},
		"Authorization":               {"Bearer " + r.ProxyToken},
		"X-Colab-Client-Agent":        {colabClientAgent},
	}

	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 30 * time.Second,
	}
	var conn *websocket.Conn
	var dialErr error
	for attempt := 0; attempt < 3; attempt++ {
		var resp *http.Response
		conn, resp, dialErr = dialer.DialContext(ctx, wsURL, headers)
		if dialErr == nil {
			break
		}
		status := 0
		if resp != nil {
			status = resp.StatusCode
			_ = resp.Body.Close()
		}
		dialErr = fmt.Errorf("连接 Jupyter Kernel WebSocket 失败 (HTTP %d): %w", status, dialErr)
		if status >= 400 && status < 500 {
			return nil, dialErr
		}
		if attempt < 2 {
			wait := time.Duration(1<<attempt) * time.Second
			if opts.OnOutput != nil {
				opts.OnOutput(fmt.Sprintf("[WS] WebSocket 握手失败，%ds 后第 %d 次重试...\n", int(wait.Seconds()), attempt+2))
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
	}
	if dialErr != nil {
		return nil, dialErr
	}
	defer conn.Close()

	doneWatch := make(chan struct{})
	go func() {
		// 定时发送 WebSocket Ping，防止 Colab 代理或中转链路在长耗时执行静默期间触发 5 分钟空闲断连 (close 1006)。
		pingTicker := time.NewTicker(25 * time.Second)
		defer pingTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = conn.Close()
				return
			case <-doneWatch:
				return
			case <-pingTicker.C:
				_ = conn.WriteControl(websocket.PingMessage, []byte("ka"), time.Now().Add(10*time.Second))
			}
		}
	}()
	defer close(doneWatch)

	msgID := uuid.NewString()
	execReq := jupyterMessage{
		Header: jupyterHeader{
			MsgID:    msgID,
			MsgType:  "execute_request",
			Username: "colab-comfyui",
			Session:  r.SessionID,
			Date:     time.Now().UTC().Format(time.RFC3339Nano),
			Version:  "5.3",
		},
		ParentHeader: map[string]any{},
		Metadata:     map[string]any{},
		Content: map[string]any{
			"code":             code,
			"silent":           false,
			"store_history":    false,
			"user_expressions": map[string]any{},
			"allow_stdin":      opts.AllowStdin,
			"stop_on_error":    true,
		},
		Buffers: []any{},
		Channel: "shell",
	}
	if err := conn.WriteJSON(execReq); err != nil {
		return nil, fmt.Errorf("发送 execute_request 失败: %w", err)
	}

	var stdoutBuf, stderrBuf strings.Builder
	var execErrText string
	gotIdle := false
	gotReply := false

	for !(gotIdle && gotReply) {
		if opts.IdleTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(opts.IdleTimeout))
		} else {
			_ = conn.SetReadDeadline(time.Time{})
		}

		msgType, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if gotIdle || gotReply {
				break
			}
			return nil, fmt.Errorf("读取 Jupyter 消息失败: %w", err)
		}
		if msgType != websocket.TextMessage && msgType != websocket.BinaryMessage {
			continue
		}

		msg, ok := decodeJupyterMessage(data)
		if !ok {
			continue
		}

		mType := msg.Header.MsgType
		if mType == "" {
			mType = msg.MsgType
		}

		if mType == "colab_request" {
			if err := r.handleColabRequest(ctx, conn, msg, opts); err != nil {
				return nil, err
			}
			continue
		}

		if mType == "status" {
			if state, _ := msg.Content["execution_state"].(string); state == "dead" || state == "restarting" {
				return nil, fmt.Errorf("远端 Jupyter 内核状态变为 %s，执行中断", state)
			}
		}

		parentID, _ := msg.ParentHeader["msg_id"].(string)
		if parentID != msgID {
			continue
		}

		switch mType {
		case "stream":
			name, _ := msg.Content["name"].(string)
			text, _ := msg.Content["text"].(string)
			if name == "stderr" {
				stderrBuf.WriteString(text)
			} else {
				stdoutBuf.WriteString(text)
			}
			if opts.OnOutput != nil && text != "" {
				opts.OnOutput(text)
			}

		case "error":
			ename, _ := msg.Content["ename"].(string)
			evalue, _ := msg.Content["evalue"].(string)
			errLine := fmt.Sprintf("%s: %s", ename, evalue)
			execErrText = errLine
			stderrBuf.WriteString(errLine + "\n")
			if opts.OnOutput != nil {
				opts.OnOutput("[ERROR] " + errLine + "\n")
			}

		case "status":
			if state, _ := msg.Content["execution_state"].(string); state == "idle" {
				gotIdle = true
			}

		case "execute_reply":
			gotReply = true
			if st, _ := msg.Content["status"].(string); st == "error" && execErrText == "" {
				ename, _ := msg.Content["ename"].(string)
				evalue, _ := msg.Content["evalue"].(string)
				execErrText = fmt.Sprintf("%s: %s", ename, evalue)
			}
		}
	}

	res := &ExecuteResult{
		Stdout: stdoutBuf.String(),
		Stderr: stderrBuf.String(),
		Error:  execErrText,
	}
	if execErrText != "" {
		return res, errors.New(execErrText)
	}
	return res, nil
}

func (r *RuntimeClient) handleColabRequest(ctx context.Context, conn *websocket.Conn, msg jupyterMessage, opts ExecuteOptions) error {
	reqMap, _ := msg.Content["request"].(map[string]any)
	authType, _ := reqMap["authType"].(string)
	if authType != "dfs_ephemeral" {
		return nil
	}
	colabMsgID, _ := msg.Metadata["colab_msg_id"].(string)
	if opts.OnDriveAuthRequest == nil {
		return errors.New("收到 Drive 授权请求但未配置处理器")
	}

	_ = conn.SetReadDeadline(time.Time{})
	if err := opts.OnDriveAuthRequest(ctx, colabMsgID); err != nil {
		return err
	}

	parentHeader := map[string]any{
		"msg_id":   msg.Header.MsgID,
		"msg_type": msg.Header.MsgType,
		"username": msg.Header.Username,
		"session":  msg.Header.Session,
		"date":     msg.Header.Date,
		"version":  msg.Header.Version,
	}
	reply := jupyterMessage{
		Header: jupyterHeader{
			MsgID:    uuid.NewString(),
			MsgType:  "input_reply",
			Username: "colab-comfyui",
			Session:  r.SessionID,
			Date:     time.Now().UTC().Format(time.RFC3339Nano),
			Version:  "5.3",
		},
		ParentHeader: parentHeader,
		Metadata:     map[string]any{},
		Content: map[string]any{
			"value": map[string]any{
				"type":         "colab_reply",
				"colab_msg_id": colabMsgID,
			},
		},
		Buffers: []any{},
		Channel: "stdin",
	}
	if err := conn.WriteJSON(reply); err != nil {
		return fmt.Errorf("回写 colab_reply 失败: %w", err)
	}
	return nil
}

func (r *RuntimeClient) listKernels(ctx context.Context) ([]string, error) {
	req, err := r.newRequest(ctx, http.MethodGet, "/api/kernels", nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /api/kernels HTTP %d", resp.StatusCode)
	}
	var list []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list))
	for _, k := range list {
		if k.ID != "" {
			ids = append(ids, k.ID)
		}
	}
	return ids, nil
}

func (r *RuntimeClient) kernelExists(ctx context.Context, id string) (bool, error) {
	kernels, err := r.listKernels(ctx)
	if err != nil {
		return false, err
	}
	for _, k := range kernels {
		if k == id {
			return true, nil
		}
	}
	return false, nil
}

func (r *RuntimeClient) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, r.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Colab-Runtime-Proxy-Token", r.ProxyToken)
	req.Header.Set("Authorization", "Bearer "+r.ProxyToken)
	req.Header.Set("X-Colab-Client-Agent", colabClientAgent)
	return req, nil
}

func (r *RuntimeClient) channelsWSURL() (string, error) {
	u, err := url.Parse(r.BaseURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/kernels/" + url.PathEscape(r.KernelID) + "/channels"
	q := u.Query()
	q.Set("session_id", r.SessionID)
	q.Set("token", r.ProxyToken)
	q.Set("colab-runtime-proxy-token", r.ProxyToken)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func decodeJupyterMessage(raw []byte) (jupyterMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return jupyterMessage{}, false
	}
	if trimmed[0] == '{' {
		var msg jupyterMessage
		if err := json.Unmarshal(trimmed, &msg); err == nil {
			return msg, true
		}
	}
	idx := bytes.IndexByte(raw, '{')
	if idx > 0 {
		var msg jupyterMessage
		if err := json.Unmarshal(raw[idx:], &msg); err == nil && (msg.Header.MsgType != "" || msg.MsgType != "") {
			return msg, true
		}
	}
	return jupyterMessage{}, false
}
