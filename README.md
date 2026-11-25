# Comfelle

用于自建服务器的 **纯 Go 单二进制** ComfyUI 认证反代与按需瞬态云算力网关。

平时本地仅驻留极轻量网关守护进程（零显卡开销），需要作画时一键唤起远端 GPU 实例并透明流式接入原生 ComfyUI，用罢同步快照、释放实例停止计费，本地全程不落盘任何生图文件。

## 核心特性

- **双层独立鉴权隔离**：
  - 外层（浏览器 ↔ 自建服务器）：独立账号密码（bcrypt 哈希 + 防爆破限速 + `HttpOnly` HMAC 签名 Cookie），未登录时**所有路径（含 `/`、`/ws`、`/api/*`）全拦截**。
  - 内层（自建服务器 ↔ 加密隧道 ↔ 远端实例内 nginx）：每次启动实例在内存中动态生成随机 24 字节强密码并启用全路径 HTTP Basic Auth，由网关自动注入认证头、剥离外层 Cookie，前端全程不接触上游隧道地址与内部密码。
- **优雅原生反代（零 iframe）**：
  - `/panel` 为独立控制面板，其余根路径 `/` 及 `/ws` 100% 透明流式反代至 ComfyUI（HTTP/2 多路复用加速首屏资源加载，独立 HTTP/1.1 通道承载 WebSocket），支持全屏操作、拖拽传图与内置模型下载器。
  - 可选在 ComfyUI 页面注入基于 Shadow DOM 的可拖拽极简悬浮球（实时显示算力规格、运行时长与额度余额，支持一键返回 `/panel` 或二次确认停机）。
- **零图片持久化**：
  - 网关纯内存流式转发、不落盘；远端同步规则硬排除 `/output/`、`/temp/`、`/input/`、`/user/`，图片仅在 ComfyUI 前端预览和按需下载。
- **会话热复用 + 免交互云存储挂载**：
  - 网关重启时自动对账本系统托管的存活实例（严格按实例标识隔离，绝不触碰账号下其他运行中的任务），零中断轮换上游密码并热接管反代与巡检。
  - 推荐配置 `rclone_config`（支持 S3 / Cloudflare R2 / 主流云盘）：全程免交互自动挂载 `models/` 与 `workflows/`；未配置 `rclone` 时自动回落至 `/panel` 面板交互式授权引导兜底。
- **运行时健康巡检 + 隧道自愈 + 指纹增量快照**：
  - 定时执行轻量运行时心跳巡检与上游探活，当加密隧道断连时自动重建验证并无缝热切上游地址。
  - ComfyUI 本体与 `custom_nodes/` 采用单文件 `.tar.gz` 快照极速拉取；后台与停机前基于文件树 SHA-256 指纹检测变更（无变动时 `O(1)` 直接跳过打包与上传），随后安全释放远端实例停止计费。

---

## 快速部署

### 1. 编译单二进制（Linux 服务器零外部运行时依赖）

```bash
go build -o comfelle ./cmd/comfelle
```

交叉编译（例如在本地开发机编译至 Linux amd64 服务器）：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o comfelle ./cmd/comfelle
```

### 2. 准备远端运行时凭据文件（`token.json`）

直接在无头服务器上完成远程授权登录：

```bash
./comfelle auth -out ./token.json
```

按终端提示在任意浏览器打开授权链接，粘贴返回的授权码回车，即可自动生成 `0600` 权限的 `token.json`（亦可使用 `scripts/export_local_token.py` 从本地桌面环境导出后上传至服务器）。

### 3. 准备配置文件 `config.yaml`

```bash
cp config.example.yaml config.yaml
./comfelle hash-password
```

将输出的 bcrypt 哈希填入 `config.yaml` 的 `auth.password_hash`。

**配置 rclone 持久化存储（推荐）**：
1. 在本地运行 `rclone config` 配置云存储远端（例如命名为 `storage`），将生成的 `rclone.conf`（保持 `chmod 600`）放入服务器工作目录。
2. 在 `config.yaml` 中填写：
   ```yaml
   runtime:
     rclone_config: "./rclone.conf"
     rclone_remote: "storage:Comfelle/ComfyUI"
   ```
3. 若留空 `rclone_config`，首次挂载存储时可在 `/panel` 控制台按提示链接完成云盘授权。

### 4. 启动服务与前置 HTTPS 反代

```bash
./comfelle serve -config ./config.yaml
```

或使用 Docker 部署（将 `config.yaml`、`token.json` 及可选 `rclone.conf` 放入宿主机 `./data` 目录）：

```bash
docker build -t comfelle .
docker run -d --name comfelle -p 127.0.0.1:8080:8080 -v "$(pwd)/data:/data" --restart unless-stopped comfelle
```

- `systemd` 服务模板位于 `deploy/comfelle.service`
- 前置 Nginx / Caddy HTTPS 配置模板位于 `deploy/nginx.example.conf` 与 `deploy/caddy.example.Caddyfile`

访问 `https://你的域名/panel` 登录后，即可按需启停远端 GPU 实例并进入原生全屏 ComfyUI。
