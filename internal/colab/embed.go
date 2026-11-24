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
	writeEnv("HF_TOKEN", hfToken)
	writeEnv("CIVITAI_TOKEN", civitaiToken)
	b.WriteString("\n")
	b.WriteString(deployScript)
	return b.String()
}

// CheckDriveMountCode 检查持久化存储（原生 Drive 或 rclone）是否已挂载。
const CheckDriveMountCode = `import os
mounted = os.path.ismount("/content/drive") or os.path.ismount("/content/drive/MyDrive/Colab/ComfyUI")
print("DRIVE_IS_MOUNTED" if mounted else "DRIVE_NOT_MOUNTED")
`

// BuildRcloneMountCode 生成 Colab 端 rclone 挂载脚本：安装 fuse3 与 rclone，校验远端目录并挂载 models/workflows。
// 仅在 Colab VM 临时盘写入 0600 配置，绝不触碰 Colab 原生 drive.mount 弹窗授权。
func BuildRcloneMountCode(rcloneConf, remote string) string {
	confJSON, _ := json.Marshal(rcloneConf)
	remoteJSON, _ := json.Marshal(strings.TrimRight(strings.TrimSpace(remote), "/"))
	return fmt.Sprintf(`import os, shutil, subprocess, threading, time
from pathlib import Path

conf_text = %s
remote = %s
conf_path = Path("/content/.rclone.conf")
remote_file = Path("/content/.rclone_remote")
mount_dir = Path("/content/drive/MyDrive/Colab/ComfyUI")
log_path = Path("/content/rclone.log")

conf_path.write_text(conf_text, encoding="utf-8")
os.chmod(conf_path, 0o600)
remote_file.write_text(remote + "\n", encoding="utf-8")
os.chmod(remote_file, 0o600)

if os.path.ismount(mount_dir):
    print("[RCLONE] 存储已挂载，跳过重复挂载", flush=True)
else:
    errs = []
    def _run_bg(fn):
        try:
            fn()
        except Exception as exc:
            errs.append(exc)

    threads = []
    need_rclone_apt = False
    if shutil.which("fusermount3") is None:
        def _install_fuse3():
            print("[RCLONE] 安装 fuse3...", flush=True)
            subprocess.run(["apt-get", "update", "-qq"], check=True)
            subprocess.run(["apt-get", "install", "-y", "-qq", "fuse3"], check=True)
        threads.append(threading.Thread(target=_run_bg, args=(_install_fuse3,)))
    if shutil.which("rclone") is None:
        def _install_rclone():
            nonlocal need_rclone_apt
            print("[RCLONE] 下载安装最新版 rclone...", flush=True)
            r = subprocess.run(
                ["bash", "-c", "curl -fsSL --retry 2 https://downloads.rclone.org/rclone-current-linux-amd64.zip -o /tmp/rclone.zip && unzip -qo /tmp/rclone.zip -d /tmp/rclone_unzip && cp /tmp/rclone_unzip/rclone-*/rclone /usr/local/bin/rclone && chmod 755 /usr/local/bin/rclone && rm -rf /tmp/rclone.zip /tmp/rclone_unzip"],
                capture_output=True, text=True,
            )
            if r.returncode != 0:
                need_rclone_apt = True
        threads.append(threading.Thread(target=_run_bg, args=(_install_rclone,)))

    for t in threads:
        t.start()
    for t in threads:
        t.join()
    if errs:
        raise errs[0]
    if need_rclone_apt and shutil.which("rclone") is None:
        subprocess.run(["apt-get", "update", "-qq"], check=True)
        subprocess.run(["apt-get", "install", "-y", "-qq", "rclone"], check=True)

    print(f"[RCLONE] 校验远端目录 {remote}...", flush=True)
    mk = subprocess.run(
        ["rclone", "mkdir", "--config", str(conf_path), remote],
        capture_output=True, text=True, timeout=30,
    )
    if mk.returncode != 0:
        raise RuntimeError(f"rclone 连接远端 {remote} 失败: {(mk.stderr or mk.stdout).strip()[-500:]}")

    mount_dir.mkdir(parents=True, exist_ok=True)
    subprocess.run(["pkill", "-f", "rclone mount"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(0.5)

    with open(log_path, "ab", buffering=0) as log_f:
        proc = subprocess.Popen(
            [
                "rclone", "mount", remote, str(mount_dir),
                "--config", str(conf_path),
                "--cache-dir", "/content/.rclone_cache",
                "--vfs-cache-mode", "full",
                "--vfs-cache-max-size", "15G",
                "--vfs-write-back", "3s",
                "--vfs-read-chunk-size", "32M",
                "--vfs-read-chunk-streams", "8",
                "--vfs-read-ahead", "512M",
                "--buffer-size", "128M",
                "--dir-cache-time", "1h",
                "--drive-pacer-min-sleep", "100ms",
                "--contimeout", "15s",
                "--timeout", "60s",
                "--low-level-retries", "5",
                "--retries", "3",
            ],
            stdout=log_f, stderr=subprocess.STDOUT, start_new_session=True,
        )

    deadline = time.time() + 30
    while time.time() < deadline:
        if os.path.ismount(mount_dir):
            print(f"[RCLONE] 已挂载 {remote} -> {mount_dir}", flush=True)
            break
        if proc.poll() is not None:
            tail = log_path.read_text(errors="replace")[-1200:] if log_path.exists() else ""
            raise RuntimeError(f"rclone mount 异常退出 (code {proc.returncode}):\n{tail}")
        time.sleep(0.5)
    else:
        tail = log_path.read_text(errors="replace")[-1200:] if log_path.exists() else ""
        raise RuntimeError(f"等待 rclone 挂载超时:\n{tail}")
`, string(confJSON), string(remoteJSON))
}

// MountDriveCode 触发 Colab 原生 Google Drive 挂载（未配置 rclone 时的兜底方案）。
const MountDriveCode = `from google.colab import drive
drive.mount("/content/drive")
`

// KeepalivePingCode 轻量保活心跳代码。
const KeepalivePingCode = `print("ping")`

// BuildSyncPluginsCode 生成调用 /content/sync_plugins.py 的代码；force=true 时跳过未变更短路强制上传。
func BuildSyncPluginsCode(force bool) string {
	extra := ""
	if force {
		extra = `, "--force"`
	}
	return fmt.Sprintf(`import subprocess, sys
from pathlib import Path
script = Path("/content/sync_plugins.py")
if script.is_file():
    r = subprocess.run([sys.executable, str(script)%s], capture_output=True, text=True)
    if r.stdout:
        print(r.stdout, end="", flush=True)
    if r.stderr:
        print(r.stderr, end="", file=sys.stderr, flush=True)
    if r.returncode != 0:
        raise RuntimeError(f"sync_plugins.py failed (exit {r.returncode}): {(r.stderr or r.stdout)[-1200:]}")
else:
    print("[SYNC] skipped (sync_plugins.py absent)")
`, extra)
}

// BuildRotateAuthAndGetURLCode 用于网关重启后热接管存活会话：
// 1. 仅在 ComfyUI 已存活时平滑轮换 nginx Basic Auth 密码（零中断）；
// 2. 若 tunnel 仍存活则直接返回现有 URL，否则调用统一脚本 /content/start_tunnel.py 重拉。
func BuildRotateAuthAndGetURLCode(comfyPassword string) string {
	pwJSON, _ := json.Marshal(comfyPassword)
	return fmt.Sprintf(`import os, re, subprocess, sys, urllib.request
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
    if url:
        print("[READY_URL] " + url)
    elif Path("/content/start_tunnel.py").is_file():
        r = subprocess.run([sys.executable, "/content/start_tunnel.py"], capture_output=True, text=True)
        if r.stdout:
            print(r.stdout, end="", flush=True)
        if r.stderr:
            print(r.stderr, end="", file=sys.stderr, flush=True)
        if r.returncode != 0:
            raise RuntimeError(f"start_tunnel.py failed (exit {r.returncode}): {(r.stderr or r.stdout)[-1200:]}")
    else:
        print("[RECONCILE] NO_URL")
`, string(pwJSON))
}

// RestartTunnelCode 在隧道掉线但 VM 存活时调用统一脚本 /content/start_tunnel.py 重拉隧道。
const RestartTunnelCode = `import subprocess, sys
r = subprocess.run([sys.executable, "/content/start_tunnel.py"], capture_output=True, text=True)
if r.stdout:
    print(r.stdout, end="", flush=True)
if r.stderr:
    print(r.stderr, end="", file=sys.stderr, flush=True)
if r.returncode != 0:
    raise RuntimeError(f"start_tunnel.py failed (exit {r.returncode}): {(r.stderr or r.stdout)[-1200:]}")
`

// RestartComfyCode 调用统一脚本 /content/start_comfy.py --restart 重启 ComfyUI 进程（保留 VM、存储挂载、环境变量与隧道）。
const RestartComfyCode = `import subprocess, sys
proc = subprocess.Popen(
    [sys.executable, "/content/start_comfy.py", "--restart"],
    stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
)
tail = []
assert proc.stdout is not None
for line in proc.stdout:
    print(line, end="", flush=True)
    tail.append(line)
    if len(tail) > 25:
        del tail[:-25]
proc.wait()
if proc.returncode != 0:
    raise RuntimeError(f"start_comfy.py --restart failed (exit {proc.returncode}): {''.join(tail)[-1200:]}")
`
