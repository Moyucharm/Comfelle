(() => {
  if (window.__colabComfyDockMounted) return;
  window.__colabComfyDockMounted = true;

  const STORAGE_KEY = "colab_comfy_dock_pos_v1";
  const EDGE_MARGIN = 12;
  const DRAG_THRESHOLD = 6;

  const host = document.createElement("div");
  host.style.cssText = "position:fixed;inset:0;pointer-events:none;z-index:2147483647;font-family:-apple-system,BlinkMacSystemFont,'Inter','Segoe UI',Roboto,sans-serif;";
  const shadow = host.attachShadow({ mode: "open" });

  shadow.innerHTML = `
    <style>
      * { box-sizing: border-box; -webkit-tap-highlight-color: transparent; }
      .fab-wrap {
        position: fixed;
        top: 0;
        left: 0;
        pointer-events: auto;
        touch-action: none;
        user-select: none;
        -webkit-user-select: none;
      }
      .fab-wrap.snapping {
        transition: left 0.2s cubic-bezier(0.22, 1, 0.36, 1), top 0.2s cubic-bezier(0.22, 1, 0.36, 1);
      }
      .ball {
        width: 46px;
        height: 46px;
        border-radius: 50%;
        padding: 0;
        border: 1px solid #303036;
        background: rgba(17, 17, 19, 0.92);
        backdrop-filter: blur(10px);
        -webkit-backdrop-filter: blur(10px);
        color: #fafafa;
        box-shadow: 0 6px 20px rgba(0, 0, 0, 0.45);
        cursor: grab;
        display: flex;
        flex-direction: column;
        align-items: center;
        justify-content: center;
        gap: 3px;
        position: relative;
        transition: transform 0.14s ease, border-color 0.14s ease, box-shadow 0.14s ease;
      }
      .ball:hover {
        border-color: #52525b;
        box-shadow: 0 8px 24px rgba(0, 0, 0, 0.55);
      }
      .ball:active, .fab-wrap.dragging .ball {
        cursor: grabbing;
        transform: scale(0.95);
      }
      .fab-wrap.open .ball {
        border-color: #fafafa;
        box-shadow: 0 0 0 2px rgba(250, 250, 250, 0.16), 0 8px 24px rgba(0, 0, 0, 0.55);
      }
      .dot {
        width: 7px;
        height: 7px;
        border-radius: 50%;
        background: #22c55e;
        flex-shrink: 0;
      }
      .dot.warn { background: #f59e0b; }
      .ball-gpu {
        font-size: 11px;
        font-weight: 600;
        line-height: 1;
        letter-spacing: -0.02em;
        color: #f4f4f5;
      }
      .card {
        position: fixed;
        width: 232px;
        max-width: calc(100vw - 24px);
        padding: 12px;
        border-radius: 12px;
        background: rgba(17, 17, 19, 0.96);
        backdrop-filter: blur(14px);
        -webkit-backdrop-filter: blur(14px);
        border: 1px solid #27272a;
        box-shadow: 0 14px 36px rgba(0, 0, 0, 0.6);
        color: #fafafa;
        pointer-events: auto;
        opacity: 0;
        transform: scale(0.95);
        visibility: hidden;
        transition: opacity 0.14s ease, transform 0.14s ease, visibility 0.14s ease;
      }
      .card.open {
        opacity: 1;
        transform: scale(1);
        visibility: visible;
      }
      .card-head {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 8px;
        padding-bottom: 10px;
        border-bottom: 1px solid #27272a;
      }
      .card-title {
        display: flex;
        align-items: center;
        gap: 7px;
        font-size: 13px;
        font-weight: 600;
        color: #fafafa;
      }
      .card-state {
        font-size: 11px;
        font-weight: 400;
        color: #a1a1aa;
      }
      .icon-close {
        width: 24px;
        height: 24px;
        border-radius: 6px;
        border: none;
        background: transparent;
        color: #71717a;
        font-size: 16px;
        line-height: 1;
        cursor: pointer;
        display: inline-flex;
        align-items: center;
        justify-content: center;
      }
      .icon-close:hover { background: #27272a; color: #fafafa; }
      .info {
        display: grid;
        grid-template-columns: 1fr 1fr;
        gap: 8px;
        padding: 10px 0;
      }
      .info-item {
        display: flex;
        flex-direction: column;
        gap: 3px;
        padding: 7px 8px;
        border-radius: 7px;
        background: #18181b;
        border: 1px solid #27272a;
      }
      .info-k { font-size: 11px; color: #71717a; }
      .info-v {
        font-size: 12px;
        font-weight: 600;
        color: #f4f4f5;
        font-variant-numeric: tabular-nums;
      }
      .card-actions {
        display: grid;
        grid-template-columns: 1fr 1fr;
        gap: 8px;
      }
      .act-btn {
        height: 36px;
        border-radius: 8px;
        font-size: 12px;
        font-weight: 500;
        font-family: inherit;
        text-decoration: none;
        display: inline-flex;
        align-items: center;
        justify-content: center;
        cursor: pointer;
        transition: background 0.12s ease, border-color 0.12s ease;
      }
      .act-panel {
        background: #18181b;
        color: #fafafa;
        border: 1px solid #3f3f46;
      }
      .act-panel:hover { background: #27272a; }
      .act-stop {
        background: rgba(239, 68, 68, 0.12);
        color: #fca5a5;
        border: 1px solid rgba(239, 68, 68, 0.34);
      }
      .act-stop:hover { background: rgba(239, 68, 68, 0.22); }
      .mask {
        display: none;
        position: fixed;
        inset: 0;
        padding: 16px;
        background: rgba(9, 9, 11, 0.72);
        align-items: center;
        justify-content: center;
        pointer-events: auto;
      }
      .mask.open { display: flex; }
      .modal {
        background: #111113;
        border: 1px solid #27272a;
        border-radius: 10px;
        padding: 18px;
        width: 300px;
        max-width: 100%;
        color: #fafafa;
        box-shadow: 0 18px 48px rgba(0, 0, 0, 0.65);
      }
      .modal h4 { margin: 0 0 6px; font-size: 14px; font-weight: 600; }
      .modal p { margin: 0 0 16px; font-size: 12px; color: #a1a1aa; line-height: 1.5; }
      .modal-actions { display: flex; justify-content: flex-end; gap: 8px; }
      .btn-cancel, .btn-confirm {
        height: 34px;
        padding: 0 14px;
        border-radius: 7px;
        font-size: 12px;
        font-weight: 500;
        font-family: inherit;
        cursor: pointer;
      }
      .btn-cancel { background: #18181b; border: 1px solid #27272a; color: #e4e4e7; }
      .btn-confirm { background: rgba(239, 68, 68, 0.16); border: 1px solid rgba(239, 68, 68, 0.38); color: #fca5a5; }
      .btn-confirm:hover { background: rgba(239, 68, 68, 0.26); }
      @media (max-width: 640px) {
        .ball { width: 50px; height: 50px; }
        .act-btn { height: 40px; font-size: 13px; }
        .btn-cancel, .btn-confirm { height: 38px; font-size: 13px; }
      }
    </style>

    <div class="fab-wrap" id="fab-wrap">
      <button class="ball" id="ball-btn" type="button" aria-label="Comfelle 控制台菜单" aria-expanded="false">
        <span class="dot" id="ball-dot"></span>
        <span class="ball-gpu" id="ball-gpu">GPU</span>
      </button>
    </div>

    <div class="card" id="card" role="dialog" aria-label="Comfelle 实例状态">
      <div class="card-head">
        <div class="card-title">
          <span class="dot" id="card-dot"></span>
          <span id="card-gpu">Comfelle</span>
          <span class="card-state" id="card-state">运行中</span>
        </div>
        <button class="icon-close" id="close-card-btn" type="button" aria-label="收起">&times;</button>
      </div>
      <div class="info">
        <div class="info-item">
          <span class="info-k">运行时间</span>
          <span class="info-v" id="uptime-val">-</span>
        </div>
        <div class="info-item">
          <span class="info-k">积分余额</span>
          <span class="info-v" id="ccu-val">-</span>
        </div>
      </div>
      <div class="card-actions">
        <a class="act-btn act-panel" id="panel-link" href="/panel" target="_blank" rel="noopener noreferrer">跳转面板</a>
        <button class="act-btn act-stop" id="stop-btn" type="button">关闭实例</button>
      </div>
    </div>

    <div class="mask" id="mask">
      <div class="modal">
        <h4>关闭实例</h4>
        <p>将同步插件并释放当前 Colab 实例，确认关闭吗？</p>
        <div class="modal-actions">
          <button class="btn-cancel" id="cancel-btn" type="button">取消</button>
          <button class="btn-confirm" id="confirm-btn" type="button">确认关闭</button>
        </div>
      </div>
    </div>
  `;

  document.documentElement.appendChild(host);

  const fabWrap = shadow.getElementById("fab-wrap");
  const ballBtn = shadow.getElementById("ball-btn");
  const ballDot = shadow.getElementById("ball-dot");
  const ballGpu = shadow.getElementById("ball-gpu");
  const card = shadow.getElementById("card");
  const cardDot = shadow.getElementById("card-dot");
  const cardGpu = shadow.getElementById("card-gpu");
  const cardState = shadow.getElementById("card-state");
  const uptimeVal = shadow.getElementById("uptime-val");
  const ccuVal = shadow.getElementById("ccu-val");
  const mask = shadow.getElementById("mask");
  const confirmBtn = shadow.getElementById("confirm-btn");

  let isOpen = false;
  let startedAtMs = null;
  let posX = 0;
  let posY = 0;

  function viewportBox() {
    const vv = window.visualViewport;
    return {
      w: vv ? Math.floor(vv.width) : window.innerWidth,
      h: vv ? Math.floor(vv.height) : window.innerHeight,
      ox: vv ? Math.floor(vv.offsetLeft) : 0,
      oy: vv ? Math.floor(vv.offsetTop) : 0
    };
  }

  function ballSize() {
    return window.innerWidth <= 640 ? 50 : 46;
  }

  function clampPos(x, y) {
    const vp = viewportBox();
    const sz = ballSize();
    const minX = vp.ox + EDGE_MARGIN;
    const maxX = Math.max(minX, vp.ox + vp.w - sz - EDGE_MARGIN);
    const minY = vp.oy + EDGE_MARGIN;
    const maxY = Math.max(minY, vp.oy + vp.h - sz - EDGE_MARGIN);
    return {
      x: Math.min(maxX, Math.max(minX, x)),
      y: Math.min(maxY, Math.max(minY, y))
    };
  }

  function applyPos(x, y, animate) {
    const p = clampPos(x, y);
    posX = p.x;
    posY = p.y;
    fabWrap.classList.toggle("snapping", !!animate);
    fabWrap.style.left = `${posX}px`;
    fabWrap.style.top = `${posY}px`;
    if (isOpen) positionCard();
  }

  function loadInitialPos() {
    const vp = viewportBox();
    const sz = ballSize();
    let side = "right";
    let yRatio = 0.14;
    try {
      const saved = JSON.parse(localStorage.getItem(STORAGE_KEY) || "null");
      if (saved && (saved.side === "left" || saved.side === "right") && typeof saved.yRatio === "number") {
        side = saved.side;
        yRatio = Math.min(0.92, Math.max(0.05, saved.yRatio));
      }
    } catch (_) {}
    const x = side === "left" ? vp.ox + EDGE_MARGIN : vp.ox + vp.w - sz - EDGE_MARGIN;
    const y = vp.oy + Math.round(vp.h * yRatio);
    applyPos(x, y, false);
  }

  function savePos() {
    const vp = viewportBox();
    const sz = ballSize();
    const side = (posX + sz / 2) < (vp.ox + vp.w / 2) ? "left" : "right";
    const yRatio = vp.h > 0 ? Math.min(0.95, Math.max(0.02, (posY - vp.oy) / vp.h)) : 0.14;
    try {
      localStorage.setItem(STORAGE_KEY, JSON.stringify({ side, yRatio }));
    } catch (_) {}
    return side;
  }

  function positionCard() {
    const vp = viewportBox();
    const sz = ballSize();
    const cardW = Math.min(232, vp.w - EDGE_MARGIN * 2);
    const cardH = card.offsetHeight || 168;
    const gap = 10;

    let left = (posX + sz / 2) > (vp.ox + vp.w / 2)
      ? (posX + sz - cardW)
      : posX;
    left = Math.min(vp.ox + vp.w - cardW - EDGE_MARGIN, Math.max(vp.ox + EDGE_MARGIN, left));

    const openAbove = (posY + sz + gap + cardH) > (vp.oy + vp.h - EDGE_MARGIN);
    let top = openAbove ? (posY - cardH - gap) : (posY + sz + gap);
    top = Math.min(vp.oy + vp.h - cardH - EDGE_MARGIN, Math.max(vp.oy + EDGE_MARGIN, top));

    card.style.left = `${left}px`;
    card.style.top = `${top}px`;
  }

  function setOpen(next) {
    isOpen = !!next;
    fabWrap.classList.toggle("open", isOpen);
    card.classList.toggle("open", isOpen);
    ballBtn.setAttribute("aria-expanded", isOpen ? "true" : "false");
    if (isOpen) positionCard();
  }

  // 桌面鼠标 + 移动端触摸统一拖拽与点击识别
  let dragState = null;

  ballBtn.addEventListener("pointerdown", (e) => {
    if (e.button !== 0 && e.pointerType === "mouse") return;
    ballBtn.setPointerCapture(e.pointerId);
    fabWrap.classList.remove("snapping");
    dragState = {
      id: e.pointerId,
      startX: e.clientX,
      startY: e.clientY,
      originX: posX,
      originY: posY,
      moved: false
    };
  });

  ballBtn.addEventListener("pointermove", (e) => {
    if (!dragState || dragState.id !== e.pointerId) return;
    const dx = e.clientX - dragState.startX;
    const dy = e.clientY - dragState.startY;
    if (!dragState.moved && Math.hypot(dx, dy) > DRAG_THRESHOLD) {
      dragState.moved = true;
      fabWrap.classList.add("dragging");
      if (isOpen) setOpen(false);
    }
    if (dragState.moved) {
      applyPos(dragState.originX + dx, dragState.originY + dy, false);
    }
  });

  function finishPointer(e) {
    if (!dragState || dragState.id !== e.pointerId) return;
    const wasMoved = dragState.moved;
    dragState = null;
    fabWrap.classList.remove("dragging");
    try { ballBtn.releasePointerCapture(e.pointerId); } catch (_) {}

    if (wasMoved) {
      const vp = viewportBox();
      const sz = ballSize();
      const side = savePos();
      const snapX = side === "left" ? vp.ox + EDGE_MARGIN : vp.ox + vp.w - sz - EDGE_MARGIN;
      applyPos(snapX, posY, true);
    } else {
      setOpen(!isOpen);
    }
  }

  ballBtn.addEventListener("pointerup", finishPointer);
  ballBtn.addEventListener("pointercancel", finishPointer);

  shadow.getElementById("close-card-btn").onclick = () => setOpen(false);
  shadow.getElementById("panel-link").onclick = () => setOpen(false);
  shadow.getElementById("stop-btn").onclick = () => {
    setOpen(false);
    mask.classList.add("open");
  };
  shadow.getElementById("cancel-btn").onclick = () => mask.classList.remove("open");
  mask.addEventListener("click", (e) => {
    if (e.target === mask) mask.classList.remove("open");
  });

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

  document.addEventListener("pointerdown", (e) => {
    if (!isOpen) return;
    const path = e.composedPath ? e.composedPath() : [];
    if (path.includes(fabWrap) || path.includes(card)) return;
    setOpen(false);
  });

  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      if (mask.classList.contains("open")) mask.classList.remove("open");
      else if (isOpen) setOpen(false);
    }
  });

  window.addEventListener("resize", () => loadInitialPos());
  if (window.visualViewport) {
    window.visualViewport.addEventListener("resize", () => applyPos(posX, posY, false));
  }

  function fmtUptime(ms) {
    const sec = Math.max(0, Math.floor(ms / 1000));
    const h = Math.floor(sec / 3600);
    const m = Math.floor((sec % 3600) / 60);
    const s = sec % 60;
    const pad = (n) => String(n).padStart(2, "0");
    return `${pad(h)}:${pad(m)}:${pad(s)}`;
  }

  function renderUptime() {
    if (!startedAtMs) {
      uptimeVal.textContent = "-";
      return;
    }
    uptimeVal.textContent = fmtUptime(Date.now() - startedAtMs);
  }

  async function poll() {
    try {
      const res = await fetch("/panel/api/status", { cache: "no-store" });
      if (res.status === 401) {
        window.location.href = "/panel/login";
        return;
      }
      if (!res.ok) return;
      const s = await res.json();
      if (s.gpu) {
        ballGpu.textContent = s.gpu;
        cardGpu.textContent = s.gpu;
      }
      if (s.state !== "RUNNING") {
        window.location.href = "/panel";
        return;
      }
      const dotCls = s.tunnel_healthy ? "dot" : "dot warn";
      ballDot.className = dotCls;
      cardDot.className = dotCls;
      cardState.textContent = s.tunnel_healthy ? "运行中" : "探活中";
      ccuVal.textContent = typeof s.ccu_balance === "number" ? s.ccu_balance.toFixed(2) : "-";
      startedAtMs = s.started_at ? Date.parse(s.started_at) : null;
      renderUptime();
    } catch (_) {}
  }

  loadInitialPos();
  poll();
  setInterval(poll, 15000);
  setInterval(renderUptime, 1000);
})();
