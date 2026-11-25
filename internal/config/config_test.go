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

func TestListenPrecedenceAndRuntimeSection(t *testing.T) {
	t.Setenv("COMFELLE_DEFAULT_LISTEN", "0.0.0.0:8080")
	t.Setenv("COMFELLE_LISTEN", "")
	t.Setenv("COMFY_PROXY_LISTEN", "")

	// 1. 新版 runtime: 配置节 + YAML 未写 server.listen 时采用 COMFELLE_DEFAULT_LISTEN
	p1 := filepath.Join(t.TempDir(), "c1.yaml")
	_ = os.WriteFile(p1, []byte("auth:\n  password_hash: \"$2a$10$abcdefghijklmnopqrstuv\"\nruntime:\n  default_gpu: \"\"\n  token_file: \"./my_tok.json\"\n"), 0o600)
	cfg1, err := Load(p1)
	if err != nil {
		t.Fatalf("Load c1 failed: %v", err)
	}
	if cfg1.Server.Listen != "0.0.0.0:8080" {
		t.Fatalf("expected default listen 0.0.0.0:8080, got %q", cfg1.Server.Listen)
	}
	if cfg1.Colab.DefaultGPU != "L4" || cfg1.Colab.TokenFile != "./my_tok.json" {
		t.Fatalf("unexpected runtime config: gpu=%q tok=%q", cfg1.Colab.DefaultGPU, cfg1.Colab.TokenFile)
	}

	// 2. 向下兼容旧版 colab: 配置节 + YAML 显式指定 server.listen
	p2 := filepath.Join(t.TempDir(), "c2.yaml")
	_ = os.WriteFile(p2, []byte("server:\n  listen: \"127.0.0.1:9090\"\nauth:\n  password_hash: \"$2a$10$abcdefghijklmnopqrstuv\"\ncolab:\n  default_gpu: \"A100\"\n  token_file: \"./google_token.json\"\n"), 0o600)
	cfg2, err := Load(p2)
	if err != nil {
		t.Fatalf("Load c2 failed: %v", err)
	}
	if cfg2.Server.Listen != "127.0.0.1:9090" {
		t.Fatalf("expected YAML listen 127.0.0.1:9090, got %q", cfg2.Server.Listen)
	}
	if cfg2.Colab.DefaultGPU != "A100" || cfg2.Colab.TokenFile != "./google_token.json" {
		t.Fatalf("legacy colab: section not preserved: gpu=%q tok=%q", cfg2.Colab.DefaultGPU, cfg2.Colab.TokenFile)
	}

	// 3. 显式传入 COMFELLE_LISTEN 时最高优先级覆盖
	t.Setenv("COMFELLE_LISTEN", "0.0.0.0:7070")
	cfg3, err := Load(p2)
	if err != nil {
		t.Fatalf("Load c3 failed: %v", err)
	}
	if cfg3.Server.Listen != "0.0.0.0:7070" {
		t.Fatalf("expected env override listen 0.0.0.0:7070, got %q", cfg3.Server.Listen)
	}
}
