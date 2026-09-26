/* =========================================================
 * FileManage 管理台 —— 前端逻辑（原生 JS，无任何依赖）
 * 页面经 go:embed 打包后由 /admin/ 伺服：
 *   - API 统一以 /api/v1 为前缀，同源调用；
 *   - token 存 localStorage（键 fm_admin_token），除登录外
 *     所有请求携带 Authorization: Bearer <token>；
 *   - 任意请求 401 时自动清 token 回到登录页。
 * ========================================================= */
"use strict";

/* ---------- 一、常量与全局状态 ---------- */
const TOKEN_KEY = "fm_admin_token"; // token 的 localStorage 键名（需求约定）
const USER_KEY = "fm_admin_user";   // 当前登录管理员信息缓存键
const API_BASE = "/api/v1";         // API 统一前缀

let token = "";            // Bearer token
let currentUser = null;    // {id, username, role}
let serverInfo = null;     // GET /admin/info 缓存（roots 等）
let usersCache = [];       // 用户列表缓存（用户组/ACL 主体下拉用）
let groupsCache = [];      // 用户组列表缓存（ACL 主体下拉用）
let aclEditor = null;      // 文件夹权限编辑器状态 {rootId, path, entries[]}

// 五种权限的键名与中文名（顺序即展示顺序）
const PERMS = [["view", "查看"], ["download", "下载"], ["upload", "上传"], ["delete", "删除"], ["share", "分享"]];

/* ---------- 二、通用工具 ---------- */
const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

// HTML 转义：所有插入 innerHTML 的动态内容都必须经过它，防止注入
function esc(v) {
  return String(v === undefined || v === null ? "" : v)
    .replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

// 时间显示：统一 toLocaleString("zh-CN")；空值显示 "-"
function fmtTime(v) {
  if (!v) return "-";
  const d = new Date(v);
  return isNaN(d.getTime()) ? "-" : d.toLocaleString("zh-CN");
}

// ISO/RFC3339 时间 → <input type="datetime-local"> 值（本地时区）
function toLocalInput(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

// 布尔值 → 中文徽章
function boolBadge(b, yesText, noText) {
  return b ? `<span class="badge green">${esc(yesText || "是")}</span>`
           : `<span class="badge gray">${esc(noText || "否")}</span>`;
}

// 表格空数据占位行
function emptyRow(colspan) { return `<tr><td class="empty" colspan="${colspan}">暂无数据</td></tr>`; }

// 复制文本到剪贴板（navigator.clipboard 失败时降级 execCommand）
async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast("已复制到剪贴板");
  } catch (e) {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand("copy"); toast("已复制到剪贴板"); }
    catch (e2) { toast("复制失败，请手动复制", false); }
    document.body.removeChild(ta);
  }
}

// 限速值显示：0 / 缺省 = 不限速
function fmtRate(n) { return n ? `${esc(n)} KB/s` : '<span class="muted">不限速</span>'; }

// 当前登录主体是否超级管理员（0.1.0 需求33：管理面分层的 UI 侧镜像判定，
// 服务端各接口另有权威强制——UI 仅隐藏入口，不构成安全边界）
function isSuper() { return !!(currentUser && currentUser.super); }

// 根据 rootId 查根目录显示名（info 未加载时回退为原始 id）
function rootName(rootId) {
  const roots = (serverInfo && serverInfo.roots) || [];
  const r = roots.find((x) => String(x.id) === String(rootId));
  return r ? (r.name || r.id) : rootId;
}

/* ---------- 三、Toast 提示条（成功绿/失败红，3 秒自动消失） ---------- */
function toast(msg, ok = true) {
  const el = document.createElement("div");
  el.className = "toast " + (ok ? "ok" : "err");
  el.textContent = msg;
  $("#toast-wrap").appendChild(el);
  requestAnimationFrame(() => el.classList.add("show")); // 下一帧触发进入动画
  setTimeout(() => { el.classList.remove("show"); setTimeout(() => el.remove(), 300); }, 3000);
}

/* ---------- 四、API 封装 ----------
 * 统一 JSON 序列化；非 2xx 读取 {error} 弹红色提示条后 throw；
 * 401 → 清除 token 并回到登录页。 */
async function api(path, { method = "GET", body } = {}) {
  const headers = {};
  if (token) headers["Authorization"] = "Bearer " + token;
  if (body !== undefined) headers["Content-Type"] = "application/json";
  let resp;
  try {
    resp = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  } catch (e) {
    toast("网络请求失败：" + e.message, false);
    throw e;
  }
  let data = null;
  const text = await resp.text(); // 响应体可能为空（如 204）
  if (text) { try { data = JSON.parse(text); } catch (e) { /* 非 JSON 响应，忽略 */ } }
  if (!resp.ok) {
    const msg = (data && (data.error || data.message)) || `请求失败（HTTP ${resp.status}）`;
    if (resp.status === 401) {
      forceLogout(); // 登录失效：清 token 回登录页
      toast(msg.startsWith("请求失败") ? "登录已失效，请重新登录" : msg, false);
    } else {
      toast(msg, false);
    }
    throw new Error(msg);
  }
  return data;
}

// 确保 admin/info 已加载（roots 下拉等依赖它），带缓存
async function ensureInfo() {
  if (!serverInfo) serverInfo = await api(API_BASE + "/admin/info");
  return serverInfo;
}

/* ---------- 五、模态框（自写 mask + card） ----------
 * buttons: [{text, primary?, danger?, onClick}] */
function openModal(title, bodyHTML, buttons = []) {
  $("#modal-title").textContent = title;
  $("#modal-body").innerHTML = bodyHTML;
  const foot = $("#modal-foot");
  foot.innerHTML = "";
  buttons.forEach((b) => {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "btn " + (b.primary ? "primary" : b.danger ? "danger" : "ghost");
    btn.textContent = b.text;
    btn.addEventListener("click", () => { if (b.onClick) b.onClick(); });
    foot.appendChild(btn);
  });
  $("#modal-mask").classList.remove("hidden");
}

function closeModal() {
  $("#modal-mask").classList.add("hidden");
  $("#modal-body").innerHTML = "";
  $("#modal-foot").innerHTML = "";
  aclEditor = null; // 关闭即释放 ACL 编辑状态
}

/* ---------- 六、登录 / 登出 / 视图切换 ---------- */
function showLogin() { $("#admin-view").classList.add("hidden"); $("#login-view").classList.remove("hidden"); }
function showAdmin() { $("#login-view").classList.add("hidden"); $("#admin-view").classList.remove("hidden"); }

// 清除本地登录态（401 或主动退出时调用），并回到登录页
function forceLogout() {
  token = ""; currentUser = null; serverInfo = null;
  usersCache = []; groupsCache = [];
  localStorage.removeItem(TOKEN_KEY);
  localStorage.removeItem(USER_KEY);
  closeModal();
  showLogin();
}

// 登录表单提交
async function handleLogin(ev) {
  ev.preventDefault();
  const username = $("#login-username").value.trim();
  const password = $("#login-password").value;
  if (!username || !password) { toast("请输入用户名和密码", false); return; }
  const btn = $("#login-btn");
  btn.disabled = true;
  btn.textContent = "登录中…";
  try {
    const data = await api(API_BASE + "/auth/login", { method: "POST", body: { username, password } });
    // 非 admin 角色即使登录成功也拒绝进入管理台
    if (!data || !data.user || data.user.role !== "admin") { toast("需要管理员权限", false); return; }
    token = data.token;
    currentUser = data.user;
    localStorage.setItem(TOKEN_KEY, token);
    localStorage.setItem(USER_KEY, JSON.stringify(currentUser));
    $("#login-password").value = "";
    enterAdmin();
    toast(`欢迎回来，${currentUser.username}`);
  } catch (e) { /* 错误信息已由 api() 弹提示条 */ }
  finally { btn.disabled = false; btn.textContent = "登 录"; }
}

// 进入管理台：填充顶栏并默认打开"概览"
function enterAdmin() {
  $("#current-user").textContent = currentUser
    ? `${currentUser.username}（${currentUser.super ? "超级管理员" : "管理员"}）` : "";
  showAdmin();
  refreshTabAccess(); // 0.1.0 需求33：降级模式页签收敛为 概览/超级管理员/数据库
  switchTab("overview");
}

/* ---------- 七、Tab 导航（切换时按需加载数据） ---------- */
const TAB_NAMES = ["overview", "users", "groups", "acls", "links", "audits", "db", "super"];
let activeTab = "overview";
function switchTab(name) {
  activeTab = name;
  TAB_NAMES.forEach((t) => $(`#tab-${t}`).classList.toggle("active", t === name));
  $$(".nav-item").forEach((btn) => btn.classList.toggle("active", btn.dataset.tab === name));
  if (name === "overview") loadOverview();
  if (name === "users") loadUsers();
  if (name === "groups") loadGroups();
  if (name === "acls") loadAcls();
  if (name === "links") loadLinks();
  if (name === "audits") loadAudits();
  if (name === "db") loadDbTab();
  if (name === "super") loadSuperTab();
}

// refreshTabAccess 按服务端状态决定页签可见性（0.1.0）：
//   - "超级管理员"页签仅超管本人可见（需求33，服务端同样强制 403）；
//   - 降级模式（无库）下依赖元数据的管理页签全部隐藏——超管仍可概览/配库/管理账户文件。
async function refreshTabAccess() {
  const isSuper = !!(currentUser && currentUser.super);
  let degraded = false;
  try { degraded = !!(await dbApi("/status")).degraded; } catch (e) { /* 探测失败按有库处理，进入时有 503 提示兜底 */ }
  dbStatusDegraded = degraded;
  $$(".nav-item").forEach((btn) => {
    const t = btn.dataset.tab;
    let show = true;
    if (t === "super") show = isSuper;
    else if (degraded && ["users", "groups", "acls", "links", "audits"].includes(t)) show = false;
    btn.style.display = show ? "" : "none";
  });
  if (degraded && ["users", "groups", "acls", "links", "audits"].includes(activeTab)) switchTab("overview");
}

/* ---------- 八、概览 tab：GET /admin/info 卡片展示 ---------- */
async function loadOverview() {
  try {
    serverInfo = await api(API_BASE + "/admin/info"); // 概览每次刷新最新值
    renderOverview(serverInfo || {});
  } catch (e) { /* 已弹提示 */ }
}

function statCard(label, valueHTML) {
  return `<div class="stat-card"><div class="stat-label">${esc(label)}</div><div class="stat-value">${valueHTML}</div></div>`;
}

function renderOverview(info) {
  const rl = info.rateLimit || {}, ver = info.versions || {};
  const cards = [
    statCard("版本", `v${esc(info.version || "-")}`),
    statCard("实例 ID", `<span class="mono small">${esc(info.instanceId || "-")}</span>`),
    statCard("数据库驱动", esc(info.dbDriver || "-")),
    statCard("Schema 版本", esc(info.schemaVersion ?? "-")),
    statCard("用户端口", `<span class="mono">${esc(info.listen || "-")}</span>`),
    statCard("管理端口（本面）", `<span class="mono">${esc(info.adminListen || "-")}</span>`),
    statCard("仅局域网访问（用户端口）", boolBadge(info.lanOnly, "是", "否")),
    statCard("管理界面 IP 准入", `<span class="badge purple">${esc((info.adminAccess && info.adminAccess.mode) || "local")}</span>`
      + (info.adminAccess && info.adminAccess.mode === "custom"
        ? ` <span class="muted small mono">${esc((info.adminAccess.ips || []).join(", "))}</span>` : "")),
    info.superAdminUsername ? statCard("超级管理员", `<span class="mono">${esc(info.superAdminUsername)}</span>`) : "",
    statCard("匿名访问", boolBadge(info.anonymousEnabled, "已开启", "已关闭")),
    statCard("TLS", boolBadge(info.tlsEnabled, "已启用", "未启用")),
    statCard("上传限速", fmtRate(rl.uploadKBps)),
    statCard("下载限速", fmtRate(rl.downloadKBps)),
    statCard("文件版本", ver.enabled
      ? `<span class="badge purple">已启用</span> <span class="muted">最多 ${esc(ver.maxVersions)} 版</span>`
      : '<span class="badge gray">未启用</span>'),
    statCard("回收站保留", `${esc(info.trashRetentionDays ?? "-")} 天`),
  ];
  $("#overview-grid").innerHTML = cards.join("");
  const roots = info.roots || [];
  $("#overview-roots").innerHTML = roots.length
    ? roots.map((r) => `<div class="root-card">
        <div class="root-name">📂 ${esc(r.name || r.id)}</div>
        <div class="root-path mono">${esc(r.path || "")}</div>
        <div class="root-id">ID: <span class="mono">${esc(r.id)}</span></div></div>`).join("")
    : '<div class="card muted" style="text-align:center">暂无数据</div>';
}

/* ---------- 九、用户管理 tab ---------- */
async function loadUsers() {
  try {
    // roots 复选框依赖 admin/info，与用户列表并行加载
    const [users] = await Promise.all([api(API_BASE + "/admin/users"), ensureInfo()]);
    usersCache = users || [];
    renderUsers();
  } catch (e) { /* 已弹提示 */ }
}

// 0.5.0 jail 模型：用户可见目录 = 管理员分配（user_roots）；未分配则用户端口一无所见
function rootsHTML(rs) {
  if (!rs || !rs.length) return '<span class="badge red">未分配（用户端口不可见）</span>';
  return rs.map((r) => `<span class="badge purple">${esc(rootName(r.rootId))}:${esc(r.subPath || "/")}${r.alias ? " · " + esc(r.alias) : ""}</span>`).join(" ");
}

function renderUsers() {
  const tbody = $("#users-tbody");
  if (!usersCache.length) { tbody.innerHTML = emptyRow(6); tbody.onclick = null; return; }
  tbody.innerHTML = usersCache.map((u) => {
    const isSelf = currentUser && String(u.id) === String(currentUser.id);
    // 需求33：管理员账户（非超管）对普通管理员只读——编辑/删除禁用（服务端同样 403）
    const adminLocked = u.role === "admin" && !isSuper();
    const lockTitle = adminLocked ? ' disabled title="只有超级管理员可以管理管理员账户"' : "";
    return `<tr>
      <td class="mono">${esc(u.id)}</td>
      <td>${esc(u.username)}${isSelf ? ' <span class="badge blue">我</span>' : ""}</td>
      <td>${u.role === "admin" ? '<span class="badge purple">管理员</span>' : '<span class="badge gray">普通用户</span>'}</td>
      <td>${rootsHTML(u.userRoots)}</td>
      <td class="muted">${fmtTime(u.createdAt)}</td>
      <td class="actions">
        <button type="button" class="btn sm ghost" data-act="edit" data-id="${esc(u.id)}"${lockTitle}>编辑</button>
        <button type="button" class="btn sm danger" data-act="del" data-id="${esc(u.id)}"
          ${isSelf ? 'disabled title="不能删除自己"' : lockTitle}>删除</button>
      </td></tr>`;
  }).join("");
  // 事件委托：编辑 / 删除（删除需 confirm，且不能删除自己）
  tbody.onclick = async (ev) => {
    const btn = ev.target.closest("button[data-act]");
    if (!btn || btn.disabled) return;
    const u = usersCache.find((x) => String(x.id) === btn.dataset.id);
    if (!u) return;
    if (btn.dataset.act === "edit") { openUserModal(u); return; }
    if (!confirm(`确定删除用户「${u.username}」吗？此操作不可恢复。`)) return;
    try {
      await api(`${API_BASE}/admin/users/${encodeURIComponent(u.id)}`, { method: "DELETE" });
      toast("用户已删除");
      loadUsers();
    } catch (e) { /* 已弹提示 */ }
  };
}

// 新建（user 为 null）/ 编辑用户模态框。
// 0.5.0 jail 模型：目录分配编辑器（一人可多个 (根,子目录)；子目录留空=整根；
// 内联 📁 浏览物理根选择子目录；分配不存在的子目录时服务端自动创建）。
function openUserModal(user) {
  const editing = !!user;
  const roots = (serverInfo && serverInfo.roots) || [];
  const jailRows = ((user && user.userRoots) || []).map((r) => ({
    rootId: String(r.rootId), subPath: r.subPath || "", alias: r.alias || "",
  }));
  let pickIdx = -1;     // 正在内联浏览目录的行
  let pickPath = "";    // 浏览游标（相对所选根）
  let pickList = [];    // 当前目录条目
  let pickErr = "";

  openModal(editing ? "编辑用户" : "新建用户", `
    <div class="form-item"><label>用户名</label>
      <input id="um-username" type="text" value="${esc(user ? user.username : "")}"
        placeholder="登录用户名" ${editing ? "disabled" : ""} autocomplete="off"></div>
    <div class="form-item"><label>${editing ? "新密码" : "密码"}</label>
      <input id="um-password" type="password" autocomplete="new-password"
        placeholder="${editing ? "留空表示不修改密码" : "登录密码"}"></div>
    <div class="form-item"><label>角色 <span class="muted">${isSuper() ? "" : "（仅超级管理员可创建/修改管理员账户）"}</span></label>
      <select id="um-role" ${user && user.role === "admin" && !isSuper() ? "disabled" : ""}>
        <option value="user" ${!user || user.role === "user" ? "selected" : ""}>普通用户</option>
        ${isSuper() ? `<option value="admin" ${user && user.role === "admin" ? "selected" : ""}>管理员</option>` : ""}
      </select></div>
    <div class="form-item"><label>分配目录 <span class="muted">（用户只能看到并管理下列目录；子目录留空 = 整个根目录。未分配 = 用户端口不可见；管理员用户端口同样只看到自己的分配，物理根浏览仅限本管理台）</span></label>
      <div id="um-jails"></div>
      <button type="button" class="btn sm ghost" id="um-addjail">＋ 添加目录分配</button></div>`,
    [{ text: "取消", onClick: closeModal }, { text: "保存", primary: true, onClick: save }]);

  function renderJails() {
    const wrap = $("#um-jails");
    if (!wrap) return;
    let html = jailRows.length ? "" : '<div class="muted" style="margin:4px 0">暂无分配 —— 该用户登录后看不到任何目录</div>';
    jailRows.forEach((r, i) => {
      html += `<div class="jail-row" data-i="${i}" style="display:flex;gap:6px;align-items:center;margin:4px 0">
        <select data-f="rootId" style="min-width:110px">${roots.map((rt) =>
          `<option value="${esc(rt.id)}" ${String(rt.id) === r.rootId ? "selected" : ""}>${esc(rt.name || rt.id)}</option>`).join("")}</select>
        <input data-f="subPath" class="mono" type="text" value="${esc(r.subPath)}" placeholder="子目录（相对根，留空=整根）" style="flex:1;min-width:120px">
        <button type="button" class="btn sm ghost" data-pick title="浏览选择目录">📁</button>
        <input data-f="alias" type="text" value="${esc(r.alias)}" placeholder="别名(可选)" style="width:86px">
        <button type="button" class="btn sm danger" data-delj title="移除该分配">✕</button>
      </div>`;
      if (pickIdx === i) {
        html += `<div class="fs-pick" style="border:1px solid var(--border);border-radius:8px;padding:8px;margin:2px 0 6px 4px">
          <div class="muted mono" style="margin-bottom:6px">${esc(rootName(r.rootId))} / ${esc(pickPath)}</div>
          <div style="display:flex;gap:6px;flex-wrap:wrap">
            <button type="button" class="btn sm ghost" data-nav="">（整根）</button>
            ${pickPath ? `<button type="button" class="btn sm ghost" data-nav="${esc(pickPath.includes("/") ? pickPath.slice(0, pickPath.lastIndexOf("/")) : "")}">↩ 上级</button>` : ""}
            <button type="button" class="btn sm primary" data-use>✔ 选择此目录</button>
          </div>
          <div style="max-height:170px;overflow:auto;margin-top:6px">
            ${pickErr ? `<div class="muted">${esc(pickErr)}</div>` : (pickList.length
              ? pickList.map((d) => `<button type="button" class="btn sm ghost fs-item" data-nav="${esc(d.path)}" style="display:block;text-align:left;width:100%;margin:2px 0">📁 ${esc(d.name)}</button>`).join("")
              : '<div class="muted">（无子目录）</div>')}
          </div></div>`;
      }
    });
    wrap.innerHTML = html;
    wrap.querySelectorAll(".jail-row").forEach((row) => {
      const i = Number(row.dataset.i);
      row.querySelectorAll("[data-f]").forEach((el) => el.addEventListener("change", () => {
        const v = el.value;
        jailRows[i][el.dataset.f] = el.dataset.f === "subPath" ? v.trim().replace(/^\/+|\/+$/g, "") : v.trim();
        if (el.dataset.f === "rootId" && pickIdx === i) { pickIdx = -1; renderJails(); }
      }));
      row.querySelector("[data-delj]").onclick = () => {
        if (pickIdx === i) pickIdx = -1; else if (pickIdx > i) pickIdx--;
        jailRows.splice(i, 1); renderJails();
      };
      row.querySelector("[data-pick]").onclick = () => {
        pickIdx = pickIdx === i ? -1 : i;
        if (pickIdx >= 0) loadPick();
        else renderJails();
      };
    });
    wrap.querySelectorAll("[data-nav]").forEach((b) => b.onclick = () => { pickPath = b.dataset.nav; loadPick(); });
    const useBtn = wrap.querySelector("[data-use]");
    if (useBtn) useBtn.onclick = () => {
      jailRows[pickIdx].subPath = pickPath; pickIdx = -1; renderJails();
    };
  }

  async function loadPick() {
    const r = jailRows[pickIdx];
    if (!r || !r.rootId) { pickList = []; pickErr = "请先选择根目录"; renderJails(); return; }
    try {
      pickList = (await api(`${API_BASE}/admin/fs?rootId=${encodeURIComponent(r.rootId)}&path=${encodeURIComponent(pickPath)}`)) || [];
      pickErr = "";
    } catch (e) { pickList = []; pickErr = "无法读取目录"; }
    renderJails();
  }

  const addBtn0 = $("#um-addjail");
  if (addBtn0) addBtn0.onclick = () => {
    jailRows.push({ rootId: roots.length ? String(roots[0].id) : "", subPath: "", alias: "" });
    renderJails();
  };
  renderJails();

  async function save() {
    const password = $("#um-password").value;
    const role = $("#um-role").value;
    const jailBody = { roots: jailRows.filter((r) => r.rootId).map((r) => ({ rootId: r.rootId, subPath: r.subPath, alias: r.alias })) };
    try {
      let id;
      if (editing) {
        const body = { role };                       // password 留空 = 不修改
        if (password) body.password = password;
        await api(`${API_BASE}/admin/users/${encodeURIComponent(user.id)}`, { method: "PUT", body });
        id = user.id;
      } else {
        const username = $("#um-username").value.trim();
        if (!username || !password) { toast("请输入用户名和密码", false); return; }
        const created = await api(API_BASE + "/admin/users", { method: "POST", body: { username, password, role } });
        id = created && created.id;
      }
      if (id) await api(`${API_BASE}/admin/users/${encodeURIComponent(id)}/roots`, { method: "PUT", body: jailBody });
      toast(editing ? "用户已保存" : "用户已创建");
      closeModal();
      loadUsers();
    } catch (e) { /* 已弹提示，模态框保持打开 */ }
  }
}

/* ---------- 十、用户组 tab ---------- */
async function loadGroups() {
  try {
    // 成员列与编辑框都需要用户列表，并行加载
    const [groups, users] = await Promise.all([
      api(API_BASE + "/admin/groups"), api(API_BASE + "/admin/users")]);
    groupsCache = groups || [];
    usersCache = users || [];
    renderGroups();
  } catch (e) { /* 已弹提示 */ }
}

// 成员 id 列表 → 用户名徽章
function membersHTML(memberIds) {
  if (!memberIds || !memberIds.length) return '<span class="muted">（无成员）</span>';
  return memberIds.map((id) => {
    const u = usersCache.find((x) => String(x.id) === String(id));
    return `<span class="badge blue">${esc(u ? u.username : "ID:" + id)}</span>`;
  }).join(" ");
}

function renderGroups() {
  const tbody = $("#groups-tbody");
  if (!groupsCache.length) { tbody.innerHTML = emptyRow(5); tbody.onclick = null; return; }
  tbody.innerHTML = groupsCache.map((g) => `<tr>
    <td class="mono">${esc(g.id)}</td>
    <td>${esc(g.name)}</td>
    <td>${membersHTML(g.members)}</td>
    <td class="muted">${fmtTime(g.createdAt)}</td>
    <td class="actions">
      <button type="button" class="btn sm ghost" data-act="edit" data-id="${esc(g.id)}">编辑</button>
      <button type="button" class="btn sm danger" data-act="del" data-id="${esc(g.id)}">删除</button>
    </td></tr>`).join("");
  tbody.onclick = async (ev) => {
    const btn = ev.target.closest("button[data-act]");
    if (!btn) return;
    const g = groupsCache.find((x) => String(x.id) === btn.dataset.id);
    if (!g) return;
    if (btn.dataset.act === "edit") { openGroupModal(g); return; }
    if (!confirm(`确定删除用户组「${g.name}」吗？`)) return;
    try {
      await api(`${API_BASE}/admin/groups/${encodeURIComponent(g.id)}`, { method: "DELETE" });
      toast("用户组已删除");
      loadGroups();
    } catch (e) { /* 已弹提示 */ }
  };
}

// 新建（group 为 null）/ 编辑用户组模态框（成员 = 用户多选 checkbox）
function openGroupModal(group) {
  const editing = !!group;
  const members = ((group && group.members) || []).map(String);
  openModal(editing ? "编辑用户组" : "新建用户组", `
    <div class="form-item"><label>组名</label>
      <input id="gm-name" type="text" value="${esc(group ? group.name : "")}" placeholder="用户组名称"></div>
    <div class="form-item"><label>成员 <span class="muted">（可多选）</span></label>
      <div class="check-list" id="gm-members">
        ${usersCache.length ? usersCache.map((u) => `<label class="check-item">
            <input type="checkbox" value="${esc(u.id)}" ${members.includes(String(u.id)) ? "checked" : ""}>
            ${esc(u.username)} ${u.role === "admin" ? '<span class="badge purple">管理员</span>' : ""}</label>`).join("")
          : '<span class="muted">暂无用户可选</span>'}
      </div></div>`,
    [{ text: "取消", onClick: closeModal }, { text: "保存", primary: true, onClick: save }]);

  async function save() {
    const name = $("#gm-name").value.trim();
    if (!name) { toast("请输入组名", false); return; }
    // 勾选的成员 → 还原为原始用户 id
    const memberList = $$("#gm-members input:checked").map((chk) => {
      const u = usersCache.find((x) => String(x.id) === chk.value);
      return u ? u.id : Number(chk.value);
    });
    try {
      if (editing) {
        await api(`${API_BASE}/admin/groups/${encodeURIComponent(group.id)}`,
          { method: "PUT", body: { name, members: memberList } });
        toast("用户组已保存");
      } else {
        await api(API_BASE + "/admin/groups", { method: "POST", body: { name, members: memberList } });
        toast("用户组已创建");
      }
      closeModal();
      loadGroups();
    } catch (e) { /* 已弹提示 */ }
  }
}

/* ---------- 十一、文件夹权限 tab（ACL） ---------- */
async function loadAcls() {
  try {
    // 主体下拉/名称展示需要 info + users + groups，并行加载
    const [acls, , users, groups] = await Promise.all([
      api(API_BASE + "/admin/acls"), ensureInfo(),
      api(API_BASE + "/admin/users"), api(API_BASE + "/admin/groups")]);
    usersCache = users || [];
    groupsCache = groups || [];
    renderAcls(acls || []);
  } catch (e) { /* 已弹提示 */ }
}

// ACL 主体 → 中文标签
function subjectLabel(type, id) {
  if (type === "public") return "🌐 公开（匿名访客）";
  if (type === "user") {
    const u = usersCache.find((x) => String(x.id) === String(id));
    return "👤 用户：" + (u ? u.username : "ID:" + id);
  }
  if (type === "group") {
    const g = groupsCache.find((x) => String(x.id) === String(id));
    return "👥 用户组：" + (g ? g.name : "ID:" + id);
  }
  return `${type}:${id}`;
}

// 权限集合 → 徽章（只展示已授权的项）
function permBadges(perms) {
  const p = perms || {};
  const on = PERMS.filter(([k]) => p[k]);
  if (!on.length) return '<span class="badge gray">无权限</span>';
  return on.map(([, label]) => `<span class="badge green">${label}</span>`).join(" ");
}

// 按 rootId + path 分组渲染全部 ACL 条目，每组一个"编辑"按钮
function renderAcls(acls) {
  const byPath = new Map();
  acls.forEach((a) => {
    const key = `${a.rootId}|${a.path || ""}`;
    if (!byPath.has(key)) byPath.set(key, { rootId: a.rootId, path: a.path || "", entries: [] });
    byPath.get(key).entries.push(a);
  });
  const groups = Array.from(byPath.values());
  const wrap = $("#acl-groups");
  if (!groups.length) {
    wrap.innerHTML = '<div class="card muted" style="text-align:center">暂无数据</div>';
    return;
  }
  wrap.innerHTML = groups.map((g, gi) => `<div class="card acl-group">
    <div class="acl-group-head">
      <div><span class="badge purple">📂 ${esc(rootName(g.rootId))}</span>
        <span class="acl-path mono">/${esc(g.path || "")}</span></div>
      <button type="button" class="btn sm primary" data-gi="${gi}">编辑</button>
    </div>
    ${g.entries.map((e) => `<div class="acl-entry">
      <span class="acl-subject">${esc(subjectLabel(e.subjectType, e.subjectId))}</span>
      <span>${permBadges(e.perms)}</span></div>`).join("")}
  </div>`).join("");
  wrap.onclick = (ev) => {
    const btn = ev.target.closest("button[data-gi]");
    if (!btn) return;
    const g = groups[Number(btn.dataset.gi)];
    if (g) openAclEditor(g.rootId, g.path, g.entries);
  };
}

// "新增路径规则"：先选 root + 输入相对路径（空 = 根目录），再进入同一个编辑器
function openNewAclPath() {
  const roots = (serverInfo && serverInfo.roots) || [];
  if (!roots.length) { toast("暂无可用根目录，无法新增规则", false); return; }
  openModal("新增路径规则", `
    <div class="form-item"><label>根目录</label>
      <select id="ap-root">${roots.map((r) => `<option value="${esc(r.id)}">${esc(r.name || r.id)}</option>`).join("")}</select></div>
    <div class="form-item"><label>路径 <span class="muted">（相对根目录的路径，留空 = 根目录）</span></label>
      <input id="ap-path" type="text" placeholder="例如：docs/reports"></div>`,
    [{ text: "取消", onClick: closeModal },
     { text: "下一步：配置权限", primary: true, onClick: () => {
        const root = roots.find((r) => String(r.id) === $("#ap-root").value) || roots[0];
        const path = $("#ap-path").value.trim().replace(/^\/+|\/+$/g, "");
        openAclEditor(root.id, path, []); // 新路径从空条目开始
      } }]);
}

// 打开 ACL 编辑器（保存时整体替换该路径条目；空数组 = 清除该路径全部 ACL）
function openAclEditor(rootId, path, entries) {
  aclEditor = {
    rootId, path,
    entries: (entries || []).map((e) => ({
      subjectType: e.subjectType || "public",
      subjectId: e.subjectId ?? 0, // 服务端约定 public 的 subjectId 为 0
      perms: Object.fromEntries(PERMS.map(([k]) => [k, !!(e.perms && e.perms[k])])),
    })),
  };
  renderAclEditor();
}

// 主体下拉 <option>：public / 用户… / 用户组…
function subjectOptionsHTML(entry) {
  const curKey = entry.subjectType === "public" ? "public" : `${entry.subjectType}:${entry.subjectId}`;
  const opt = (key, label) => `<option value="${esc(key)}" ${curKey === key ? "selected" : ""}>${esc(label)}</option>`;
  return opt("public", "🌐 公开（匿名访客）")
    + `<optgroup label="用户">${usersCache.map((u) => opt("user:" + u.id, u.username)).join("")}</optgroup>`
    + `<optgroup label="用户组">${groupsCache.map((g) => opt("group:" + g.id, g.name)).join("")}</optgroup>`;
}

function renderAclEditor() {
  const { rootId, path, entries } = aclEditor;
  const rowsHTML = entries.map((e, i) => `<div class="acl-edit-row" data-i="${i}">
    <select class="acl-subject-sel">${subjectOptionsHTML(e)}</select>
    <span class="acl-perms">${PERMS.map(([k, label]) => `<label class="perm-check">
      <input type="checkbox" data-perm="${k}" ${e.perms[k] ? "checked" : ""}>${label}</label>`).join("")}</span>
    <button type="button" class="btn sm danger" data-del="${i}" title="删除此条目">✕</button></div>`).join("");
  openModal(`编辑权限 —— ${rootName(rootId)} : /${path || ""}`, `
    <div class="acl-edit-head"><span class="muted">主体</span>
      <span class="muted">权限（查看 / 下载 / 上传 / 删除 / 分享）</span></div>
    <div id="acl-rows">${rowsHTML ||
      '<div class="muted" style="padding:10px 2px">暂无条目。保存空条目列表 = 清除该路径的全部 ACL。</div>'}</div>
    <button type="button" id="acl-add" class="btn ghost sm">＋ 添加条目</button>`,
    [{ text: "取消", onClick: closeModal }, { text: "保存", primary: true, onClick: saveAcl }]);
  bindAclEditorEvents();
}

// 绑定编辑器内事件（每次重渲染后重新绑定）
function bindAclEditorEvents() {
  const body = $("#modal-body");
  body.querySelectorAll(".acl-subject-sel").forEach((sel) => {
    sel.addEventListener("change", () => {
      const i = Number(sel.closest(".acl-edit-row").dataset.i);
      const v = sel.value;
      if (v === "public") {
        aclEditor.entries[i].subjectType = "public";
        aclEditor.entries[i].subjectId = 0;
      } else {
        const idx = v.indexOf(":");
        const type = v.slice(0, idx); // user | group
        const idStr = v.slice(idx + 1);
        const list = type === "user" ? usersCache : groupsCache;
        const found = list.find((x) => String(x.id) === idStr);
        aclEditor.entries[i].subjectType = type;
        aclEditor.entries[i].subjectId = found ? found.id : Number(idStr);
      }
    });
  });
  // 权限复选框：直接改状态，无需重渲染
  body.querySelectorAll("input[data-perm]").forEach((chk) => {
    chk.addEventListener("change", () => {
      const i = Number(chk.closest(".acl-edit-row").dataset.i);
      aclEditor.entries[i].perms[chk.dataset.perm] = chk.checked;
    });
  });
  // 删除行 / 添加条目（默认：public + 仅查看）
  body.querySelectorAll("button[data-del]").forEach((btn) => {
    btn.addEventListener("click", () => {
      aclEditor.entries.splice(Number(btn.dataset.del), 1);
      renderAclEditor();
    });
  });
  const addBtn = $("#acl-add");
  if (addBtn) addBtn.addEventListener("click", () => {
    const perms = Object.fromEntries(PERMS.map(([k]) => [k, false]));
    perms.view = true;
    aclEditor.entries.push({ subjectType: "public", subjectId: 0, perms });
    renderAclEditor();
  });
}

// 保存：PUT /api/v1/acl，整体替换该路径的条目
async function saveAcl() {
  try {
    await api(API_BASE + "/acl", { method: "PUT", body: {
      rootId: aclEditor.rootId, path: aclEditor.path,
      entries: aclEditor.entries.map((e) => ({ subjectType: e.subjectType, subjectId: e.subjectId, perms: e.perms })),
    } });
    toast("权限已保存");
    closeModal();
    loadAcls();
  } catch (e) { /* 已弹提示 */ }
}

/* ---------- 十二、分享链接 tab ---------- */
async function loadLinks() {
  try {
    const [links] = await Promise.all([api(API_BASE + "/admin/links"), ensureInfo()]);
    renderLinks(links || []);
  } catch (e) { /* 已弹提示 */ }
}

// 有效期列：null = 永久；已过期显示红色"已过期"
function fmtExpires(expiresAt) {
  if (!expiresAt) return '<span class="badge gray">永久</span>';
  const d = new Date(expiresAt);
  if (isNaN(d.getTime())) return "-";
  if (d.getTime() < Date.now())
    return `<span class="expired-text">已过期</span> <span class="muted small">${fmtTime(expiresAt)}</span>`;
  return fmtTime(expiresAt);
}

function renderLinks(links) {
  const tbody = $("#links-tbody");
  if (!links.length) { tbody.innerHTML = emptyRow(6); tbody.onclick = null; return; }
  tbody.innerHTML = links.map((l) => {
    const fullUrl = `${location.origin}/s/${l.token}`; // 完整 URL 用 location.origin 拼接
    return `<tr>
      <td><span class="link-cell"><code class="mono">/s/${esc(l.token)}</code>
        <button type="button" class="btn sm ghost" data-act="copy" data-url="${esc(fullUrl)}">复制</button></span></td>
      <td><span class="mono">${esc(l.rootId)}:${esc(l.path || "/")}</span>
        ${l.isDir ? '<span class="badge blue">目录</span>' : ""}</td>
      <td>${l.hasPassword ? '<span class="badge purple">有</span>' : '<span class="badge gray">无</span>'}</td>
      <td>${fmtExpires(l.expiresAt)}</td>
      <td>${l.disabled ? '<span class="badge red">禁用</span>' : '<span class="badge green">启用</span>'}</td>
      <td class="actions">
        <button type="button" class="btn sm ghost" data-act="edit" data-token="${esc(l.token)}">编辑</button>
        <button type="button" class="btn sm danger" data-act="del" data-token="${esc(l.token)}">删除</button>
      </td></tr>`;
  }).join("");
  tbody.onclick = async (ev) => {
    const btn = ev.target.closest("button[data-act]");
    if (!btn) return;
    if (btn.dataset.act === "copy") { copyText(btn.dataset.url); return; }
    const link = links.find((x) => x.token === btn.dataset.token);
    if (!link) return;
    if (btn.dataset.act === "edit") { openLinkEditor(link); return; }
    if (!confirm(`确定删除分享链接 /s/${link.token} 吗？删除后立即失效。`)) return;
    try {
      await api(`${API_BASE}/share/links/${encodeURIComponent(link.token)}`, { method: "DELETE" });
      toast("链接已删除");
      loadLinks();
    } catch (e) { /* 已弹提示 */ }
  };
}

// 编辑链接：新密码留空不改 / 移除密码 / 有效期 datetime-local 或永久 / 禁用开关
function openLinkEditor(link) {
  const forever = !link.expiresAt;
  openModal("编辑分享链接", `
    <div class="form-item"><label>链接</label>
      <code class="mono" style="color:var(--accent-2)">/s/${esc(link.token)}</code>
      <span class="muted small">（目标 ${esc(link.rootId)}:${esc(link.path || "/")}）</span></div>
    <div class="form-item"><label>新密码 <span class="muted">（留空表示不修改）</span></label>
      <input id="le-password" type="text" placeholder="输入新密码" autocomplete="off"></div>
    <div class="form-item"><label class="check-item">
      <input id="le-nopass" type="checkbox" ${link.hasPassword ? "" : "disabled"}> 移除密码（改为免密链接）</label></div>
    <div class="form-item"><label class="check-item">
      <input id="le-forever" type="checkbox" ${forever ? "checked" : ""}> 永久有效</label>
      <input id="le-expires" type="datetime-local" value="${toLocalInput(link.expiresAt)}"
        ${forever ? "disabled" : ""} style="margin-top:8px"></div>
    <div class="form-item"><label class="check-item">
      <input id="le-disabled" type="checkbox" ${link.disabled ? "checked" : ""}> 禁用此链接</label></div>`,
    [{ text: "取消", onClick: closeModal }, { text: "保存", primary: true, onClick: save }]);
  // 联动：勾选"永久"禁用时间输入；勾选"移除密码"清空并禁用密码输入
  $("#le-forever").addEventListener("change", (e) => { $("#le-expires").disabled = e.target.checked; });
  $("#le-nopass").addEventListener("change", (e) => {
    $("#le-password").disabled = e.target.checked;
    if (e.target.checked) $("#le-password").value = "";
  });

  async function save() {
    const body = {};
    if ($("#le-nopass").checked) body.noPassword = true;
    else if ($("#le-password").value) body.password = $("#le-password").value;
    if ($("#le-forever").checked) body.expiresAt = null; // null = 永久
    else {
      const v = $("#le-expires").value;
      if (v) body.expiresAt = new Date(v).toISOString(); // RFC3339
    }
    body.disabled = $("#le-disabled").checked;
    try {
      await api(`${API_BASE}/share/links/${encodeURIComponent(link.token)}`, { method: "PUT", body });
      toast("链接已更新");
      closeModal();
      loadLinks();
    } catch (e) { /* 已弹提示 */ }
  }
}

// 新建链接：root + 路径 + 有效期 + 密码（默认自动生成）
function openNewLink() {
  const roots = (serverInfo && serverInfo.roots) || [];
  if (!roots.length) { toast("暂无可用根目录，无法创建链接", false); return; }
  openModal("新建分享链接", `
    <div class="form-item"><label>根目录</label>
      <select id="nl-root">${roots.map((r) => `<option value="${esc(r.id)}">${esc(r.name || r.id)}</option>`).join("")}</select></div>
    <div class="form-item"><label>路径 <span class="muted">（相对根目录，留空 = 根目录；可为文件或目录）</span></label>
      <input id="nl-path" type="text" placeholder="例如：docs/report.pdf"></div>
    <div class="form-item"><label class="check-item">
      <input id="nl-forever" type="checkbox" checked> 永久有效</label>
      <input id="nl-expires" type="datetime-local" disabled style="margin-top:8px"></div>
    <div class="form-item"><label class="check-item">
      <input id="nl-usepwd" type="checkbox" checked> 设置访问密码</label>
      <div class="sub-options">
        <label class="check-item"><input id="nl-auto" type="checkbox" checked> 自动生成密码</label>
        <input id="nl-pwd" type="text" placeholder="自定义密码" autocomplete="off" disabled>
      </div></div>`,
    [{ text: "取消", onClick: closeModal }, { text: "创建", primary: true, onClick: submit }]);
  // 联动：有效期与密码选项的启用状态
  $("#nl-forever").addEventListener("change", (e) => { $("#nl-expires").disabled = e.target.checked; });
  $("#nl-usepwd").addEventListener("change", (e) => {
    $("#nl-auto").disabled = !e.target.checked;
    $("#nl-pwd").disabled = !e.target.checked || $("#nl-auto").checked;
  });
  $("#nl-auto").addEventListener("change", (e) => {
    $("#nl-pwd").disabled = $("#nl-usepwd").checked && e.target.checked;
    if (e.target.checked) $("#nl-pwd").value = "";
  });

  async function submit() {
    const root = roots.find((r) => String(r.id) === $("#nl-root").value) || roots[0];
    const body = { rootId: root.id, path: $("#nl-path").value.trim().replace(/^\/+|\/+$/g, "") };
    if (!$("#nl-forever").checked) {
      const v = $("#nl-expires").value;
      if (!v) { toast("请选择有效期时间，或勾选“永久有效”", false); return; }
      body.expiresAt = new Date(v).toISOString(); // RFC3339
    }
    if ($("#nl-usepwd").checked) {
      if ($("#nl-auto").checked) body.autoPassword = true;
      else {
        const p = $("#nl-pwd").value;
        if (!p) { toast("请输入自定义密码，或勾选“自动生成密码”", false); return; }
        body.password = p;
      }
    }
    try {
      const res = await api(API_BASE + "/share/links", { method: "POST", body });
      closeModal();
      showLinkCreated(res || {});
      loadLinks();
    } catch (e) { /* 已弹提示 */ }
  }
}

// 创建成功：醒目模态框展示完整 URL + 明文密码（仅此一次）
function showLinkCreated(res) {
  const fullUrl = res.url && /^https?:\/\//i.test(res.url) ? res.url : `${location.origin}/s/${res.token || ""}`;
  openModal("✅ 链接创建成功", `
    <div class="created-box">
      ${res.password ? '<p class="warn-text">⚠️ 密码明文仅此一次展示，关闭后无法再次查看，请立即保存！</p>' : ""}
      <div class="created-row"><span class="created-label">完整链接</span>
        <code id="created-url" class="mono">${esc(fullUrl)}</code>
        <button type="button" id="copy-url" class="btn sm ghost">复制</button></div>
      ${res.password
        ? `<div class="created-row"><span class="created-label">访问密码</span>
            <code id="created-pwd" class="mono big-pwd">${esc(res.password)}</code>
            <button type="button" id="copy-pwd" class="btn sm ghost">复制</button></div>`
        : '<div class="created-row"><span class="created-label">访问密码</span><span class="muted">未设置（免密链接）</span></div>'}
      <div class="created-row"><span class="created-label">有效期</span>
        <span>${res.expiresAt ? esc(fmtTime(res.expiresAt)) : "永久"}</span></div>
    </div>`,
    [{ text: "我已保存，完成", primary: true, onClick: closeModal }]);
  $("#copy-url").addEventListener("click", () => copyText(fullUrl));
  const pwdBtn = $("#copy-pwd");
  if (pwdBtn) pwdBtn.addEventListener("click", () => copyText(res.password));
}

/* ---------- 十三之二、审计日志 tab（0.5.1） ---------- */
const AUDIT_ACTIONS = {
  login: ["登录成功", "green"], login_fail: ["登录失败", "red"], register: ["引导注册", "purple"],
  user_create: ["创建用户", "blue"], user_update: ["修改用户", "blue"], user_delete: ["删除用户", "red"],
  roots_assign: ["分配目录", "purple"], acl_set: ["设置权限", "purple"], acl_delete: ["删除权限条目", "gray"],
  share_create: ["创建分享", "blue"], share_delete: ["删除分享", "gray"],
  file_delete: ["删除入回收站", "red"], rollback: ["版本回滚", "purple"],
  db_switch: ["切换数据库", "purple"], db_disconnect: ["断开数据库", "red"], db_unlock_fail: ["配库解锁失败", "red"],
  db_export: ["导出到文件存储", "blue"], db_pause: ["暂停客户端", "orange"], db_resume: ["恢复客户端", "green"],
  super_update: ["超管账户变更", "purple"],
};

async function loadAudits() {
  try {
    const events = await api(API_BASE + "/admin/audits?limit=300") || [];
    renderAudits(events);
  } catch (e) { /* 已弹提示 */ }
}

function renderAudits(events) {
  const tbody = $("#audits-tbody");
  if (!events.length) { tbody.innerHTML = emptyRow(6); return; }
  tbody.innerHTML = events.map((ev) => {
    const [label, color] = AUDIT_ACTIONS[ev.action] || [ev.action, "gray"];
    const who = ev.username ? `${esc(ev.username)}${ev.userId ? " <span class='muted small'>#" + esc(ev.userId) + "</span>" : ""}` : '<span class="muted">匿名</span>';
    const target = ev.rootId ? `<span class="mono small">${esc(ev.rootId)}${ev.path ? ":" + esc(ev.path) : ""}</span>` : '<span class="muted">-</span>';
    return `<tr>
      <td class="muted small">${fmtTime(ev.createdAt)}</td>
      <td>${who}</td>
      <td><span class="badge ${color}">${esc(label)}</span></td>
      <td>${target}</td>
      <td class="small">${esc(ev.detail || "-")}</td>
      <td class="mono small">${esc(ev.ip || "-")}</td></tr>`;
  }).join("");
}

/* ---------- 十三之三、数据库 tab（0.7.0 管理台配库/热切换） ---------- */
let dbDrivers = null;        // GET /admin/db/drivers 缓存（静态）
let dbStatus = null;         // GET /admin/db/status 最近一次结果
let dbStatusDegraded = false; // 最近一次探测的降级状态（refreshTabAccess 同步）

// dbApi：数据库配置面专用封装（0.1.0 需求33：统一 Bearer 登录态——超管或 DB
// 管理员；0.7.0 的 X-DB-Unlock 解锁头随 firstBoot 流程移除）。与 api() 的区别：
// 401 仅在已有 token 时登出（status 探测在未登录状态也会调用）；409 交给调用方处理。
async function dbApi(path, { method = "GET", body } = {}) {
  const headers = {};
  if (token) headers["Authorization"] = "Bearer " + token;
  if (body !== undefined) headers["Content-Type"] = "application/json";
  let resp;
  try {
    resp = await fetch(API_BASE + "/admin/db" + path, {
      method, headers, body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch (e) { toast("网络请求失败：" + e.message, false); throw e; }
  let data = null;
  const text = await resp.text();
  if (text) { try { data = JSON.parse(text); } catch (e) { /* 忽略 */ } }
  if (!resp.ok) {
    const msg = (data && (data.error || data.message)) || `请求失败（HTTP ${resp.status}）`;
    if (resp.status === 401 && token) forceLogout();
    else toast(msg, false);
    const err = new Error(msg); err.status = resp.status; err.data = data;
    throw err;
  }
  return data || {};
}

async function loadDbTab() {
  try {
    dbStatus = await dbApi("/status");
    if (!dbDrivers) dbDrivers = (await dbApi("/drivers")).drivers || [];
    renderDb();
  } catch (e) { /* 已弹提示 */ }
}

function updateDbDsnHint() {
  const d = (dbDrivers || []).find((x) => x.id === $("#db-driver").value);
  const isFile = !!d && d.id === "file";
  $("#db-dsn").placeholder = isFile ? "留空 = <dataDir>/filestore" : (d ? d.dsnHint || "" : "请先选择驱动类型");
  $("#db-dsn").disabled = false; // 0.1.1 file 驱动可留空但不再禁用输入
  $("#db-dsn-hint").textContent = d ? (d.dsnHint || "") : "";
}

function renderDb() {
  const st = dbStatus || {};
  const banners = [];
  if (st.degraded) {
    banners.push('<div class="card" style="border-color:#e6a23c">ℹ️ 未配置数据库：服务端处于降级模式（仅匿名只读浏览）。以<b>超级管理员</b>账户登录后即可在此选择并连接数据库（首启口令见服务端启动日志或账户文件）。</div>');
  }
  if (!st.access) {
    banners.push('<div class="card muted">以超级管理员或数据库管理员登录后查看数据库详情与切换操作。</div>');
  }
  $("#db-banner").innerHTML = banners.join("");

  const cards = [
    statCard("数据库状态", st.configured ? '<span class="badge green">已连接</span>' : '<span class="badge yellow">降级模式（无库）</span>'),
    statCard("客户端操作", st.paused ? '<span class="badge red">维护暂停中</span>' : '<span class="badge green">正常</span>'),
    statCard("数据目录 dataDir", `<span class="mono small">${esc(st.dataDir || "-")}</span>`),
    statCard("用户主目录", `<span class="mono small">${esc(st.usersDir || "-")}</span>`),
    statCard("配置 Schema", esc(st.schemaVersion ?? "-")),
    statCard("服务端版本", `v${esc(st.serverVersion || "-")}`),
  ];
  if (st.access && st.configured) {
    cards.push(statCard("驱动", esc(st.driver || "-")));
    cards.push(statCard(st.driver === "file" ? "存储目录（脱敏）" : "DSN（脱敏）", `<span class="mono small">${esc(st.dsn || "-")}</span>`));
    cards.push(statCard("连通性", st.connected ? '<span class="badge green">正常</span>' : '<span class="badge red">失败</span>'));
  }
  $("#db-status").innerHTML = cards.join("");
  const pauseBtn = $("#db-pause-btn");
  if (pauseBtn) pauseBtn.textContent = st.paused ? "恢复客户端操作" : "暂停客户端操作";

  $("#db-form-card").classList.toggle("hidden", !st.access);
  if (st.access) {
    const sel = $("#db-driver");
    if (sel.options.length === 0) {
      sel.innerHTML = dbDrivers.map((d) => `<option value="${esc(d.id)}">${esc(d.name)}</option>`).join("");
    }
    if (st.configured && !sel.value) sel.value = st.driver || "sqlite3";
    updateDbDsnHint();
    const mig = $("#db-migrate-chk");
    mig.disabled = !st.configured;
    if (!st.configured) mig.checked = false;
    if (!st.configured) $("#db-force-row").classList.add("hidden");
  }
  if (st.access && st.configured && st.tables) {
    $("#db-tables").classList.remove("hidden");
    $("#db-tables-body").innerHTML = Object.entries(st.tables)
      .map(([k, v]) => `${esc(k)}: <b>${esc(v)}</b>`).join("　｜　");
  } else {
    $("#db-tables").classList.add("hidden");
  }
}

async function dbTest() {
  const driver = $("#db-driver").value;
  const dsn = $("#db-dsn").value.trim();
  if (driver === "file") { toast("文件存储无需测连（切换时自动创建/校验目录）", false); return; }
  if (!dsn) { toast("请输入 DSN / 文件路径", false); return; }
  $("#db-test-btn").disabled = true;
  try {
    await dbApi("/test", { method: "POST", body: { driver, dsn } });
    toast("连接成功 ✓");
  } catch (e) { /* 已弹提示 */ }
  finally { $("#db-test-btn").disabled = false; }
}

async function dbConnect() {
  const driver = $("#db-driver").value;
  const dsn = $("#db-dsn").value.trim();
  const migrate = $("#db-migrate-chk").checked && !$("#db-migrate-chk").disabled;
  const force = $("#db-force-chk").checked;
  if (driver !== "file" && !dsn) { toast("请输入 DSN / 文件路径", false); return; }
  if (driver === "file" && !migrate && !confirm("切换到文件存储将加载该目录既有数据（不勾选迁移则不复制当前库数据）。确认继续？")) return;
  if (migrate && !confirm("切换将把全部元数据复制到新存储并立即热生效（复制期间自动暂停客户端操作；旧库不受影响；未迁移时所有登录失效需重登）。确认继续？")) return;
  $("#db-connect-btn").disabled = true;
  try {
    const res = await dbApi("/connect", { method: "POST", body: { driver, dsn, migrate, force } });
    if (res.warning) toast(res.warning, false);
    else if (res.migrated) toast(`已切换并迁移 ${Object.values(res.migrated).reduce((a, b) => a + b, 0)} 行元数据`);
    else if (res.degraded) toast("存储不可用（异常路径），依赖元数据的端点暂时 503");
    else toast("存储已连接/切换 ✓");
    serverInfo = null;
    $("#db-force-row").classList.add("hidden");
    $("#db-force-chk").checked = false;
    // 会话校验（0.1.0 需求33）：超管令牌走账户文件纪元、不随库失效；
    // 数据库管理员的会话存在旧库——未迁移时其登录态作废。
    if (token && currentUser && !currentUser.super && (res.degraded || !res.migrated)) forceLogout();
    await loadDbTab();
    await refreshTabAccess(); // 配库后页签可见性联动
    if (!token) await updateLoginView();
  } catch (e) {
    if (e.status === 409) {
      $("#db-force-row").classList.remove("hidden");
      toast("目标存储非空——如确需迁入请勾选“强制”后重试", false);
    }
  } finally { $("#db-connect-btn").disabled = false; }
}

// dbExport 0.1.1 纯导出：当前库全表复制到文件存储目录，不切换引擎。
async function dbExport() {
  const dsn = $("#db-dsn").value.trim(); // 复用表单目录字段（留空=<dataDir>/filestore）
  const force = $("#db-export-force-chk").checked;
  if (!confirm(`将当前存储全部元数据导出到文件存储（${dsn || "<dataDir>/filestore"}），导出期间自动暂停客户端操作。确认继续？`)) return;
  $("#db-export-btn").disabled = true;
  try {
    const res = await dbApi("/export", { method: "POST", body: { dsn, force } });
    toast(`导出完成：${Object.values(res.exported).reduce((a, b) => a + b, 0)} 行 → ${res.dir}（引擎未切换）`);
  } catch (e) {
    if (e.status === 409) {
      $("#db-export-force-chk").checked = true;
      toast("目标文件存储非空——已勾选“清空覆盖”，确认后重试", false);
    }
  } finally { $("#db-export-btn").disabled = false; }
}

// dbPauseToggle 0.1.1 手动维护屏障开关（可选中断按钮）。
async function dbPauseToggle() {
  const next = !(dbStatus && dbStatus.paused);
  $("#db-pause-btn").disabled = true;
  try {
    const res = await dbApi("/pause", { method: "POST", body: { paused: next } });
    toast(next ? "已暂停客户端一切操作（管理台配置面仍可用）" : "客户端操作已恢复");
    dbStatus = Object.assign({}, dbStatus, { paused: res.paused });
    renderDb();
  } catch (e) { /* 已弹提示 */ }
  finally { $("#db-pause-btn").disabled = false; }
}

// 登录页探测（0.1.0）：降级模式不再另设配库入口——用超管账户登录后在"数据库"页配库。
async function updateLoginView() {
  const hint = $("#login-hint");
  try {
    const st = await dbApi("/status");
    if (st.degraded) {
      hint.textContent = "数据库未配置（降级模式）。请使用超级管理员账户"
        + (st.superAdmin ? "「" + st.superAdmin + "」" : "")
        + "登录（首次启动口令见服务端日志），登录后在“数据库”页完成配置。";
      hint.classList.remove("hidden");
    } else {
      hint.classList.add("hidden");
    }
  } catch (e) {
    hint.classList.add("hidden");
  }
}

/* ---------- 十三之四、超级管理员 tab（0.1.0 需求32/34） ---------- */
let superInfo = null; // GET /admin/super 最近一次结果

async function loadSuperTab() {
  try {
    superInfo = await api(API_BASE + "/admin/super");
    renderSuper();
  } catch (e) { /* 非超管 403：隐藏入口并回到概览（refreshTabAccess 已按 super 标记隐藏页签） */ }
}

function renderSuper() {
  const st = superInfo || {};
  $("#super-username").value = st.username || "";
  $("#super-pw-state").innerHTML = st.plaintext
    ? `<span class="badge yellow">明文（未使用）</span> <span class="muted">当前口令：<code class="mono">${esc(st.password)}</code> —— 首次成功登录后自动转密文</span>`
    : '<span class="badge green">密文（bcrypt）</span> <span class="muted">已使用，无法回显；如需重置请编辑 admin-account.json 或配置 adminPassword 后重启</span>';
  $("#super-status").innerHTML = [
    statCard("账户文件", `<span class="mono small">${esc((st.username || "admin"))} @ &lt;dataDir&gt;/admin-account.json</span>`),
    statCard("凭据纪元", esc(st.generation ?? "-")),
  ].join("");
}

async function saveSuper() {
  const username = $("#super-username").value.trim();
  const password = $("#super-password").value;
  if (!username && !password) { toast("请输入要修改的账户名或新密码", false); return; }
  const body = {};
  if (superInfo && username !== superInfo.username) body.username = username;
  if (password) body.password = password;
  if (!body.username && !body.password) { toast("没有需要保存的修改", false); return; }
  try {
    const res = await api(API_BASE + "/admin/super", { method: "PUT", body });
    if (res.token) { // 纪元已推进：换用服务端重发的新令牌，避免自踢下线
      token = res.token;
      currentUser = { ...(currentUser || {}), username: res.username, super: true };
      localStorage.setItem(TOKEN_KEY, token);
      localStorage.setItem(USER_KEY, JSON.stringify(currentUser));
      $("#current-user").textContent = `${res.username}（超级管理员）`;
    }
    $("#super-password").value = "";
    toast("超级管理员账户已保存" + (res.plaintext ? "（口令为明文，首次登录后转密文）" : ""));
    await loadSuperTab();
  } catch (e) { /* 已弹提示 */ }
}

/* ---------- 十四、初始化 ---------- */
function init() {
  // 恢复登录态：token + 管理员信息都有本地缓存才自动进入
  token = localStorage.getItem(TOKEN_KEY) || "";
  try { currentUser = JSON.parse(localStorage.getItem(USER_KEY) || "null"); }
  catch (e) { currentUser = null; }
  if (token && currentUser && currentUser.role === "admin") enterAdmin();
  else { forceLogout(); updateLoginView(); } // 停在登录页并探测降级引导提示（0.1.0）

  // 登录 / 退出
  $("#login-form").addEventListener("submit", handleLogin);
  $("#logout-btn").addEventListener("click", () => { forceLogout(); updateLoginView(); toast("已退出登录"); });

  // 数据库 tab（0.1.0：解锁/引导注册已移除，登录态统一走超管/DB 管理员）
  $("#db-refresh-btn").addEventListener("click", loadDbTab);
  $("#db-driver").addEventListener("change", updateDbDsnHint);
  $("#db-test-btn").addEventListener("click", dbTest);
  $("#db-connect-btn").addEventListener("click", dbConnect);
  $("#db-export-btn").addEventListener("click", dbExport);   // 0.1.1 纯导出
  $("#db-pause-btn").addEventListener("click", dbPauseToggle); // 0.1.1 维护暂停开关

  // 超级管理员 tab（0.1.0 需求32/34）
  $("#super-save-btn").addEventListener("click", saveSuper);

  // tab 导航
  $$(".nav-item").forEach((btn) => btn.addEventListener("click", () => switchTab(btn.dataset.tab)));

  // 模态框关闭：× 按钮 / 点击遮罩 / Esc 键
  $("#modal-close").addEventListener("click", closeModal);
  $("#modal-mask").addEventListener("click", (e) => { if (e.target === $("#modal-mask")) closeModal(); });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && !$("#modal-mask").classList.contains("hidden")) closeModal();
  });

  // 各 tab 的"新建"按钮（打开前确保依赖数据已加载）
  $("#user-new-btn").addEventListener("click", async () => {
    try { await ensureInfo(); } catch (e) { return; } // roots 复选框需要 info
    openUserModal(null);
  });
  $("#group-new-btn").addEventListener("click", async () => {
    try { usersCache = (await api(API_BASE + "/admin/users")) || []; } catch (e) { return; }
    openGroupModal(null);
  });
  $("#acl-new-btn").addEventListener("click", async () => {
    try { await ensureInfo(); } catch (e) { return; }
    openNewAclPath();
  });
  $("#link-new-btn").addEventListener("click", async () => {
    try { await ensureInfo(); } catch (e) { return; }
    openNewLink();
  });
  $("#audit-refresh-btn").addEventListener("click", loadAudits);
}

document.addEventListener("DOMContentLoaded", init);
