package colab

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	// 与官方 google-colab-cli 保持一致的公开 OAuth 客户端与远程复制粘贴回调页。
	defaultOAuthClientID     = "764086051850-6qr4p6gpi6hn506pt8ejuq83di341hur.apps.googleusercontent.com"
	defaultOAuthClientSecret = "d-FL95Q19q7MQmFpd7hHD0Ty"
	remoteRedirectURI        = "https://sdk.cloud.google.com/applicationdefaultauthcode.html"
)

var defaultOAuthScopes = []string{
	"openid",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/colaboratory",
	"https://www.googleapis.com/auth/drive.file",
}

// StoredToken 兼容 Python google-colab-cli 导出的 token JSON 与本程序自身持久化的格式。
type StoredToken struct {
	Token        string   `json:"token"`
	AccessToken  string   `json:"access_token,omitempty"`
	RefreshToken string   `json:"refresh_token"`
	TokenURI     string   `json:"token_uri,omitempty"`
	ClientID     string   `json:"client_id,omitempty"`
	ClientSecret string   `json:"client_secret,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	Expiry       string   `json:"expiry,omitempty"`
}

// PersistentTokenSource 读取本地 0600 JSON Token 并在自动刷新后安全回写磁盘。
type PersistentTokenSource struct {
	mu      sync.Mutex
	path    string
	stored  StoredToken
	base    oauth2.TokenSource
	current *oauth2.Token
}

// LoadTokenSource 从本地 JSON 文件加载 Google OAuth2 凭据源。
func LoadTokenSource(path string) (*PersistentTokenSource, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 Google OAuth Token 文件 %q 失败: %w", path, err)
	}
	var st StoredToken
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("解析 Google OAuth Token 文件 %q 失败: %w", path, err)
	}
	if st.RefreshToken == "" && st.Token == "" && st.AccessToken == "" {
		return nil, errors.New("Google OAuth Token 文件中缺少有效 token/refresh_token")
	}

	if st.ClientID == "" {
		st.ClientID = defaultOAuthClientID
	}
	if st.ClientSecret == "" {
		st.ClientSecret = defaultOAuthClientSecret
	}
	if st.TokenURI == "" {
		st.TokenURI = google.Endpoint.TokenURL
	}
	if len(st.Scopes) == 0 {
		st.Scopes = append([]string(nil), defaultOAuthScopes...)
	}

	access := st.Token
	if access == "" {
		access = st.AccessToken
	}
	var exp time.Time
	if st.Expiry != "" {
		if parsed, err := parseExpiry(st.Expiry); err == nil {
			exp = parsed
		}
	}

	tok := &oauth2.Token{
		AccessToken:  access,
		RefreshToken: st.RefreshToken,
		TokenType:    "Bearer",
		Expiry:       exp,
	}

	cfg := &oauth2.Config{
		ClientID:     st.ClientID,
		ClientSecret: st.ClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:  google.Endpoint.AuthURL,
			TokenURL: st.TokenURI,
		},
		Scopes:      st.Scopes,
		RedirectURL: remoteRedirectURI,
	}

	base := cfg.TokenSource(context.Background(), tok)
	return &PersistentTokenSource{
		path:    path,
		stored:  st,
		base:    base,
		current: tok,
	}, nil
}

// Token 返回有效的 OAuth2 Token；若触发自动刷新则以 0600 权限原子写回文件。
func (p *PersistentTokenSource) Token() (*oauth2.Token, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	tok, err := p.base.Token()
	if err != nil {
		return nil, err
	}
	if p.current == nil || tok.AccessToken != p.current.AccessToken || !tok.Expiry.Equal(p.current.Expiry) {
		p.current = tok
		p.stored.Token = tok.AccessToken
		p.stored.AccessToken = ""
		if tok.RefreshToken != "" {
			p.stored.RefreshToken = tok.RefreshToken
		}
		if !tok.Expiry.IsZero() {
			p.stored.Expiry = tok.Expiry.UTC().Format(time.RFC3339Nano)
		}
		_ = saveStoredToken(p.path, p.stored)
	}
	return tok, nil
}

// HTTPClient 返回自动携带并刷新 Bearer Token 的 HTTP 客户端。
func (p *PersistentTokenSource) HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &oauth2.Transport{
			Source: p,
			Base:   http.DefaultTransport,
		},
	}
}

// RunRemoteAuthFlow 在无头终端中执行远程复制粘贴 OAuth2 登录，并将凭据写入 0600 文件。
func RunRemoteAuthFlow(ctx context.Context, tokenPath string, out io.Writer, in io.Reader) error {
	cfg := &oauth2.Config{
		ClientID:     defaultOAuthClientID,
		ClientSecret: defaultOAuthClientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       defaultOAuthScopes,
		RedirectURL:  remoteRedirectURI,
	}

	authURL := cfg.AuthCodeURL(
		"comfelle",
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.SetAuthURLParam("token_usage", "remote"),
	)

	fmt.Fprintln(out, "请在任意浏览器打开以下链接登录 Google 账号并同意授权：")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  "+authURL)
	fmt.Fprintln(out)
	fmt.Fprint(out, "授权完成后请粘贴页面显示的 Authorization Code 并回车: ")

	reader := bufio.NewReader(in)
	code, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("读取授权码失败: %w", err)
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return errors.New("授权码不能为空")
	}

	tok, err := cfg.Exchange(ctx, code)
	if err != nil {
		return fmt.Errorf("交换 OAuth2 Token 失败: %w", err)
	}

	st := StoredToken{
		Token:        tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenURI:     google.Endpoint.TokenURL,
		ClientID:     defaultOAuthClientID,
		ClientSecret: defaultOAuthClientSecret,
		Scopes:       defaultOAuthScopes,
	}
	if !tok.Expiry.IsZero() {
		st.Expiry = tok.Expiry.UTC().Format(time.RFC3339Nano)
	}
	if err := saveStoredToken(tokenPath, st); err != nil {
		return err
	}
	fmt.Fprintf(out, "已成功保存 Google OAuth2 Token 到 %s (权限 0600)\n", tokenPath)
	return nil
}

func saveStoredToken(path string, st StoredToken) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建目录 %q 失败: %w", dir, err)
		}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入临时 Token 文件失败: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func parseExpiry(raw string) (time.Time, error) {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999",
		"2006-01-02T15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析时间 %q", raw)
}
