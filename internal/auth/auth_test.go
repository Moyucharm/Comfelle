package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestAuth(t *testing.T) *Authenticator {
	t.Helper()
	hash, err := HashPassword("super-secret-pass")
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}
	a, err := New("admin", hash, "0123456789abcdef0123456789abcdef", time.Hour, true, false)
	if err != nil {
		t.Fatalf("New authenticator failed: %v", err)
	}
	return a
}

func TestAuthenticateAndRateLimit(t *testing.T) {
	a := newTestAuth(t)
	req := httptest.NewRequest(http.MethodPost, "/panel/login", nil)
	req.RemoteAddr = "10.0.0.5:12345"

	for i := 0; i < maxFailedAttempts; i++ {
		ok, wait := a.Authenticate(req, "admin", "wrong")
		if ok || wait > 0 {
			t.Fatalf("attempt %d: expected fail without wait, got ok=%v wait=%v", i, ok, wait)
		}
	}

	// 第 6 次应被限速拦截，即使密码正确也不放行
	ok, wait := a.Authenticate(req, "admin", "super-secret-pass")
	if ok || wait <= 0 {
		t.Fatalf("expected rate limit active, got ok=%v wait=%v", ok, wait)
	}

	// 不同 IP 可正常登录
	req2 := httptest.NewRequest(http.MethodPost, "/panel/login", nil)
	req2.RemoteAddr = "10.0.0.6:12345"
	ok, wait = a.Authenticate(req2, "admin", "super-secret-pass")
	if !ok || wait != 0 {
		t.Fatalf("expected clean IP login success, got ok=%v wait=%v", ok, wait)
	}
}

func TestRequireAuthBlocksUnauthenticatedRequests(t *testing.T) {
	a := newTestAuth(t)
	reachedUpstream := false
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reachedUpstream = true
		w.WriteHeader(http.StatusOK)
	})
	handler := a.RequireAuth(upstream)

	// 1. 未登录访问首页 -> 302 跳转 /panel/login，不可触达上游
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/panel/login" {
		t.Fatalf("expected 302 -> /panel/login, got %d (%s)", rec.Code, rec.Header().Get("Location"))
	}
	if reachedUpstream {
		t.Fatal("unauthenticated GET / must never reach upstream")
	}

	// 2. 未登录访问 WebSocket /ws -> 401
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || reachedUpstream {
		t.Fatalf("expected 401 for unauthenticated WS, got %d (reached=%v)", rec.Code, reachedUpstream)
	}

	// 3. 未登录访问 ComfyUI API -> 401
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/prompt", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || reachedUpstream {
		t.Fatalf("expected 401 for unauthenticated API, got %d", rec.Code)
	}

	// 4. 签发 Cookie 后可正常通过
	loginRec := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/panel/login", nil)
	if err := a.IssueSession(loginRec, loginReq); err != nil {
		t.Fatalf("IssueSession failed: %v", err)
	}
	cookies := loginRec.Result().Cookies()
	if len(cookies) == 0 || !cookies[0].HttpOnly {
		t.Fatalf("expected HttpOnly session cookie, got %+v", cookies)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookies[0])
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !reachedUpstream {
		t.Fatalf("expected 200 after valid login cookie, got %d (reached=%v)", rec.Code, reachedUpstream)
	}

	// 5. 篡改 Cookie 必须被拒绝
	reachedUpstream = false
	tampered := *cookies[0]
	tampered.Value += "tampered"
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/prompt", nil)
	req.AddCookie(&tampered)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || reachedUpstream {
		t.Fatalf("expected 401 for tampered cookie, got %d", rec.Code)
	}
}
