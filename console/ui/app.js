// SPDX-License-Identifier: Apache-2.0
//
// The liteio console shell. It is intentionally framework-free: the foundation
// (doc 33) ships login, the authenticated app frame, and the call into the signed
// admin bridge that every later view builds on. The header X-Liteio-Console is sent
// on every API call so the server can reject cross-site requests (a custom header a
// browser cannot set cross-origin without a CORS preflight the server never grants).

const app = document.getElementById("app");

async function api(method, path, body) {
  const opts = {
    method,
    headers: { "X-Liteio-Console": "1" },
    credentials: "same-origin",
  };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  return fetch(path, opts);
}

async function whoami() {
  const r = await api("GET", "/api/session");
  return r.ok ? r.json() : null;
}

function renderLogin(message) {
  app.innerHTML = `
    <form class="login" id="login">
      <h1>liteio console</h1>
      <label for="ak">Access key</label>
      <input id="ak" name="accessKey" autocomplete="username" autofocus />
      <label for="sk">Secret key</label>
      <input id="sk" name="secretKey" type="password" autocomplete="current-password" />
      <button type="submit">Sign in</button>
      <div class="error">${message || ""}</div>
    </form>`;
  document.getElementById("login").addEventListener("submit", async (e) => {
    e.preventDefault();
    const r = await api("POST", "/api/login", {
      accessKey: e.target.accessKey.value,
      secretKey: e.target.secretKey.value,
    });
    if (r.ok) {
      start();
    } else {
      renderLogin("Sign-in failed. Check the credentials and that the user has admin rights.");
    }
  });
}

function renderApp(session) {
  app.innerHTML = `
    <div class="topbar">
      <h1>liteio console</h1>
      <div><span class="muted">${session.accessKey}</span>
      <button class="link" id="logout">Sign out</button></div>
    </div>
    <section id="dashboard"><p class="muted">Loading cluster info...</p></section>`;
  document.getElementById("logout").addEventListener("click", async () => {
    await api("POST", "/api/logout");
    renderLogin("");
  });
  loadDashboard();
}

// loadDashboard fetches the deployment topology and health through the bridge and
// renders the cluster overview. A 404 means the node serves IAM only (no object
// layer), so the dashboard says so rather than erroring.
async function loadDashboard() {
  const el = document.getElementById("dashboard");
  const r = await api("GET", "/api/admin/info");
  if (r.status === 404) {
    el.innerHTML = `<p class="muted">This node serves identity management only.</p>`;
    return;
  }
  if (!r.ok) {
    el.innerHTML = `<p class="error">Could not load cluster info.</p>`;
    return;
  }
  const info = await r.json();
  const health = await (await api("GET", "/api/admin/health")).json();
  el.innerHTML = `
    <h2>Cluster</h2>
    <dl class="grid">
      <div><dt>Version</dt><dd>${esc(info.version)}</dd></div>
      <div><dt>Deployment</dt><dd><code>${esc(info.deploymentId)}</code></dd></div>
      <div><dt>Status</dt><dd class="status-${esc(health.status)}">${esc(health.status)}</dd></div>
      <div><dt>Pools / sets</dt><dd>${info.poolCount} / ${info.setCount}</dd></div>
      <div><dt>Drives online</dt><dd>${info.onlineDriveCount} / ${info.driveCount}</dd></div>
    </dl>
    ${renderSets(info)}`;
}

function renderSets(info) {
  const rows = [];
  let pool = 0;
  for (const p of info.pools) {
    let set = 0;
    for (const s of p.sets) {
      const state = !s.available ? "unavailable" : !s.healthy ? "degraded" : "healthy";
      rows.push(`<tr>
        <td>${pool}.${set}</td>
        <td>${s.onlineCount} / ${s.driveCount}</td>
        <td>${s.parity}</td>
        <td>${s.readQuorum}</td>
        <td class="status-${state}">${state}</td>
      </tr>`);
      set++;
    }
    pool++;
  }
  return `<h2>Erasure sets</h2>
    <table class="sets">
      <thead><tr><th>Set</th><th>Online</th><th>Parity (M)</th><th>Read quorum (K)</th><th>State</th></tr></thead>
      <tbody>${rows.join("")}</tbody>
    </table>`;
}

// esc escapes text before it goes into innerHTML, so a drive endpoint or version
// string can never inject markup.
function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
}

async function start() {
  const session = await whoami();
  if (session) renderApp(session);
  else renderLogin("");
}

start();
