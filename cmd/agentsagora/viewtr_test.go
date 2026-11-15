package main

// i18n fixtures: the Korean and Japanese strings in this file are test data for the
// translation feature (language guessing) on purpose.

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// The page, script and style carry the translation pieces (viewtr.go).
func TestViewTrPieces(t *testing.T) {
	v := setupView(t)
	_, c := v.login(t, v.viewerTok, "")
	_, page := v.do(t, "GET", "/view", c, nil, "")
	for _, want := range []string{`<select id="tl" class="tlsel" hidden`, `<option value="ko">한국어</option>`, `<option value="ja">日本語</option>`,
		`<option value="en">English</option>`, `<option value="">Original</option>`, `id="tl-note"`, `id="tl-msg"`, `id="tl-x"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("page lacks %s", want)
		}
	}
	_, js := v.do(t, "GET", "/view/app.js", c, nil, "")
	for _, want := range []string{
		`typeof Translator !== "undefined"`, "Translator.create({", "Translator.availability({", "function trMissing(", // feature detect
		`"Translation: on the server PC, open https://localhost" + port + "/view in Chrome 138+."`, "desktop Chrome 138+ (not on phones)",
		`indexedDB.open("a2-view-tl", 1)`, `crypto.subtle.digest("SHA-256"`, "TR_MAX = 5000", "store.count()", "c.delete(); c.continue();", // cache
		"TR_SKIP = { PRE: 1, CODE: 1, A: 1 }", "function trCodeLike(", // code is not translated
		"body.appendChild(trBody(m))", "trInit();", "new IntersectionObserver(trSeen", `var t = "en";`, `localStorage.getItem("a2-view-tl-note")`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js lacks %s", want)
		}
	}
	_, css := v.do(t, "GET", "/view/app.css", c, nil, "")
	for _, want := range []string{".tlnote{", ".tlsel{", ".md.trd{"} {
		if !strings.Contains(css, want) {
			t.Fatalf("app.css lacks %s", want)
		}
	}
	for _, bad := range []string{"innerHTML", "eval(", "new Function", "https://translate", "googleapis"} {
		if strings.Contains(js, bad) {
			t.Fatalf("app.js has %s", bad)
		}
	}
}

// trHarness extends the Markdown fake DOM (viewobs_test.go) with the properties trTexts reads.
const trHarness = `
Object.defineProperty(N.prototype, "childNodes", { get: function () { return this.kids; } });
Object.defineProperty(N.prototype, "nodeType", { get: function () { return this.tag === "#text" ? 3 : 1; } });
Object.defineProperty(N.prototype, "tagName", { get: function () { return this.tag.toUpperCase(); } });
Object.defineProperty(N.prototype, "data", { get: function () { return this.text; }, set: function (s) { this.text = s; } });
N.prototype.getAttribute = function (k) { return this.attrs[k] === undefined ? null : this.attrs[k]; };
var created = 0, calls = 0;
globalThis.Translator = { create: async function (o) { created++; return { translate: async function (s) { calls++; return "<" + o.sourceLanguage + ">" + s; } }; } };
`

func TestViewTrNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	in := "Build **failed** on the server\n```\nmake build; exit 1\n```\nsee `go test` and https://example.com/x\nビルドが失敗しました\n$ go vet ./...\n123"
	inJSON, _ := json.Marshal(in)
	script := mdHarness + trHarness + viewMDJS + viewTrCoreJS + `
(async function () {
  var src = [trSrc("hello world"), trSrc("ビルド"), trSrc("빌드 실패"), trSrc("12 34"), trCodeLike("$ go vet"), trCodeLike("x := 1;"), trCodeLike("plain words")];
  var n = mdRender(` + string(inJSON) + `);
  var ch = await trNode(n, "ko", trLine, function () { return true; });
  var again = await trLine("Build ", "ko");
  var same = await trLine("한국어 문장", "ko");
  var stop = mdRender("one line here"), stopped = await trNode(stop, "ko", trLine, function () { return false; });
  process.stdout.write(JSON.stringify({ src: src, ch: ch, out: ser(n), again: again, same: same, created: created, calls: calls, stopped: stopped, stopOut: ser(stop) }));
})().catch(function (e) { console.error(e); process.exit(1); });
`
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(script)
	b, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node: %v\n%s", err, ee.Stderr)
		}
		t.Fatal(err)
	}
	var got struct {
		Src     []any  `json:"src"`
		Ch      bool   `json:"ch"`
		Out     string `json:"out"`
		Again   string `json:"again"`
		Same    string `json:"same"`
		Created int    `json:"created"`
		Calls   int    `json:"calls"`
		Stopped any    `json:"stopped"`
		StopOut string `json:"stopOut"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("node output %q: %v", b, err)
	}
	wantSrc := []any{"en", "ja", "ko", nil, true, true, false}
	for i, w := range wantSrc {
		if got.Src[i] != w {
			t.Errorf("src/codeLike %d: %v, want %v", i, got.Src[i], w)
		}
	}
	if !got.Ch {
		t.Error("nothing translated")
	}
	for _, w := range []string{
		"&lt;en&gt;Build <strong>&lt;en&gt;failed</strong> &lt;en&gt;on the server", // prose; spaces kept around each translated piece
		`<pre class="md-code"><code>make build; exit 1</code></pre>`,                // code block as is
		"<code>go test</code>", // inline code as is
		">example.com/x</a>",   // link text as is
		"&lt;ja&gt;ビルドが失敗しました", // Japanese source
		"$ go vet ./...",       // command line as is
	} {
		if !strings.Contains(got.Out, w) {
			t.Errorf("translated node lacks %s:\n%s", w, got.Out)
		}
	}
	if strings.Contains(got.Out, "&lt;en&gt;$") || strings.Contains(got.Out, "&lt;en&gt;make") || strings.Contains(got.Out, "&lt;en&gt;123") {
		t.Errorf("code or numbers translated:\n%s", got.Out)
	}
	if got.Again != "<en>Build " || got.Same != "한국어 문장" {
		t.Errorf("trLine: %q %q", got.Again, got.Same)
	}
	if got.Created != 2 { // en>ko and ja>ko, each made once
		t.Errorf("translators created %d, want 2", got.Created)
	}
	if got.Stopped != nil || strings.Contains(got.StopOut, "&lt;en&gt;") {
		t.Errorf("stopped run: %v %s", got.Stopped, got.StopOut)
	}
	callsBefore := got.Calls
	if callsBefore < 4 {
		t.Errorf("translate calls %d", callsBefore)
	}
}
