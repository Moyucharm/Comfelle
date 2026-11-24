package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"colab-comfyui/internal/auth"
	"colab-comfyui/internal/colab"
	"colab-comfyui/internal/config"
	"colab-comfyui/internal/manager"
	"colab-comfyui/internal/proxy"
	"colab-comfyui/internal/web"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "hash-password":
			if err := cmdHashPassword(); err != nil {
				log.Fatalf("生成密码哈希失败: %v", err)
			}
			return
		case "auth-google":
			if err := cmdAuthGoogle(os.Args[2:]); err != nil {
				log.Fatalf("Google OAuth 登录失败: %v", err)
			}
			return
		case "serve":
			if err := cmdServe(os.Args[2:]); err != nil {
				log.Fatalf("启动服务失败: %v", err)
			}
			return
		case "-h", "--help", "help":
			printUsage()
			return
		}
	}

	if err := cmdServe(os.Args[1:]); err != nil {
		log.Fatalf("启动服务失败: %v", err)
	}
}

func printUsage() {
	fmt.Println(`用法:
  colab-comfyui [serve] -config ./config.yaml    启动反向代理与 Colab 控制网关
  colab-comfyui hash-password                    交互生成 bcrypt 密码哈希（填入 config.yaml）
  colab-comfyui auth-google -out ./google_token.json  无头终端完成 Google OAuth2 登录并生成 Token 文件`)
}

func cmdHashPassword() error {
	var password string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "请输入外层登录密码: ")
		pwBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		fmt.Fprint(os.Stderr, "请再次输入确认: ")
		pw2Bytes, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		if string(pwBytes) != string(pw2Bytes) {
			return fmt.Errorf("两次输入的密码不一致")
		}
		password = string(pwBytes)
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && len(line) == 0 {
			return err
		}
		password = strings.TrimRight(line, "\r\n")
	}

	hashed, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	fmt.Println(hashed)
	return nil
}

func cmdAuthGoogle(args []string) error {
	fs := flag.NewFlagSet("auth-google", flag.ExitOnError)
	outPath := fs.String("out", "./google_token.json", "保存 Google OAuth2 Token 的文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return colab.RunRemoteAuthFlow(context.Background(), *outPath, os.Stdout, os.Stdin)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "./config.yaml", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	store, err := manager.OpenStateStore(cfg.Colab.StateFile)
	if err != nil {
		return fmt.Errorf("初始化状态文件失败: %w", err)
	}

	sessionSecret := cfg.Auth.SessionSecret
	if sessionSecret == "" {
		sessionSecret = store.Snapshot().SessionSecret
	}

	authenticator, err := auth.New(
		cfg.Auth.Username,
		cfg.Auth.PasswordHash,
		sessionSecret,
		time.Duration(cfg.Auth.SessionTTLHours)*time.Hour,
		cfg.Server.TrustProxy,
		cfg.Server.CookieSecure,
	)
	if err != nil {
		return fmt.Errorf("初始化鉴权器失败: %w", err)
	}

	tokenSource, err := colab.LoadTokenSource(cfg.Colab.TokenFile)
	if err != nil {
		return err
	}
	colabClient := colab.NewClient(tokenSource.HTTPClient(45*time.Second), cfg.Colab.AuthUser)

	mgr := manager.New(cfg, colabClient, store)
	proxyHandler := proxy.New(mgr, cfg.Server.EnableFloatingDock)
	webHandler := web.New(authenticator, mgr)

	rootRouter := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/panel" || strings.HasPrefix(r.URL.Path, "/panel/") {
			webHandler.ServeHTTP(w, r)
			return
		}
		proxyHandler.ServeHTTP(w, r)
	})

	// 最外层全局鉴权守卫：未登录时除 /panel/login 外全部拦截，绝不触达上游。
	securedHandler := authenticator.RequireAuth(rootRouter)

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           securedHandler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 启动时后台对账：自动认领并热接管云端已存活的 Colab 会话。
	reconcileCtx, reconcileCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	go func() {
		defer reconcileCancel()
		mgr.Reconcile(reconcileCtx)
	}()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("colab-comfyui 网关已监听 %s（控制台路径: /panel）", cfg.Server.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		mgr.Shutdown()
		return err
	case sig := <-sigCh:
		log.Printf("收到信号 %s，正在优雅关闭本地网关（不会主动销毁运行中的 Colab VM）...", sig)
		reconcileCancel()
		mgr.Shutdown()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
