package colab

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed deploy.py
var deployScript string

// BuildDeployCode 生成带环境变量注入的远端部署 Python 脚本。
func BuildDeployCode(comfyPassword, hfToken, civitaiToken string) string {
	var b strings.Builder
	b.WriteString("import os\n")
	writeEnv := func(k, v string) {
		valJSON, _ := json.Marshal(v)
		fmt.Fprintf(&b, "os.environ[%q] = %s\n", k, string(valJSON))
	}
	writeEnv("COMFY_PASSWORD", comfyPassword)
	if hfToken != "" {
		writeEnv("HF_TOKEN", hfToken)
	}
	if civitaiToken != "" {
		writeEnv("CIVITAI_TOKEN", civitaiToken)
	}
	b.WriteString("\n")
	b.WriteString(deployScript)
	return b.String()
}

// CheckDriveMountCode 检查 /content/drive 是否已挂载。
const CheckDriveMountCode = `import os
print("DRIVE_IS_MOUNTED" if os.path.ismount("/content/drive") else "DRIVE_NOT_MOUNTED")
`

// MountDriveCode 触发 Colab 原生 Google Drive 挂载（配合内核 dfs_ephemeral 拦截器）。
const MountDriveCode = `from google.colab import drive
drive.mount("/content/drive")
`

// KeepalivePingCode 轻量保活心跳代码。
const KeepalivePingCode = `print("ping")`

// SyncPluginsCode 停机前将 custom_nodes 同步回 Google Drive（排除下载器与缓存）。
const SyncPluginsCode = `import os, subprocess
src = "/content/ComfyUI/custom_nodes/"
dst = "/content/drive/MyDrive/Colab/ComfyUI/custom_nodes/"
if os.path.ismount("/content/drive") and os.path.isdir(src):
    os.makedirs(dst, exist_ok=True)
    r = subprocess.run([
        "rsync", "-a", "--delete",
        "--exclude", "colab_model_downloader",
        "--exclude", "colab_local_image",
        "--exclude", "__pycache__",
        "--exclude", "*.pyc",
        src, dst
    ], capture_output=True, text=True)
    print(f"[SYNC] exit={r.returncode}")
    if r.stderr:
        print(r.stderr[-500:])
else:
    print("[SYNC] skipped (drive or custom_nodes absent)")
`

// BuildRotateAuthAndGetURLCode 用于中间件重启后热接管存活会话：
// 1. 仅在 ComfyUI 已存活时平滑轮换 nginx Basic Auth 密码（零中断）；
// 2. 若 tunnel 仍存活则直接返回现有 URL，否则自动重拉 cloudflared。
func BuildRotateAuthAndGetURLCode(comfyPassword string) string {
	pwJSON, _ := json.Marshal(comfyPassword)
	return fmt.Sprintf(`import os, re, subprocess, time, urllib.request
from pathlib import Path

pw = %s
def comfy_up():
    try:
        with urllib.request.urlopen("http://127.0.0.1:8188/system_stats", timeout=3) as r:
            return r.status == 200
    except Exception:
        return False

if not comfy_up():
    print("[RECONCILE] COMFY_NOT_RUNNING")
else:
    ht = subprocess.run(["htpasswd", "-niB", "comfy"], input=pw + "\n", capture_output=True, text=True, check=True).stdout
    Path("/content/.comfy_htpasswd").write_text(ht)
    os.chmod("/content/.comfy_htpasswd", 0o644)
    conf = Path("/content/nginx-comfy.conf")
    pidf = Path("/content/nginx-comfy.pid")
    if conf.exists() and pidf.exists():
        subprocess.run(["nginx", "-s", "reload", "-c", str(conf)], check=False)
    url = ""
    urlf = Path("/content/comfy_url.txt")
    cf_alive = subprocess.run(["pgrep", "-f", "cloudflared tunnel"], stdout=subprocess.DEVNULL).returncode == 0
    if cf_alive and urlf.exists():
        m = re.search(r"https://[a-zA-Z0-9-]+\.trycloudflare\.com", urlf.read_text(errors="replace"))
        if m:
            url = m.group(0)
    if not url:
        subprocess.run(["pkill", "-f", "cloudflared tunnel"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(1)
        cfl = Path("/content/cloudflared.log")
        if cfl.exists():
            cfl.unlink()
        f = open(cfl, "ab", buffering=0)
        subprocess.Popen(["/usr/local/bin/cloudflared", "tunnel", "--protocol", "http2", "--url", "http://127.0.0.1:8787"], stdout=f, stderr=subprocess.STDOUT, start_new_session=True)
        pat = re.compile(r"https://[a-zA-Z0-9-]+\.trycloudflare\.com")
        for _ in range(60):
            if cfl.exists():
                m = pat.search(cfl.read_text(errors="replace"))
                if m:
                    url = m.group(0)
                    urlf.write_text(url + "\n")
                    break
            time.sleep(1)
    if url:
        print("[READY_URL] " + url)
    else:
        print("[RECONCILE] NO_URL")
`, string(pwJSON))
}

// RestartTunnelCode 在隧道掉线但 VM 存活时自动拉起新的 cloudflared 隧道。
const RestartTunnelCode = `import re, subprocess, time
from pathlib import Path
subprocess.run(["pkill", "-f", "cloudflared tunnel"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
time.sleep(1)
cfl = Path("/content/cloudflared.log")
if cfl.exists():
    cfl.unlink()
f = open(cfl, "ab", buffering=0)
subprocess.Popen(["/usr/local/bin/cloudflared", "tunnel", "--protocol", "http2", "--url", "http://127.0.0.1:8787"], stdout=f, stderr=subprocess.STDOUT, start_new_session=True)
pat = re.compile(r"https://[a-zA-Z0-9-]+\.trycloudflare\.com")
for _ in range(60):
    if cfl.exists():
        m = pat.search(cfl.read_text(errors="replace"))
        if m:
            url = m.group(0)
            Path("/content/comfy_url.txt").write_text(url + "\n")
            print("[READY_URL] " + url)
            break
    time.sleep(1)
`

// RestartComfyCode 仅重启 ComfyUI 进程（保留 VM、Drive 挂载和隧道，方便装完插件生效）。
const RestartComfyCode = `import subprocess, sys, time, urllib.request
from pathlib import Path
subprocess.run(["pkill", "-f", "ComfyUI/main.py"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
time.sleep(3)
log_path = Path("/content/comfyui.log")
log_f = open(log_path, "ab", buffering=0)
subprocess.Popen([sys.executable, "/content/ComfyUI/main.py", "--listen", "127.0.0.1", "--port", "8188", "--dont-print-server", "--disable-mmap"], stdout=log_f, stderr=subprocess.STDOUT, start_new_session=True)
for _ in range(90):
    try:
        with urllib.request.urlopen("http://127.0.0.1:8188/system_stats", timeout=3) as r:
            if r.status == 200:
                print("[RESTART] ComfyUI UP")
                break
    except Exception:
        pass
    time.sleep(2)
else:
    raise RuntimeError("Timeout waiting for ComfyUI restart")
`
