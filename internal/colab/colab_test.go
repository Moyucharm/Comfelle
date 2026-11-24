package colab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestPropagateDriveCredentialsRecordAndFileID(t *testing.T) {
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

	res, err := c.PropagateDriveCredentials(context.Background(), "ep-1", "1MyRealDriveNotebookID", true)
	if err != nil {
		t.Fatalf("PropagateDriveCredentials failed: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success=true")
	}
	if seenRecord != "true" || seenFileID != "1MyRealDriveNotebookID" || seenXSRF != "xsrf-tok-1" {
		t.Fatalf("unexpected params: record=%q file_id=%q xsrf=%q", seenRecord, seenFileID, seenXSRF)
	}

	// 未配置 file_id 时回退 record=false + empty.ipynb
	_, err = c.PropagateDriveCredentials(context.Background(), "ep-1", "", true)
	if err != nil {
		t.Fatalf("fallback PropagateDriveCredentials failed: %v", err)
	}
	if seenRecord != "false" || seenFileID != "empty.ipynb" {
		t.Fatalf("unexpected fallback: record=%q file_id=%q", seenRecord, seenFileID)
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
	_ = json.Valid([]byte("{}"))
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
