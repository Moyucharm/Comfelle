package manager

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"

	"colab-comfyui/internal/auth"
)

// PersistedState 存储在 state.json（权限 0600）中的非敏感元数据与自动生成的会话签名密钥。
type PersistedState struct {
	SessionSecret string `json:"session_secret"`
	NotebookHash  string `json:"notebook_hash"`
	Endpoint      string `json:"endpoint,omitempty"`
	KernelID      string `json:"kernel_id,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	Accelerator   string `json:"accelerator,omitempty"`
}

// StateStore 负责原子读写 state.json。
type StateStore struct {
	mu   sync.Mutex
	path string
	data PersistedState
}

// OpenStateStore 加载或初始化状态文件，确保存在稳定的 notebook_hash 与 session_secret。
func OpenStateStore(path string) (*StateStore, error) {
	s := &StateStore{path: path}
	if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &s.data)
	}

	dirty := false
	if _, err := uuid.Parse(s.data.NotebookHash); err != nil {
		s.data.NotebookHash = uuid.NewString()
		dirty = true
	}
	if len(s.data.SessionSecret) < 32 {
		sec, err := auth.RandomHex(32)
		if err != nil {
			return nil, fmt.Errorf("生成 session_secret 失败: %w", err)
		}
		s.data.SessionSecret = sec
		dirty = true
	}
	if dirty {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Snapshot 返回当前持久化状态副本。
func (s *StateStore) Snapshot() PersistedState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data
}

// Update 原子修改并落盘状态。
func (s *StateStore) Update(fn func(st *PersistedState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.data)
	return s.saveLocked()
}

func (s *StateStore) saveLocked() error {
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	return os.Rename(tmp, s.path)
}
