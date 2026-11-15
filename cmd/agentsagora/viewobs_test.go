package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// The page, script and style carry the observer UI features (viewobs.go).
func TestViewObsPieces(t *testing.T) {
	v := setupView(t)
	_, c := v.login(t, v.viewerTok, "")
	_, page := v.do(t, "GET", "/view", c, nil, "")
	for _, want := range []string{`id="q"`, `id="q-clear"`, `id="h-btn"`, `id="health"`, `id="h-range"`, `id="h-quiet"`,
		`id="h-members"`, `id="h-matrix"`, `id="h-quietlist"`, `id="help"`, `id="keys-btn"`, `id="help-close"`, `id="toast"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("page lacks %s", want)
		}
	}
	_, js := v.do(t, "GET", "/view/app.js", c, nil, "")
	for _, want := range []string{
		"function mdRender(", "mdRender(m.text)", `a.rel = "noopener noreferrer"`, "x.protocol === \"http:\" || x.protocol === \"https:\"", // 1 markdown
		"function parseQuery(", "function matchQuery(", "(from|to|in|channel|is):", `"unread"`, `"read"`, // 2 search
		"function healthStats(", "function renderHealth(", "median(a.times)", "S.rc[m.id]", // 3 health
		`"/view#m="`, "/^#m=(\\d+)$/", `"hashchange"`, "function reveal(", "function copyLink(", // 4 links
		"function noticeNew(", "NOTICE.n = 0", "document.hidden && NOTICE.n", // 5 background tab
		"function onKey(", "function typing(", `"?": function`, "j: function", "k: function", // 6 keys
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js lacks %s", want)
		}
	}
	_, css := v.do(t, "GET", "/view/app.css", c, nil, "")
	for _, want := range []string{".md-code", ".md-table", ".md-q", ".qbar", ".health", ".mx td.cell", ".help", ".toast", ".msg.hl", "max-width:760px"} {
		if !strings.Contains(css, want) {
			t.Fatalf("app.css lacks %s", want)
		}
	}
	// still no markup from data and no inline handlers in the page
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", ".style.cssText"} {
		if strings.Contains(js, bad) {
			t.Fatalf("app.js uses %s", bad)
		}
	}
}

// No new server route: the features run in the page. Unknown /view paths stay 404 behind the
// same guard, and a cross-origin request is still refused before any handler.
func TestViewObsNoNewRoute(t *testing.T) {
	v := setupView(t)
	_, c := v.login(t, v.viewerTok, "")
	for _, p := range []string{"/view/api/health", "/view/api/search"} {
		resp, _ := v.do(t, "GET", p, c, nil, "")
		if resp.StatusCode != 404 {
			t.Fatalf("%s: %d, want 404", p, resp.StatusCode)
		}
	}
	resp, _ := v.do(t, "GET", "/view/app.js", c, map[string]string{"Origin": "https://evil.example"}, "")
	if resp.StatusCode != 403 {
		t.Fatalf("cross-origin app.js: %d", resp.StatusCode)
	}
}

// mdHarness runs viewMDJS under node with a minimal fake DOM and prints, per input, the
// built tree serialized with text escaped. Read from stdin by `node -`.
const mdHarness = `
var document = {
  createElement: function (tag) { return new N(tag); },
  createTextNode: function (s) { var n = new N("#text"); n.text = String(s); return n; }
};
function N(tag) { this.tag = tag; this.kids = []; this.attrs = {}; this.className = ""; }
N.prototype.appendChild = function (c) { this.kids.push(c); return c; };
N.prototype.setAttribute = function (k, v) { this.attrs[k] = String(v); };
Object.defineProperty(N.prototype, "textContent", { set: function (s) { this.kids = [document.createTextNode(s)]; } });
function esc(s) { return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;"); }
function ser(n) {
  if (n.tag === "#text") return esc(n.text);
  var a = n.className ? " class=\"" + esc(n.className) + "\"" : "";
  ["href", "target", "rel"].forEach(function (k) { if (n[k] !== undefined) a += " " + k + "=\"" + esc(n[k]) + "\""; });
  return "<" + n.tag + a + ">" + n.kids.map(ser).join("") + "</" + n.tag + ">";
}
`

func TestViewMarkdownNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	cases := []struct {
		in   string
		want []string
		not  []string
	}{
		{"<script>alert(1)</script>", []string{"&lt;script&gt;alert(1)&lt;/script&gt;"}, []string{"<script"}},
		{`<img src=x onerror="alert(1)">`, []string{"&lt;img src=x onerror=&quot;alert(1)&quot;&gt;"}, []string{"<img"}},
		{"[x](javascript:alert(1)) <javascript:alert(2)|y> data:text/html,hi", nil, []string{"<a"}},
		{"see [docs](https://example.com/a?b=1) and <https://ex.example/p|label> or https://plain.example/x/y.",
			[]string{`<a href="https://example.com/a?b=1" target="_blank" rel="noopener noreferrer">docs</a>`,
				`<a href="https://ex.example/p" target="_blank" rel="noopener noreferrer">label</a>`,
				`<a href="https://plain.example/x/y" target="_blank" rel="noopener noreferrer">plain.example/x/y</a>.`}, nil},
		{"**bold** *also* _it_ ~~gone~~ ~x~ \x60a<b>\x60", []string{"<strong>bold</strong>", "<strong>also</strong>", "<em>it</em>",
			"<del>gone</del>", "<del>x</del>", "<code>a&lt;b&gt;</code>"}, nil},
		{"snake_case_name 2*3*4 10~20", nil, []string{"<em>", "<strong>", "<del>"}},
		{"# Title\n> quoted **q**\n---\ntext", []string{`<div class="md-h">Title</div>`, `<blockquote class="md-q"><div class="md-p">quoted <strong>q</strong></div></blockquote>`,
			`<hr class="md-hr"></hr>`, `<div class="md-p">text</div>`}, nil},
		{"- a\n  - b\n1. c\n- [x] done", []string{`<div class="md-li lv0"><span class="mk">•</span><span class="lt">a</span></div>`,
			`<div class="md-li lv1"><span class="mk">◦</span><span class="lt">b</span></div>`, `<div class="md-li lv0 ol"><span class="mk">1.</span>`,
			`<span class="mk">☑</span><span class="lt">done</span>`}, nil},
		{"| a | b |\n|---|:--:|\n| 1 | <i>2</i> |", []string{`<table class="md-table"><thead><tr><th>a</th><th>b</th></tr></thead><tbody><tr><td>1</td><td>&lt;i&gt;2&lt;/i&gt;</td></tr></tbody></table>`}, nil},
		{"\x60\x60\x60go\n<b>x</b> **no**\n\x60\x60\x60\nafter", []string{`<pre class="md-code"><code>&lt;b&gt;x&lt;/b&gt; **no**</code></pre>`, `<div class="md-p">after</div>`}, []string{"<strong>"}},
		// i18n fixture: Japanese text on purpose (list and punctuation handling)
		{"手順: 1. 確認 2. 修正 3. テスト", []string{"手順:\n1. 確認\n2. 修正\n3. テスト"}, nil},
	}
	ins := make([]string, len(cases))
	for i, c := range cases {
		ins[i] = c.in
	}
	inJSON, _ := json.Marshal(ins)
	script := mdHarness + viewMDJS + "\nvar out = " + string(inJSON) + ".map(function (s) { return ser(mdRender(s)); });\nprocess.stdout.write(JSON.stringify(out));\n"
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(script)
	b, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node: %v\n%s", err, ee.Stderr)
		}
		t.Fatal(err)
	}
	var outs []string
	if err := json.Unmarshal(b, &outs); err != nil || len(outs) != len(cases) {
		t.Fatalf("node output %q: %v", b, err)
	}
	for i, c := range cases {
		for _, w := range c.want {
			if !strings.Contains(outs[i], w) {
				t.Errorf("case %d %q:\n got %s\nwant %s", i, c.in, outs[i], w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(outs[i], n) {
				t.Errorf("case %d %q: has %s: %s", i, c.in, n, outs[i])
			}
		}
	}
}

// The whole app.js parses (node compiles it without running it).
func TestViewJSParsesNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	src, _ := json.Marshal(viewJS)
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader("new Function(" + string(src) + ");\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("app.js does not parse: %v\n%s", err, out)
	}
}
