package colab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	defaultColabDomain = "https://colab.research.google.com"
	xssiPrefix         = ")]}'\n"
	colabClientAgent   = "colab-cli"
)

// RuntimeProxyInfo 描述单个 Colab VM 的 Jupyter 代理地址与短期访问令牌。
type RuntimeProxyInfo struct {
	Token                 string `json:"token"`
	TokenExpiresInSeconds int    `json:"tokenExpiresInSeconds"`
	URL                   string `json:"url"`
}

// Assignment 表示一个运行中的 Colab VM 实例分配记录。
type Assignment struct {
	Accelerator      string           `json:"accelerator"`
	Endpoint         string           `json:"endpoint"`
	Variant          any              `json:"variant"`
	MachineShape     any              `json:"machineShape"`
	RuntimeProxyInfo RuntimeProxyInfo `json:"runtimeProxyInfo"`
}

// CCUInfo 表示账号 Compute Units 余额与实时消耗速率。
type CCUInfo struct {
	CurrentBalance        float64 `json:"currentBalance"`
	ConsumptionRateHourly float64 `json:"consumptionRateHourly"`
	AssignmentsCount      int     `json:"assignmentsCount"`
}

// DrivePropagationResult 表示 credentials-propagation 接口返回的校验结果。
type DrivePropagationResult struct {
	Success                 bool   `json:"success"`
	UnauthorizedRedirectURI string `json:"unauthorized_redirect_uri"`
	Token                   string `json:"token"`
}

// APIError 包装 Colab HTTP 后端返回的非 2xx 错误。
type APIError struct {
	StatusCode int
	Method     string
	Endpoint   string
	Body       string
}

func (e *APIError) Error() string {
	detail := summarizeErrorBody(e.StatusCode, e.Body)
	if detail == "" {
		return fmt.Sprintf("Colab API %s %s 返回 HTTP %d", e.Method, e.Endpoint, e.StatusCode)
	}
	return fmt.Sprintf("Colab API %s %s 返回 HTTP %d: %s", e.Method, e.Endpoint, e.StatusCode, detail)
}

func summarizeErrorBody(status int, body string) string {
	s := strings.TrimSpace(body)
	if s == "" {
		return http.StatusText(status)
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html") {
		if start := strings.Index(lower, "<title>"); start >= 0 {
			rest := s[start+len("<title>"):]
			restLower := lower[start+len("<title>"):]
			if end := strings.Index(restLower, "</title>"); end >= 0 {
				if title := strings.TrimSpace(rest[:end]); title != "" {
					return title
				}
			}
		}
		return http.StatusText(status)
	}
	runes := []rune(s)
	if len(runes) > 300 {
		return string(runes[:300]) + "..."
	}
	return s
}

// Client 封装 Google Colab 会话分配、释放、计费查询与 Drive 凭据传播协议。
type Client struct {
	domain     string
	authUser   string
	httpClient *http.Client
}

// NewClient 创建 Colab API 客户端。
func NewClient(httpClient *http.Client, authUser string) *Client {
	if authUser == "" {
		authUser = "0"
	}
	return &Client{
		domain:     defaultColabDomain,
		authUser:   authUser,
		httpClient: httpClient,
	}
}

// SetDomain 仅供单元测试覆盖后端地址。
func (c *Client) SetDomain(domain string) {
	c.domain = strings.TrimRight(domain, "/")
}

// UUIDToWebSafeBase64 与官方 google-colab-cli 保持完全一致的 notebook_hash 编码。
func UUIDToWebSafeBase64(id uuid.UUID) string {
	s := strings.ReplaceAll(id.String(), "-", "_")
	if len(s) < 44 {
		s += strings.Repeat(".", 44-len(s))
	}
	return s
}

// ListAssignments 列出当前 Google 账号在 Colab 后端所有活跃的 VM 实例。
func (c *Client) ListAssignments(ctx context.Context) ([]Assignment, error) {
	body, err := c.doRequest(ctx, http.MethodGet, "/tun/m/assignments", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	var resp struct {
		Assignments []Assignment `json:"assignments"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析 assignments 响应失败: %w", err)
	}
	return resp.Assignments, nil
}

// GetCCUInfo 查询当前账号的 Compute Units 余额与每小时消耗速率。
func (c *Client) GetCCUInfo(ctx context.Context) (*CCUInfo, error) {
	body, err := c.doRequest(ctx, http.MethodGet, "/tun/m/ccu-info", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	var info CCUInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("解析 ccu-info 响应失败: %w", err)
	}
	return &info, nil
}

// Assign 使用稳定的 notebookHash 申请（或复用同一 notebookHash 下已存在的）Colab VM。
func (c *Client) Assign(ctx context.Context, notebookHash uuid.UUID, gpu string, highMem bool) (*Assignment, error) {
	params := buildAssignParams(notebookHash, gpu, highMem)

	getBody, err := c.doRequest(ctx, http.MethodGet, "/tun/m/assign", params, nil, nil)
	if err != nil {
		return nil, err
	}

	var existing Assignment
	if err := json.Unmarshal(getBody, &existing); err == nil && existing.Endpoint != "" && existing.RuntimeProxyInfo.URL != "" {
		if existing.Accelerator == "" {
			existing.Accelerator = normalizeAcceleratorLabel(gpu)
		}
		return &existing, nil
	}

	var xsrf struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(getBody, &xsrf); err != nil || xsrf.Token == "" {
		return nil, fmt.Errorf("无法从 /tun/m/assign 获取分配令牌: %s", string(getBody))
	}

	const maxRetries = 3
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		headers := map[string]string{"X-Goog-Colab-Token": xsrf.Token}
		postBody, err := c.doRequest(ctx, http.MethodPost, "/tun/m/assign", params, headers, nil)
		if err == nil {
			var assigned Assignment
			if err := json.Unmarshal(postBody, &assigned); err != nil {
				return nil, fmt.Errorf("解析 assign POST 响应失败: %w", err)
			}
			if assigned.Accelerator == "" {
				assigned.Accelerator = normalizeAcceleratorLabel(gpu)
			}
			return &assigned, nil
		}

		lastErr = err
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			if apiErr.StatusCode == http.StatusPreconditionFailed {
				return nil, fmt.Errorf("Colab 拒绝分配实例 (HTTP 412)：活跃会话过多或当前 GPU 资源紧张，请先停止多余实例或更换 GPU")
			}
			if apiErr.StatusCode == http.StatusBadRequest && gpu != "" && strings.ToUpper(gpu) != "CPU" {
				return nil, fmt.Errorf("Colab 拒绝了加速器 %q (HTTP 400)：账号可能无此 GPU 配额，请尝试切换其他规格", gpu)
			}
			retryable := apiErr.StatusCode == http.StatusBadGateway ||
				apiErr.StatusCode == http.StatusServiceUnavailable ||
				apiErr.StatusCode == http.StatusGatewayTimeout
			if !retryable || attempt >= maxRetries {
				return nil, err
			}
		} else if attempt >= maxRetries {
			return nil, err
		}

		wait := time.Duration(5<<attempt) * time.Second
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}

		reBody, reErr := c.doRequest(ctx, http.MethodGet, "/tun/m/assign", params, nil, nil)
		if reErr == nil {
			var recon Assignment
			if json.Unmarshal(reBody, &recon) == nil && recon.Endpoint != "" && recon.RuntimeProxyInfo.URL != "" {
				return &recon, nil
			}
			_ = json.Unmarshal(reBody, &xsrf)
		}
	}
	return nil, lastErr
}

// Unassign 彻底释放指定 endpoint 的 Colab VM 并停止计费。
func (c *Client) Unassign(ctx context.Context, endpoint string) error {
	path := "/tun/m/unassign/" + url.PathEscape(endpoint)
	body, err := c.doRequest(ctx, http.MethodGet, path, nil, nil, nil)
	if err != nil {
		return err
	}
	var xsrf struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &xsrf); err != nil || xsrf.Token == "" {
		return fmt.Errorf("无法从 unassign 获取 XSRF 令牌: %s", string(body))
	}
	headers := map[string]string{"X-Goog-Colab-Token": xsrf.Token}
	_, err = c.doRequest(ctx, http.MethodPost, path, nil, headers, nil)
	return err
}

// PropagateDriveCredentials 调用 Colab Drive 凭据传播接口（与官方 google-colab-cli 一致，固定 record=false + empty.ipynb）。
func (c *Client) PropagateDriveCredentials(ctx context.Context, endpoint string, dryRun bool) (*DrivePropagationResult, error) {
	dryStr := "false"
	if dryRun {
		dryStr = "true"
	}

	path := "/tun/m/credentials-propagation/" + url.PathEscape(endpoint)
	params := url.Values{
		"authtype":  {"dfs_ephemeral"},
		"version":   {"2"},
		"dryrun":    {dryStr},
		"propagate": {"true"},
		"record":    {"false"},
	}

	getBody, err := c.doRequest(ctx, http.MethodGet, path, params, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("credentials-propagation GET 失败: %w", err)
	}
	var tokenResp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(getBody, &tokenResp); err != nil || tokenResp.Token == "" {
		return nil, fmt.Errorf("credentials-propagation 未返回有效 XSRF token: %s", string(getBody))
	}

	var formBuf bytes.Buffer
	mw := multipart.NewWriter(&formBuf)
	if err := mw.WriteField("file_id", "empty.ipynb"); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	headers := map[string]string{
		"x-goog-colab-token": tokenResp.Token,
		"Content-Type":       mw.FormDataContentType(),
	}
	postBody, err := c.doRequest(ctx, http.MethodPost, path, params, headers, bytes.NewReader(formBuf.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("credentials-propagation POST(dryrun=%s) 失败: %w", dryStr, err)
	}

	res := &DrivePropagationResult{Success: true}
	if len(bytes.TrimSpace(postBody)) > 0 {
		_ = json.Unmarshal(postBody, res)
	}
	return res, nil
}

func (c *Client) doRequest(ctx context.Context, method, path string, params url.Values, extraHeaders map[string]string, body io.Reader) ([]byte, error) {
	q := url.Values{}
	for k, vals := range params {
		for _, v := range vals {
			q.Add(k, v)
		}
	}
	q.Set("authuser", c.authUser)

	fullURL := c.domain + path + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Colab-Client-Agent", colabClientAgent)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Method:     method,
			Endpoint:   path,
			Body:       string(raw),
		}
	}
	return stripXSSI(raw), nil
}

func stripXSSI(raw []byte) []byte {
	if bytes.HasPrefix(raw, []byte(xssiPrefix)) {
		return raw[len(xssiPrefix):]
	}
	return raw
}

func buildAssignParams(notebookHash uuid.UUID, gpu string, highMem bool) url.Values {
	params := url.Values{
		"nbh": {UUIDToWebSafeBase64(notebookHash)},
	}
	upper := strings.ToUpper(strings.TrimSpace(gpu))
	switch upper {
	case "", "CPU", "NONE":
		params.Set("variant", "DEFAULT")
	default:
		params.Set("variant", "GPU")
		params.Set("accelerator", upper)
	}
	if highMem && upper != "L4" && upper != "V5E1" && upper != "V6E1" {
		params.Set("shape", "hm")
	}
	return params
}

func normalizeAcceleratorLabel(gpu string) string {
	upper := strings.ToUpper(strings.TrimSpace(gpu))
	if upper == "" || upper == "NONE" {
		return "CPU"
	}
	return upper
}
