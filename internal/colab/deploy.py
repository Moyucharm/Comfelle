"""在 Google Colab VM 内执行的幂等 ComfyUI 部署脚本。

特性：
- 仅持久化 app / models / custom_nodes / workflows 到 Google Drive；
- 绝不持久化图片（output / input / temp / user 均排除在 Drive 同步之外，不启用任何本地收图扩展）；
- 使用本机 nginx (127.0.0.1:8787) 对全路径启用 HTTP Basic Auth（用户名 comfy，bcrypt 哈希）；
- 通过 Cloudflare Quick Tunnel 暴露加密隧道并输出 [READY_URL]；
- 幂等设计：对已运行的 ComfyUI / nginx / 插件同步守护支持平滑接管与密码热轮换。
"""

import json
import os
import re
import shutil
import subprocess
import sys
import threading
import time
import urllib.request
from pathlib import Path

DRIVE_MOUNT_PATH = '/content/drive'
PERSIST_ROOT = Path('/content/drive/MyDrive/Colab/ComfyUI')
LOCAL_COMFY_PATH = Path('/content/ComfyUI')
COMFY_REPO_URL = 'https://github.com/comfyanonymous/ComfyUI.git'
COMFY_REPO_BRANCH = 'master'
PERSIST_NODES_DIR = PERSIST_ROOT / 'custom_nodes'
MANAGER_REPO_URL = 'https://github.com/ltdrdata/ComfyUI-Manager.git'
COMFY_PORT = 8188
PROXY_PORT = 8787
COMFY_LAUNCH_ARGS = ['--dont-print-server', '--disable-mmap']
PLUGIN_SYNC_INTERVAL_SEC = 300
CLOUDFLARED_URL = 'https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64'
CLOUDFLARED_BIN = '/usr/local/bin/cloudflared'
URL_FILE = Path('/content/comfy_url.txt')
NGINX_CONF = Path('/content/nginx-comfy.conf')
NGINX_PID = Path('/content/nginx-comfy.pid')
HTPASSWD_FILE = Path('/content/.comfy_htpasswd')

DOWNLOADER_BACKEND_SOURCE = r'''"""Optional ComfyUI model downloader limited to Hugging Face and Civitai."""
import os
import queue
import re
import subprocess
import threading
import urllib.parse
import urllib.request
from pathlib import Path
from aiohttp import web
import folder_paths
import server

TASKS = []
Q = queue.Queue()
ALLOWED_DIRS = {'checkpoints','diffusion_models','text_encoders','vae','loras','clip','clip_vision','controlnet','unet','upscale_models','embeddings','hypernetworks','diffusers','ipadapter'}
ARIA2_ARGS = ['--console-log-level=error','--summary-interval=1','--max-connection-per-server=8','-x','8','-s','8','-c','--allow-overwrite=false']

def official_host(url):
    parsed = urllib.parse.urlparse(url)
    if parsed.scheme != 'https' or parsed.username or parsed.password or parsed.port not in (None, 443):
        raise ValueError('HTTPS official download URL required')
    host = (parsed.hostname or '').lower()
    if host not in ('huggingface.co', 'www.civitai.com', 'civitai.com'):
        raise ValueError('Only huggingface.co and civitai.com are allowed')
    return host

USER_AGENT = 'ComfyUI-Colab-Downloader'

def resolve(url):
    host = official_host(url)
    headers = {'User-Agent': USER_AGENT}
    token = os.environ.get('HF_TOKEN', '') if host == 'huggingface.co' else os.environ.get('CIVITAI_TOKEN', '')
    if token:
        headers['Authorization'] = 'Bearer ' + token
    class SafeRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers_, newurl):
            dest = urllib.parse.urlparse(newurl)
            if dest.scheme != 'https':
                raise ValueError('Redirect must use HTTPS')
            next_req = super().redirect_request(req, fp, code, msg, headers_, newurl)
            if next_req is not None and dest.hostname != host:
                next_req.remove_header('Authorization')
            return next_req
    req = urllib.request.Request(url, headers=headers)
    with urllib.request.build_opener(SafeRedirect()).open(req, timeout=90) as resp:
        final_url = resp.geturl()
        disp = resp.headers.get('Content-Disposition', '')
        m = re.search(r"filename\*=UTF-8''([^;]+)", disp, re.I)
        if m:
            filename = urllib.parse.unquote(m.group(1))
        else:
            m = re.search(r'filename="?([^";]+)', disp, re.I)
            filename = m.group(1) if m else urllib.parse.unquote(os.path.basename(urllib.parse.urlparse(final_url).path))
    filename = os.path.basename(filename.replace('\\', '/')).strip()
    if not filename or filename in ('.', '..') or '/' in filename or '\x00' in filename:
        raise ValueError('Could not resolve a safe filename')
    aria2_headers = ['User-Agent: ' + USER_AGENT]
    final_host = (urllib.parse.urlparse(final_url).hostname or '').lower()
    if token and final_host == host:
        aria2_headers.append('Authorization: Bearer ' + token)
    return final_url, filename, aria2_headers

def download(task, url, dest):
    try:
        final_url, filename, aria2_headers = resolve(url)
        task['filename'] = filename
        destfile = Path(dest) / filename
        if destfile.exists() and not Path(str(destfile) + '.aria2').exists():
            task['status'] = 'Already exists'
            return
        task['status'] = 'Downloading...'
        header_args = [f'--header={h}' for h in aria2_headers]
        cmd = ['aria2c', *ARIA2_ARGS, *header_args, '-d', str(dest), '-o', filename, final_url]
        proc = subprocess.run(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
        if proc.returncode:
            raise RuntimeError((proc.stderr or '')[-300:])
        task['status'] = '下载完成: ' + filename
        if hasattr(folder_paths, 'filename_list_cache'):
            folder_paths.filename_list_cache.clear()
    except Exception as exc:
        task['status'] = '失败: ' + str(exc)[:200]

def worker():
    while True:
        task, url, folder = Q.get()
        try:
            download(task, url, folder)
        finally:
            Q.task_done()

threading.Thread(target=worker, daemon=True).start()

@server.PromptServer.instance.routes.get('/colab_downloader/dirs')
async def dirs(request):
    base = Path(folder_paths.models_dir)
    return web.json_response({'dirs': sorted(p.name for p in base.iterdir() if p.is_dir() and p.name in ALLOWED_DIRS)})

@server.PromptServer.instance.routes.get('/colab_downloader/tasks')
async def tasks(request):
    return web.json_response({'tasks': TASKS[:100]})

@server.PromptServer.instance.routes.post('/colab_downloader/download')
async def add(request):
    try:
        data = await request.json()
        subdir = data.get('subdir', '')
        url = data.get('url', '').strip()
        if not isinstance(subdir, str) or subdir not in ALLOWED_DIRS:
            raise ValueError('Choose one of the allowed model directories')
        official_host(url)
        base = Path(folder_paths.models_dir).resolve()
        target = (base / subdir).resolve()
        if os.path.commonpath([str(base), str(target)]) != str(base):
            raise ValueError('Invalid model directory')
        target.mkdir(parents=True, exist_ok=True)
        task = {'subdir': subdir, 'filename': 'Waiting', 'status': '排队中'}
        TASKS.insert(0, task)
        Q.put((task, url, target))
        return web.json_response({'ok': True})
    except (ValueError, TypeError) as exc:
        return web.json_response({'error': str(exc)}, status=400)

NODE_CLASS_MAPPINGS = {}
WEB_DIRECTORY = './js'
'''

DOWNLOADER_FRONTEND_SOURCE = r'''import { app } from "../../scripts/app.js";

const esc = (s) => String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
app.registerExtension({
    name: "colab.ModelDownloader",
    async setup() {
        const btn = document.createElement("button");
        btn.textContent = "Model Downloader";
        btn.style.cssText = "position:fixed;top:8px;right:240px;z-index:9998;padding:6px 12px;background:#2563eb;color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:13px;";
        document.body.appendChild(btn);

        const modal = document.createElement("div");
        modal.style.cssText = "display:none;position:fixed;top:50px;right:20px;width:440px;background:#1e1e1e;color:#e5e5e5;border:1px solid #444;border-radius:6px;padding:16px;z-index:9999;box-shadow:0 8px 24px rgba(0,0,0,0.6);font-family:sans-serif;font-size:13px;";
        modal.innerHTML = `
            <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:12px;">
                <strong>模型下载器 (aria2c)</strong>
                <button id="cd-close" style="background:none;border:none;color:#aaa;cursor:pointer;font-size:16px;">X</button>
            </div>
            <div style="margin-bottom:8px;">
                <label style="display:block;margin-bottom:4px;">目标目录 (models/...):</label>
                <input id="cd-subdir" list="cd-dirs-list" type="text" placeholder="diffusion_models / text_encoders / vae / loras" style="width:100%;box-sizing:border-box;padding:6px;background:#2d2d2d;color:#fff;border:1px solid #555;border-radius:4px;" />
                <datalist id="cd-dirs-list"></datalist>
            </div>
            <div style="margin-bottom:12px;">
                <label style="display:block;margin-bottom:4px;">下载 URL:</label>
                <input id="cd-url" type="text" placeholder="https://..." style="width:100%;box-sizing:border-box;padding:6px;background:#2d2d2d;color:#fff;border:1px solid #555;border-radius:4px;" />
            </div>
            <button id="cd-submit" style="width:100%;padding:8px;background:#16a34a;color:#fff;border:none;border-radius:4px;cursor:pointer;font-weight:bold;">加入下载队列</button>
            <div id="cd-tasks" style="margin-top:12px;max-height:200px;overflow-y:auto;border-top:1px solid #333;padding-top:8px;"></div>
        `;
        document.body.appendChild(modal);

        async function refreshDirs() {
            const res = await fetch("/colab_downloader/dirs");
            const data = await res.json();
            const dl = modal.querySelector("#cd-dirs-list");
            dl.innerHTML = data.dirs.map(d => `<option value="${esc(d)}"></option>`).join("");
        }

        let completedSet = new Set();
        async function refreshTasks() {
            if (modal.style.display === "none") return;
            const res = await fetch("/colab_downloader/tasks");
            const data = await res.json();
            const box = modal.querySelector("#cd-tasks");
            box.innerHTML = data.tasks.map(t => `<div style="padding:4px 0;border-bottom:1px solid #2a2a2a;"><div><b>[${esc(t.subdir)}]</b> ${esc(t.filename)}</div><div style="color:#9ca3af;">${esc(t.status)}</div></div>`).join("") || "<div>暂无下载任务</div>";
            for (const t of data.tasks) {
                if (t.status.startsWith("下载完成:") && !completedSet.has(t.filename)) {
                    completedSet.add(t.filename);
                    if (app.refreshComboInNodes) {
                        await app.refreshComboInNodes();
                    }
                }
            }
        }

        btn.onclick = async () => {
            modal.style.display = modal.style.display === "none" ? "block" : "none";
            if (modal.style.display === "block") {
                await refreshDirs();
                await refreshTasks();
            }
        };
        modal.querySelector("#cd-close").onclick = () => { modal.style.display = "none"; };
        modal.querySelector("#cd-submit").onclick = async () => {
            const subdir = modal.querySelector("#cd-subdir").value.trim();
            const urlInput = modal.querySelector("#cd-url");
            const url = urlInput.value.trim();
            if (!subdir || !url) return;
            await fetch("/colab_downloader/download", {
                method: "POST",
                headers: {"Content-Type": "application/json"},
                body: JSON.stringify({subdir, url})
            });
            urlInput.value = "";
            await refreshDirs();
            await refreshTasks();
        };
        setInterval(refreshTasks, 2000);
    }
});
'''


def run(argv, **kwargs):
    return subprocess.run(argv, check=True, **kwargs)


def emit_stage(pct, title):
    print(f'[STAGE {pct}] {title}', flush=True)


def run_with_heartbeat(label, argv, *, env=None, cwd=None, interval=8):
    start = time.monotonic()
    print(f'{label}...', flush=True)
    proc = subprocess.Popen(
        argv,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        env=env,
        cwd=cwd,
    )
    tail = []
    done = threading.Event()

    def _reader():
        try:
            assert proc.stdout is not None
            for raw in proc.stdout:
                line = raw.strip()
                if line:
                    tail.append(line)
                    if len(tail) > 25:
                        del tail[:-25]
        finally:
            done.set()

    t = threading.Thread(target=_reader, daemon=True)
    t.start()

    while proc.poll() is None:
        if done.wait(interval):
            break
        if proc.poll() is None:
            elapsed = int(time.monotonic() - start)
            hint = f' | {tail[-1][:80]}' if tail else ''
            print(f'{label} 进行中，已耗时 {elapsed}s{hint}', flush=True)

    proc.wait()
    t.join(timeout=2)
    elapsed = int(time.monotonic() - start)
    if proc.returncode != 0:
        msg = '\n'.join('  ' + l for l in tail[-20:])
        raise RuntimeError(f'{label} 失败 (exit {proc.returncode}, {elapsed}s):\n{msg}')
    print(f'{label} 完成，耗时 {elapsed}s', flush=True)


def comfy_is_up():
    try:
        with urllib.request.urlopen(f'http://127.0.0.1:{COMFY_PORT}/system_stats', timeout=3) as resp:
            return resp.status == 200
    except Exception:
        return False


RCLONE_CONF = Path('/content/.rclone.conf')
RCLONE_REMOTE_FILE = Path('/content/.rclone_remote')
COMFY_ENV_FILE = Path('/content/.comfy_env.json')
SYNC_PLUGINS_SCRIPT = Path('/content/sync_plugins.py')
START_TUNNEL_SCRIPT = Path('/content/start_tunnel.py')
START_COMFY_SCRIPT = Path('/content/start_comfy.py')


def persist_comfy_env():
    data = {
        'HF_TOKEN': os.environ.get('HF_TOKEN', ''),
        'CIVITAI_TOKEN': os.environ.get('CIVITAI_TOKEN', ''),
    }
    COMFY_ENV_FILE.write_text(json.dumps(data), encoding='utf-8')
    COMFY_ENV_FILE.chmod(0o600)


def rclone_remote_root():
    if RCLONE_CONF.is_file() and RCLONE_REMOTE_FILE.is_file() and shutil.which('rclone'):
        return RCLONE_REMOTE_FILE.read_text(encoding='utf-8').strip().rstrip('/')
    return ''


def storage_is_mounted():
    return os.path.ismount(PERSIST_ROOT) or os.path.ismount(DRIVE_MOUNT_PATH)


def rclone_lsf(rel_path):
    remote = rclone_remote_root()
    if not remote:
        return []
    target = f'{remote}/{rel_path.lstrip("/")}' if rel_path else remote
    r = subprocess.run(
        ['rclone', 'lsf', '--config', str(RCLONE_CONF), '--max-depth', '1', target],
        capture_output=True, text=True, timeout=30,
    )
    if r.returncode != 0:
        return []
    return [line.strip() for line in r.stdout.splitlines() if line.strip()]


def pull_archive(label, archive_name, dst_dir):
    dst_dir.mkdir(parents=True, exist_ok=True)
    remote = rclone_remote_root()
    if remote:
        if archive_name not in rclone_lsf(''):
            return False
        local_tmp = Path('/tmp') / archive_name
        local_tmp.unlink(missing_ok=True)
        run_with_heartbeat(
            f'{label}（rclone 直拉单文件）',
            ['rclone', 'copyto', '--config', str(RCLONE_CONF), f'{remote}/{archive_name}', str(local_tmp)],
            interval=8,
        )
        size_mb = local_tmp.stat().st_size / (1024 * 1024)
        run_with_heartbeat(
            f'{label}（解压 {size_mb:.1f} MB）',
            ['tar', '-xzf', str(local_tmp), '-C', str(dst_dir)],
            interval=8,
        )
        local_tmp.unlink(missing_ok=True)
        return True

    src_archive = PERSIST_ROOT / archive_name
    if not src_archive.is_file() or src_archive.stat().st_size == 0:
        return False
    size_mb = src_archive.stat().st_size / (1024 * 1024)
    run_with_heartbeat(
        f'{label}（解压 {size_mb:.1f} MB）',
        ['tar', '-xzf', str(src_archive), '-C', str(dst_dir)],
        interval=8,
    )
    return True


def push_archive(label, src_dir, archive_name, excludes):
    start = time.monotonic()
    local_tmp = Path('/tmp') / (archive_name + '.tmp')
    local_tmp.unlink(missing_ok=True)
    run_with_heartbeat(f'{label}（本地打包）', ['tar', '-czf', str(local_tmp), *excludes, '-C', str(src_dir), '.'], interval=8)
    size_mb = local_tmp.stat().st_size / (1024 * 1024)
    remote = rclone_remote_root()
    if remote:
        run_with_heartbeat(
            f'{label}（rclone 同步上传 {size_mb:.1f} MB）',
            ['rclone', 'copyto', '--config', str(RCLONE_CONF), str(local_tmp), f'{remote}/{archive_name}'],
            interval=8,
        )
    else:
        dst_archive = PERSIST_ROOT / archive_name
        dst_archive.parent.mkdir(parents=True, exist_ok=True)
        remote_tmp = dst_archive.with_name(dst_archive.name + '.tmp')
        shutil.copyfile(local_tmp, remote_tmp)
        os.replace(remote_tmp, dst_archive)
    local_tmp.unlink(missing_ok=True)
    elapsed = int(time.monotonic() - start)
    print(f'{label} 已写入持久化存储，大小 {size_mb:.1f} MB，耗时 {elapsed}s', flush=True)


def ensure_helper_scripts():
    SYNC_PLUGINS_SCRIPT.write_text(
        '''#!/usr/bin/env python3
import hashlib, os, shutil, subprocess, sys, time
from pathlib import Path

src = Path("/content/ComfyUI/custom_nodes")
dst = Path("/content/drive/MyDrive/Colab/ComfyUI/custom_nodes.tar.gz")
rclone_conf = Path("/content/.rclone.conf")
rclone_remote_file = Path("/content/.rclone_remote")
manifest_file = Path("/tmp/.custom_nodes.manifest")

force = "--force" in sys.argv[1:]
record_only = "--record-only" in sys.argv[1:]

TOP_SKIP = {"colab_model_downloader", "colab_local_image"}
DIR_SKIP = {".git", "__pycache__"}

def calc_digest(root: Path) -> str:
    entries = []
    root_str = str(root)
    for dirpath, dirnames, filenames in os.walk(root_str, followlinks=False):
        rel_dir = os.path.relpath(dirpath, root_str)
        if rel_dir == ".":
            dirnames[:] = sorted(d for d in dirnames if d not in TOP_SKIP and d not in DIR_SKIP)
        else:
            dirnames[:] = sorted(d for d in dirnames if d not in DIR_SKIP)
            entries.append(f"D:{rel_dir}")
        for fn in sorted(filenames):
            if fn.endswith(".pyc"):
                continue
            full = os.path.join(dirpath, fn)
            rel = fn if rel_dir == "." else f"{rel_dir}/{fn}"
            try:
                if os.path.islink(full):
                    entries.append(f"L:{rel}:{os.readlink(full)}")
                else:
                    st = os.stat(full, follow_symlinks=False)
                    entries.append(f"F:{rel}:{st.st_size}:{st.st_mtime_ns}")
            except OSError:
                continue
    return hashlib.sha256("\\n".join(entries).encode("utf-8")).hexdigest()

if not src.is_dir():
    print("[SYNC] skipped (custom_nodes absent)", flush=True)
    sys.exit(0)

digest = calc_digest(src)
if record_only:
    manifest_file.write_text(digest, encoding="utf-8")
    sys.exit(0)

has_rclone = rclone_conf.is_file() and rclone_remote_file.is_file() and shutil.which("rclone") is not None
mounted = os.path.ismount("/content/drive") or os.path.ismount("/content/drive/MyDrive/Colab/ComfyUI")
if not (has_rclone or mounted):
    print("[SYNC] skipped (storage unavailable)", flush=True)
    sys.exit(0)

if not force and manifest_file.is_file() and manifest_file.read_text(encoding="utf-8").strip() == digest:
    print("[SYNC] custom_nodes 未变更，跳过打包上传", flush=True)
    sys.exit(0)

start = time.monotonic()
local_tmp = Path("/tmp/custom_nodes.tar.gz.tmp")
local_tmp.unlink(missing_ok=True)
r = subprocess.run([
    "tar", "-czf", str(local_tmp),
    "--exclude=./colab_model_downloader",
    "--exclude=./colab_local_image",
    "--exclude=.git",
    "--exclude=__pycache__",
    "--exclude=*.pyc",
    "-C", str(src), ".",
], capture_output=True, text=True)
if r.returncode != 0:
    print(f"[SYNC] tar failed (exit {r.returncode}): {(r.stderr or r.stdout)[-500:]}", flush=True)
    sys.exit(r.returncode)

size_mb = local_tmp.stat().st_size / (1024 * 1024)
if has_rclone:
    remote = rclone_remote_file.read_text(encoding="utf-8").strip().rstrip("/")
    up = subprocess.run(
        ["rclone", "copyto", "--config", str(rclone_conf), str(local_tmp), f"{remote}/custom_nodes.tar.gz"],
        capture_output=True, text=True,
    )
    local_tmp.unlink(missing_ok=True)
    if up.returncode != 0:
        print(f"[SYNC] rclone copyto failed (exit {up.returncode}): {(up.stderr or up.stdout)[-500:]}", flush=True)
        sys.exit(up.returncode)
    manifest_file.write_text(digest, encoding="utf-8")
    print(f"[SYNC] rclone ok ({size_mb:.1f} MB, {int(time.monotonic() - start)}s)", flush=True)
else:
    dst.parent.mkdir(parents=True, exist_ok=True)
    remote_tmp = dst.with_name(dst.name + ".tmp")
    shutil.copyfile(local_tmp, remote_tmp)
    os.replace(remote_tmp, dst)
    local_tmp.unlink(missing_ok=True)
    manifest_file.write_text(digest, encoding="utf-8")
    print(f"[SYNC] ok ({size_mb:.1f} MB, {int(time.monotonic() - start)}s)", flush=True)
''',
        encoding='utf-8',
    )
    SYNC_PLUGINS_SCRIPT.chmod(0o755)

    START_TUNNEL_SCRIPT.write_text(
        f'''#!/usr/bin/env python3
import re, subprocess, sys, time
from pathlib import Path

subprocess.run(["pkill", "-f", "cloudflared tunnel"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
time.sleep(1)
cf_log = Path("/content/cloudflared.log")
cf_log.unlink(missing_ok=True)
with open(cf_log, "ab", buffering=0) as log_f:
    proc = subprocess.Popen(
        [{CLOUDFLARED_BIN!r}, "tunnel", "--protocol", "http2", "--url", "http://127.0.0.1:{PROXY_PORT}"],
        stdout=log_f, stderr=subprocess.STDOUT, start_new_session=True,
    )
pattern = re.compile(r"https://[a-zA-Z0-9-]+\\.trycloudflare\\.com")
deadline = time.time() + 90
while time.time() < deadline:
    if proc.poll() is not None:
        out = cf_log.read_text(errors="replace")[-1500:] if cf_log.exists() else ""
        print(f"[TUNNEL] cloudflared exited early (code {{proc.returncode}}):\\n{{out}}", file=sys.stderr, flush=True)
        sys.exit(1)
    if cf_log.exists():
        m = pattern.search(cf_log.read_text(errors="replace"))
        if m:
            url = m.group(0)
            Path({str(URL_FILE)!r}).write_text(url + "\\n", encoding="utf-8")
            print(f"[READY_URL] {{url}}", flush=True)
            sys.exit(0)
    time.sleep(1)
print("[TUNNEL] Timeout waiting for Cloudflare Quick Tunnel URL", file=sys.stderr, flush=True)
sys.exit(1)
''',
        encoding='utf-8',
    )
    START_TUNNEL_SCRIPT.chmod(0o755)

    START_COMFY_SCRIPT.write_text(
        f'''#!/usr/bin/env python3
import json, os, subprocess, sys, time, urllib.request
from pathlib import Path

PORT = {COMFY_PORT}
MAIN_PY = Path({str(LOCAL_COMFY_PATH / 'main.py')!r})
LAUNCH_ARGS = {COMFY_LAUNCH_ARGS!r}
ENV_FILE = Path({str(COMFY_ENV_FILE)!r})
LOG_PATH = Path("/content/comfyui.log")

def is_up():
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{{PORT}}/system_stats", timeout=3) as resp:
            return resp.status == 200
    except Exception:
        return False

restart = "--restart" in sys.argv[1:]
if restart:
    subprocess.run(["pkill", "-f", "ComfyUI/main.py"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    for _ in range(10):
        if subprocess.run(["pgrep", "-f", "ComfyUI/main.py"], stdout=subprocess.DEVNULL).returncode != 0:
            break
        time.sleep(0.5)
elif is_up():
    print(f"[RUN] ComfyUI already listening on 127.0.0.1:{{PORT}}", flush=True)
    sys.exit(0)

child_env = os.environ.copy()
if ENV_FILE.is_file():
    try:
        saved = json.loads(ENV_FILE.read_text(encoding="utf-8"))
        for k in ("HF_TOKEN", "CIVITAI_TOKEN"):
            v = saved.get(k, "")
            if v:
                child_env[k] = v
            else:
                child_env.pop(k, None)
    except Exception:
        pass

with open(LOG_PATH, "ab", buffering=0) as log_f:
    proc = subprocess.Popen(
        [sys.executable, str(MAIN_PY), "--listen", "127.0.0.1", "--port", str(PORT), *LAUNCH_ARGS],
        stdout=log_f, stderr=subprocess.STDOUT, env=child_env, start_new_session=True,
    )
print(f"[RUN] ComfyUI started (pid={{proc.pid}})", flush=True)
start_ts = time.monotonic()
last_report = start_ts
deadline = time.time() + 180
while time.time() < deadline:
    if proc.poll() is not None:
        tail = LOG_PATH.read_text(errors="replace")[-2000:] if LOG_PATH.exists() else ""
        print(f"ComfyUI exited early (code {{proc.returncode}}):\\n{{tail}}", file=sys.stderr, flush=True)
        sys.exit(1)
    if is_up():
        elapsed = int(time.monotonic() - start_ts)
        print(f"[RUN] ComfyUI HTTP endpoint ready，耗时 {{elapsed}}s", flush=True)
        sys.exit(0)
    now = time.monotonic()
    if now - last_report >= 8.0:
        elapsed = int(now - start_ts)
        last_line = ""
        try:
            lines = [l.strip() for l in LOG_PATH.read_text(errors="replace").splitlines() if l.strip()]
            if lines:
                last_line = f" | {{lines[-1][:80]}}"
        except Exception:
            pass
        print(f"[RUN] 等待 ComfyUI 启动就绪，已耗时 {{elapsed}}s{{last_line}}", flush=True)
        last_report = now
    time.sleep(2)

print(f"Timeout waiting for ComfyUI to listen on port {{PORT}}", file=sys.stderr, flush=True)
sys.exit(1)
''',
        encoding='utf-8',
    )
    START_COMFY_SCRIPT.chmod(0o755)


def start_plugin_sync_daemon(interval=PLUGIN_SYNC_INTERVAL_SEC):
    check = subprocess.run(['pgrep', '-f', '/content/plugin_sync.sh'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if check.returncode == 0:
        print('[SYNC] plugin sync daemon already running', flush=True)
        return
    script = Path('/content/plugin_sync.sh')
    script.write_text(
        '#!/bin/bash\n'
        'while true; do\n'
        f'  sleep {interval}\n'
        f'  {sys.executable} {SYNC_PLUGINS_SCRIPT} >> /content/plugin_sync.log 2>&1\n'
        'done\n',
        encoding='utf-8',
    )
    script.chmod(0o755)
    subprocess.Popen(['bash', str(script)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
    print(f'[SYNC] detached plugin archive sync daemon started ({interval}s interval)', flush=True)


def link_workflows():
    drive_dir = PERSIST_ROOT / 'workflows'
    drive_dir.mkdir(parents=True, exist_ok=True)
    local = LOCAL_COMFY_PATH / 'user' / 'default' / 'workflows'
    local.parent.mkdir(parents=True, exist_ok=True)
    if local.is_symlink():
        if local.resolve() == drive_dir.resolve():
            return
        local.unlink()
    elif local.is_dir():
        shutil.copytree(local, drive_dir, dirs_exist_ok=True)
        shutil.rmtree(local)
    elif local.exists():
        local.unlink()
    local.symlink_to(drive_dir, target_is_directory=True)
    print('[WORKFLOWS] linked ->', drive_dir, flush=True)


def install_requirements(req_path, env=None):
    req = str(req_path)
    req_name = Path(req).parent.name + '/' + Path(req).name
    attempts = [
        ['uv', 'pip', 'install', '--system', '-q', '-r', req],
        ['uv', 'pip', 'install', '--python', sys.executable, '-q', '-r', req],
        [sys.executable, '-m', 'pip', 'install', '-q', '--no-input', '-r', req],
    ]
    for cmd in attempts:
        try:
            run_with_heartbeat(
                f'[DEPS] 安装依赖 {req_name} ({cmd[0]})',
                cmd,
                env=env if cmd[0] == 'uv' else None,
                interval=10,
            )
            return
        except RuntimeError as exc:
            print(str(exc), flush=True)
    raise RuntimeError('Dependency install failed for ' + req)


def install_requirements_batch(req_paths, env=None):
    reqs = [str(p) for p in req_paths if Path(p).is_file()]
    if not reqs:
        return
    req_args = []
    for r in reqs:
        req_args.extend(['-r', r])
    label = f'[DEPS] 合并安装 {len(reqs)} 份依赖清单'
    attempts = [
        ['uv', 'pip', 'install', '--system', '-q', *req_args],
        ['uv', 'pip', 'install', '--python', sys.executable, '-q', *req_args],
    ]
    for cmd in attempts:
        try:
            run_with_heartbeat(f'{label} ({cmd[0]})', cmd, env=env, interval=10)
            return
        except RuntimeError as exc:
            print(f'[DEPS] 合并安装未一次性通过，将逐份安装兜底: {str(exc)[:200]}', flush=True)
    for r in reqs:
        install_requirements(r, env=env)


def prepare():
    if not storage_is_mounted():
        raise RuntimeError('持久化存储未挂载到 /content/drive 或 /content/drive/MyDrive/Colab/ComfyUI')
    PERSIST_ROOT.mkdir(parents=True, exist_ok=True)
    missing = [pkg for pkg, bin_name in [
        ('aria2', 'aria2c'), ('rsync', 'rsync'), ('nginx', 'nginx'),
        ('apache2-utils', 'htpasswd'), ('curl', 'curl')
    ] if shutil.which(bin_name) is None]

    errors = []

    def _task(fn):
        try:
            fn()
        except Exception as exc:
            errors.append(exc)

    threads = []
    if missing:
        def _apt():
            run_with_heartbeat('[ENV] 更新软件源', ['apt-get', 'update', '-qq'])
            run_with_heartbeat(f'[ENV] 安装系统工具 ({", ".join(missing)})', ['apt-get', 'install', '-y', '-qq', *missing])
        threads.append(threading.Thread(target=_task, args=(_apt,)))
    if shutil.which('uv') is None:
        threads.append(threading.Thread(target=_task, args=(
            lambda: run_with_heartbeat('[ENV] 安装 uv 包管理器', [sys.executable, '-m', 'pip', 'install', '-q', 'uv']),
        )))
    if not Path(CLOUDFLARED_BIN).exists():
        def _cf():
            run_with_heartbeat('[ENV] 下载 cloudflared 隧道客户端', ['curl', '-fsSL', CLOUDFLARED_URL, '-o', CLOUDFLARED_BIN])
            os.chmod(CLOUDFLARED_BIN, 0o755)
        threads.append(threading.Thread(target=_task, args=(_cf,)))

    for t in threads:
        t.start()
    for t in threads:
        t.join()
    if errors:
        raise errors[0]


def install_comfy():
    tar_excludes = [
        '--exclude=./models', '--exclude=./custom_nodes',
        '--exclude=./input', '--exclude=./output',
        '--exclude=./temp', '--exclude=./user',
        '--exclude=./.git', '--exclude=__pycache__', '--exclude=*.pyc',
    ]
    if (LOCAL_COMFY_PATH / 'main.py').is_file():
        print('[APP] 本地 /content/ComfyUI 已就绪，跳过重复解压', flush=True)
    elif not pull_archive('[APP] 恢复 ComfyUI 快照', 'app.tar.gz', LOCAL_COMFY_PATH):
        run_with_heartbeat('[APP] 极速拉取 ComfyUI 源码', ['git', 'clone', '--depth', '1', '-b', COMFY_REPO_BRANCH, COMFY_REPO_URL, str(LOCAL_COMFY_PATH)])
        push_archive('[APP] 上传单文件 ComfyUI 快照', LOCAL_COMFY_PATH, 'app.tar.gz', tar_excludes)

    local = LOCAL_COMFY_PATH / 'models'
    remote = PERSIST_ROOT / 'models'
    remote.mkdir(parents=True, exist_ok=True)
    if local.is_symlink():
        if local.resolve() != remote.resolve():
            local.unlink()
            local.symlink_to(remote, target_is_directory=True)
    elif local.is_dir():
        for source in local.rglob('*'):
            if source.is_file():
                dst = remote / source.relative_to(local)
                if not dst.exists():
                    dst.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copy2(source, dst)
        shutil.rmtree(local)
        local.symlink_to(remote, target_is_directory=True)
    elif local.exists():
        raise RuntimeError(f'Unexpected model path: {local}')
    else:
        local.symlink_to(remote, target_is_directory=True)

    for name in ['input', 'output', 'temp', 'custom_nodes', 'user']:
        (LOCAL_COMFY_PATH / name).mkdir(exist_ok=True)
    link_workflows()
    print('[APP] Ready (models/workflows symlinked, zero image persistence).', flush=True)


def install_nodes():
    local = LOCAL_COMFY_PATH / 'custom_nodes'
    local.mkdir(parents=True, exist_ok=True)

    pulled = pull_archive('[NODES] 恢复 custom_nodes 快照', 'custom_nodes.tar.gz', local)
    if not pulled:
        remote = rclone_remote_root()
        if remote:
            if rclone_lsf('custom_nodes'):
                run_with_heartbeat(
                    '[NODES] 首次迁移旧 custom_nodes 目录',
                    [
                        'rclone', 'copy',
                        '--config', str(RCLONE_CONF),
                        '--transfers', '4',
                        '--checkers', '8',
                        '--tpslimit', '8',
                        '--tpslimit-burst', '8',
                        '--drive-pacer-min-sleep', '100ms',
                        '--drive-chunk-size', '64M',
                        '--fast-list',
                        '--exclude', '.git/**',
                        '--exclude', '__pycache__/**',
                        '--exclude', '*.pyc',
                        '--exclude', 'colab_model_downloader/**',
                        '--exclude', 'colab_local_image/**',
                        '--stats', '5s',
                        '--stats-one-line',
                        '--stats-log-level', 'NOTICE',
                        f'{remote}/custom_nodes',
                        str(local),
                    ],
                    interval=6,
                )
                run_with_heartbeat('[NODES] 打包并上传 custom_nodes 快照', [sys.executable, str(SYNC_PLUGINS_SCRIPT), '--force'], interval=8)
        elif PERSIST_NODES_DIR.is_dir() and any(PERSIST_NODES_DIR.iterdir()):
            excludes = ['--exclude', '.git', '--exclude', '__pycache__', '--exclude', '*.pyc', '--exclude', 'colab_model_downloader', '--exclude', 'colab_local_image']
            run_with_heartbeat('[NODES] 首次从旧目录迁移 custom_nodes', ['rsync', '-a', *excludes, str(PERSIST_NODES_DIR) + '/', str(local) + '/'], interval=8)
            run_with_heartbeat('[NODES] 打包并上传 custom_nodes 快照', [sys.executable, str(SYNC_PLUGINS_SCRIPT), '--force'], interval=8)

    stale_local_img = local / 'colab_local_image'
    if stale_local_img.exists():
        shutil.rmtree(stale_local_img, ignore_errors=True)

    manager = local / 'ComfyUI-Manager'
    cloned_manager = False
    if not manager.exists():
        run_with_heartbeat('[NODES] 克隆 ComfyUI-Manager', ['git', 'clone', '--depth', '1', MANAGER_REPO_URL, str(manager)])
        cloned_manager = True

    downloader = local / 'colab_model_downloader'
    (downloader / 'js').mkdir(parents=True, exist_ok=True)
    (downloader / '__init__.py').write_text(DOWNLOADER_BACKEND_SOURCE)
    (downloader / 'js' / 'downloader.js').write_text(DOWNLOADER_FRONTEND_SOURCE)

    emit_stage(76, '安装 Python 依赖')
    env = os.environ.copy()
    env['UV_CACHE_DIR'] = '/content/uv_cache'
    req_list = []
    if (LOCAL_COMFY_PATH / 'requirements.txt').is_file():
        req_list.append(LOCAL_COMFY_PATH / 'requirements.txt')
    installers = []
    for folder in sorted(local.iterdir()):
        if not folder.is_dir() or folder.name.startswith('.'):
            continue
        req = folder / 'requirements.txt'
        if req.is_file():
            req_list.append(req)
        installer = folder / 'install.py'
        if installer.is_file():
            installers.append((folder, installer))

    install_requirements_batch(req_list, env=env)
    total = len(installers)
    for idx, (folder, installer) in enumerate(installers, 1):
        run_with_heartbeat(f'[NODES] 运行插件安装脚本 ({idx} 共 {total}): {folder.name}', [sys.executable, str(installer)], cwd=folder)

    if pulled and not cloned_manager:
        subprocess.run([sys.executable, str(SYNC_PLUGINS_SCRIPT), '--record-only'], check=False)


def configure_nginx(password):
    hashed = subprocess.run(['htpasswd', '-niB', 'comfy'], input=password + '\n', capture_output=True, text=True, check=True).stdout
    HTPASSWD_FILE.write_text(hashed)
    os.chmod(HTPASSWD_FILE, 0o644)
    config = f'''worker_processes 1;
error_log /content/nginx-comfy-error.log warn;
pid {NGINX_PID};
events {{ worker_connections 1024; }}
http {{
  access_log off;
  server_tokens off;
  client_max_body_size 500m;
  gzip on;
  gzip_vary on;
  gzip_proxied any;
  gzip_comp_level 5;
  gzip_min_length 1024;
  gzip_types text/plain text/css application/json application/javascript text/javascript application/xml image/svg+xml;
  map $http_upgrade $connection_upgrade {{ default upgrade; '' close; }}
  server {{
    listen 127.0.0.1:{PROXY_PORT};
    auth_basic "Private ComfyUI";
    auth_basic_user_file {HTPASSWD_FILE};
    location / {{
      proxy_pass http://127.0.0.1:{COMFY_PORT};
      proxy_http_version 1.1;
      proxy_set_header Upgrade $http_upgrade;
      proxy_set_header Connection $connection_upgrade;
      proxy_set_header Host $host;
      proxy_set_header X-Real-IP $remote_addr;
      proxy_set_header Authorization "";
      proxy_set_header X-Forwarded-Proto https;
      proxy_read_timeout 3600s;
      proxy_send_timeout 3600s;
      proxy_buffering off;
    }}
  }}
}}
'''
    NGINX_CONF.write_text(config)
    run(['nginx', '-t', '-c', str(NGINX_CONF)])

    if NGINX_PID.exists():
        try:
            pid = int(NGINX_PID.read_text().strip())
            os.kill(pid, 0)
            run(['nginx', '-s', 'reload', '-c', str(NGINX_CONF)])
            print('[NGINX] reloaded with active Basic Auth', flush=True)
            return
        except Exception:
            pass
    run(['nginx', '-c', str(NGINX_CONF)])
    print('[NGINX] started on 127.0.0.1:' + str(PROXY_PORT), flush=True)


def start_cloudflared():
    res = subprocess.run([sys.executable, str(START_TUNNEL_SCRIPT)], capture_output=True, text=True)
    if res.stdout:
        print(res.stdout, end='', flush=True)
    if res.stderr:
        print(res.stderr, end='', file=sys.stderr, flush=True)
    if res.returncode != 0:
        raise RuntimeError(f'start_tunnel.py failed (exit {res.returncode}): {(res.stderr or res.stdout)[-1200:]}')


def start_comfy():
    proc = subprocess.Popen(
        [sys.executable, str(START_COMFY_SCRIPT)],
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
    )
    tail = []
    assert proc.stdout is not None
    for line in proc.stdout:
        print(line, end='', flush=True)
        tail.append(line)
        if len(tail) > 25:
            del tail[:-25]
    proc.wait()
    if proc.returncode != 0:
        raise RuntimeError(f'start_comfy.py failed (exit {proc.returncode}): {"".join(tail)[-1200:]}')


def main():
    pw = (os.environ.get('COMFY_PASSWORD') or '').strip()
    if not pw:
        raise RuntimeError('COMFY_PASSWORD environment variable is required')

    persist_comfy_env()
    ensure_helper_scripts()
    already_running = comfy_is_up() and (LOCAL_COMFY_PATH / 'main.py').is_file()
    if already_running:
        emit_stage(86, '复用运行中的 ComfyUI 并刷新隧道')
        configure_nginx(pw)
        start_plugin_sync_daemon()
        start_cloudflared()
        return

    emit_stage(36, '准备 Colab 运行环境')
    prepare()
    emit_stage(48, '恢复 ComfyUI 本体')
    install_comfy()
    emit_stage(62, '恢复 custom_nodes 插件')
    install_nodes()
    emit_stage(86, '配置加密鉴权与同步守护')
    configure_nginx(pw)
    start_plugin_sync_daemon()
    emit_stage(92, '启动 ComfyUI 与加密隧道')
    start_comfy()
    start_cloudflared()


if __name__ == '__main__':
    main()
