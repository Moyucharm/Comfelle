package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeGPU(t *testing.T) {
	cases := map[string]string{
		"":        "L4",
		"  ":      "L4",
		"unknown": "L4",
		"cpu":     "CPU",
		"NONE":    "CPU",
		"t4":      "T4",
		"a100":    "A100",
		"h100":    "H100",
	}
	for in, want := range cases {
		if got := NormalizeGPU(in); got != want {
			t.Fatalf("NormalizeGPU(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListenPrecedence(t *testing.T) {
	t.Setenv("COMFY_DEFAULT_LISTEN", "0.0.0.0:8080")
	t.Setenv("COMFY_PROXY_LISTEN", "")

	// 1. YAML 未写 server.listen 时采用 COMFY_DEFAULT_LISTEN
	p1 := filepath.Join(t.TempDir(), "c1.yaml")
	_ = os.WriteFile(p1, []byte("auth:\n  password_hash: \"$2a$10$abcdefghijklmnopqrstuv\"\ncolab:\n  default_gpu: \"\"\n"), 0o600)
	cfg1, err := Load(p1)
	if err != nil {
		t.Fatalf("Load c1 failed: %v", err)
	}
	if cfg1.Server.Listen != "0.0.0.0:8080" {
		t.Fatalf("expected default listen 0.0.0.0:8080, got %q", cfg1.Server.Listen)
	}
	if cfg1.Colab.DefaultGPU != "L4" {
		t.Fatalf("expected empty default_gpu to fallback to L4, got %q", cfg1.Colab.DefaultGPU)
	}

	// 2. YAML 显式指定 server.listen 时优先于 COMFY_DEFAULT_LISTEN
	p2 := filepath.Join(t.TempDir(), "c2.yaml")
	_ = os.WriteFile(p2, []byte("server:\n  listen: \"127.0.0.1:9090\"\nauth:\n  password_hash: \"$2a$10$abcdefghijklmnopqrstuv\"\n"), 0o600)
	cfg2, err := Load(p2)
	if err != nil {
		t.Fatalf("Load c2 failed: %v", err)
	}
	if cfg2.Server.Listen != "127.0.0.1:9090" {
		t.Fatalf("expected YAML listen 127.0.0.1:9090, got %q", cfg2.Server.Listen)
	}

	// 3. 显式传入 COMFY_PROXY_LISTEN 时最高优先级覆盖
	t.Setenv("COMFY_PROXY_LISTEN", "0.0.0.0:7070")
	cfg3, err := Load(p2)
	if err != nil {
		t.Fatalf("Load c3 failed: %v", err)
	}
	if cfg3.Server.Listen != "0.0.0.0:7070" {
		t.Fatalf("expected env override listen 0.0.0.0:7070, got %q", cfg3.Server.Listen)
	}
}
