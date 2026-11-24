package colab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func TestUUIDToWebSafeBase64(t *testing.T) {
	id := uuid.MustParse("12345678-1234-5678-1234-567812345678")
	got := UUIDToWebSafeBase64(id)
	want := "12345678_1234_5678_1234_567812345678........"
	if got != want || len(got) != 44 {
		t.Fatalf("got %q (len %d), want %q", got, len(got), want)
	}
}

func TestPropagateDriveCredentialsUsesStandardEmptyNotebook(t *testing.T) {
	var seenRecord string
	var seenFileID string
	var seenXSRF string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/tun/m/credentials-propagation/ep-1") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(")]}'\n" + `{"token":"xsrf-tok-1"}`))
			return
		}
		if r.Method == http.MethodPost {
			seenRecord = r.URL.Query().Get("record")
			seenXSRF = r.Header.Get("x-goog-colab-token")
			_ = r.ParseMultipartForm(1 << 20)
			seenFileID = r.FormValue("file_id")
			_, _ = w.Write([]byte(")]}'\n" + `{"success":true}`))
			return
		}
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), "0")
	c.SetDomain(srv.URL)

	for _, dryRun := range []bool{true, false} {
		seenRecord, seenFileID, seenXSRF = "", "", ""
		res, err := c.PropagateDriveCredentials(context.Background(), "ep-1", dryRun)
		if err != nil {
			t.Fatalf("PropagateDriveCredentials(dryRun=%v) failed: %v", dryRun, err)
		}
		if !res.Success {
			t.Fatalf("expected success=true for dryRun=%v", dryRun)
		}
		if seenRecord != "false" || seenFileID != "empty.ipynb" || seenXSRF != "xsrf-tok-1" {
			t.Fatalf("unexpected params for dryRun=%v: record=%q file_id=%q xsrf=%q", dryRun, seenRecord, seenFileID, seenXSRF)
		}
	}
}

func TestRuntimeExecuteWithColabDriveHook(t *testing.T) {
	var gotColabReply bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/kernels":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/kernels":
			_, _ = w.Write([]byte(`{"id":"kern-42"}`))
		case r.URL.Path == "/api/kernels/kern-42/channels":
			upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()

			var execReq jupyterMessage
			if err := conn.ReadJSON(&execReq); err != nil {
				return
			}
			parent := map[string]any{"msg_id": execReq.Header.MsgID}

			// 模拟 Colab 发起 dfs_ephemeral Drive 授权拦截请求
			_ = conn.WriteJSON(map[string]any{
				"header":        map[string]any{"msg_id": "req-1", "msg_type": "colab_request"},
				"parent_header": parent,
				"metadata":      map[string]any{"colab_msg_id": "cmsg-99"},
				"content":       map[string]any{"request": map[string]any{"authType": "dfs_ephemeral"}},
				"msg_type":      "colab_request",
			})

			var reply jupyterMessage
			if err := conn.ReadJSON(&reply); err == nil {
				if reply.Header.MsgType == "input_reply" && reply.Channel == "stdin" {
					val, _ := reply.Content["value"].(map[string]any)
					if val["colab_msg_id"] == "cmsg-99" {
						gotColabReply = true
					}
				}
			}

			// 回传 stdout、status:idle 和 execute_reply
			_ = conn.WriteJSON(map[string]any{
				"header":        map[string]any{"msg_id": "m2", "msg_type": "stream"},
				"parent_header": parent,
				"content":       map[string]any{"name": "stdout", "text": "Mounted at /content/drive\n"},
			})
			_ = conn.WriteJSON(map[string]any{
				"header":        map[string]any{"msg_id": "m3", "msg_type": "status"},
				"parent_header": parent,
				"content":       map[string]any{"execution_state": "idle"},
			})
			_ = conn.WriteJSON(map[string]any{
				"header":        map[string]any{"msg_id": "m4", "msg_type": "execute_reply"},
				"parent_header": parent,
				"content":       map[string]any{"status": "ok"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	rt := NewRuntimeClient(srv.URL, "proxy-tok", "", "sess-1")
	hookCalled := false
	res, err := rt.Execute(context.Background(), MountDriveCode, ExecuteOptions{
		AllowStdin:  true,
		IdleTimeout: 5 * time.Second,
		OnDriveAuthRequest: func(ctx context.Context, colabMsgID string) error {
			hookCalled = (colabMsgID == "cmsg-99")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !hookCalled || !gotColabReply {
		t.Fatalf("expected colab_request hook & reply, hookCalled=%v gotColabReply=%v", hookCalled, gotColabReply)
	}
	if !strings.Contains(res.Stdout, "Mounted at /content/drive") {
		t.Fatalf("unexpected stdout: %q", res.Stdout)
	}
}

func TestAPIErrorSummarizesHTMLBody(t *testing.T) {
	htmlBody := `<!DOCTYPE html><html lang=en><title>Error 404 (Not Found)!!1</title><style>*{margin:0}</style><body>404.</body></html>`
	apiErr := &APIError{
		StatusCode: http.StatusNotFound,
		Method:     http.MethodGet,
		Endpoint:   "/tun/m/unassign/gpu-l4-test",
		Body:       htmlBody,
	}
	got := apiErr.Error()
	if strings.Contains(got, "<style>") || strings.Contains(got, "<!DOCTYPE") {
		t.Fatalf("expected HTML tags to be stripped from error string, got %q", got)
	}
	if !strings.Contains(got, "Error 404 (Not Found)!!1") {
		t.Fatalf("expected title summary in error string, got %q", got)
	}

	plainErr := &APIError{
		StatusCode: http.StatusBadRequest,
		Method:     http.MethodPost,
		Endpoint:   "/tun/m/assign",
		Body:       `{"error":"quota exceeded"}`,
	}
	if !strings.Contains(plainErr.Error(), `{"error":"quota exceeded"}`) {
		t.Fatalf("expected plain JSON body to be preserved, got %q", plainErr.Error())
	}
}

func TestBuildRcloneMountCodeEscapesConfigAndRemote(t *testing.T) {
	code := BuildRcloneMountCode("[gdrive]\ntype = drive\ntoken = {\"access_token\":\"abc\"}\n", "gdrive:Colab/ComfyUI/")
	for _, want := range []string{"fuse3", `"--vfs-cache-mode", "full"`, `"--vfs-cache-max-size", "15G"`, `"--vfs-read-chunk-streams", "8"`, `"--dir-cache-time", "1h"`, `"--timeout", "60s"`, "need_rclone_apt"} {
		if !strings.Contains(code, want) {
			t.Fatalf("expected %s in rclone mount script", want)
		}
	}
	if !strings.Contains(code, `"gdrive:Colab/ComfyUI"`) {
		t.Fatalf("expected trimmed remote path in rclone mount script, got:\n%s", code)
	}
	if strings.Contains(code, "--allow-other") {
		t.Fatalf("must not pass --allow-other on Colab root container")
	}
}

func TestRuntimeExecuteRetriesTransientWSDialFailure(t *testing.T) {
	wsAttempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/kernels":
			_, _ = w.Write([]byte(`[{"id":"kern-retry"}]`))
		case r.URL.Path == "/api/kernels/kern-retry/channels":
			wsAttempts++
			if wsAttempts == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			var execReq jupyterMessage
			if err := conn.ReadJSON(&execReq); err != nil {
				return
			}
			parent := map[string]any{"msg_id": execReq.Header.MsgID}
			_ = conn.WriteJSON(map[string]any{
				"header":        map[string]any{"msg_id": "m1", "msg_type": "stream"},
				"parent_header": parent,
				"content":       map[string]any{"name": "stdout", "text": "OK_AFTER_RETRY\n"},
			})
			_ = conn.WriteJSON(map[string]any{
				"header":        map[string]any{"msg_id": "m2", "msg_type": "status"},
				"parent_header": parent,
				"content":       map[string]any{"execution_state": "idle"},
			})
			_ = conn.WriteJSON(map[string]any{
				"header":        map[string]any{"msg_id": "m3", "msg_type": "execute_reply"},
				"parent_header": parent,
				"content":       map[string]any{"status": "ok"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	rt := NewRuntimeClient(srv.URL, "proxy-tok", "", "sess-retry")
	res, err := rt.Execute(context.Background(), `print("OK_AFTER_RETRY")`, ExecuteOptions{IdleTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("expected Execute to succeed after WS retry, got: %v", err)
	}
	if wsAttempts != 2 || !strings.Contains(res.Stdout, "OK_AFTER_RETRY") {
		t.Fatalf("expected 2 WS attempts and OK_AFTER_RETRY, got attempts=%d stdout=%q", wsAttempts, res.Stdout)
	}
}

func TestBuildDeployCodeExcludesGitAndThrottlesRcloneMigration(t *testing.T) {
	code := BuildDeployCode("pw123", "hf_tok", "civ_tok")
	for _, want := range []string{
		`"--exclude=.git"`,
		`'--exclude', '.git/**'`,
		`'--transfers', '4'`,
		`'--tpslimit', '8'`,
		`'--drive-pacer-min-sleep', '100ms'`,
		`gzip on;`,
		`application/json`,
		`/tmp/.custom_nodes.manifest`,
		`/content/start_comfy.py`,
		`/content/.comfy_env.json`,
	} {
		if !strings.Contains(code, want) {
			t.Fatalf("expected deploy.py to contain %s", want)
		}
	}
	if strings.Contains(code, `'--transfers', '32'`) {
		t.Fatalf("deploy.py must not use 32 concurrent rclone transfers against Google Drive")
	}

	if py, err := exec.LookPath("python3"); err == nil {
		for name, src := range map[string]string{
			"deploy":       code,
			"rclone_mount": BuildRcloneMountCode("[gdrive]\ntype=drive\n", "gdrive:Colab/ComfyUI"),
			"sync_force":   BuildSyncPluginsCode(true),
			"sync_auto":    BuildSyncPluginsCode(false),
			"rotate_auth":  BuildRotateAuthAndGetURLCode("pw123"),
			"restart":      RestartComfyCode,
			"tunnel":       RestartTunnelCode,
		} {
			cmd := exec.Command(py, "-c", "import ast, sys; ast.parse(sys.stdin.read())")
			cmd.Stdin = strings.NewReader(src)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("embedded python %q has syntax error: %v\n%s", name, err, string(out))
			}
		}
	}
}
