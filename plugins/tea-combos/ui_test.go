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
  const MKNOWN = ["model", "provider", "weight", "role", "quotaPool", "accountStrategy", "accountPool"];
  const explicit = (s) => s === "round-robin" || s === "quota-aware-round-robin";
  function validate(c) {
    for (const k of Object.keys(c)) if (!KNOWN.includes(k)) return "unknown field " + k;
    if (!["", "inherit", "round-robin", "quota-aware-round-robin"].includes(c.accountStrategy || "")) return "bad combo accountStrategy";
    for (const [i, mem] of (c.members || []).entries()) {
      for (const k of Object.keys(mem)) if (!MKNOWN.includes(k)) return "unknown member field " + k;
      const eff = mem.accountStrategy || c.accountStrategy || "";
      if (explicit(eff) && !mem.accountPool) return "member " + i + " requires accountPool";
      if (!explicit(eff) && mem.accountPool) return "member " + i + " sets accountPool without explicit strategy";
    }
    return "";
  }
  async function fetchStub(url, init) {
    init = init || {};
    const method = init.method || "GET";
    const u = new URL(url, "http://host");
    const name = u.searchParams.get("name");
    const body = init.body ? JSON.parse(init.body) : undefined;
    requests.push({ method, name, body });
    const reply = (status, data) => ({ ok: status < 400, status, statusText: "", text: async () => data === undefined ? "" : JSON.stringify(data) });
    if (method === "GET" && !name) return reply(200, { combos: store });
    if (method === "GET") { const c = store.find((x) => x.name === name); return c ? reply(200, c) : reply(404, { error: "not found" }); }
    const err = validate(body);
    if (err) return reply(400, { error: err });
    const saved = JSON.parse(JSON.stringify(body));
    if (opts.dropAccountPool) saved.members.forEach((x) => { delete x.accountPool; });
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
  return { registry, requests, store };
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
  out.violations = violations;
  console.log(JSON.stringify(out));
})().catch((e) => { console.error(e && e.stack || String(e)); process.exit(1); });
`

type harnessOut struct {
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
