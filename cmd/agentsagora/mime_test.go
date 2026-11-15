package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestUploadMIMEByExtension(t *testing.T) {
	dir := t.TempDir()
	for ext, want := range map[string]string{
		".ts": "text/plain", ".tsx": "text/plain", ".js": "text/plain", ".jsx": "text/plain", ".go": "text/plain",
		".py": "text/plain", ".sh": "text/plain", ".txt": "text/plain", ".yaml": "text/plain", ".yml": "text/plain",
		".toml": "text/plain", ".csv": "text/plain", ".log": "text/plain", ".diff": "text/plain", ".patch": "text/plain",
		".TSX": "text/plain", ".md": "text/markdown", ".json": "application/json",
		".png": "image/png", ".jpg": "image/jpeg", ".pdf": "application/pdf",
	} {
		got, err := uploadMIME(filepath.Join(dir, "a"+ext))
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", ext, got, err, want)
		}
	}
}

func TestUploadMIMENoExtension(t *testing.T) {
	dir := t.TempDir()
	txt := filepath.Join(dir, "Makefile")
	bin := filepath.Join(dir, "blob")
	// i18n fixture: Japanese text on purpose (MIME sniffing of non-ASCII text)
	os.WriteFile(txt, []byte("all:\n\techo 日本語\n"), 0o600)
	os.WriteFile(bin, []byte{0xff, 0xfe, 0, 1}, 0o600)
	if m, err := uploadMIME(txt); err != nil || m != "text/plain" {
		t.Errorf("text without extension: %q %v", m, err)
	}
	if _, err := uploadMIME(bin); err == nil {
		t.Error("binary without extension should need --mime")
	}
	if _, err := uploadMIME(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file without extension should need --mime")
	}
}

func TestUploadMIMEUnknownBinaryExtensionUnchanged(t *testing.T) {
	for _, n := range []string{"a.exe", "a.bin", "a.zip", "a.html"} {
		_, err := uploadMIME(filepath.Join(t.TempDir(), n))
		if err == nil || !strings.Contains(err.Error(), "give --mime") {
			t.Errorf("%s: err %v", n, err)
		}
	}
}

func TestServerAcceptsJSONMIME(t *testing.T) {
	f := setup(t)
	good := []byte(`{"a": [1, 2], "b": "日本語"}`)
	if code, out := putFile(t, f, "frank", "general", "x.json", "application/json", good); code != 200 {
		t.Errorf("json put %d %v", code, out)
	}
	if code, _ := putFile(t, f, "frank", "general", "x.json", "application/json; charset=utf-8", good); code != 200 {
		t.Errorf("json put with charset %d", code)
	}
	if code, _ := putFile(t, f, "frank", "general", "x.json", "application/json", []byte{0xff, 0xfe, 0}); code != 415 {
		t.Errorf("binary as json %d, want 415", code)
	}
	if code, _ := putFile(t, f, "erin", "team", "x.json", "application/json", good); code != 403 {
		t.Errorf("channel rules still apply: %d", code)
	}
	// nothing else was widened
	for _, m := range []string{"application/xml", "application/javascript", "text/html", "application/octet-stream"} {
		if code, _ := putFile(t, f, "frank", "general", "x.bin", m, good); code != 415 {
			t.Errorf("%s: %d, want 415", m, code)
		}
	}
	// same size limit as text/plain
	f.sv.files.maxFile = 100
	big := []byte(`{"k":"` + strings.Repeat("a", 200) + `"}`)
	if code, _ := putFile(t, f, "frank", "general", "big.json", "application/json", big); code != 413 {
		t.Errorf("over max file %d, want 413", code)
	}
}

func TestPutGetJSONRoundTripKeepsBytes(t *testing.T) {
	f := setup(t)
	t.Setenv("AGENTSAGORA_URL", f.srv.URL)
	t.Setenv("AGENTSAGORA_TOKEN_FILE", filepath.Join(configDir(), "token-bob"))
	dir := t.TempDir()
	src := filepath.Join(dir, "cfg.json")
	// CRLF, trailing spaces and no final newline must survive unchanged
	want := []byte("{\r\n  \"a\" : 1,\r\n\t\"b\": \"x y\"  \r\n}")
	if err := os.WriteFile(src, want, 0o600); err != nil {
		t.Fatal(err)
	}
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := cmdPut([]string{"--channel", "general", src})
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	buf.ReadFrom(r)
	if err != nil {
		t.Fatal(err)
	}
	id := regexp.MustCompile(`id=([0-9a-f]{24})`).FindStringSubmatch(buf.String())
	if id == nil {
		t.Fatalf("put output %q", buf.String())
	}
	_, files := f.req(t, "GET", "/files?channel=general", f.tokens["bob"], nil)
	if !strings.Contains(fmt.Sprint(files), "application/json") {
		t.Errorf("stored mime: %v", files)
	}
	dst := filepath.Join(dir, "out.json")
	if err := cmdGet([]string{"--out", dst, id[1]}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, want) {
		t.Errorf("bytes changed: %q", got)
	}
}
