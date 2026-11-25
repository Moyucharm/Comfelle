package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	SessionCookieName = "comfelle_session"
	maxFailedAttempts = 5
	rateLimitWindow   = 15 * time.Minute
)

// Authenticator 负责外层独立登录鉴权、签名 Cookie 校验与登录防爆破限速。
type Authenticator struct {
	username     string
	passwordHash []byte
	secret       []byte
	ttl          time.Duration
	trustProxy   bool
	cookieSecure bool

	mu       sync.Mutex
	failures map[string][]time.Time
}

// New 创建外层鉴权器。
func New(username, passwordHash, sessionSecret string, ttl time.Duration, trustProxy, cookieSecure bool) (*Authenticator, error) {
	if username == "" {
		return nil, errors.New("用户名不能为空")
	}
	if passwordHash == "" {
		return nil, errors.New("密码哈希不能为空")
	}
	if len(sessionSecret) < 16 {
		return nil, errors.New("session_secret 长度不能少于 16 字符")
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	return &Authenticator{
		username:     username,
		passwordHash: []byte(passwordHash),
		secret:       []byte(sessionSecret),
		ttl:          ttl,
		trustProxy:   trustProxy,
		cookieSecure: cookieSecure,
		failures:     make(map[string][]time.Time),
	}, nil
}

// HashPassword 生成 bcrypt 密码哈希。
func HashPassword(plain string) (string, error) {
	if len(plain) < 6 {
		return "", errors.New("密码长度建议不少于 6 位")
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hashed), nil
}

// RandomHex 生成密码学安全随机十六进制串。
func RandomHex(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Authenticate 校验用户名和密码，并执行按 IP 的失败次数限速。
func (a *Authenticator) Authenticate(r *http.Request, username, password string) (ok bool, retryAfter time.Duration) {
	ip := a.clientIP(r)
	if wait := a.checkRateLimit(ip); wait > 0 {
		return false, wait
	}

	userMatch := subtle.ConstantTimeCompare([]byte(username), []byte(a.username)) == 1
	passErr := bcrypt.CompareHashAndPassword(a.passwordHash, []byte(password))
	if userMatch && passErr == nil {
		a.clearFailures(ip)
		return true, 0
	}

	a.recordFailure(ip)
	return false, 0
}

// IssueSession 签发 HttpOnly 签名 Cookie。
func (a *Authenticator) IssueSession(w http.ResponseWriter, r *http.Request) error {
	nonce, err := RandomHex(12)
	if err != nil {
		return err
	}
	expiresAt := time.Now().Add(a.ttl)
	payload := fmt.Sprintf("%s|%d|%s", base64.RawURLEncoding.EncodeToString([]byte(a.username)), expiresAt.Unix(), nonce)
	sig := a.sign(payload)
	value := payload + "." + sig

	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(a.ttl.Seconds()),
		HttpOnly: true,
		Secure:   a.isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// ClearSession 清除登录 Cookie。
func (a *Authenticator) ClearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   a.isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// VerifyRequest 校验请求是否携带有效登录态。
func (a *Authenticator) VerifyRequest(r *http.Request) bool {
	c, err := r.Cookie(SessionCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	dot := strings.LastIndexByte(c.Value, '.')
	if dot <= 0 || dot >= len(c.Value)-1 {
		return false
	}
	payload := c.Value[:dot]
	sig := c.Value[dot+1:]
	expected := a.sign(payload)
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return false
	}

	parts := strings.Split(payload, "|")
	if len(parts) != 3 {
		return false
	}
	userBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || subtle.ConstantTimeCompare(userBytes, []byte(a.username)) != 1 {
		return false
	}
	expUnix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > expUnix {
		return false
	}
	return true
}

// RequireAuth 全局鉴权中间件：未登录时页面请求跳转 /panel/login，API/WebSocket 直接返回 401。
func (a *Authenticator) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/panel/login" {
			next.ServeHTTP(w, r)
			return
		}
		if a.VerifyRequest(r) {
			next.ServeHTTP(w, r)
			return
		}

		// WebSocket 或 API/非页面导航请求直接返回 401，绝不触达上游。
		if isWebSocketRequest(r) || isAPIRequest(r) || r.Method != http.MethodGet {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/panel/login", http.StatusFound)
	})
}

// RequireSameOriginCSRF 对状态变更接口强制校验自定义请求头或同源来源，防止 CSRF。
func RequireSameOriginCSRF(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Requested-With"), "fetch")
}

func (a *Authenticator) sign(payload string) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *Authenticator) isSecureRequest(r *http.Request) bool {
	if a.cookieSecure || r.TLS != nil {
		return true
	}
	if a.trustProxy && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		return true
	}
	return false
}

func (a *Authenticator) clientIP(r *http.Request) string {
	if a.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, _ := strings.Cut(xff, ","); strings.TrimSpace(first) != "" {
				return strings.TrimSpace(first)
			}
		}
		if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
			return xri
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

func (a *Authenticator) checkRateLimit(ip string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-rateLimitWindow)
	recent := a.pruneLocked(ip, cutoff)
	if len(recent) >= maxFailedAttempts {
		oldest := recent[0]
		wait := oldest.Add(rateLimitWindow).Sub(now)
		if wait > 0 {
			return wait
		}
	}
	return 0
}

func (a *Authenticator) recordFailure(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	recent := a.pruneLocked(ip, now.Add(-rateLimitWindow))
	a.failures[ip] = append(recent, now)
}

func (a *Authenticator) clearFailures(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.failures, ip)
}

func (a *Authenticator) pruneLocked(ip string, cutoff time.Time) []time.Time {
	list := a.failures[ip]
	if len(list) == 0 {
		return nil
	}
	kept := list[:0]
	for _, t := range list {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(a.failures, ip)
		return nil
	}
	a.failures[ip] = kept
	return kept
}

func isWebSocketRequest(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func isAPIRequest(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/panel/api/") || strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	if accept != "" && !strings.Contains(accept, "text/html") && !strings.Contains(accept, "*/*") {
		return true
	}
	return false
}
