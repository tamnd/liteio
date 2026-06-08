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
    <nav class="tabs">
      <button class="tab" data-view="cluster">Cluster</button>
      <button class="tab" data-view="buckets">Buckets</button>
      <button class="tab" data-view="identity">Identity</button>
    </nav>
    <section id="view"><p class="muted">Loading...</p></section>`;
  document.getElementById("logout").addEventListener("click", async () => {
    await api("POST", "/api/logout");
    renderLogin("");
  });
  for (const tab of document.querySelectorAll(".tab")) {
    tab.addEventListener("click", () => selectView(tab.dataset.view));
  }
  selectView("cluster");
}

// selectView switches the active tab and renders its view into #view.
function selectView(name) {
  for (const tab of document.querySelectorAll(".tab")) {
    tab.classList.toggle("active", tab.dataset.view === name);
  }
  if (name === "buckets") {
    loadBuckets();
  } else if (name === "identity") {
    loadIdentity();
  } else {
    loadDashboard();
  }
}

// loadDashboard fetches the deployment topology and health through the bridge and
// renders the cluster overview. A 404 means the node serves IAM only (no object
// layer), so the dashboard says so rather than erroring.
async function loadDashboard() {
  const el = document.getElementById("view");
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
      <div><dt>Usable free</dt><dd>${bytes(info.usableFree)} / ${bytes(info.usableCapacity)}</dd></div>
      <div><dt>Raw capacity</dt><dd>${bytes(info.rawCapacity)}</dd></div>
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
      // A set reporting fewer drives than it has flags the capacity as partial.
      const cap = s.drivesReporting < s.driveCount
        ? `${bytes(s.usableFree)} / ${bytes(s.usableCapacity)} *`
        : `${bytes(s.usableFree)} / ${bytes(s.usableCapacity)}`;
      rows.push(`<tr>
        <td>${pool}.${set}</td>
        <td>${s.onlineCount} / ${s.driveCount}</td>
        <td>${s.parity}</td>
        <td>${s.readQuorum}</td>
        <td>${cap}</td>
        <td class="status-${state}">${state}</td>
      </tr>`);
      set++;
    }
    pool++;
  }
  return `<h2>Erasure sets</h2>
    <table class="sets">
      <thead><tr><th>Set</th><th>Online</th><th>Parity (M)</th><th>Read quorum (K)</th><th>Usable free</th><th>State</th></tr></thead>
      <tbody>${rows.join("")}</tbody>
    </table>
    <p class="muted">* capacity is partial: not every drive reported.</p>`;
}

// maxUpload caps a console upload. The signed bridge buffers and hashes the whole
// body for a single-shot signature (it does not do chunked payload signing), so the
// console uploads small objects only; larger/streaming uploads are a planned feature.
const maxUpload = 1024 * 1024;

// s3 calls the S3 bridge. Unlike api() it does not send a JSON content type (S3
// speaks XML) and returns the raw Response so callers can read text, a blob, or a
// status. An optional body (a Blob or ArrayBuffer) rides a PUT through the bridge.
async function s3(method, path, body) {
  const opts = {
    method,
    headers: { "X-Liteio-Console": "1" },
    credentials: "same-origin",
  };
  if (body !== undefined) opts.body = body;
  return fetch("/api/s3" + path, opts);
}

// parseXML turns an S3 XML response body into a Document for querying.
function parseXML(text) {
  return new DOMParser().parseFromString(text, "application/xml");
}

// loadBuckets lists the deployment's buckets through the S3 bridge and renders them
// as a table; each row opens the object browser for that bucket. A 404 means no S3
// handler is wired (an identity-only node).
async function loadBuckets() {
  const el = document.getElementById("view");
  const r = await s3("GET", "/");
  if (r.status === 404) {
    el.innerHTML = `<p class="muted">Object storage is not available on this node.</p>`;
    return;
  }
  if (!r.ok) {
    el.innerHTML = `<p class="error">Could not list buckets.</p>`;
    return;
  }
  const doc = parseXML(await r.text());
  const buckets = [...doc.querySelectorAll("Buckets > Bucket")].map((b) => ({
    name: b.querySelector("Name")?.textContent ?? "",
    created: b.querySelector("CreationDate")?.textContent ?? "",
  }));
  const rows = buckets.map((b) => `<tr>
      <td><button class="link bucket" data-bucket="${esc(b.name)}">${esc(b.name)}</button></td>
      <td class="muted">${esc(b.created)}</td>
    </tr>`).join("");
  el.innerHTML = `
    <div class="topbar">
      <h2>Buckets</h2>
      <form id="mkbucket"><input id="bname" placeholder="new-bucket-name" /><button type="submit">Create</button></form>
    </div>
    <div class="error" id="bucket-error"></div>
    ${buckets.length ? `<table class="sets"><thead><tr><th>Name</th><th>Created</th></tr></thead><tbody>${rows}</tbody></table>`
      : `<p class="muted">No buckets yet.</p>`}`;
  document.getElementById("mkbucket").addEventListener("submit", (e) => {
    e.preventDefault();
    createBucket(document.getElementById("bname").value.trim());
  });
  for (const b of document.querySelectorAll(".bucket")) {
    b.addEventListener("click", () => loadObjects(b.dataset.bucket));
  }
}

// createBucket creates a bucket through the S3 bridge, then refreshes the list. A
// failed create shows the S3 error rather than silently doing nothing.
async function createBucket(name) {
  const err = document.getElementById("bucket-error");
  if (!name) {
    err.textContent = "Enter a bucket name.";
    return;
  }
  const r = await s3("PUT", "/" + encodeURIComponent(name));
  if (!r.ok) {
    err.textContent = `Create failed (${r.status}).`;
    return;
  }
  loadBuckets();
}

// loadObjects lists the objects in a bucket (ListObjectsV2, top level) and renders
// them with an upload control, per-object download and delete actions, and a back
// link to the bucket list.
async function loadObjects(bucket) {
  const el = document.getElementById("view");
  const r = await s3("GET", "/" + encodeURIComponent(bucket) + "?list-type=2");
  if (!r.ok) {
    el.innerHTML = `<p class="error">Could not list objects in ${esc(bucket)} (${r.status}).</p>
      <button class="link" id="back">Back to buckets</button>`;
    document.getElementById("back").addEventListener("click", loadBuckets);
    return;
  }
  const doc = parseXML(await r.text());
  const objects = [...doc.querySelectorAll("Contents")].map((c) => ({
    key: c.querySelector("Key")?.textContent ?? "",
    size: Number(c.querySelector("Size")?.textContent ?? 0),
    modified: c.querySelector("LastModified")?.textContent ?? "",
  }));
  const rows = objects.map((o) => `<tr>
      <td>${esc(o.key)}</td>
      <td>${bytes(o.size)}</td>
      <td class="muted">${esc(o.modified)}</td>
      <td class="actions">
        <button class="link get" data-key="${esc(o.key)}">Download</button>
        <button class="link del" data-key="${esc(o.key)}">Delete</button>
      </td>
    </tr>`).join("");
  el.innerHTML = `
    <div class="topbar">
      <h2>${esc(bucket)}</h2>
      <button class="link" id="back">Back to buckets</button>
    </div>
    <form id="upload"><input type="file" id="ufile" /><button type="submit">Upload</button></form>
    <div class="error" id="object-error"></div>
    ${objects.length ? `<table class="sets"><thead><tr><th>Key</th><th>Size</th><th>Modified</th><th></th></tr></thead><tbody>${rows}</tbody></table>`
      : `<p class="muted">This bucket is empty.</p>`}`;
  document.getElementById("back").addEventListener("click", loadBuckets);
  document.getElementById("upload").addEventListener("submit", (e) => {
    e.preventDefault();
    uploadObject(bucket, document.getElementById("ufile").files[0]);
  });
  for (const b of document.querySelectorAll("button.get")) {
    b.addEventListener("click", () => downloadObject(bucket, b.dataset.key));
  }
  for (const b of document.querySelectorAll("button.del")) {
    b.addEventListener("click", () => deleteObject(bucket, b.dataset.key));
  }
}

// objectPath joins a bucket and key into the bridge path, escaping each segment of
// the key independently so a slash in the key stays a path separator.
function objectPath(bucket, key) {
  const k = key.split("/").map(encodeURIComponent).join("/");
  return "/" + encodeURIComponent(bucket) + "/" + k;
}

// uploadObject PUTs a chosen file into the bucket through the S3 bridge, then
// refreshes the listing. The bridge buffers and signs the whole body, so the upload
// is capped at maxUpload; a larger file is refused with a clear message rather than a
// confusing bridge error.
async function uploadObject(bucket, file) {
  const err = document.getElementById("object-error");
  if (!file) {
    err.textContent = "Choose a file to upload.";
    return;
  }
  if (file.size > maxUpload) {
    err.textContent = `File is too large for the console uploader (${bytes(maxUpload)} limit). Large uploads are a planned feature.`;
    return;
  }
  const r = await s3("PUT", objectPath(bucket, file.name), file);
  if (!r.ok) {
    err.textContent = `Upload failed (${r.status}).`;
    return;
  }
  loadObjects(bucket);
}

// downloadObject fetches an object through the bridge and hands it to the browser as
// a file download. The bridge requires the CSRF header, so a plain link will not do;
// the body comes back as a blob the page turns into a temporary object URL.
async function downloadObject(bucket, key) {
  const err = document.getElementById("object-error");
  const r = await s3("GET", objectPath(bucket, key));
  if (!r.ok) {
    err.textContent = `Download failed (${r.status}).`;
    return;
  }
  const url = URL.createObjectURL(await r.blob());
  const a = document.createElement("a");
  a.href = url;
  a.download = key.split("/").pop();
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

// deleteObject removes an object through the bridge, then refreshes the listing. A
// failed delete shows the S3 status rather than silently doing nothing.
async function deleteObject(bucket, key) {
  const err = document.getElementById("object-error");
  const r = await s3("DELETE", objectPath(bucket, key));
  if (!r.ok) {
    err.textContent = `Delete failed (${r.status}).`;
    return;
  }
  loadObjects(bucket);
}

// loadIdentity renders the IAM management view: the users and policies the admin
// bridge serves. Groups and service accounts are a planned follow-up. A 404 on the
// users list means the node does not expose the admin API.
async function loadIdentity() {
  const el = document.getElementById("view");
  const ur = await api("GET", "/api/admin/users");
  if (ur.status === 404) {
    el.innerHTML = `<p class="muted">Identity management is not available on this node.</p>`;
    return;
  }
  if (!ur.ok) {
    el.innerHTML = `<p class="error">Could not load users.</p>`;
    return;
  }
  const users = (await ur.json()).users ?? [];
  const policies = (await (await api("GET", "/api/admin/policies")).json()).policies ?? [];
  const groups = (await (await api("GET", "/api/admin/groups")).json()).groups ?? [];
  const userRows = users.map((u) => `<tr>
      <td><button class="link user" data-key="${esc(u)}">${esc(u)}</button></td>
      <td class="actions"><button class="link del-user" data-key="${esc(u)}">Delete</button></td>
    </tr>`).join("");
  const policyRows = policies.map((p) => `<tr>
      <td><button class="link policy" data-name="${esc(p)}">${esc(p)}</button></td>
      <td class="actions"><button class="link del-policy" data-name="${esc(p)}">Delete</button></td>
    </tr>`).join("");
  const groupRows = groups.map((g) => `<tr>
      <td><button class="link group" data-name="${esc(g)}">${esc(g)}</button></td>
      <td class="actions"><button class="link del-group" data-name="${esc(g)}">Delete</button></td>
    </tr>`).join("");
  el.innerHTML = `
    <h2>Users</h2>
    <form id="mkuser" class="iam-form">
      <input id="uaccess" placeholder="access key" />
      <input id="usecret" type="password" placeholder="secret key" />
      <input id="upolicies" placeholder="policies (comma-separated)" />
      <button type="submit">Create</button>
    </form>
    <div class="error" id="user-error"></div>
    ${users.length ? `<table class="sets"><tbody>${userRows}</tbody></table>` : `<p class="muted">No users yet.</p>`}
    <div id="user-detail"></div>
    <h2>Policies</h2>
    <form id="mkpolicy" class="iam-form">
      <input id="paccess" placeholder="policy name" />
      <button type="submit">New / edit</button>
    </form>
    <div class="error" id="policy-error"></div>
    ${policies.length ? `<table class="sets"><tbody>${policyRows}</tbody></table>` : `<p class="muted">No policies.</p>`}
    <div id="policy-detail"></div>
    <h2>Groups</h2>
    <form id="mkgroup" class="iam-form">
      <input id="gname" placeholder="group name" />
      <input id="gpolicies" placeholder="policies (comma-separated)" />
      <button type="submit">Create</button>
    </form>
    <div class="error" id="group-error"></div>
    ${groups.length ? `<table class="sets"><tbody>${groupRows}</tbody></table>` : `<p class="muted">No groups.</p>`}
    <div id="group-detail"></div>`;

  document.getElementById("mkuser").addEventListener("submit", (e) => {
    e.preventDefault();
    createUser(document.getElementById("uaccess").value.trim(),
      document.getElementById("usecret").value,
      document.getElementById("upolicies").value);
  });
  for (const b of document.querySelectorAll("button.user")) {
    b.addEventListener("click", () => showUser(b.dataset.key, policies));
  }
  for (const b of document.querySelectorAll("button.del-user")) {
    b.addEventListener("click", () => deleteUser(b.dataset.key));
  }
  document.getElementById("mkpolicy").addEventListener("submit", (e) => {
    e.preventDefault();
    editPolicy(document.getElementById("paccess").value.trim());
  });
  for (const b of document.querySelectorAll("button.policy")) {
    b.addEventListener("click", () => showPolicy(b.dataset.name));
  }
  for (const b of document.querySelectorAll("button.del-policy")) {
    b.addEventListener("click", () => deletePolicy(b.dataset.name));
  }
  document.getElementById("mkgroup").addEventListener("submit", (e) => {
    e.preventDefault();
    createGroup(document.getElementById("gname").value.trim(),
      document.getElementById("gpolicies").value);
  });
  for (const b of document.querySelectorAll("button.group")) {
    b.addEventListener("click", () => showGroup(b.dataset.name));
  }
  for (const b of document.querySelectorAll("button.del-group")) {
    b.addEventListener("click", () => deleteGroup(b.dataset.name));
  }
}

// splitList turns a comma-separated input into a trimmed, non-empty name list.
function splitList(s) {
  return s.split(",").map((x) => x.trim()).filter(Boolean);
}

// createUser creates a user through the admin bridge, then refreshes the view.
async function createUser(accessKey, secretKey, policiesCSV) {
  const err = document.getElementById("user-error");
  if (!accessKey || !secretKey) {
    err.textContent = "Access key and secret key are required.";
    return;
  }
  const r = await api("PUT", "/api/admin/users/" + encodeURIComponent(accessKey),
    { secretKey, policies: splitList(policiesCSV) });
  if (!r.ok) {
    err.textContent = `Create failed (${r.status}).`;
    return;
  }
  loadIdentity();
}

// deleteUser removes a user (and its service accounts) through the admin bridge.
async function deleteUser(accessKey) {
  const err = document.getElementById("user-error");
  const r = await api("DELETE", "/api/admin/users/" + encodeURIComponent(accessKey));
  if (!r.ok) {
    err.textContent = `Delete failed (${r.status}).`;
    return;
  }
  loadIdentity();
}

// showUser fetches a user's attached policies and groups and renders an attach /
// detach panel below the table.
async function showUser(accessKey, allPolicies) {
  const panel = document.getElementById("user-detail");
  const r = await api("GET", "/api/admin/users/" + encodeURIComponent(accessKey));
  if (!r.ok) {
    panel.innerHTML = `<p class="error">Could not load ${esc(accessKey)} (${r.status}).</p>`;
    return;
  }
  const u = await r.json();
  const attached = u.policies ?? [];
  const detachable = attached.map((p) => `<li>${esc(p)}
      <button class="link detach" data-policy="${esc(p)}">detach</button></li>`).join("");
  const options = allPolicies.map((p) => `<option value="${esc(p)}">${esc(p)}</option>`).join("");
  panel.innerHTML = `
    <h3>${esc(accessKey)}</h3>
    <p class="muted">Groups: ${(u.groups ?? []).map(esc).join(", ") || "none"}</p>
    <ul class="attached">${detachable || "<li class=\"muted\">No policies attached.</li>"}</ul>
    <form id="attach"><select id="apolicy">${options}</select><button type="submit">Attach</button></form>
    <div class="error" id="attach-error"></div>
    <h4>Service accounts</h4>
    <div id="svcaccts"><p class="muted">Loading...</p></div>`;
  document.getElementById("attach").addEventListener("submit", (e) => {
    e.preventDefault();
    attachPolicy(accessKey, document.getElementById("apolicy").value, allPolicies);
  });
  for (const b of document.querySelectorAll("button.detach")) {
    b.addEventListener("click", () => detachPolicy(accessKey, b.dataset.policy, allPolicies));
  }
  loadServiceAccounts(accessKey, allPolicies);
}

// loadServiceAccounts lists the service accounts whose parent is the given user and
// renders a create form and per-account delete. Listing is keyed by parent user, so
// it lives inside the user detail panel rather than as a top-level section.
async function loadServiceAccounts(parent, allPolicies) {
  const box = document.getElementById("svcaccts");
  const r = await api("GET", "/api/admin/service-accounts?user=" + encodeURIComponent(parent));
  if (!r.ok) {
    box.innerHTML = `<p class="error">Could not load service accounts (${r.status}).</p>`;
    return;
  }
  const keys = (await r.json()).serviceAccounts ?? [];
  const items = keys.map((k) => `<li>${esc(k)}
      <button class="link del-svc" data-key="${esc(k)}">delete</button></li>`).join("");
  box.innerHTML = `
    <ul class="attached">${items || "<li class=\"muted\">None.</li>"}</ul>
    <form id="mksvc" class="iam-form">
      <input id="svckey" placeholder="access key" />
      <input id="svcsecret" type="password" placeholder="secret key" />
      <button type="submit">Create</button>
    </form>
    <div class="error" id="svc-error"></div>`;
  document.getElementById("mksvc").addEventListener("submit", (e) => {
    e.preventDefault();
    createServiceAccount(parent,
      document.getElementById("svckey").value.trim(),
      document.getElementById("svcsecret").value, allPolicies);
  });
  for (const b of document.querySelectorAll("button.del-svc")) {
    b.addEventListener("click", () => deleteServiceAccount(parent, b.dataset.key, allPolicies));
  }
}

// createServiceAccount creates a service account under a parent user. Omitting an
// inline policy gives it the parent's full rights, which is the common case.
async function createServiceAccount(parent, accessKey, secretKey, allPolicies) {
  const err = document.getElementById("svc-error");
  if (!accessKey || !secretKey) {
    err.textContent = "Access key and secret key are required.";
    return;
  }
  const r = await api("PUT", "/api/admin/service-accounts",
    { accessKey, secretKey, parentUser: parent });
  if (!r.ok) {
    err.textContent = `Create failed (${r.status}).`;
    return;
  }
  showUser(parent, allPolicies);
}

// deleteServiceAccount removes a service account, then re-renders the user panel.
async function deleteServiceAccount(parent, accessKey, allPolicies) {
  const err = document.getElementById("svc-error");
  const r = await api("DELETE", "/api/admin/service-accounts/" + encodeURIComponent(accessKey));
  if (!r.ok) {
    err.textContent = `Delete failed (${r.status}).`;
    return;
  }
  showUser(parent, allPolicies);
}

// attachPolicy attaches a policy to a user, then re-renders the user panel.
async function attachPolicy(accessKey, policy, allPolicies) {
  const err = document.getElementById("attach-error");
  if (!policy) {
    err.textContent = "Choose a policy.";
    return;
  }
  const r = await api("PUT", `/api/admin/users/${encodeURIComponent(accessKey)}/policies/${encodeURIComponent(policy)}`);
  if (!r.ok) {
    err.textContent = `Attach failed (${r.status}).`;
    return;
  }
  showUser(accessKey, allPolicies);
}

// detachPolicy detaches a policy from a user, then re-renders the user panel.
async function detachPolicy(accessKey, policy, allPolicies) {
  const err = document.getElementById("attach-error");
  const r = await api("DELETE", `/api/admin/users/${encodeURIComponent(accessKey)}/policies/${encodeURIComponent(policy)}`);
  if (!r.ok) {
    err.textContent = `Detach failed (${r.status}).`;
    return;
  }
  showUser(accessKey, allPolicies);
}

// showPolicy fetches a policy document and renders it pretty-printed.
async function showPolicy(name) {
  const panel = document.getElementById("policy-detail");
  const r = await api("GET", "/api/admin/policies/" + encodeURIComponent(name));
  if (!r.ok) {
    panel.innerHTML = `<p class="error">Could not load policy ${esc(name)} (${r.status}).</p>`;
    return;
  }
  const doc = await r.json();
  panel.innerHTML = `<h3>${esc(name)}</h3>
    <pre class="policy-doc">${esc(JSON.stringify(doc, null, 2))}</pre>
    <button class="link" id="edit-policy">Edit</button>`;
  document.getElementById("edit-policy").addEventListener("click", () => editPolicy(name, JSON.stringify(doc, null, 2)));
}

// editPolicy opens an editor for a policy document (new or existing) and PUTs it to
// the admin bridge on save. A canned policy name is rejected by the server.
async function editPolicy(name, current) {
  const panel = document.getElementById("policy-detail");
  if (!name) {
    document.getElementById("policy-error").textContent = "Enter a policy name.";
    return;
  }
  const seed = current ?? `{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Action": ["s3:GetObject"], "Resource": ["arn:aws:s3:::*"] }
  ]
}`;
  panel.innerHTML = `<h3>Edit ${esc(name)}</h3>
    <textarea id="policy-body" rows="14" class="policy-edit">${esc(seed)}</textarea>
    <div><button id="save-policy">Save</button></div>
    <div class="error" id="policy-edit-error"></div>`;
  document.getElementById("save-policy").addEventListener("click", () => savePolicy(name));
}

// savePolicy validates the editor's JSON locally, then PUTs it to the admin bridge.
async function savePolicy(name) {
  const err = document.getElementById("policy-edit-error");
  const text = document.getElementById("policy-body").value;
  let doc;
  try {
    doc = JSON.parse(text);
  } catch (e) {
    err.textContent = "Policy is not valid JSON.";
    return;
  }
  const r = await api("PUT", "/api/admin/policies/" + encodeURIComponent(name), doc);
  if (!r.ok) {
    err.textContent = `Save failed (${r.status}).`;
    return;
  }
  loadIdentity();
}

// deletePolicy removes a custom policy through the admin bridge (canned policies are
// refused by the server).
async function deletePolicy(name) {
  const err = document.getElementById("policy-error");
  const r = await api("DELETE", "/api/admin/policies/" + encodeURIComponent(name));
  if (!r.ok) {
    err.textContent = `Delete failed (${r.status}); canned policies cannot be removed.`;
    return;
  }
  loadIdentity();
}

// createGroup creates a group with an initial policy set through the admin bridge.
async function createGroup(name, policiesCSV) {
  const err = document.getElementById("group-error");
  if (!name) {
    err.textContent = "Enter a group name.";
    return;
  }
  const r = await api("PUT", "/api/admin/groups/" + encodeURIComponent(name),
    { policies: splitList(policiesCSV) });
  if (!r.ok) {
    err.textContent = `Create failed (${r.status}).`;
    return;
  }
  loadIdentity();
}

// deleteGroup removes a group through the admin bridge (the store drops it from every
// member's membership).
async function deleteGroup(name) {
  const err = document.getElementById("group-error");
  const r = await api("DELETE", "/api/admin/groups/" + encodeURIComponent(name));
  if (!r.ok) {
    err.textContent = `Delete failed (${r.status}).`;
    return;
  }
  loadIdentity();
}

// showGroup fetches a group's policies and members and renders an add/remove-member
// panel below the group table.
async function showGroup(name) {
  const panel = document.getElementById("group-detail");
  const r = await api("GET", "/api/admin/groups/" + encodeURIComponent(name));
  if (!r.ok) {
    panel.innerHTML = `<p class="error">Could not load group ${esc(name)} (${r.status}).</p>`;
    return;
  }
  const g = await r.json();
  const members = (g.members ?? []).map((m) => `<li>${esc(m)}
      <button class="link drop-member" data-key="${esc(m)}">remove</button></li>`).join("");
  panel.innerHTML = `
    <h3>${esc(name)}</h3>
    <p class="muted">Policies: ${(g.policies ?? []).map(esc).join(", ") || "none"}</p>
    <ul class="attached">${members || "<li class=\"muted\">No members.</li>"}</ul>
    <form id="addmember"><input id="memberkey" placeholder="user access key" /><button type="submit">Add member</button></form>
    <div class="error" id="member-error"></div>`;
  document.getElementById("addmember").addEventListener("submit", (e) => {
    e.preventDefault();
    addMember(name, document.getElementById("memberkey").value.trim());
  });
  for (const b of document.querySelectorAll("button.drop-member")) {
    b.addEventListener("click", () => removeMember(name, b.dataset.key));
  }
}

// addMember adds a user to a group, then re-renders the group panel.
async function addMember(group, key) {
  const err = document.getElementById("member-error");
  if (!key) {
    err.textContent = "Enter a user access key.";
    return;
  }
  const r = await api("PUT", `/api/admin/groups/${encodeURIComponent(group)}/members/${encodeURIComponent(key)}`);
  if (!r.ok) {
    err.textContent = `Add failed (${r.status}).`;
    return;
  }
  showGroup(group);
}

// removeMember removes a user from a group, then re-renders the group panel.
async function removeMember(group, key) {
  const err = document.getElementById("member-error");
  const r = await api("DELETE", `/api/admin/groups/${encodeURIComponent(group)}/members/${encodeURIComponent(key)}`);
  if (!r.ok) {
    err.textContent = `Remove failed (${r.status}).`;
    return;
  }
  showGroup(group);
}

// bytes formats a byte count in binary units (KiB, MiB, ...) for the dashboard.
// A zero or missing value reads as "0 B" rather than blank.
function bytes(n) {
  n = Number(n) || 0;
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"];
  let u = 0;
  while (n >= 1024 && u < units.length - 1) {
    n /= 1024;
    u++;
  }
  return `${u === 0 ? n : n.toFixed(1)} ${units[u]}`;
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
