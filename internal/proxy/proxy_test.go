package proxy

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

type staticProvider struct {
	target Target
}

func (s staticProvider) ProxyTarget() Target { return s.target }

func TestProxyWhenNotReady(t *testing.T) {
	h := New(staticProvider{target: Target{Ready: false}}, true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/panel" {
		t.Fatalf("expected redirect to /panel, got %d (%s)", rec.Code, rec.Header().Get("Location"))
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/system_stats", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for API when stopped, got %d", rec.Code)
	}
}

func TestProxyAuthInjectionCookieStripDockAndBinaryIntegrity(t *testing.T) {
	expectedAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("comfy:ephemeral-pw-123"))
	pngBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x01, 0x02, 0x03}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != expectedAuth {
			t.Errorf("upstream missing Basic Auth: got %q want %q", got, expectedAuth)
			w.Header().Set("WWW-Authenticate", `Basic realm="Private ComfyUI"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if gotCookie := r.Header.Get("Cookie"); gotCookie != "" {
			t.Errorf("outer cookie leaked to upstream: %q", gotCookie)
		}
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("WWW-Authenticate", `Basic realm="should-be-stripped"`)
			_, _ = w.Write([]byte("<html><body><h1>ComfyUI</h1></body></html>"))
		case "/view":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes)
		case "/ws":
			upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
			c, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer c.Close()
			mt, msg, err := c.ReadMessage()
			if err == nil {
				_ = c.WriteMessage(mt, append([]byte("echo:"), msg...))
			}
		}
	}))
	defer up.Close()

	u, _ := url.Parse(up.URL)
	gw := httptest.NewServer(New(staticProvider{target: Target{
		URL:      u,
		Username: "comfy",
		Password: "ephemeral-pw-123",
		Ready:    true,
	}}, true))
	defer gw.Close()

	// 1. 首页 HTML 自动注入悬浮胶囊，并剥离 WWW-Authenticate
	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/", nil)
	req.Header.Set("Cookie", "colab_comfy_session=secret-outer-cookie")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	defer resp.Body.Close()
	var htmlBuf bytes.Buffer
	_, _ = htmlBuf.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status=%d", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") != "" {
		t.Fatal("WWW-Authenticate header should be stripped")
	}
	if !strings.Contains(htmlBuf.String(), "/panel/dock.js") {
		t.Fatalf("expected dock.js script injected, got: %s", htmlBuf.String())
	}

	// 2. 二进制图片 /view 原字节透传，不被修改
	imgReq, _ := http.NewRequest(http.MethodGet, gw.URL+"/view", nil)
	imgReq.Header.Set("Cookie", "colab_comfy_session=secret-outer-cookie")
	imgResp, err := http.DefaultClient.Do(imgReq)
	if err != nil {
		t.Fatalf("GET /view failed: %v", err)
	}
	defer imgResp.Body.Close()
	var imgBuf bytes.Buffer
	_, _ = imgBuf.ReadFrom(imgResp.Body)
	if !bytes.Equal(imgBuf.Bytes(), pngBytes) {
		t.Fatalf("binary PNG modified: got %x want %x", imgBuf.Bytes(), pngBytes)
	}

	// 3. WebSocket /ws 双向透明代理且自动注入 Basic Auth
	wsURL := "ws" + strings.TrimPrefix(gw.URL, "http") + "/ws"
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"Cookie": {"colab_comfy_session=secret-outer-cookie"},
		"Origin": {gw.URL},
	})
	if err != nil {
		t.Fatalf("WebSocket dial through proxy failed: %v", err)
	}
	defer wsConn.Close()
	if err := wsConn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatalf("WS write failed: %v", err)
	}
	_, reply, err := wsConn.ReadMessage()
	if err != nil || string(reply) != "echo:hello" {
		t.Fatalf("WS read unexpected: reply=%q err=%v", string(reply), err)
	}
}
