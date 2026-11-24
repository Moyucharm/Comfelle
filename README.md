# colab-comfyui

用于自建服务器的 **纯 Go 单二进制** Google Colab ComfyUI 认证反代与实例管理网关。

## 核心特性

- **双层独立鉴权隔离**：
  - 外层（浏览器 ↔ 服务器）：独立账号密码（bcrypt 哈希 + 防爆破限速 + `HttpOnly` HMAC 签名 Cookie），未登录时**所有路径（含 `/`、`/ws`、`/api/*`）全拦截**。
  - 内层（服务器 ↔ Cloudflare Tunnel ↔ Colab 内 nginx）：每次启动 Colab 实例在内存中动态生成随机 24 字节强密码并启用全路径 HTTP Basic Auth，由网关自动注入认证头、剥离外层 Cookie，前端全程不接触 Tunnel URL 与 Colab 内部密码。
- **优雅原生反代（零 iframe）**：
  - `/panel` 为独立控制面板，其余根路径 `/` 及 `/ws` 100% 透明流式反代至 ComfyUI，支持全屏操作、拖拽传图、WebSocket 与模型下载器。
  - 可选在 ComfyUI 右上角注入基于 Shadow DOM 的极简悬浮胶囊（显示 GPU/连通状态，一键回 `/panel` 或二次确认停机）。
- **零图片持久化**：
  - 网关纯内存流式转发、不落盘；Colab 侧同步规则硬排除 `/output/`、`/temp/`、`/input/`、`/user/`，图片仅在 ComfyUI 前端预览和按需下载。
- **会话热复用 + 私有笔记本 Drive 持久化授权**：
  - 网关重启时自动对账云端活跃会话，零中断轮换 nginx 密码并热接管反代与保活。
  - 配置 Google Drive 中归你自己独有的空白 `.ipynb` `drive_notebook_file_id` 后，驱动 `credentials-propagation` 启用 `record=true` + 真实 `file_id`，并在首次授权后支持跨实例重连免重复弹窗（同时保留 `/panel` 弹窗引导兜底）。
- **内置保活守护 + 隧道自愈 + 停机前自动同步插件**：
  - 默认每 8 分钟发送轻量 Jupyter Kernel 心跳，防止 Colab 判空闲回收，同时探活上游并在 `cloudflared` 掉线时自动拉起热切。
  - 点击关闭实例前自动 `rsync` 持久化 `custom_nodes/` 到 Google Drive，再释放 VM 停止计费。

---

## 快速部署

### 1. 编译单二进制（Linux 服务器零 Python 依赖）

```bash
go build -o colab-comfyui ./cmd/colab-comfyui
```

交叉编译（例如在开发机编译给 Linux amd64 服务器）：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o colab-comfyui ./cmd/colab-comfyui
```

### 2. 准备 Google OAuth2 凭据文件（`google_token.json`）

**方式 A（推荐）：从本机已登录的 `google-colab-cli` 直接导出**

```bash
~/.local/share/uv/tools/google-colab-cli/bin/python scripts/export_local_token.py -o ./google_token.json
```

然后将 `google_token.json`（保持 `chmod 600`）上传到服务器工作目录。

**方式 B：直接在无头服务器上完成远程 OAuth2 登录**

```bash
./colab-comfyui auth-google -out ./google_token.json
```

按提示在任意浏览器打开授权链接，粘贴返回的 Authorization Code 回车即可自动生成 `0600` 权限的 `google_token.json`。

### 3. 准备配置文件 `config.yaml`

```bash
cp config.example.yaml config.yaml
./colab-comfyui hash-password
```

将输出的 bcrypt 哈希填入 `config.yaml` 的 `auth.password_hash`。

**配置 Drive 免重复授权（推荐）**：
1. 在自己的 Google Drive 中新建一个空白 Colab 笔记本（仅自己可见、**不要共享**）。
2. 复制浏览器地址栏中的文件 ID（例如 `https://colab.research.google.com/drive/1AbCdEf...` 中的 `1AbCdEf...`）。
3. 填入 `config.yaml` 的 `colab.drive_notebook_file_id`。

### 4. 启动服务与前置 HTTPS 反代

```bash
./colab-comfyui serve -config ./config.yaml
```

- `systemd` 服务模板见 [colab-comfyui.service](file:///home/azusa/dev/colab-comfyui/deploy/colab-comfyui.service)
- 前置 Nginx / Caddy HTTPS 配置模板见 [nginx.example.conf](file:///home/azusa/dev/colab-comfyui/deploy/nginx.example.conf) 与 [caddy.example.Caddyfile](file:///home/azusa/dev/colab-comfyui/deploy/caddy.example.Caddyfile)

访问 `https://你的域名/panel` 登录后，即可一键启停 Colab 实例并进入原生全屏 ComfyUI。
