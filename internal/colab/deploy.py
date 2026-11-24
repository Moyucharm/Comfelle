"""在 Google Colab VM 内执行的幂等 ComfyUI 部署脚本。

特性：
- 仅持久化 app / models / custom_nodes / workflows 到 Google Drive；
- 绝不持久化图片（output / input / temp / user 均排除在 Drive 同步之外，不启用任何本地收图扩展）；
- 使用本机 nginx (127.0.0.1:8787) 对全路径启用 HTTP Basic Auth（用户名 comfy，bcrypt 哈希）；
- 通过 Cloudflare Quick Tunnel 暴露加密隧道并输出 [READY_URL]；
- 幂等设计：对已运行的 ComfyUI / nginx / 插件同步守护支持平滑接管与密码热轮换。
"""

import os
import re
import shutil
import socket
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

DRIVE_MOUNT_PATH = '/content/drive'
PERSIST_ROOT = Path('/content/drive/MyDrive/Colab/ComfyUI')
LOCAL_COMFY_PATH = Path('/content/ComfyUI')
COMFY_REPO_URL = 'https://github.com/comfyanonymous/ComfyUI.git'
COMFY_REPO_BRANCH = 'master'
PERSIST_APP_PATH = PERSIST_ROOT / 'app'
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

def resolve(url):
    host = official_host(url)
    headers = {'User-Agent': 'ComfyUI-Colab-Downloader'}
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
    return final_url, filename

def download(task, url, dest):
    try:
        final_url, filename = resolve(url)
        task['filename'] = filename
        destfile = Path(dest) / filename
        if destfile.exists() and not Path(str(destfile) + '.aria2').exists():
            task['status'] = 'Already exists'
            return
        task['status'] = 'Downloading...'
        cmd = ['aria2c', *ARIA2_ARGS, '-d', str(dest), '-o', filename, final_url]
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


def comfy_is_up():
    try:
        with urllib.request.urlopen(f'http://127.0.0.1:{COMFY_PORT}/system_stats', timeout=3) as resp:
            return resp.status == 200
    except Exception:
        return False


def start_plugin_sync_daemon(interval=PLUGIN_SYNC_INTERVAL_SEC):
    check = subprocess.run(['pgrep', '-f', '/content/plugin_sync.sh'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if check.returncode == 0:
        print('[SYNC] plugin sync daemon already running', flush=True)
        return
    script = Path('/content/plugin_sync.sh')
    script.write_text(
        '#!/bin/bash\n'
        'while true; do\n'
        '  rsync -a --delete --exclude colab_model_downloader --exclude colab_local_image \\\n'
        '        --exclude __pycache__ --exclude "*.pyc" \\\n'
        '        /content/ComfyUI/custom_nodes/ /content/drive/MyDrive/Colab/ComfyUI/custom_nodes/ \\\n'
        '        >> /content/plugin_sync.log 2>&1\n'
        f'  sleep {interval}\n'
        'done\n'
    )
    script.chmod(0o755)
    subprocess.Popen(['bash', str(script)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
    print(f'[SYNC] detached plugin sync daemon started ({interval}s interval)', flush=True)


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
    attempts = [
        ['uv', 'pip', 'install', '--system', '-q', '-r', req],
        ['uv', 'pip', 'install', '--python', sys.executable, '-q', '-r', req],
        [sys.executable, '-m', 'pip', 'install', '-q', '--no-input', '-r', req],
    ]
    for cmd in attempts:
        print('[DEPS] $ ' + ' '.join(cmd), flush=True)
        proc = subprocess.run(cmd, capture_output=True, text=True, env=env if cmd[0] == 'uv' else None)
        if proc.returncode == 0:
            print('[DEPS] ok', flush=True)
            return
        tail = (proc.stderr or proc.stdout or '').strip().splitlines()[-20:]
        print('[DEPS] failed (exit %d):' % proc.returncode, flush=True)
        print('\n'.join('  ' + line for line in tail), flush=True)
    raise RuntimeError('Dependency install failed for ' + req)


def prepare():
    if not os.path.ismount(DRIVE_MOUNT_PATH):
        raise RuntimeError('Google Drive 未挂载到 /content/drive')
    PERSIST_ROOT.mkdir(parents=True, exist_ok=True)
    missing = [pkg for pkg, bin_name in [
        ('aria2', 'aria2c'), ('rsync', 'rsync'), ('nginx', 'nginx'),
        ('apache2-utils', 'htpasswd'), ('curl', 'curl')
    ] if shutil.which(bin_name) is None]
    if missing:
        run(['apt-get', 'update', '-qq'])
        run(['apt-get', 'install', '-y', '-qq', *missing])
    if shutil.which('uv') is None:
        run([sys.executable, '-m', 'pip', 'install', '-q', 'uv'])
    if not Path(CLOUDFLARED_BIN).exists():
        run(['curl', '-fsSL', CLOUDFLARED_URL, '-o', CLOUDFLARED_BIN])
        os.chmod(CLOUDFLARED_BIN, 0o755)


def install_comfy():
    excludes = [
        '--exclude', '/models/', '--exclude', '/custom_nodes/',
        '--exclude', '/input/', '--exclude', '/output/',
        '--exclude', '/temp/', '--exclude', '/user/',
        '--exclude', '/__pycache__/', '--exclude', '*.pyc',
    ]
    has_snapshot = (PERSIST_APP_PATH / 'main.py').is_file() and (PERSIST_APP_PATH / 'requirements.txt').is_file()
    if PERSIST_APP_PATH.exists() and any(PERSIST_APP_PATH.iterdir()) and not has_snapshot:
        raise RuntimeError(f'Incomplete persisted ComfyUI at {PERSIST_APP_PATH}')

    if has_snapshot:
        print('[APP] Restoring persistent ComfyUI from Drive...', flush=True)
        LOCAL_COMFY_PATH.mkdir(parents=True, exist_ok=True)
        run(['rsync', '-a', *excludes, str(PERSIST_APP_PATH) + '/', str(LOCAL_COMFY_PATH) + '/'])
    else:
        if not (LOCAL_COMFY_PATH / 'main.py').exists():
            print('[APP] First install: cloning ComfyUI...', flush=True)
            run(['git', 'clone', '--depth', '1', '-b', COMFY_REPO_BRANCH, COMFY_REPO_URL, str(LOCAL_COMFY_PATH)])
        PERSIST_APP_PATH.mkdir(parents=True, exist_ok=True)
        run(['rsync', '-a', *excludes, str(LOCAL_COMFY_PATH) + '/', str(PERSIST_APP_PATH) + '/'])

    env = os.environ.copy()
    env['UV_CACHE_DIR'] = '/content/uv_cache'
    install_requirements(LOCAL_COMFY_PATH / 'requirements.txt', env=env)

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
    (PERSIST_ROOT / 'custom_nodes').mkdir(exist_ok=True)
    link_workflows()
    print('[APP] Ready (ephemeral output/input/temp, zero image persistence).', flush=True)


def install_nodes():
    local = LOCAL_COMFY_PATH / 'custom_nodes'
    remote = PERSIST_ROOT / 'custom_nodes'
    excludes = ['--exclude', '__pycache__', '--exclude', '*.pyc', '--exclude', 'colab_model_downloader', '--exclude', 'colab_local_image']
    run(['rsync', '-a', *excludes, str(remote) + '/', str(local) + '/'])

    # 确保彻底移除任何残留的旧版 WebSocket 收图扩展
    stale_local_img = local / 'colab_local_image'
    if stale_local_img.exists():
        shutil.rmtree(stale_local_img, ignore_errors=True)

    manager = local / 'ComfyUI-Manager'
    if not manager.exists():
        run(['git', 'clone', '--depth', '1', MANAGER_REPO_URL, str(manager)])

    downloader = local / 'colab_model_downloader'
    (downloader / 'js').mkdir(parents=True, exist_ok=True)
    (downloader / '__init__.py').write_text(DOWNLOADER_BACKEND_SOURCE)
    (downloader / 'js' / 'downloader.js').write_text(DOWNLOADER_FRONTEND_SOURCE)

    for folder in local.iterdir():
        if not folder.is_dir() or folder.name.startswith('.'):
            continue
        req = folder / 'requirements.txt'
        if req.is_file():
            install_requirements(req)
        installer = folder / 'install.py'
        if installer.is_file():
            run([sys.executable, str(installer)], cwd=folder)


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

    # 若 nginx 已在运行则平滑 reload，否则全新启动
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
    subprocess.run(['pkill', '-f', 'cloudflared tunnel'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(1)
    cf_log = Path('/content/cloudflared.log')
    if cf_log.exists():
        cf_log.unlink()

    log_f = open(cf_log, 'ab', buffering=0)
    proc = subprocess.Popen(
        [CLOUDFLARED_BIN, 'tunnel', '--protocol', 'http2', '--url', f'http://127.0.0.1:{PROXY_PORT}'],
        stdout=log_f, stderr=subprocess.STDOUT, start_new_session=True,
    )

    deadline = time.time() + 90
    pattern = re.compile(r'https://[a-zA-Z0-9-]+\.trycloudflare\.com')
    while time.time() < deadline:
        if proc.poll() is not None:
            out = cf_log.read_text(errors='replace')[-1500:]
            raise RuntimeError(f'cloudflared exited early (code {proc.returncode}):\n{out}')
        if cf_log.exists():
            text = cf_log.read_text(errors='replace')
            m = pattern.search(text)
            if m:
                url = m.group(0)
                URL_FILE.write_text(url + '\n')
                print(f'[READY_URL] {url}', flush=True)
                return url
        time.sleep(1)
    raise RuntimeError('Timeout waiting for Cloudflare Quick Tunnel URL')


def start_comfy():
    if comfy_is_up():
        print('[RUN] ComfyUI already listening on 127.0.0.1:' + str(COMFY_PORT), flush=True)
        return
    log_path = Path('/content/comfyui.log')
    log_f = open(log_path, 'ab', buffering=0)
    proc = subprocess.Popen(
        [sys.executable, str(LOCAL_COMFY_PATH / 'main.py'),
         '--listen', '127.0.0.1', '--port', str(COMFY_PORT), *COMFY_LAUNCH_ARGS],
        stdout=log_f, stderr=subprocess.STDOUT, start_new_session=True,
    )
    print(f'[RUN] ComfyUI started (pid={proc.pid})', flush=True)
    deadline = time.time() + 180
    while time.time() < deadline:
        if proc.poll() is not None:
            tail = log_path.read_text(errors='replace')[-2000:]
            raise RuntimeError(f'ComfyUI exited early (code {proc.returncode}):\n{tail}')
        if comfy_is_up():
            print('[RUN] ComfyUI HTTP endpoint ready', flush=True)
            return
        time.sleep(2)
    raise RuntimeError('Timeout waiting for ComfyUI to listen on port ' + str(COMFY_PORT))


def main():
    pw = (os.environ.get('COMFY_PASSWORD') or '').strip()
    if not pw:
        raise RuntimeError('COMFY_PASSWORD environment variable is required')

    already_running = comfy_is_up() and (LOCAL_COMFY_PATH / 'main.py').is_file()
    if already_running:
        print('[REUSE] 检测到 ComfyUI 已在运行，跳过重复安装并刷新认证与隧道...', flush=True)
        configure_nginx(pw)
        start_plugin_sync_daemon()
        start_cloudflared()
        return

    print('[1/5] Preparing Colab environment...', flush=True)
    prepare()
    print('[2/5] Restoring ComfyUI source & persistent models...', flush=True)
    install_comfy()
    print('[3/5] Restoring custom nodes & model downloader...', flush=True)
    install_nodes()
    print('[4/5] Configuring nginx HTTP Basic Auth...', flush=True)
    configure_nginx(pw)
    start_plugin_sync_daemon()
    print('[5/5] Starting ComfyUI & Cloudflare Quick Tunnel...', flush=True)
    start_comfy()
    start_cloudflared()


if __name__ == '__main__':
    main()
