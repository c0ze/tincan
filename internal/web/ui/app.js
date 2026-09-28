// tincan web UI. Plain JS; every URL is built from the <meta> base. The only
// HTML injected is the server-rendered, escaped message body (renderBody).
"use strict";
const BASE = document.querySelector('meta[name="tincan-base"]').content;
const $ = (id) => document.getElementById(id);
const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
};

const state = {
  machines: [],        // [{key, name, online}]
  rooms: {},           // key -> [room]
  threads: {},         // key/rid -> [thread]
  current: null,       // {key, rid, tid}  tid === "activity" for the activity view
  messages: new Map(), // id -> message (current thread)
  meta: null,
  chains: {},
  seq: 0,
  msgGen: 0,           // bumped per refreshMessages call; drops stale in-flight responses
  agents: { presets: [], listeners: [] },
  progress: new Map(), // request id -> {cursor, text}
  sources: {},
  quotas: {},          // key -> [quota entry]
  showArchived: {},    // key/rid -> true when the room lists its archived threads
};

function fmtLeft(iso) {
  if (!iso) return "";
  const s = Math.max(0, (new Date(iso) - Date.now()) / 1000);
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m`;
}

function quotaFor(key, preset) {
  const hits = (state.quotas[key] || []).filter((q) => (q.presets || []).includes(preset));
  return hits.find((q) => q.explicit) || hits[0];
}

function quotaText(q) {
  return q && q.percent != null ? ` ${Math.round(q.percent)}%` : "";
}

async function loadQuotas(m) {
  state.quotas[m.key] = m.online ? await api(m.key, "quotas").catch(() => []) : [];
}

function renderLimits(m) {
  const box = el("div", "limits");
  for (const q of state.quotas[m.key] || []) {
    const row = el("div", "limit " + q.state + (q.percent >= 95 ? " hot" : ""));
    row.title = q.state === "error" ? q.error : q.fetched_at ? "fetched " + new Date(q.fetched_at).toLocaleTimeString() : "";
    const label = el("span", "label", q.label);
    const bar = el("span", "bar");
    const fill = el("span", "fill");
    fill.style.width = Math.min(100, q.percent || 0) + "%";
    bar.append(fill);
    const right = q.state === "error" ? "error" : q.percent == null ? "?" : `${Math.round(q.percent)}% · ${fmtLeft(q.reset_at)}`;
    row.append(label, bar, el("span", "val", right));
    if (q.short_percent != null) row.append(el("span", "short", `short ${Math.round(q.short_percent)}%` + (q.short_reset_at ? ` · ${fmtLeft(q.short_reset_at)}` : "")));
    box.append(row);
  }
  return box;
}

function prefix(key) { return key === "local" ? "api/" : `api/peers/${encodeURIComponent(key)}/`; }

async function api(key, path, opts = {}) {
  const init = { method: opts.method || "GET", headers: {} };
  if (init.method !== "GET") {
    init.headers["X-Tincan-Request"] = "1";
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(opts.body || {});
  }
  const res = await fetch(BASE + prefix(key) + path, init);
  const ct = res.headers.get("Content-Type") || "";
  const data = ct.includes("json") ? await res.json() : await res.text();
  if (!res.ok) throw new Error((data && data.error) || res.statusText);
  return data;
}

function clientId() { return (crypto.randomUUID ? crypto.randomUUID() : String(Date.now()) + Math.random()).replace(/[^a-z0-9-]/gi, ""); }

function parseHash() {
  try {
    const [key, rid, tid] = location.hash.replace(/^#\//, "").split("/").map(decodeURIComponent);
    return key && rid ? { key, rid, tid: tid || "activity" } : null;
  } catch (e) {
    return null; // malformed percent-encoding in location.hash
  }
}

function go(key, rid, tid) {
  location.hash = `#/${encodeURIComponent(key)}/${rid}/${tid}`;
  $("app").classList.remove("drawer");
}

// ---------- sidebar ----------
async function loadMachines() {
  const self = await api("local", "self");
  state.machines = [{ key: "local", name: self.machine, online: true },
    ...self.peers.map((p) => ({ key: p.name, name: p.name, online: p.online }))];
  await Promise.all(state.machines.map((m) => Promise.all([loadRooms(m), loadQuotas(m)])));
  renderSidebar();
}

async function loadRooms(m) {
  if (!m.online) { state.rooms[m.key] = []; return; }
  try {
    state.rooms[m.key] = await api(m.key, "rooms");
    await Promise.all(state.rooms[m.key].filter((r) => !r.missing).map((r) => loadThreads(m.key, r.id)));
  } catch (e) { m.online = false; state.rooms[m.key] = []; }
}

async function loadThreads(key, rid) {
  state.threads[key + "/" + rid] = await api(key, `rooms/${encodeURIComponent(rid)}/threads`);
}

function renderSidebar() {
  const nav = $("machines");
  nav.replaceChildren();
  const showHidden = $("show-hidden").checked;
  const cur = state.current || {};
  for (const m of state.machines) {
    const box = el("div", "machine" + (m.online ? "" : " offline"));
    const name = el("div", "name");
    name.append(el("span", "dot" + (m.online ? " on" : "")), document.createTextNode(m.name + (m.online ? "" : " (offline)")));
    box.append(name, renderLimits(m));
    for (const r of state.rooms[m.key] || []) {
      if (r.hidden && !showHidden) continue;
      box.append(renderRoom(m, r, cur));
    }
    if (m.online) {
      const add = el("div", "item new add-room", "+ add room");
      add.onclick = () => addRoom(m);
      box.append(add);
    }
    nav.append(box);
  }
}

function renderRoom(m, r, cur) {
  const room = el("div", "room" + (r.hidden ? " is-hidden" : "") + (r.missing ? " missing" : ""));
  const rn = el("div", "name" + (cur.key === m.key && cur.rid === r.id ? " on" : ""));
  rn.title = r.path;
  rn.append(el("span", "label", "# " + r.name + (r.missing ? " (missing)" : "")));
  const toggle = el("button", "icon room-toggle", r.hidden ? "unhide" : "hide");
  toggle.title = r.hidden ? "Show this room in the sidebar" : "Hide this room from the sidebar";
  toggle.onclick = (e) => { e.stopPropagation(); setRoomHidden(m, r); };
  rn.append(toggle);
  room.append(rn);
  if (r.missing) return room; // its directory is gone: nothing to open, only Hide
  rn.onclick = () => go(m.key, r.id, "activity");
  const threadKey = m.key + "/" + r.id;
  const threads = state.threads[threadKey] || [];
  const showArch = !!state.showArchived[threadKey];
  for (const t of threads) {
    const archived = t.status === "archived";
    if (archived && !showArch && cur.tid !== t.id) continue;
    const it = el("div", "item" + (archived ? " archived" : "") + (cur.tid === t.id ? " on" : ""), "› " + t.title);
    if (t.running) it.append(el("span", "count", ` ${t.running}●`));
    it.onclick = () => go(m.key, r.id, t.id);
    room.append(it);
  }
  const act = el("div", "item" + (cur.key === m.key && cur.rid === r.id && cur.tid === "activity" ? " on" : ""),
    `› Activity${r.running ? ` (${r.running} running)` : ""}`);
  act.onclick = () => go(m.key, r.id, "activity");
  const add = el("div", "item new", "+ new thread");
  add.onclick = () => newThread(m.key, r.id);
  room.append(act, add);
  const nArchived = threads.filter((t) => t.status === "archived").length;
  if (nArchived || showArch) {
    const arch = el("div", "item new", showArch ? "hide archived" : `show archived (${nArchived})`);
    arch.onclick = () => { state.showArchived[threadKey] = !showArch; renderSidebar(); };
    room.append(arch);
  }
  return room;
}

async function addRoom(m) {
  const path = (prompt(`Add a room on ${m.name}: absolute path of its directory`) || "").trim();
  if (!path) return;
  try {
    await api(m.key, "rooms", { method: "POST", body: { path } });
    await loadRooms(m);
    renderSidebar();
  } catch (e) { alert(e.message); }
}

async function setRoomHidden(m, r) {
  try {
    await api(m.key, `rooms/${encodeURIComponent(r.id)}`, { method: "PATCH", body: { hidden: !r.hidden } });
    await loadRooms(m);
    renderSidebar();
  } catch (e) { alert(e.message); }
}

// ---------- new thread ----------
async function newThread(key, rid) {
  const agents = await api(key, `rooms/${encodeURIComponent(rid)}/agents`);
  const sel = $("nt-primary");
  sel.replaceChildren(...[...agents.presets, ...agents.listeners].map((a) => el("option", "", a)));
  $("nt-title").value = "";
  const dlg = $("new-thread");
  dlg.returnValue = "";
  dlg.showModal();
  dlg.onclose = async () => {
    if (dlg.returnValue !== "ok") return;
    try {
      const t = await api(key, `rooms/${encodeURIComponent(rid)}/threads`, { method: "POST",
        body: { title: $("nt-title").value, primary: sel.value, client_id: clientId() } });
      await loadThreads(key, rid);
      go(key, rid, t.id);
    } catch (e) { alert(e.message); }
  };
}

// ---------- thread view ----------
async function openCurrent() {
  const cur = parseHash();
  state.current = cur;
  state.messages = new Map();
  state.progress = new Map();
  state.seq = 0;
  state.meta = null;
  renderSidebar();
  $("view").replaceChildren();
  $("composer").hidden = true;
  $("stop").hidden = $("archive").hidden = true;
  $("chips").replaceChildren();
  if (!cur) { $("title").textContent = "Pick a room"; return; }
  if (cur.tid === "activity") return showActivity();
  state.agents = await api(cur.key, `rooms/${encodeURIComponent(cur.rid)}/agents`).catch(() => ({ presets: [], listeners: [] }));
  await refreshMessages();
}

// refreshMessages can be called again (composer submit, SSE note, poll)
// before an earlier call's fetch resolves. A generation counter drops any
// response that isn't from the most recently issued call, and a view-change
// check (key/rid/tid) drops a response that arrives after navigation moved
// on to a different thread; state.seq itself is never allowed to move
// backwards, though messages are always merged by id regardless.
async function refreshMessages() {
  const cur = state.current;
  if (!cur || cur.tid === "activity") return;
  const gen = ++state.msgGen;
  const page = await api(cur.key, `rooms/${encodeURIComponent(cur.rid)}/threads/${encodeURIComponent(cur.tid)}/messages?after=${state.seq}`);
  if (gen !== state.msgGen) return; // a newer call superseded this one
  const now = state.current;
  if (!now || now.key !== cur.key || now.rid !== cur.rid || now.tid !== cur.tid) return; // view changed
  state.meta = page.meta;
  state.chains = page.chains || {};
  for (const m of page.messages) state.messages.set(m.id, m);
  if (page.seq >= state.seq) state.seq = page.seq;
  renderThread();
}

function sorted() { return [...state.messages.values()].sort((a, b) => a.n - b.n); }

function renderThread() {
  const meta = state.meta;
  $("title").textContent = meta.title;
  const archived = meta.status === "archived";
  $("composer").hidden = archived;
  $("stop").hidden = archived;
  $("archive").hidden = false;
  $("archive").textContent = archived ? "Unarchive" : "Archive";
  const chips = new Map();
  for (const name of Object.keys(meta.listeners || {})) chips.set(name, "idle");
  for (const m of sorted()) if (m.role === "agent" && m.listener) {
    if (m.state === "running" || m.state === "pending") chips.set(m.listener, "busy");
    else if (!chips.has(m.listener)) chips.set(m.listener, "idle");
  }
  const cur = state.current;
  $("chips").replaceChildren(...[...chips].map(([name, s]) => {
    const preset = (meta.listeners || {})[name] || name.split(".")[0];
    const q = quotaFor(cur.key, preset);
    const c = el("span", "chip" + (q && q.percent >= 95 ? " hot" : ""));
    c.append(el("span", "dot" + (s === "busy" ? " busy" : " on")), document.createTextNode(name + quotaText(q)));
    return c;
  }));
  const view = $("view");
  const nearBottom = view.scrollHeight - view.scrollTop - view.clientHeight < 80;
  view.replaceChildren(...sorted().map(renderMessage));
  if (nearBottom) view.scrollTop = view.scrollHeight;
}

function renderBody(m) {
  const body = el("div", "body");
  body.innerHTML = m.html; // server-rendered with every input escaped (RenderMarkdown)
  return body;
}

function renderMessage(m) {
  const box = el("div", `msg ${m.role}` + (m.state === "error" || m.state === "uncollectable" ? " error" : ""));
  const who = el("div", "who", m.author);
  who.append(el("span", "when", new Date(m.time).toLocaleTimeString()));
  box.append(who);
  if (m.state === "pending" || m.state === "running") {
    const c = state.chains[m.chain];
    const used = c ? c.used : 0;
    box.append(el("div", "working", `working… (${used}/${state.meta.budget} handoffs used)`));
    const live = el("details", "live");
    live.append(el("summary", "", "live output"));
    const pre = el("pre", "", (state.progress.get(m.request_id) || {}).text || "");
    live.append(pre);
    box.append(live);
    return box;
  }
  if (m.state === "suggested") {
    box.append(el("div", "", "Handoff not sent: chain budget reached."));
    const send = el("button", "", "Send");
    send.onclick = () => post(m.text);
    box.append(el("div", "muted", m.text), el("div", "tools")).lastChild.append(send);
    return box;
  }
  if (m.state === "uncollectable") { box.append(el("div", "body", "result expired")); return box; }
  box.append(m.text ? renderBody(m) : el("div", "body muted", m.role === "agent" ? "(no output)" : ""));
  if (m.state === "error" || m.state === "cancelled") {
    const tools = el("div", "tools");
    if (!m.retried) {
      const retry = el("button", "", "Retry");
      retry.onclick = () => act(`messages/${m.id}/retry`);
      tools.append(retry);
    }
    const log = el("button", "secondary", "Open log");
    log.onclick = () => openLog(m.listener);
    tools.append(log);
    box.append(tools);
  }
  return box;
}

// pollProgress runs on an interval; a slow poll must not overlap the next
// one (both would read the same cursor and append the same output twice).
let pollBusy = false;
async function pollProgress() {
  const cur = state.current;
  if (pollBusy || !cur || cur.tid === "activity") return;
  pollBusy = true;
  try {
    let changed = false;
    for (const m of state.messages.values()) {
      if (m.state !== "running" || !m.request_id) continue;
      const p = state.progress.get(m.request_id) || { cursor: 0, text: "" };
      try {
        const r = await api(cur.key, `rooms/${encodeURIComponent(cur.rid)}/requests/${m.request_id}/progress?cursor=${p.cursor}`);
        if (r.events && r.events.length) {
          p.text = (p.text + r.events.map((e) => e.text).join("")).slice(-20000);
          changed = true;
        }
        p.cursor = r.next_cursor;
        state.progress.set(m.request_id, p);
      } catch (e) { /* next poll retries */ }
    }
    if (changed && state.current === cur && state.meta) renderThread();
  } finally {
    pollBusy = false;
  }
}

async function act(path, body) {
  const cur = state.current;
  try {
    await api(cur.key, `rooms/${encodeURIComponent(cur.rid)}/threads/${encodeURIComponent(cur.tid)}/${path}`, { method: "POST", body });
    await refreshMessages();
  } catch (e) { alert(e.message); }
}

// post sends text with id as its client_id (a fresh one when omitted) and
// reports whether the server accepted it.
async function post(text, id) {
  const cur = state.current;
  if (!text.trim()) return true;
  try {
    await api(cur.key, `rooms/${encodeURIComponent(cur.rid)}/threads/${encodeURIComponent(cur.tid)}/messages`, { method: "POST", body: { text, client_id: id || clientId() } });
  } catch (e) { alert(e.message); return false; }
  try { await refreshMessages(); } catch (e) { alert(e.message); }
  return true;
}

async function openLog(name) {
  const cur = state.current;
  try {
    const text = await api(cur.key, `rooms/${encodeURIComponent(cur.rid)}/agents/${encodeURIComponent(name)}/log`);
    const w = window.open("", "_blank");
    if (w) { const pre = w.document.createElement("pre"); pre.textContent = text; w.document.body.append(pre); }
  } catch (e) { alert(e.message); }
}

// ---------- activity ----------
async function showActivity() {
  const cur = state.current;
  const room = (state.rooms[cur.key] || []).find((r) => r.id === cur.rid);
  $("title").textContent = (room ? room.name : "") + " · Activity";
  const data = await api(cur.key, `rooms/${encodeURIComponent(cur.rid)}/activity`);
  const now = state.current;
  if (!now || now.key !== cur.key || now.rid !== cur.rid || now.tid !== "activity") return; // view changed while awaiting
  const view = $("view");
  const lt = el("table", "activity");
  lt.append(row("th", ["Listener", "Mode", "State", ""]));
  for (const l of data.listeners) {
    const tr = row("td", [l.name, l.mode + (l.preset ? ` · ${l.preset}` : ""), l.busy ? "busy" : "idle", ""]);
    if (l.mode === "hosted" && !l.name.includes(".")) {
      const b = el("button", "", "Message this agent");
      b.onclick = async () => {
        const t = await api(cur.key, `rooms/${encodeURIComponent(cur.rid)}/threads`, { method: "POST", body: { title: `with ${l.name}`, primary: l.name, client_id: clientId() } });
        await loadThreads(cur.key, cur.rid);
        go(cur.key, cur.rid, t.id);
      };
      tr.lastChild.append(b);
    }
    lt.append(tr);
  }
  const rt = el("table", "activity");
  rt.append(row("th", ["Request", "From → agent", "Status", "Updated", "Result"]));
  for (const r of data.requests) {
    rt.append(row("td", [r.id, `${r.from || "?"} → ${r.agent}`, r.status, new Date(r.updated).toLocaleString(), r.result || ""]));
  }
  view.replaceChildren(el("h3", "", "Listeners"), data.listeners.length ? lt : el("p", "muted", "No listeners."),
    el("h3", "", "Recent requests"), data.requests.length ? rt : el("p", "muted", "No journaled requests."));
}

function row(cell, values) {
  const tr = el("tr");
  for (const v of values) tr.append(el(cell, "", v));
  return tr;
}

// ---------- composer + @ autocomplete ----------
function mentionAtCursor(input) {
  const upto = input.value.slice(0, input.selectionStart);
  const m = upto.match(/(^|[\s(\[,])@([A-Za-z0-9._-]*)$/);
  return m ? { start: upto.length - m[2].length, word: m[2] } : null;
}

function updateSuggest() {
  const input = $("input");
  const box = $("suggest");
  const at = mentionAtCursor(input);
  const names = [...state.agents.presets, ...Object.keys((state.meta || {}).listeners || {}), ...state.agents.listeners, "you"];
  const hits = at ? [...new Set(names)].filter((n) => n.startsWith(at.word)).slice(0, 8) : [];
  box.hidden = hits.length === 0;
  const key = (state.current || {}).key;
  box.replaceChildren(...hits.map((n, i) => {
    const d = el("div", i === 0 ? "on" : "", "@" + n + quotaText(quotaFor(key, n)));
    d.dataset.name = n;
    d.onmousedown = (e) => { e.preventDefault(); complete(n); };
    return d;
  }));
}

function complete(name) {
  const input = $("input");
  const at = mentionAtCursor(input);
  if (!at) return;
  input.value = input.value.slice(0, at.start) + name + " " + input.value.slice(input.selectionStart);
  $("suggest").hidden = true;
  input.focus();
}

function wireComposer() {
  const input = $("input");
  input.addEventListener("input", updateSuggest);
  input.addEventListener("keydown", (e) => {
    const box = $("suggest");
    if (!box.hidden && (e.key === "Tab" || e.key === "Enter")) {
      e.preventDefault();
      complete(box.querySelector(".on").dataset.name);
      return;
    }
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      $("composer").requestSubmit();
    }
  });
  // One client_id per composed message: a failed post restores the text,
  // and re-sending the same text reuses the id, so a post that reached the
  // server before failing is deduplicated instead of appearing twice.
  const draft = { text: null, id: null };
  $("composer").addEventListener("submit", async (e) => {
    e.preventDefault();
    const text = input.value;
    if (!text.trim()) return;
    if (draft.text !== text) { draft.text = text; draft.id = clientId(); }
    input.value = "";
    $("suggest").hidden = true;
    if (await post(text, draft.id)) {
      draft.text = draft.id = null;
    } else if (!input.value) {
      input.value = text;
    }
  });
  $("stop").onclick = () => act("stop");
  $("archive").onclick = () => act("archive", { archived: state.meta.status !== "archived" });
  $("open-drawer").onclick = () => $("app").classList.add("drawer");
  $("close-drawer").onclick = () => $("app").classList.remove("drawer");
  $("show-hidden").onchange = renderSidebar;
}

// ---------- live updates ----------
function connect(key) {
  if (state.sources[key]) state.sources[key].close();
  const src = new EventSource(BASE + prefix(key) + "events");
  state.sources[key] = src;
  src.onopen = () => {
    if (key === "local") { loadMachines().then(openCurrent); return; }
    const m = state.machines.find((x) => x.key === key);
    if (!m) return;
    Promise.all([loadRooms(m), loadQuotas(m)]).then(() => {
      renderSidebar();
      if (state.current && state.current.key === key) openCurrent();
    });
  };
  src.onmessage = async (ev) => {
    const n = JSON.parse(ev.data);
    const cur = state.current;
    if (n.kind === "peer") {
      // Only the local stream's own hub knows about peer up/down; a "peer"
      // note relayed through a peer's proxied stream describes that peer's
      // own peers, not ours, and must be ignored here.
      if (key !== "local") return;
      const wasOnline = {};
      for (const m of state.machines) wasOnline[m.key] = m.online;
      await loadMachines();
      for (const m of state.machines) {
        if (m.key === "local") continue;
        if (m.online && !wasOnline[m.key]) {
          connect(m.key);
        } else if (!m.online && wasOnline[m.key] && state.sources[m.key]) {
          state.sources[m.key].close();
          delete state.sources[m.key];
        }
      }
      return;
    }
    if (n.kind === "quota") {
      const m = state.machines.find((x) => x.key === key);
      if (m) await loadQuotas(m);
      renderSidebar();
      if (state.meta && state.current && state.current.tid !== "activity") renderThread();
      return;
    }
    if (n.room) await loadThreads(key, n.room).catch(() => {});
    if (n.kind === "activity" || n.kind === "thread") {
      const m = state.machines.find((x) => x.key === key);
      if (m) await loadRooms(m);
    }
    renderSidebar();
    if (!cur || cur.key !== key || cur.rid !== n.room) return;
    if (cur.tid === "activity" && n.kind === "activity") showActivity();
    if (n.thread === cur.tid) refreshMessages();
  };
}

async function start() {
  wireComposer();
  window.addEventListener("hashchange", openCurrent);
  await loadMachines();
  connect("local");
  for (const m of state.machines) if (m.key !== "local" && m.online) connect(m.key);
  await openCurrent();
  setInterval(pollProgress, 1500);
}

start().catch((e) => { $("title").textContent = "tincan: " + e.message; });
