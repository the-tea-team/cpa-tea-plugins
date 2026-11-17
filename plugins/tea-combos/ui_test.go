package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestEmbeddedUIPreservesDefaultAffinity guards the editor's affinity default. A combo that omits
// sessionAffinity is affinity-enabled (Combo.IsEnabled/default rules), so the page must show it as
// on and must not send false merely because an operator opened and saved an existing combo.
func TestEmbeddedUIPreservesDefaultAffinity(t *testing.T) {
	page := string(uiHTML)
	if !strings.Contains(page, `<input id="combo-affinity" name="sessionAffinity" type="checkbox" checked>`) {
		t.Fatal("new-combo editor must default session affinity to checked")
	}
	if !strings.Contains(page, "combo.sessionAffinity === false") {
		t.Fatal("editor must treat an omitted sessionAffinity as enabled")
	}
}

// TestEmbeddedUIAccountStrategyControls checks the static markup: a combo account strategy
// dropdown that is separate from the member strategy, with exactly the supported values, and no
// leftover claim that combo-local account scheduling is unavailable.
func TestEmbeddedUIAccountStrategyControls(t *testing.T) {
	page := string(uiHTML)
	for _, want := range []string{
		`<select id="combo-strategy" name="strategy">`,
		`<select id="combo-account-strategy" name="accountStrategy">`,
		"Member strategy",
		"Account strategy",
		"accountPool",
		"quotaPool",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	options := selectOptions(t, page, "combo-account-strategy")
	if want := []string{"inherit", "round-robin", "quota-aware-round-robin"}; !reflect.DeepEqual(options, want) {
		t.Errorf("combo account strategy options = %v, want %v", options, want)
	}
	if strings.Contains(page, "combo-local account scheduling is unavailable") {
		t.Error("page still claims combo-local account scheduling is unavailable")
	}
}

// TestEmbeddedUIRendersTextSafely forbids every DOM sink that parses markup. Combo names, model
// IDs, pool IDs and server error strings are operator or upstream text and must only ever reach
// the page as text nodes.
func TestEmbeddedUIRendersTextSafely(t *testing.T) {
	page := string(uiHTML)
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "srcdoc", "createContextualFragment", "DOMParser"} {
		if strings.Contains(page, sink) {
			t.Errorf("page uses markup-parsing sink %q", sink)
		}
	}
}

func selectOptions(t *testing.T, page, id string) []string {
	t.Helper()
	start := strings.Index(page, `<select id="`+id+`"`)
	if start < 0 {
		t.Fatalf("select %q not found", id)
	}
	end := strings.Index(page[start:], "</select>")
	if end < 0 {
		t.Fatalf("select %q is not closed", id)
	}
	var values []string
	for _, part := range strings.Split(page[start:start+end], `<option value="`)[1:] {
		values = append(values, part[:strings.Index(part, `"`)])
	}
	return values
}

// uiHarness drives the real embedded page script against a fake DOM and a fake management API
// (it mimics the plugin's strict decoding and accountPool validation). Node is the only runtime
// that can execute the page, so the behavioural tests skip without it.
const uiHarness = `"use strict";
const fs = require("fs"), vm = require("vm");
const html = fs.readFileSync(process.argv[2], "utf8");
const scriptMatch = html.match(/<script>([\s\S]*)<\/script>/);
if (!scriptMatch) { throw new Error("page has no script"); }
const violations = [];
const tick = () => new Promise((r) => setTimeout(r, 5));
const flush = async () => { for (let i = 0; i < 8; i++) { await tick(); } };

class El {
  constructor(tag) {
    this.tagName = String(tag).toUpperCase(); this.id = ""; this.children = []; this.parent = null;
    this.attrs = {}; this.listeners = {}; this.className = ""; this._text = ""; this._value = null;
    this.checked = false; this.hidden = false; this.disabled = false; this.type = ""; this.required = false;
  }
  get textContent() { return this._text + this.children.map((c) => c.textContent).join(" "); }
  set textContent(v) { this.children.forEach((c) => { c.parent = null; }); this.children = []; this._text = String(v); }
  get value() {
    if (this.tagName === "SELECT") {
      const options = this.children.filter((c) => c.tagName === "OPTION");
      if (this._value !== null && options.some((o) => o.value === this._value)) { return this._value; }
      return options.length ? options[0].value : "";
    }
    return this._value === null ? "" : this._value;
  }
  set value(v) { this._value = String(v); }
  get previousElementSibling() { if (!this.parent) return null; const i = this.parent.children.indexOf(this); return i > 0 ? this.parent.children[i - 1] : null; }
  get nextElementSibling() { if (!this.parent) return null; const i = this.parent.children.indexOf(this); return i >= 0 && i < this.parent.children.length - 1 ? this.parent.children[i + 1] : null; }
  appendChild(c) { if (c.parent) c.remove(); c.parent = this; this.children.push(c); return c; }
  insertBefore(n, ref) { if (n.parent) n.remove(); n.parent = this; const i = this.children.indexOf(ref); if (i < 0) this.children.push(n); else this.children.splice(i, 0, n); return n; }
  remove() { if (this.parent) { this.parent.children = this.parent.children.filter((x) => x !== this); this.parent = null; } }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return this.attrs[k] === undefined ? null : this.attrs[k]; }
  addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); }
  focus() {}
  classes() { return String(this.className).split(/\s+/).filter(Boolean); }
  descendants() { const out = []; const walk = (n) => n.children.forEach((c) => { out.push(c); walk(c); }); walk(this); return out; }
  querySelectorAll(selector) {
    const tokens = selector.trim().split(/\s+/);
    const match = (el, tok) => tok.startsWith(".") ? el.classes().includes(tok.slice(1)) : el.tagName === tok.toUpperCase();
    return this.descendants().filter((d) => {
      if (!match(d, tokens[tokens.length - 1])) return false;
      let node = d.parent, i = tokens.length - 2;
      while (i >= 0 && node && node !== this.parent) { if (match(node, tokens[i])) i--; node = node.parent; }
      return i < 0;
    });
  }
  querySelector(selector) { const all = this.querySelectorAll(selector); return all.length ? all[0] : null; }
}
for (const sink of ["innerHTML", "outerHTML"]) {
  Object.defineProperty(El.prototype, sink, { set(v) { violations.push(sink + " assigned: " + String(v).slice(0, 40)); }, get() { return ""; } });
}
El.prototype.insertAdjacentHTML = function (_p, v) { violations.push("insertAdjacentHTML: " + String(v).slice(0, 40)); };

function option(value) { const o = new El("option"); o.value = value; o.textContent = value; return o; }

function makePage(store, opts) {
  opts = opts || {};
  const registry = {};
  const bodyHTML = html.slice(0, html.indexOf("<script>"));
  const tagRe = /<(\w+)\b([^>]*)>/g;
  let m;
  while ((m = tagRe.exec(bodyHTML))) {
    const idm = m[2].match(/\bid="([^"]+)"/);
    if (!idm) continue;
    const e = new El(m[1]); e.id = idm[1]; e.checked = /\bchecked\b/.test(m[2]);
    if (/\bhidden\b/.test(m[2])) e.hidden = true;
    if (m[1] === "select") {
      const end = bodyHTML.indexOf("</select>", m.index);
      const re = /<option value="([^"]*)"/g; let om; const chunk = bodyHTML.slice(m.index, end);
      while ((om = re.exec(chunk))) e.appendChild(option(om[1]));
    }
    registry[e.id] = e;
  }
  const requests = [];
  const KNOWN = ["name", "accountStrategy", "strategy", "members", "sessionAffinity", "ttl", "enabled", "cooldownSeconds"];
  const MKNOWN = ["model", "provider", "weight", "role", "quotaPool", "accountStrategy", "accountPool", "accountId", "connectionId"];
  const TARGETS = {
    accounts: [
      { id: "acct-empty", provider: "codex", label: "Empty" },
      { id: "acct-a", provider: "codex", label: "Codex A" },
      { id: "acct-b", provider: "codex", label: "Codex B" },
      { id: "acct-claude", provider: "claude", label: "Claude A" },
    ],
    connections: [
      { id: "conn-empty", accountId: "acct-empty", provider: "codex", label: "Empty", models: [] },
      { id: "conn-a1", accountId: "acct-a", provider: "codex", label: "A One", models: ["gpt-x", "m1"], disabled: false, unavailable: false },
      { id: "conn-a2", accountId: "acct-a", provider: "codex", label: "A Two <script>", models: ["gpt-x"], disabled: true, unavailable: false },
      { id: "conn-b1", accountId: "acct-b", provider: "codex", label: "B One", models: ["gpt-x"], disabled: false, unavailable: true },
      { id: "conn-c1", accountId: "acct-claude", provider: "claude", label: "Claude One", models: ["claude-y"], disabled: false, unavailable: false },
    ],
  };
  const explicit = (s) => s === "round-robin" || s === "quota-aware-round-robin";
  function validate(c) {
    for (const k of Object.keys(c)) if (!KNOWN.includes(k)) return "unknown field " + k;
    if (!["", "inherit", "round-robin", "quota-aware-round-robin"].includes(c.accountStrategy || "")) return "bad combo accountStrategy";
    for (const [i, mem] of (c.members || []).entries()) {
      for (const k of Object.keys(mem)) if (!MKNOWN.includes(k)) return "unknown member field " + k;
      if (mem.accountId && mem.connectionId) return "member " + i + " pins accountId and connectionId";
      const eff = mem.accountStrategy || c.accountStrategy || "";
      if (explicit(eff) && !mem.accountPool && !mem.accountId && !mem.connectionId) return "member " + i + " requires accountPool";
      if (!explicit(eff) && mem.accountPool && !mem.accountId && !mem.connectionId) return "member " + i + " sets accountPool without explicit strategy";
    }
    return "";
  }
  let releaseTargets;
  const targetGate = new Promise((resolve) => { releaseTargets = resolve; });
  async function fetchStub(url, init) {
    init = init || {};
    const method = init.method || "GET";
    const u = new URL(url, "http://host");
    const name = u.searchParams.get("name");
    const body = init.body ? JSON.parse(init.body) : undefined;
    requests.push({ method, name, body, path: u.pathname, headers: init.headers || {} });
    const reply = (status, data) => ({ ok: status < 400, status, statusText: "", text: async () => data === undefined ? "" : JSON.stringify(data) });
    if (method === "GET" && u.pathname.endsWith("/targets")) { if (opts.deferTargets) await targetGate; return (opts.targetsStatuses ? opts.targetsStatuses.shift() : opts.targetsStatus) ? reply(503, { error: "targets unavailable" }) : reply(200, TARGETS); }
    if (opts.dropPin) { /* handled after clone */ }
    if (method === "GET" && !name) return reply(200, { combos: store });
    if (method === "GET") { const c = store.find((x) => x.name === name); return c ? reply(200, c) : reply(404, { error: "not found" }); }
    const err = validate(body);
    if (err) return reply(400, { error: err });
    const saved = JSON.parse(JSON.stringify(body));
    if (opts.dropAccountPool) saved.members.forEach((x) => { delete x.accountPool; });
    if (opts.dropPin) saved.members.forEach((x) => { delete x.accountId; delete x.connectionId; });
    const idx = store.findIndex((x) => x.name === (name || body.name));
    if (method === "POST") { store.push(saved); return reply(201, saved); }
    if (idx >= 0) store[idx] = saved;
    return reply(200, saved);
  }
  const ctx = vm.createContext({
    document: {
      getElementById: (id) => { if (!registry[id]) { throw new Error("no element #" + id); } return registry[id]; },
      createElement: (t) => new El(t),
    },
    window: { location: { pathname: "/v0/resource/plugins/tea-combos/combos" }, confirm: () => true },
    localStorage: { getItem: () => "secret", setItem() {}, removeItem() {} },
    fetch: fetchStub, encodeURIComponent, setTimeout, console,
  });
  vm.runInContext(scriptMatch[1], ctx);
  return { registry, requests, store, releaseTargets };
}

const submit = (page) => page.registry["combo-form"].listeners.submit[0]({ preventDefault() {} });
const clickEdit = (page, index) => {
  const row = page.registry["combo-rows"].children[index];
  const edit = row.descendants().find((d) => d.tagName === "BUTTON" && d.textContent === "Edit");
  edit.listeners.click[0]();
};
const memberRows = (page) => page.registry["members"].children;
const field = (row, cls) => row.querySelector("." + cls);

function seedCombo() {
  return {
    name: "general", strategy: "quota-aware-round-robin", accountStrategy: "quota-aware-round-robin",
    members: [
      { model: "gpt-x", provider: "codex", weight: 2, role: "primary", quotaPool: "obs-codex", accountPool: "codex-main" },
      { model: "claude-y", provider: "claude", accountStrategy: "inherit", role: "fallback" },
      { model: "local-z", accountStrategy: "inherit", quotaPool: "obs-only" },
    ],
    ttl: "2h", enabled: true, cooldownSeconds: 30,
  };
}

(async () => {
  const out = {};

  // 1. Open and save an existing combo untouched: the body must equal what the server holds.
  {
    const store = [seedCombo()]; const page = makePage(store); await flush();
    clickEdit(page, 0); await flush();
    out.editorText = page.registry["members"].textContent;
    out.comboAccountSelected = page.registry["combo-account-strategy"].value;
    out.memberFields = memberRows(page).map((r) => ({
      strategy: field(r, "member-account-strategy").value, pool: field(r, "member-account-pool").value, quota: field(r, "member-pool").value,
    }));
    submit(page); await flush();
    const put = page.requests.find((r) => r.method === "PUT");
    out.untouched = { original: seedCombo(), sent: put && put.body, editorError: page.registry["editor-error"].textContent, editorHidden: page.registry["editor-panel"].hidden };
  }

  // 2. Change the account policy independently of the member strategy, observation pool and
  // accountPool; check the payload and that it was read back.
  {
    const store = [seedCombo()]; const page = makePage(store); await flush();
    clickEdit(page, 0); await flush();
    page.registry["combo-account-strategy"].value = "round-robin";
    const rows = memberRows(page);
    field(rows[1], "member-account-strategy").value = "quota-aware-round-robin";
    field(rows[1], "member-account-pool").value = "claude-main";
    field(rows[2], "member-pool").value = "";
    submit(page); await flush();
    const puts = page.requests.filter((r) => r.method === "PUT");
    const gets = page.requests.filter((r) => r.method === "GET" && r.name === "general");
    out.edited = { sent: puts[0] && puts[0].body, putCount: puts.length, readbackGets: gets.length, stored: store[0], editorHidden: page.registry["editor-panel"].hidden, table: page.registry["combo-rows"].textContent, editorError: page.registry["editor-error"].textContent };
  }

  // 3. Client-side validation: explicit strategy without accountPool, and pool without strategy.
  {
    const store = [seedCombo()]; const page = makePage(store); await flush();
    clickEdit(page, 0); await flush();
    field(memberRows(page)[0], "member-account-pool").value = "";
    submit(page); await flush();
    out.missingPool = { puts: page.requests.filter((r) => r.method === "PUT").length, error: page.registry["editor-error"].textContent };
    field(memberRows(page)[0], "member-account-pool").value = "codex-main";
    field(memberRows(page)[2], "member-account-pool").value = "stray";
    submit(page); await flush();
    out.strayPool = { puts: page.requests.filter((r) => r.method === "PUT").length, error: page.registry["editor-error"].textContent };
  }

  // 4. A server that does not persist accountPool must be reported, not silently accepted.
  {
    const store = [seedCombo()]; const page = makePage(store, { dropAccountPool: true }); await flush();
    clickEdit(page, 0); await flush();
    field(memberRows(page)[0], "member-account-pool").value = "codex-other";
    submit(page); await flush();
    out.lossy = { error: page.registry["editor-error"].textContent, editorHidden: page.registry["editor-panel"].hidden };
  }

  // 5. New combo carries account strategy and pool; unset optional fields stay omitted.
  {
    const store = []; const page = makePage(store); await flush();
    page.registry["new-combo"].listeners.click[0](); await flush();
    page.registry["combo-name"].value = "fresh";
    page.registry["combo-account-strategy"].value = "round-robin";
    const row = memberRows(page)[0];
    field(row, "member-model").value = "m1";
    field(row, "member-account-pool").value = "pool-a";
    submit(page); await flush();
    const post = page.requests.find((r) => r.method === "POST");
    out.created = { sent: post && post.body, error: page.registry["editor-error"].textContent };
  }

  // 6. Hostile text is rendered as text only.
  {
    const evil = "<img src=x onerror=alert(1)>";
    const seed = seedCombo(); seed.name = evil; seed.members[0].model = evil; seed.members[0].accountPool = evil;
    const store = [seed]; const page = makePage(store); await flush();
    out.escape = { table: page.registry["combo-rows"].textContent, hasEvil: page.registry["combo-rows"].textContent.includes(evil) };
    clickEdit(page, 0); await flush();
    out.escape.modelValue = field(memberRows(page)[0], "member-model").value;
    out.escape.poolValue = field(memberRows(page)[0], "member-account-pool").value;
  }
  // 7-11. Pin controls: filtering, mutual exclusion, saved unknown retention, clear, pool intersection.
  {
    const seed = seedCombo();
    seed.members[0].connectionId = "conn-gone"; delete seed.members[0].accountStrategy;
    seed.members[1].accountId = "acct-claude"; seed.members[1].accountStrategy = "inherit";
    const store = [seed]; const page = makePage(store); await flush();
    clickEdit(page, 0); await flush();
    const rows = memberRows(page);
    const opts = (sel) => sel.children.map((o) => ({ value: o.value, text: o.textContent }));
    const targetsReq = page.requests.find((r) => r.path.endsWith("/targets"));
    out.pins = {
      targetsPath: targetsReq && targetsReq.path, auth: targetsReq && targetsReq.headers.Authorization,
      row0Conn: field(rows[0], "member-connection-pin").value, row0ConnOpts: opts(field(rows[0], "member-connection-pin")),
      row0AcctOpts: opts(field(rows[0], "member-account-pin")),
      row1Acct: field(rows[1], "member-account-pin").value, row1AcctOpts: opts(field(rows[1], "member-account-pin")),
      row1ConnOpts: opts(field(rows[1], "member-connection-pin")),
      editorText: page.registry["members"].textContent,
    };
    // explicit user choice clears the other
    field(rows[1], "member-connection-pin").value = "conn-c1";
    (field(rows[1], "member-connection-pin").listeners.change || []).forEach((fn) => fn());
    (field(rows[1], "member-model").listeners.change || []).forEach((fn) => fn());
    out.pins.afterConnPick = { acct: field(rows[1], "member-account-pin").value, conn: field(rows[1], "member-connection-pin").value };
    field(rows[1], "member-account-pin").value = "acct-claude";
    (field(rows[1], "member-account-pin").listeners.change || []).forEach((fn) => fn());
    out.pins.afterAcctPick = { acct: field(rows[1], "member-account-pin").value, conn: field(rows[1], "member-connection-pin").value };
    // untouched row0 keeps unknown pin; row1 account pin + pool should save
    field(rows[1], "member-account-pool").value = "claude-main";
    submit(page); await flush();
    const put = page.requests.find((r) => r.method === "PUT");
    out.pins.sent = put && put.body; out.pins.error = page.registry["editor-error"].textContent; out.pins.hidden = page.registry["editor-panel"].hidden;
    // reopen and clear row1 pin explicitly
    clickEdit(page, 0); await flush();
    const rows2 = memberRows(page);
    field(rows2[1], "member-account-pin").value = "";
    (field(rows2[1], "member-account-pin").listeners.change || []).forEach((fn) => fn());
    field(rows2[1], "member-provider").value = "other";
    (field(rows2[1], "member-provider").listeners.change || []).forEach((fn) => fn());
    field(rows2[1], "member-model").value = "other-model";
    (field(rows2[1], "member-model").listeners.change || []).forEach((fn) => fn());
    out.pins.clearedValue = field(rows2[1], "member-account-pin").value;
  }

  {
    const seed = seedCombo(); seed.members[0].connectionId = "conn-gone";
    const page = makePage([seed]); await flush(); clickEdit(page, 0); await flush();
    const row = memberRows(page)[0]; const sel = field(row, "member-connection-pin");
    out.savedLabel = sel.children.find((o) => o.value === "conn-gone").textContent;
    sel.value = "conn-a1"; (sel.listeners.change || []).forEach((fn) => fn());
    field(row, "member-model").value = "other"; (field(row, "member-model").listeners.change || []).forEach((fn) => fn());
    out.selectedPin = { value: sel.value, label: sel.children.find((o) => o.value === sel.value).textContent };
  }
  {
    const seed = seedCombo(); seed.members[0].accountId = "acct-zzz";
    const page = makePage([seed], { targetsStatuses: [503, 0, 0] }); await flush();
    clickEdit(page, 0); await flush();
    page.registry["cancel-edit"].listeners.click[0](); clickEdit(page, 0); await flush();
    out.retry = { count: page.requests.filter((r) => r.path.endsWith("/targets")).length, pin: field(memberRows(page)[0], "member-account-pin").value };
    page.registry["refresh"].listeners.click[0](); await flush();
    out.retry.refreshCount = page.requests.filter((r) => r.path.endsWith("/targets")).length;
    out.retry.refreshPin = field(memberRows(page)[0], "member-account-pin").value;
  }
  {
    const page = makePage([seedCombo()], { deferTargets: true }); await flush();
    clickEdit(page, 0); await flush();
    page.registry["forget-key"].listeners.click[0]();
    page.releaseTargets(); await flush();
    const sel = field(memberRows(page)[0], "member-connection-pin");
    out.forgotOptions = sel.children.map((o) => o.value);
  }
  // A pending targets response must not restore a user-cleared saved pin.
  {
    const seed = seedCombo(); seed.members[0].accountId = "acct-gone";
    const page = makePage([seed], { deferTargets: true }); await flush();
    clickEdit(page, 0); await flush();
    const row = memberRows(page)[0];
    out.raceInitial = field(row, "member-account-pin").value;
    field(row, "member-account-pin").value = "";
    (field(row, "member-account-pin").listeners.change || []).forEach((fn) => fn());
    page.releaseTargets(); await flush();
    out.raceAfter = field(row, "member-account-pin").value;
  }

  // targets failure preserves editing/saving (no pins available, saved pin retained as option)
  {
    const seed = seedCombo(); seed.members[0].accountId = "acct-zzz";
    const store = [seed]; const page = makePage(store, { targetsStatus: 503 }); await flush();
    clickEdit(page, 0); await flush();
    const row = memberRows(page)[0];
    out.targetsFail = { acct: field(row, "member-account-pin").value, notice: page.registry["notice"].textContent + page.registry["editor-error"].textContent };
    submit(page); await flush();
    const put = page.requests.find((r) => r.method === "PUT");
    out.targetsFail.sent = put && put.body; out.targetsFail.hidden = page.registry["editor-panel"].hidden; out.targetsFail.error = page.registry["editor-error"].textContent;
  }

  // dropPin readback surfaced; explicit strategy now satisfied by pin (no pool needed validation)
  {
    const store = [seedCombo()]; const page = makePage(store, { dropPin: true }); await flush();
    clickEdit(page, 0); await flush();
    const row = memberRows(page)[0];
    field(row, "member-connection-pin").value = "conn-a1";
    (field(row, "member-connection-pin").listeners.change || []).forEach((fn) => fn());
    submit(page); await flush();
    out.pinLossy = { error: page.registry["editor-error"].textContent, hidden: page.registry["editor-panel"].hidden };
  }
  {
    const store = [seedCombo()]; const page = makePage(store); await flush();
    clickEdit(page, 0); await flush();
    const row = memberRows(page)[0];
    field(row, "member-account-pool").value = "";
    field(row, "member-connection-pin").value = "conn-a1";
    (field(row, "member-connection-pin").listeners.change || []).forEach((fn) => fn());
    submit(page); await flush();
    const put = page.requests.find((r) => r.method === "PUT");
    out.pinSatisfies = { sent: put && put.body, error: page.registry["editor-error"].textContent, hidden: page.registry["editor-panel"].hidden };
  }

  out.violations = violations;
  console.log(JSON.stringify(out));
})().catch((e) => { console.error(e && e.stack || String(e)); process.exit(1); });
`

type harnessOut struct {
	ForgotOptions        []string         `json:"forgotOptions"`
	SavedLabel           string           `json:"savedLabel"`
	SelectedPin          map[string]any   `json:"selectedPin"`
	Retry                map[string]any   `json:"retry"`
	RaceInitial          string           `json:"raceInitial"`
	RaceAfter            string           `json:"raceAfter"`
	EditorText           string           `json:"editorText"`
	ComboAccountSelected string           `json:"comboAccountSelected"`
	MemberFields         []map[string]any `json:"memberFields"`
	Untouched            struct {
		Original     map[string]any `json:"original"`
		Sent         map[string]any `json:"sent"`
		EditorError  string         `json:"editorError"`
		EditorHidden bool           `json:"editorHidden"`
	} `json:"untouched"`
	Edited struct {
		Sent         map[string]any `json:"sent"`
		PutCount     int            `json:"putCount"`
		ReadbackGets int            `json:"readbackGets"`
		Stored       map[string]any `json:"stored"`
		EditorHidden bool           `json:"editorHidden"`
		EditorError  string         `json:"editorError"`
		Table        string         `json:"table"`
	} `json:"edited"`
	MissingPool struct {
		Puts  int    `json:"puts"`
		Error string `json:"error"`
	} `json:"missingPool"`
	StrayPool struct {
		Puts  int    `json:"puts"`
		Error string `json:"error"`
	} `json:"strayPool"`
	Lossy struct {
		Error        string `json:"error"`
		EditorHidden bool   `json:"editorHidden"`
	} `json:"lossy"`
	Created struct {
		Sent  map[string]any `json:"sent"`
		Error string         `json:"error"`
	} `json:"created"`
	Escape struct {
		Table      string `json:"table"`
		HasEvil    bool   `json:"hasEvil"`
		ModelValue string `json:"modelValue"`
		PoolValue  string `json:"poolValue"`
	} `json:"escape"`
	Pins struct {
		TargetsPath   string           `json:"targetsPath"`
		Auth          string           `json:"auth"`
		Row0Conn      string           `json:"row0Conn"`
		Row0ConnOpts  []map[string]any `json:"row0ConnOpts"`
		Row0AcctOpts  []map[string]any `json:"row0AcctOpts"`
		Row1Acct      string           `json:"row1Acct"`
		Row1AcctOpts  []map[string]any `json:"row1AcctOpts"`
		Row1ConnOpts  []map[string]any `json:"row1ConnOpts"`
		EditorText    string           `json:"editorText"`
		AfterConnPick map[string]any   `json:"afterConnPick"`
		AfterAcctPick map[string]any   `json:"afterAcctPick"`
		Sent          map[string]any   `json:"sent"`
		Error         string           `json:"error"`
		Hidden        bool             `json:"hidden"`
		ClearedValue  string           `json:"clearedValue"`
	} `json:"pins"`
	TargetsFail struct {
		Acct   string         `json:"acct"`
		Notice string         `json:"notice"`
		Sent   map[string]any `json:"sent"`
		Hidden bool           `json:"hidden"`
		Error  string         `json:"error"`
	} `json:"targetsFail"`
	PinLossy struct {
		Error  string `json:"error"`
		Hidden bool   `json:"hidden"`
	} `json:"pinLossy"`
	PinSatisfies struct {
		Sent   map[string]any `json:"sent"`
		Error  string         `json:"error"`
		Hidden bool           `json:"hidden"`
	} `json:"pinSatisfies"`
	Violations []string `json:"violations"`
}

func runUIHarness(t *testing.T) harnessOut {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping page behaviour test")
	}
	dir := t.TempDir()
	page := filepath.Join(dir, "index.html")
	script := filepath.Join(dir, "harness.js")
	if err := os.WriteFile(page, uiHTML, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte(uiHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, script, page)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("page harness failed: %v\n%s", err, stderr.String())
	}
	var out harnessOut
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode harness output: %v\n%s", err, raw)
	}
	return out
}

func jsonEqual(got, want any) bool {
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	var gv, wv any
	_ = json.Unmarshal(g, &gv)
	_ = json.Unmarshal(w, &wv)
	return reflect.DeepEqual(gv, wv)
}

func TestEmbeddedUIBehaviour(t *testing.T) {
	out := runUIHarness(t)

	t.Run("separate pool labels", func(t *testing.T) {
		for _, want := range []string{"Account pool (accountPool)", "eligibility", "Observation pool (quotaPool)", "scoring only", "Member account strategy"} {
			if !strings.Contains(out.EditorText, want) {
				t.Errorf("member editor lacks label text %q; got %q", want, out.EditorText)
			}
		}
		if out.ComboAccountSelected != "quota-aware-round-robin" {
			t.Errorf("combo account strategy select = %q", out.ComboAccountSelected)
		}
		want := []map[string]any{
			{"strategy": "", "pool": "codex-main", "quota": "obs-codex"},
			{"strategy": "inherit", "pool": "", "quota": ""},
			{"strategy": "inherit", "pool": "", "quota": "obs-only"},
		}
		if !jsonEqual(out.MemberFields, want) {
			t.Errorf("member controls loaded %v, want %v", out.MemberFields, want)
		}
	})

	t.Run("untouched save is lossless", func(t *testing.T) {
		if out.Untouched.Sent == nil {
			t.Fatalf("no PUT sent; editor error %q", out.Untouched.EditorError)
		}
		if !jsonEqual(out.Untouched.Sent, out.Untouched.Original) {
			t.Errorf("untouched save changed the combo:\n sent     %v\n original %v", out.Untouched.Sent, out.Untouched.Original)
		}
		if !out.Untouched.EditorHidden {
			t.Errorf("editor stayed open after a clean save: %q", out.Untouched.EditorError)
		}
	})

	t.Run("controls are independent, sent and read back", func(t *testing.T) {
		if out.Edited.PutCount != 1 || out.Edited.ReadbackGets < 1 {
			t.Fatalf("puts=%d readback GETs=%d error=%q", out.Edited.PutCount, out.Edited.ReadbackGets, out.Edited.EditorError)
		}
		want := map[string]any{
			"name": "general", "strategy": "quota-aware-round-robin", "accountStrategy": "round-robin",
			"members": []any{
				map[string]any{"model": "gpt-x", "provider": "codex", "weight": 2, "role": "primary", "quotaPool": "obs-codex", "accountPool": "codex-main"},
				map[string]any{"model": "claude-y", "provider": "claude", "role": "fallback", "accountStrategy": "quota-aware-round-robin", "accountPool": "claude-main"},
				map[string]any{"model": "local-z", "accountStrategy": "inherit"},
			},
			"ttl": "2h", "enabled": true, "cooldownSeconds": 30,
		}
		if !jsonEqual(out.Edited.Sent, want) {
			t.Errorf("sent payload:\n got  %v\n want %v", out.Edited.Sent, want)
		}
		if !jsonEqual(out.Edited.Stored, want) {
			t.Errorf("stored payload differs: %v", out.Edited.Stored)
		}
		if !out.Edited.EditorHidden {
			t.Errorf("editor should close after a verified save: %q", out.Edited.EditorError)
		}
		for _, want := range []string{"round-robin", "claude-main", "codex-main", "obs-codex", "accountPool", "quotaPool"} {
			if !strings.Contains(out.Edited.Table, want) {
				t.Errorf("table lacks %q: %q", want, out.Edited.Table)
			}
		}
	})

	t.Run("accountPool validation", func(t *testing.T) {
		if out.MissingPool.Puts != 0 || !strings.Contains(out.MissingPool.Error, "accountPool") || !strings.Contains(out.MissingPool.Error, "quotaPool") {
			t.Errorf("explicit strategy without pool: puts=%d error=%q", out.MissingPool.Puts, out.MissingPool.Error)
		}
		if out.StrayPool.Puts != 0 || !strings.Contains(out.StrayPool.Error, "accountPool") {
			t.Errorf("pool without explicit strategy: puts=%d error=%q", out.StrayPool.Puts, out.StrayPool.Error)
		}
	})

	t.Run("read-back mismatch is surfaced", func(t *testing.T) {
		if !strings.Contains(out.Lossy.Error, "read-back") || !strings.Contains(out.Lossy.Error, "accountPool") {
			t.Errorf("lossy server not reported: %q", out.Lossy.Error)
		}
		if out.Lossy.EditorHidden {
			t.Error("editor must stay open when the stored combo differs")
		}
	})

	t.Run("new combo", func(t *testing.T) {
		want := map[string]any{
			"name": "fresh", "strategy": "round-robin", "accountStrategy": "round-robin",
			"members":         []any{map[string]any{"model": "m1", "accountPool": "pool-a"}},
			"sessionAffinity": true, "enabled": true,
		}
		if !jsonEqual(out.Created.Sent, want) {
			t.Errorf("created payload:\n got  %v\n want %v (error %q)", out.Created.Sent, want, out.Created.Error)
		}
	})

	t.Run("pin controls filter and retain", func(t *testing.T) {
		if !strings.HasSuffix(out.Pins.TargetsPath, "/targets") || out.Pins.Auth != "Bearer secret" {
			t.Errorf("targets request path/auth = %q %q", out.Pins.TargetsPath, out.Pins.Auth)
		}
		if out.Pins.Row0Conn != "conn-gone" {
			t.Errorf("unknown saved connection pin erased: %q opts=%v", out.Pins.Row0Conn, out.Pins.Row0ConnOpts)
		}
		foundGone := false
		for _, o := range out.Pins.Row0ConnOpts {
			if o["value"] == "conn-empty" {
				t.Error("connection without model support offered")
			}
			if o["value"] == "conn-gone" && strings.Contains(o["text"].(string), "saved") {
				foundGone = true
			}
		}
		if !foundGone {
			t.Errorf("unknown pin not a labeled saved option: %v", out.Pins.Row0ConnOpts)
		}
		// row0 gpt-x/codex: accounts acct-a,acct-b only; connections conn-a1,a2,b1 (+gone), disabled/unavailable marked
		if len(out.Pins.Row0AcctOpts) != 3 { // blank + 2
			t.Errorf("row0 account opts filtered wrong: %v", out.Pins.Row0AcctOpts)
		}
		joined := ""
		for _, o := range out.Pins.Row0ConnOpts {
			joined += o["text"].(string) + ";"
		}
		if !strings.Contains(joined, "disabled") || !strings.Contains(joined, "unavailable") {
			t.Errorf("disabled/unavailable not marked: %q", joined)
		}
		if out.Pins.Row1Acct != "acct-claude" {
			t.Errorf("row1 account pin = %q", out.Pins.Row1Acct)
		}
		for _, want := range []string{"Pinned account (accountId)", "Pinned connection (connectionId)", "pin overrides"} {
			if !strings.Contains(out.Pins.EditorText, want) {
				t.Errorf("pin help missing %q", want)
			}
		}
	})

	t.Run("pin mutual exclusion, clear and pool intersection", func(t *testing.T) {
		if out.Pins.AfterConnPick["acct"] != "" || out.Pins.AfterConnPick["conn"] != "conn-c1" {
			t.Errorf("conn pick did not clear account: %v", out.Pins.AfterConnPick)
		}
		if out.Pins.AfterAcctPick["conn"] != "" || out.Pins.AfterAcctPick["acct"] != "acct-claude" {
			t.Errorf("account pick did not clear conn: %v", out.Pins.AfterAcctPick)
		}
		if out.Pins.Sent == nil || !out.Pins.Hidden {
			t.Fatalf("pin+pool save failed: sent=%v hidden=%v error=%q", out.Pins.Sent, out.Pins.Hidden, out.Pins.Error)
		}
		members, _ := out.Pins.Sent["members"].([]any)
		if len(members) != 3 {
			t.Fatalf("members %v", out.Pins.Sent)
		}
		m0, _ := members[0].(map[string]any)
		m1, _ := members[1].(map[string]any)
		if m0["connectionId"] != "conn-gone" {
			t.Errorf("untouched unknown pin not preserved on save: %v", m0)
		}
		if m1["accountId"] != "acct-claude" || m1["accountPool"] != "claude-main" || m1["connectionId"] != nil {
			t.Errorf("pin+pool intersection not saved: %v", m1)
		}
		if out.Pins.ClearedValue != "" {
			t.Errorf("explicit clear failed: %q", out.Pins.ClearedValue)
		}
	})

	t.Run("unavailable pin labels", func(t *testing.T) {
		if out.SavedLabel != "conn-gone (saved pin — unavailable for this model/provider)" {
			t.Errorf("saved label: %q", out.SavedLabel)
		}
		if out.SelectedPin["value"] != "conn-a1" || out.SelectedPin["label"] != "conn-a1 (selected pin — unavailable for this model/provider)" {
			t.Errorf("selected pin: %v", out.SelectedPin)
		}
	})
	t.Run("targets retry and refresh", func(t *testing.T) {
		if out.Retry["count"] != float64(2) || out.Retry["refreshCount"] != float64(3) || out.Retry["pin"] != "acct-zzz" || out.Retry["refreshPin"] != "acct-zzz" {
			t.Errorf("retry: %v", out.Retry)
		}
	})
	t.Run("forgotten key discards pending inventory", func(t *testing.T) {
		if len(out.ForgotOptions) != 1 || out.ForgotOptions[0] != "" {
			t.Errorf("stale inventory exposed: %v", out.ForgotOptions)
		}
	})
	t.Run("pending targets respect clear", func(t *testing.T) {
		if out.RaceInitial != "acct-gone" || out.RaceAfter != "" {
			t.Errorf("pending targets restored pin: initial=%q after=%q", out.RaceInitial, out.RaceAfter)
		}
	})
	t.Run("targets failure preserves saved pin", func(t *testing.T) {
		if out.TargetsFail.Acct != "acct-zzz" {
			t.Errorf("saved pin lost on targets failure: %q", out.TargetsFail.Acct)
		}
		if out.TargetsFail.Sent == nil || !out.TargetsFail.Hidden {
			t.Errorf("save after targets failure broken: hidden=%v error=%q", out.TargetsFail.Hidden, out.TargetsFail.Error)
		} else {
			members, _ := out.TargetsFail.Sent["members"].([]any)
			m0, _ := members[0].(map[string]any)
			if m0["accountId"] != "acct-zzz" {
				t.Errorf("pin erased on targets-failure save: %v", m0)
			}
		}
	})

	t.Run("pin readback and pin satisfies explicit strategy", func(t *testing.T) {
		if !strings.Contains(out.PinLossy.Error, "read-back") || !strings.Contains(out.PinLossy.Error, "connectionId") || out.PinLossy.Hidden {
			t.Errorf("pin loss not surfaced: %q hidden=%v", out.PinLossy.Error, out.PinLossy.Hidden)
		}
		if out.PinSatisfies.Sent == nil || !out.PinSatisfies.Hidden {
			t.Errorf("pin should satisfy explicit strategy without pool: error=%q", out.PinSatisfies.Error)
		} else {
			members, _ := out.PinSatisfies.Sent["members"].([]any)
			m0, _ := members[0].(map[string]any)
			if m0["connectionId"] != "conn-a1" || m0["accountPool"] != nil {
				t.Errorf("pin-satisfies payload wrong: %v", m0)
			}
		}
	})

	t.Run("hostile text stays text", func(t *testing.T) {
		evil := "<img src=x onerror=alert(1)>"
		if !out.Escape.HasEvil || out.Escape.ModelValue != evil || out.Escape.PoolValue != evil {
			t.Errorf("text was altered: %+v", out.Escape)
		}
		if len(out.Violations) != 0 {
			t.Errorf("markup sinks used at runtime: %v", out.Violations)
		}
	})
}
