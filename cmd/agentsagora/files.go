package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxFileBytes  = 20 << 20 // 20 MB per file
	maxTotalBytes = 2 << 30  // 2 GB for all files; the oldest go first
	maxNameBytes  = 128
)

// FileMeta is the stored description of one shared file (<id>.json next to <id>.bin).
type FileMeta struct {
	ID      string `json:"id"`
	TS      string `json:"ts"`
	From    string `json:"from"`
	Channel string `json:"channel"`
	Name    string `json:"name"` // display only; never used as a path
	MIME    string `json:"mime"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

// httpErr carries an HTTP status for an error that goes back to the caller as is.
type httpErr struct {
	code int
	msg  string
}

func (e *httpErr) Error() string { return e.msg }

func errCode(code int, format string, a ...any) error {
	return &httpErr{code: code, msg: fmt.Sprintf(format, a...)}
}

// 96-bit random ids: 24 hex characters, short enough not to look like a token to the general lint.
var fileIDRe = regexp.MustCompile(`^[0-9a-f]{24}$`)

// allowedMIME maps each allowed type to its magic-byte check.
var allowedMIME = map[string]func([]byte) bool{
	"image/png":  func(b []byte) bool { return bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) },
	"image/jpeg": func(b []byte) bool { return bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}) },
	"image/gif": func(b []byte) bool {
		return bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a"))
	},
	"image/webp":       func(b []byte) bool { return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP" },
	"application/pdf":  func(b []byte) bool { return bytes.HasPrefix(b, []byte("%PDF-")) },
	"text/plain":       isText,
	"text/markdown":    isText,
	"application/json": isText,
}

func isText(b []byte) bool { return utf8.Valid(b) && bytes.IndexByte(b, 0) < 0 }

func isTextMIME(m string) bool { return strings.HasPrefix(m, "text/") }

// checkMIME returns the bare media type if it is allowed and the content matches it.
func checkMIME(m string, data []byte) (string, error) {
	base, _, err := mime.ParseMediaType(m)
	if err != nil {
		return "", errCode(http.StatusUnsupportedMediaType, "bad mime %q", m)
	}
	check, ok := allowedMIME[base]
	if !ok {
		return "", errCode(http.StatusUnsupportedMediaType, "mime %s not allowed (allowed: png, jpeg, webp, gif, pdf, text/plain, text/markdown, application/json)", base)
	}
	if !check(data) {
		return "", errCode(http.StatusUnsupportedMediaType, "content does not match mime %s", base)
	}
	return base, nil
}

// checkName accepts a plain display name: no path separators, no control characters.
func checkName(name string) error {
	if name == "" || len(name) > maxNameBytes || !utf8.ValidString(name) || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") ||
		strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) }) {
		return errCode(http.StatusBadRequest, "bad file name (1-%d bytes, no / \\ .. or control characters)", maxNameBytes)
	}
	return nil
}

type fileStore struct {
	dir      string
	mu       sync.Mutex
	files    []FileMeta // oldest first
	total    int64
	maxFile  int64
	maxTotal int64
}

func openFileStore(dir string) (*fileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fs := &fileStore{dir: dir, maxFile: maxFileBytes, maxTotal: maxTotalBytes}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		n := e.Name()
		if strings.HasSuffix(n, ".tmp") || strings.HasSuffix(n, ".new") {
			os.Remove(filepath.Join(dir, n)) // left over from a crash
			continue
		}
		id, ok := strings.CutSuffix(n, ".json")
		if !ok || !fileIDRe.MatchString(id) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		var m FileMeta
		if json.Unmarshal(b, &m) != nil || m.ID != id {
			continue
		}
		st, err := os.Stat(filepath.Join(dir, id+".bin"))
		if err != nil || st.Size() != m.Size {
			continue
		}
		fs.files = append(fs.files, m)
		fs.total += m.Size
	}
	slices.SortFunc(fs.files, func(a, b FileMeta) int { return strings.Compare(a.TS+a.ID, b.TS+b.ID) })
	return fs, nil
}

func (fs *fileStore) paths(id string) (bin, meta string) {
	return filepath.Join(fs.dir, id+".bin"), filepath.Join(fs.dir, id+".json")
}

// put validates and stores a file. The id is random; nothing from the caller goes into a path.
func (fs *fileStore) put(from, channel, name, mimeType string, data []byte, now time.Time) (FileMeta, error) {
	if err := checkName(name); err != nil {
		return FileMeta{}, err
	}
	if int64(len(data)) > fs.maxFile {
		return FileMeta{}, errCode(http.StatusRequestEntityTooLarge, "file over %d bytes", fs.maxFile)
	}
	if len(data) == 0 {
		return FileMeta{}, errCode(http.StatusBadRequest, "empty file")
	}
	base, err := checkMIME(mimeType, data)
	if err != nil {
		return FileMeta{}, err
	}
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return FileMeta{}, err
	}
	sum := sha256.Sum256(data)
	m := FileMeta{ID: hex.EncodeToString(raw), TS: now.In(jst).Format(time.RFC3339Nano), From: from, Channel: channel,
		Name: name, MIME: base, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for len(fs.files) > 0 && fs.total+m.Size > fs.maxTotal {
		old := fs.files[0]
		b, mp := fs.paths(old.ID)
		os.Remove(b)
		os.Remove(mp)
		fs.files = fs.files[1:]
		fs.total -= old.Size
	}
	bin, meta := fs.paths(m.ID)
	tmp := bin + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return FileMeta{}, err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, bin)
	}
	if werr != nil {
		os.Remove(tmp)
		return FileMeta{}, werr
	}
	mb, _ := json.Marshal(m)
	if err := writeFile0600(meta, append(mb, '\n')); err != nil {
		os.Remove(bin)
		return FileMeta{}, err
	}
	fs.files = append(fs.files, m)
	fs.total += m.Size
	return m, nil
}

func (fs *fileStore) meta(id string) (FileMeta, bool) {
	if !fileIDRe.MatchString(id) {
		return FileMeta{}, false
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, m := range fs.files {
		if m.ID == id {
			return m, true
		}
	}
	return FileMeta{}, false
}

func (fs *fileStore) read(id string) ([]byte, error) {
	if !fileIDRe.MatchString(id) {
		return nil, errors.New("bad id")
	}
	bin, _ := fs.paths(id)
	return os.ReadFile(bin)
}

// list returns the files in the given channels, newest first.
func (fs *fileStore) list(channels []string) []FileMeta {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := []FileMeta{}
	for i := len(fs.files) - 1; i >= 0; i-- {
		if slices.Contains(channels, fs.files[i].Channel) {
			out = append(out, fs.files[i])
		}
	}
	return out
}
