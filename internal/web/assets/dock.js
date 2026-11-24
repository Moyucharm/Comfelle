(() => {
  if (window.__colabComfyDockMounted) return;
  window.__colabComfyDockMounted = true;

  const host = document.createElement("div");
  host.style.cssText = "position:fixed;top:8px;right:12px;z-index:2147483647;font-family:-apple-system,BlinkMacSystemFont,'Inter','Segoe UI',Roboto,sans-serif;";
  const shadow = host.attachShadow({ mode: "open" });

  shadow.innerHTML = `
    <style>
      .pill {
        display: inline-flex;
        align-items: center;
        gap: 6px;
        height: 28px;
        padding: 0 8px 0 10px;
        border-radius: 6px;
        background: rgba(17, 17, 19, 0.92);
        backdrop-filter: blur(8px);
        border: 1px solid #27272a;
        color: #fafafa;
        font-size: 12px;
        box-shadow: 0 4px 14px rgba(0, 0, 0, 0.35);
        user-select: none;
      }
      .dot {
        width: 6px;
        height: 6px;
        border-radius: 50%;
        background: #22c55e;
        flex-shrink: 0;
      }
      .dot.warn { background: #f59e0b; }
      .gpu { font-weight: 500; color: #e4e4e7; margin-right: 2px; }
      .sep { width: 1px; height: 12px; background: #27272a; }
      a, button {
        background: transparent;
        border: none;
        color: #a1a1aa;
        font-size: 12px;
        font-weight: 500;
        cursor: pointer;
        padding: 2px 5px;
        border-radius: 4px;
        text-decoration: none;
      }
      a:hover { color: #fafafa; background: #27272a; }
      .stop { color: #fca5a5; }
      .stop:hover { color: #fecaca; background: rgba(239, 68, 68, 0.16); }
      .mask {
        display: none;
        position: fixed;
        inset: 0;
        background: rgba(9, 9, 11, 0.72);
        align-items: center;
        justify-content: center;
      }
      .mask.open { display: flex; }
      .modal {
        background: #111113;
        border: 1px solid #27272a;
        border-radius: 8px;
        padding: 18px;
        width: 300px;
        color: #fafafa;
      }
      .modal h4 { margin: 0 0 6px; font-size: 14px; font-weight: 600; }
      .modal p { margin: 0 0 16px; font-size: 12px; color: #a1a1aa; line-height: 1.5; }
      .actions { display: flex; justify-content: flex-end; gap: 8px; }
      .btn-cancel { background: #18181b; border: 1px solid #27272a; color: #e4e4e7; height: 30px; padding: 0 12px; }
      .btn-confirm { background: rgba(239, 68, 68, 0.14); border: 1px solid rgba(239, 68, 68, 0.35); color: #fca5a5; height: 30px; padding: 0 12px; }
      .btn-confirm:hover { background: rgba(239, 68, 68, 0.24); }
    </style>
    <div class="pill">
      <span class="dot" id="dot"></span>
      <span class="gpu" id="gpu">Colab</span>
      <span class="sep"></span>
      <a href="/panel">面板</a>
      <button class="stop" id="stop-btn" type="button">关机</button>
    </div>
    <div class="mask" id="mask">
      <div class="modal">
        <h4>关闭实例</h4>
        <p>将同步插件并释放当前 Colab 实例。</p>
        <div class="actions">
          <button class="btn-cancel" id="cancel-btn" type="button">取消</button>
          <button class="btn-confirm" id="confirm-btn" type="button">确认关闭</button>
        </div>
      </div>
    </div>
  `;

  document.documentElement.appendChild(host);

  const dot = shadow.getElementById("dot");
  const gpuEl = shadow.getElementById("gpu");
  const mask = shadow.getElementById("mask");
  const confirmBtn = shadow.getElementById("confirm-btn");

  shadow.getElementById("stop-btn").onclick = () => mask.classList.add("open");
  shadow.getElementById("cancel-btn").onclick = () => mask.classList.remove("open");
  confirmBtn.onclick = async () => {
    confirmBtn.disabled = true;
    confirmBtn.textContent = "关闭中";
    try {
      await fetch("/panel/api/stop", {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-Requested-With": "fetch" },
        body: JSON.stringify({ force: false })
      });
      window.location.href = "/panel";
    } catch (_) {
      confirmBtn.disabled = false;
      confirmBtn.textContent = "确认关闭";
    }
  };

  async function poll() {
    try {
      const res = await fetch("/panel/api/status", { cache: "no-store" });
      if (res.status === 401) {
        window.location.href = "/panel/login";
        return;
      }
      if (!res.ok) return;
      const s = await res.json();
      if (s.gpu) gpuEl.textContent = s.gpu;
      if (s.state !== "RUNNING") {
        window.location.href = "/panel";
        return;
      }
      dot.className = s.tunnel_healthy ? "dot" : "dot warn";
    } catch (_) {}
  }

  poll();
  setInterval(poll, 15000);
})();
