package manager

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"colab-comfyui/internal/colab"
	"colab-comfyui/internal/config"
)

type fakeColabAPI struct {
	mu           sync.Mutex
	listed       []colab.Assignment
	assignedEP   string
	unassignedEP []string
	unassignCh   chan string
	unassignFn   func(ctx context.Context, endpoint string) error
}

func (f *fakeColabAPI) ListAssignments(ctx context.Context) ([]colab.Assignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]colab.Assignment(nil), f.listed...), nil
}

func (f *fakeColabAPI) Assign(ctx context.Context, notebookHash uuid.UUID, gpu string, highMem bool) (*colab.Assignment, error) {
	f.mu.Lock()
	f.assignedEP = "gpu-l4-test-ep"
	f.mu.Unlock()
	// 故意返回一个无法连通的本地地址，触发 EnsureKernel 失败，验证是否会自动 Unassign。
	return &colab.Assignment{
		Accelerator: "L4",
		Endpoint:    "gpu-l4-test-ep",
		RuntimeProxyInfo: colab.RuntimeProxyInfo{
			URL:                   "http://127.0.0.1:1",
			Token:                 "fake-tok",
			TokenExpiresInSeconds: 3600,
		},
	}, nil
}

func (f *fakeColabAPI) Unassign(ctx context.Context, endpoint string) error {
	f.mu.Lock()
	f.unassignedEP = append(f.unassignedEP, endpoint)
	fn := f.unassignFn
	f.mu.Unlock()
	select {
	case f.unassignCh <- endpoint:
	default:
	}
	if fn != nil {
		return fn(ctx, endpoint)
	}
	return nil
}

func (f *fakeColabAPI) GetCCUInfo(ctx context.Context) (*colab.CCUInfo, error) {
	f.mu.Lock()
	n := len(f.listed)
	f.mu.Unlock()
	return &colab.CCUInfo{CurrentBalance: 100, AssignmentsCount: n}, nil
}

func (f *fakeColabAPI) PropagateDriveCredentials(ctx context.Context, endpoint string, dryRun bool) (*colab.DrivePropagationResult, error) {
	return &colab.DrivePropagationResult{Success: true}, nil
}

func TestStartFailureAutoUnassignsVM(t *testing.T) {
	cfg := config.Default()
	cfg.Colab.StateFile = filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStateStore(cfg.Colab.StateFile)
	if err != nil {
		t.Fatalf("OpenStateStore failed: %v", err)
	}

	api := &fakeColabAPI{unassignCh: make(chan string, 1)}
	mgr := New(cfg, api, store)
	defer mgr.Shutdown()

	if err := mgr.Start("L4", false); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	select {
	case ep := <-api.unassignCh:
		if ep != "gpu-l4-test-ep" {
			t.Fatalf("expected unassign ep gpu-l4-test-ep, got %q", ep)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for automatic Unassign after startup failure")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := mgr.Status(context.Background())
		if snap.State == StateError && !snap.HasActiveVM {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	snap := mgr.Status(context.Background())
	t.Fatalf("expected StateError with HasActiveVM=false after cleanup, got state=%s hasActive=%v", snap.State, snap.HasActiveVM)
}

func TestNeverTouchesUnownedAssignments(t *testing.T) {
	cfg := config.Default()
	cfg.Colab.StateFile = filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStateStore(cfg.Colab.StateFile)
	if err != nil {
		t.Fatalf("OpenStateStore failed: %v", err)
	}

	const trainingEP = "gpu-a100-user-training"
	api := &fakeColabAPI{
		listed: []colab.Assignment{
			{
				Accelerator: "A100",
				Endpoint:    trainingEP,
				RuntimeProxyInfo: colab.RuntimeProxyInfo{
					URL:                   "http://127.0.0.1:2",
					Token:                 "train-tok",
					TokenExpiresInSeconds: 3600,
				},
			},
		},
		unassignCh: make(chan string, 4),
	}
	mgr := New(cfg, api, store)
	defer mgr.Shutdown()

	// 1. Reconcile 绝不认领外部训练实例
	mgr.Reconcile(context.Background())
	snap := mgr.Status(context.Background())
	if snap.State != StateStopped || snap.HasActiveVM {
		t.Fatalf("expected STOPPED and HasActiveVM=false despite external VM, got state=%s hasActive=%v", snap.State, snap.HasActiveVM)
	}

	// 2. Stop(true) 绝不关闭外部训练实例
	if err := mgr.Stop(true); err != nil {
		t.Fatalf("Stop(true) failed: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// 3. Start 绝不劫持外部训练实例，而是走独立 Assign；即使启动失败也只释放自己新开的 gpu-l4-test-ep
	if err := mgr.Start("L4", false); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	select {
	case ep := <-api.unassignCh:
		if ep == trainingEP {
			t.Fatalf("BUG: unassigned external training VM %q!", ep)
		}
		if ep != "gpu-l4-test-ep" {
			t.Fatalf("expected unassign only own ep gpu-l4-test-ep, got %q", ep)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for own ep cleanup")
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	for _, ep := range api.unassignedEP {
		if ep == trainingEP {
			t.Fatalf("external training VM %q must NEVER be unassigned", trainingEP)
		}
	}
}

func TestStopTreatsUnassign404AsStoppedWhenCloudConfirmsGone(t *testing.T) {
	cfg := config.Default()
	cfg.Colab.StateFile = filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStateStore(cfg.Colab.StateFile)
	if err != nil {
		t.Fatalf("OpenStateStore failed: %v", err)
	}
	const ep = "gpu-l4-s-already-gone"
	_ = store.Update(func(st *PersistedState) {
		st.Endpoint = ep
	})

	api := &fakeColabAPI{
		listed: nil, // 云端权威列表已无该实例
		unassignFn: func(ctx context.Context, endpoint string) error {
			return &colab.APIError{
				StatusCode: http.StatusNotFound,
				Method:     http.MethodGet,
				Endpoint:   "/tun/m/unassign/" + endpoint,
				Body:       `<!DOCTYPE html><title>Error 404 (Not Found)!!1</title>`,
			}
		},
	}
	mgr := New(cfg, api, store)
	defer mgr.Shutdown()

	if err := mgr.Stop(true); err != nil {
		t.Fatalf("Stop(true) failed: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap := mgr.Status(context.Background())
		if snap.State == StateStopped && !snap.HasActiveVM {
			if snap.ErrorMessage != "" {
				t.Fatalf("expected empty error message, got %q", snap.ErrorMessage)
			}
			if store.Snapshot().Endpoint != "" {
				t.Fatalf("expected persisted endpoint cleared, got %q", store.Snapshot().Endpoint)
			}
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	snap := mgr.Status(context.Background())
	t.Fatalf("expected StateStopped with HasActiveVM=false, got state=%s hasActive=%v err=%q", snap.State, snap.HasActiveVM, snap.ErrorMessage)
}

func TestStopFailsWhenUnassign404ButEndpointStillListed(t *testing.T) {
	cfg := config.Default()
	cfg.Colab.StateFile = filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStateStore(cfg.Colab.StateFile)
	if err != nil {
		t.Fatalf("OpenStateStore failed: %v", err)
	}
	const ep = "gpu-l4-s-still-billing"
	_ = store.Update(func(st *PersistedState) {
		st.Endpoint = ep
	})

	api := &fakeColabAPI{
		listed: []colab.Assignment{
			{Accelerator: "L4", Endpoint: ep},
		},
		unassignFn: func(ctx context.Context, endpoint string) error {
			return &colab.APIError{
				StatusCode: http.StatusNotFound,
				Method:     http.MethodGet,
				Endpoint:   "/tun/m/unassign/" + endpoint,
				Body:       `<!DOCTYPE html><title>Error 404 (Not Found)!!1</title>`,
			}
		},
	}
	mgr := New(cfg, api, store)
	defer mgr.Shutdown()

	if err := mgr.Stop(true); err != nil {
		t.Fatalf("Stop(true) failed: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap := mgr.Status(context.Background())
		if snap.State == StateError {
			if !snap.HasActiveVM {
				t.Fatalf("expected HasActiveVM=true when cloud still lists VM")
			}
			if strings.Contains(snap.ErrorMessage, "<!DOCTYPE") {
				t.Fatalf("expected summarized error without raw HTML, got %q", snap.ErrorMessage)
			}
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	snap := mgr.Status(context.Background())
	t.Fatalf("expected StateError when VM is still listed, got state=%s", snap.State)
}

func TestForceStopSupersedesInFlightStopWithoutErrorRace(t *testing.T) {
	cfg := config.Default()
	cfg.Colab.StateFile = filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStateStore(cfg.Colab.StateFile)
	if err != nil {
		t.Fatalf("OpenStateStore failed: %v", err)
	}
	const ep = "gpu-l4-s-race"
	_ = store.Update(func(st *PersistedState) {
		st.Endpoint = ep
	})

	firstStarted := make(chan struct{})
	firstCancelled := make(chan struct{})
	var callNum int
	var mu sync.Mutex

	api := &fakeColabAPI{
		listed: []colab.Assignment{{Accelerator: "L4", Endpoint: ep}},
	}
	api.unassignFn = func(ctx context.Context, endpoint string) error {
		mu.Lock()
		callNum++
		n := callNum
		mu.Unlock()
		if n == 1 {
			close(firstStarted)
			<-ctx.Done()
			close(firstCancelled)
			// 即使旧协程忽略 ctx 并返回 404，也不应覆盖新协程的 STOPPED 状态。
			return &colab.APIError{
				StatusCode: http.StatusNotFound,
				Method:     http.MethodGet,
				Endpoint:   "/tun/m/unassign/" + endpoint,
				Body:       `<!DOCTYPE html><title>Error 404 (Not Found)!!1</title>`,
			}
		}
		// 第二次强制关机成功释放云端实例
		api.mu.Lock()
		api.listed = nil
		api.mu.Unlock()
		return nil
	}

	mgr := New(cfg, api, store)
	defer mgr.Shutdown()

	// 先发起一次强制关机并卡在 Unassign 中，再发起第二次强制关机接管
	if err := mgr.Stop(true); err != nil {
		t.Fatalf("first Stop failed: %v", err)
	}
	<-firstStarted

	if err := mgr.Stop(true); err != nil {
		t.Fatalf("second Stop failed: %v", err)
	}
	select {
	case <-firstCancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("expected first Stop context to be cancelled by superseding Stop")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap := mgr.Status(context.Background())
		if snap.State == StateStopped && !snap.HasActiveVM {
			// 等待片刻确认旧协程不会回写 StateError
			time.Sleep(100 * time.Millisecond)
			snap2 := mgr.Status(context.Background())
			if snap2.State != StateStopped || snap2.ErrorMessage != "" {
				t.Fatalf("stale Stop goroutine clobbered state: state=%s err=%q", snap2.State, snap2.ErrorMessage)
			}
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	snap := mgr.Status(context.Background())
	t.Fatalf("expected final StateStopped, got state=%s err=%q", snap.State, snap.ErrorMessage)
}

func TestKeepaliveStopsOnPrunedAndNeverRunsWhenNotRunning(t *testing.T) {
	cfg := config.Default()
	cfg.Colab.StateFile = filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStateStore(cfg.Colab.StateFile)
	if err != nil {
		t.Fatalf("OpenStateStore failed: %v", err)
	}

	api := &fakeColabAPI{listed: nil}
	mgr := New(cfg, api, store)
	defer mgr.Shutdown()

	// 1. 模拟运行中实例被 Colab 云端回收：runKeepaliveTick 必须取消 keepaliveCancel，防止残留僵尸协程。
	kaCtx, kaCancel := context.WithCancel(context.Background())
	mgr.mu.Lock()
	mgr.state = StateRunning
	mgr.endpoint = "gpu-l4-pruned"
	mgr.keepaliveCancel = kaCancel
	mgr.mu.Unlock()

	failures := 0
	mgr.runKeepaliveTick(kaCtx, &failures)

	if kaCtx.Err() == nil {
		t.Fatal("expected keepalive context to be cancelled when session is pruned")
	}
	if snap := mgr.Status(context.Background()); snap.State != StateStopped || snap.HasActiveVM {
		t.Fatalf("expected STOPPED with HasActiveVM=false, got state=%s hasActive=%v", snap.State, snap.HasActiveVM)
	}

	// 2. 非 RUNNING 状态（如启动失败残留 endpoint 的 StateError）不得启动或执行保活。
	mgr.mu.Lock()
	mgr.state = StateError
	mgr.endpoint = "gpu-l4-failed-start"
	mgr.proxyURL = "http://127.0.0.1:1"
	mgr.proxyToken = "fake-tok"
	mgr.proxyTokenExp = time.Now().Add(time.Hour)
	mgr.mu.Unlock()

	mgr.startKeepalive()
	mgr.mu.RLock()
	hasKA := mgr.keepaliveCancel != nil
	mgr.mu.RUnlock()
	if hasKA {
		t.Fatal("startKeepalive must be a no-op when state is not RUNNING")
	}

	mgr.runKeepaliveTick(context.Background(), &failures)
	if snap := mgr.Status(context.Background()); snap.State != StateError || !snap.HasActiveVM {
		t.Fatalf("runKeepaliveTick must not touch non-RUNNING state, got state=%s hasActive=%v", snap.State, snap.HasActiveVM)
	}
}

func TestStructuredStageProgressAndRcloneStatus(t *testing.T) {
	cfg := config.Default()
	cfg.Colab.StateFile = filepath.Join(t.TempDir(), "state.json")
	cfg.Colab.RcloneConfig = "./rclone.conf"
	store, err := OpenStateStore(cfg.Colab.StateFile)
	if err != nil {
		t.Fatalf("OpenStateStore failed: %v", err)
	}

	api := &fakeColabAPI{}
	mgr := New(cfg, api, store)
	defer mgr.Shutdown()

	if snap := mgr.Status(context.Background()); !snap.RcloneMode {
		t.Fatalf("expected RcloneMode=true when rclone_config is set")
	}

	// 模拟 deploy.py 输出结构化 [STAGE <pct>] 行与普通详情行
	mgr.setProgress(35, "准备部署", "启动云端部署脚本")
	mgr.appendStreamOutput("[STAGE 62] 恢复 custom_nodes 插件\n[NODES] 正在通过 rclone copyto 拉取插件快照\n")

	snap := mgr.Status(context.Background())
	if snap.ProgressPct != 62 {
		t.Fatalf("expected ProgressPct=62, got %d", snap.ProgressPct)
	}
	if snap.ProgressStage != "恢复 custom_nodes 插件" {
		t.Fatalf("unexpected ProgressStage: %q", snap.ProgressStage)
	}
	if !strings.Contains(snap.ProgressDetail, "rclone copyto") {
		t.Fatalf("unexpected ProgressDetail: %q", snap.ProgressDetail)
	}
}
