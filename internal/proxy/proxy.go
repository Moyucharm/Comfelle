package proxy

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	dockScriptTag   = `<script src="/panel/dock.js" defer></script>`
	maxInjectHTMLSz = 2 << 20 // 2 MiB，仅针对 ComfyUI 根入口 index.html
)

// Target 描述当前反向代理的上游地址与 Basic Auth 凭据。
type Target struct {
	URL      *url.URL
	Username string
	Password string
	Ready    bool
}

// TargetProvider 由实例管理器实现，向反代层提供实时动态上游。
type TargetProvider interface {
	ProxyTarget() Target
}

// Handler 负责将已鉴权流量透明流式反代至动态 Cloudflare Tunnel，
// 自动注入 Colab nginx Basic Auth，剥离外层 Cookie，且不在服务器落盘任何图片/响应。
type Handler struct {
	provider           TargetProvider
	enableFloatingDock bool
	httpTransport      *http.Transport
	wsTransport        *http.Transport
}

func newUpstreamTransport(enableHTTP2 bool) *http.Transport {
	t := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		ForceAttemptHTTP2:     enableHTTP2,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 0, // 允许 ComfyUI 长耗时生图请求不提前超时
	}
	if !enableHTTP2 {
		// WebSocket Upgrade 必须走 HTTP/1.1，显式置空 TLSNextProto 禁用 ALPN h2 协商。
		t.TLSNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)
	}
	return t
}

// New 创建动态反向代理处理器。
func New(provider TargetProvider, enableFloatingDock bool) *Handler {
	// 普通 HTTP 请求启用 HTTP/2 多路复用，避免首页数十个 JS/CSS/API 请求重复做 TLS 握手；
	// 仅 WebSocket Upgrade 请求使用独立 HTTP/1.1 Transport，兼顾兼容性与首屏加载速度。
	return &Handler{
		provider:           provider,
		enableFloatingDock: enableFloatingDock,
		httpTransport:      newUpstreamTransport(true),
		wsTransport:        newUpstreamTransport(false),
	}
}

// ServeHTTP 实现 http.Handler：未就绪时引导至 /panel，就绪后 100% 透明流式反代 ComfyUI。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	isWS := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
	target := h.provider.ProxyTarget()
	if !target.Ready || target.URL == nil {
		if isWS || strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "Colab instance is not running", http.StatusServiceUnavailable)
			return
		}
		http.Redirect(w, r, "/panel", http.StatusFound)
		return
	}

	shouldInjectDock := h.enableFloatingDock &&
		r.Method == http.MethodGet &&
		(r.URL.Path == "/" || r.URL.Path == "/index.html") &&
		!isWS

	basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(target.Username+":"+target.Password))
	upstreamOrigin := target.URL.Scheme + "://" + target.URL.Host
	transport := h.httpTransport
	if isWS {
		transport = h.wsTransport
	}

	rp := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1, // 立即刷新，保证流式与事件推送无延迟
		ErrorLog:      log.New(io.Discard, "", 0),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target.URL)
			pr.Out.Host = target.URL.Host
			// 剥离外层服务器 Session Cookie，绝不泄露给 Cloudflare 或上游
			pr.Out.Header.Del("Cookie")
			// 自动注入 Colab nginx Basic Auth
			pr.Out.Header.Set("Authorization", basicAuth)
			// 改写 Origin，兼容 aiohttp WebSocket / 插件同源校验
			if pr.In.Header.Get("Origin") != "" {
				pr.Out.Header.Set("Origin", upstreamOrigin)
			}
			// 仅在需要给根 HTML 注入悬浮胶囊脚本时，请求未压缩的 index.html（体积仅数 KB）
			if shouldInjectDock {
				pr.Out.Header.Del("Accept-Encoding")
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// 避免浏览器弹出上游 Basic Auth 弹窗
			resp.Header.Del("WWW-Authenticate")
			if !shouldInjectDock {
				return nil
			}
			if resp.StatusCode != http.StatusOK {
				return nil
			}
			ct := strings.ToLower(resp.Header.Get("Content-Type"))
			if !strings.Contains(ct, "text/html") {
				return nil
			}
			if resp.Header.Get("Content-Encoding") != "" {
				return nil
			}
			return injectDockScript(resp)
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			if strings.Contains(strings.ToLower(req.Header.Get("Accept")), "text/html") {
				rw.Header().Set("Content-Type", "text/html; charset=utf-8")
				rw.WriteHeader(http.StatusBadGateway)
				_, _ = rw.Write([]byte(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>ComfyUI 暂不可达</title><style>body{background:#0f172a;color:#e2e8f0;font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0}.card{background:#1e293b;border:1px solid #334155;border-radius:12px;padding:28px;max-width:420px;text-align:center}a{display:inline-block;margin-top:16px;padding:10px 18px;background:#3b82f6;color:#fff;text-decoration:none;border-radius:8px}</style></head><body><div class="card"><h2>ComfyUI 上游隧道暂时无响应</h2><p>可能正在重启或隧道正在自愈，请稍候刷新，或返回控制台查看实时日志。</p><a href="/panel">前往控制台</a></div></body></html>`))
				return
			}
			http.Error(rw, "Bad Gateway: upstream ComfyUI unreachable", http.StatusBadGateway)
		},
	}

	rp.ServeHTTP(w, r)
}

func injectDockScript(resp *http.Response) error {
	limited := io.LimitReader(resp.Body, maxInjectHTMLSz+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()

	// 若 HTML 超出限制则原样回放，避免截断。
	if len(raw) > maxInjectHTMLSz {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}

	if bytes.Contains(raw, []byte("/panel/dock.js")) {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}

	lower := bytes.ToLower(raw)
	idx := bytes.LastIndex(lower, []byte("</body>"))
	var out []byte
	if idx >= 0 {
		out = make([]byte, 0, len(raw)+len(dockScriptTag)+1)
		out = append(out, raw[:idx]...)
		out = append(out, []byte(dockScriptTag+"\n")...)
		out = append(out, raw[idx:]...)
	} else {
		out = append(raw, []byte("\n"+dockScriptTag)...)
	}

	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	resp.Header.Del("ETag")
	return nil
}
