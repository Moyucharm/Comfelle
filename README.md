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
- **会话热复用 + 免弹窗持久化存储（rclone / 原生 Drive 双支持）**：
  - 网关重启时自动对账本系统托管的云端存活会话（严格按 `Endpoint` 隔离，绝不触碰账号下其他训练实例），零中断轮换 nginx 密码并热接管反代与保活。
  - 推荐配置 `rclone_config`（支持 Google Drive / Cloudflare R2 / S3）：全程免除 Colab `drive.mount` 弹窗与重复授权邮件；未配置 `rclone` 时自动回落至 `/panel` 交互式授权引导兜底。
- **内置保活守护 + 隧道自愈 + 增量感知快照同步**：
  - 默认每 8 分钟发送轻量 Jupyter Kernel 心跳，同时探活上游并在 `cloudflared` 掉线时自动重建验证并无缝热切。
  - ComfyUI 本体与 `custom_nodes/` 采用单文件 `.tar.gz` 快照极速拉取；后台与关机前基于文件指纹检测变更（未改动时 `O(1)` 秒跳过重复打包上传），释放 VM 停止计费。

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

**配置 rclone 免弹窗持久化存储（推荐）**：
1. 在本机运行 `rclone config` 创建 Google Drive（或 R2 / S3）远端（例如命名为 `gdrive`），将生成的 `rclone.conf`（保持 `chmod 600`）放到服务器目录。
2. 在 `config.yaml` 中填写：
   ```yaml
   colab:
     rclone_config: "./rclone.conf"
     rclone_remote: "gdrive:Colab/ComfyUI"
   ```
3. 若留空 `rclone_config`，首次挂载时可在 `/panel` 控制台按弹窗链接完成 Colab 原生 Google Drive 授权。

### 4. 启动服务与前置 HTTPS 反代

```bash
./colab-comfyui serve -config ./config.yaml
```

或使用 Docker 部署（将 `config.yaml`、`google_token.json` 及可选 `rclone.conf` 放入宿主机 `./data` 目录）：

```bash
docker build -t colab-comfyui .
docker run -d --name colab-comfyui -p 127.0.0.1:8080:8080 -v "$(pwd)/data:/data" --restart unless-stopped colab-comfyui
```

- `systemd` 服务模板见 [colab-comfyui.service](file:///home/azusa/dev/colab-comfyui/deploy/colab-comfyui.service)
- 前置 Nginx / Caddy HTTPS 配置模板见 [nginx.example.conf](file:///home/azusa/dev/colab-comfyui/deploy/nginx.example.conf) 与 [caddy.example.Caddyfile](file:///home/azusa/dev/colab-comfyui/deploy/caddy.example.Caddyfile)

访问 `https://你的域名/panel` 登录后，即可一键启停 Colab 实例并进入原生全屏 ComfyUI。
