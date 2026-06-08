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
    <p class="muted">Signed in. Bucket, identity, and cluster views load here as they ship.</p>`;
  document.getElementById("logout").addEventListener("click", async () => {
    await api("POST", "/api/logout");
    renderLogin("");
  });
}

async function start() {
  const session = await whoami();
  if (session) renderApp(session);
  else renderLogin("");
}

start();
