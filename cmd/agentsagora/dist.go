package main

// Build identity and binary distribution.
//
// The build id of a binary is the first 12 hex of the sha256 of the file itself, and its
// build time is the file's mtime; no linker flags are needed, so every build path
// (go build or a cross-compile) gives a comparable id. `a2 version` prints it.
//
// The server publishes the cross-built clients from its own bin dir (--dist-dir, default:
// the dir of the server binary): GET /dist/manifest and GET /dist/<os>-<arch> for the fixed
// list in distTargets, member token required. No other file is ever served.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"time"
)

// distTargets maps /dist/<target> to the file name in the dist dir. Fixed list.
var distTargets = map[string]string{
	"darwin-arm64":  "agentsagora-darwin-arm64",
	"darwin-amd64":  "agentsagora-darwin-amd64",
	"windows-amd64": "agentsagora-windows-amd64.exe",
	"linux-amd64":   "agentsagora",
}

var distOrder = []string{"darwin-arm64", "darwin-amd64", "windows-amd64", "linux-amd64"}

func osArch() string { return runtime.GOOS + "-" + runtime.GOARCH }

// distFile is one entry of the manifest.
type distFile struct {
	Build  string `json:"build"` // first 12 hex of SHA256
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Built  string `json:"built"` // file mtime, JST RFC3339
	Path   string `json:"path"`  // /dist/<target>
}

// buildInfo describes the running binary.
type buildInfo struct {
	App     string `json:"app"`
	Version string `json:"version"` // source version label (appVersion)
	Commit  string `json:"commit,omitempty"`
	Build   string `json:"build"`
	Built   string `json:"built"`
	OSArch  string `json:"os_arch"`
	Started string `json:"started,omitempty"`
}

type manifest struct {
	App     string              `json:"app"`
	Version string              `json:"version"` // <linux build>@<linux built>
	Server  buildInfo           `json:"server"`  // the running server
	Files   map[string]distFile `json:"files"`
}

func shortBuild(sha string) string {
	if len(sha) >= 12 {
		return sha[:12]
	}
	return sha
}

// hashFile returns the sha256 hex and size of a regular file.
func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// selfPath is the real path of the running binary (symlinks such as bin/a2 resolved).
func selfPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Abs(p)
}

var self struct {
	once sync.Once
	info buildInfo
	sha  string
	path string
}

// selfInfo hashes the running binary once (at the first call; the server calls it at start).
func selfInfo() (buildInfo, string, string) {
	self.once.Do(func() {
		bi := buildInfo{App: appName, Version: appVersion, OSArch: osArch()}
		if info, ok := debug.ReadBuildInfo(); ok {
			mod := false
			for _, s := range info.Settings {
				switch s.Key {
				case "vcs.revision":
					bi.Commit = shortBuild(s.Value)
				case "vcs.modified":
					mod = s.Value == "true"
				}
			}
			if bi.Commit != "" && mod {
				bi.Commit += "+dirty"
			}
		}
		if p, err := selfPath(); err == nil {
			self.path = p
			if st, err := os.Stat(p); err == nil {
				bi.Built = st.ModTime().In(jst).Format(time.RFC3339)
			}
			if sha, _, err := hashFile(p); err == nil {
				self.sha = sha
				bi.Build = shortBuild(sha)
			}
		}
		self.info = bi
	})
	return self.info, self.sha, self.path
}

func versionLine(bi buildInfo) string {
	c := bi.Commit
	if c == "" {
		c = "-"
	}
	return fmt.Sprintf("%s %s commit %s build %s built %s %s", bi.App, bi.Version, c, bi.Build, bi.Built, bi.OSArch)
}

// cmdVersion: a2 version. The server's self-update runs this on a new binary as a smoke
// test, so it must stay offline and quick.
func cmdVersion(args []string) error {
	if len(args) != 0 {
		return usageError("version takes no arguments")
	}
	bi, _, _ := selfInfo()
	fmt.Println(versionLine(bi))
	return nil
}

// distCache keeps the hash of each dist file until its size or mtime changes.
type distCache struct {
	dir string
	mu  sync.Mutex
	ent map[string]distCacheEnt
}

type distCacheEnt struct {
	size  int64
	mtime time.Time
	f     distFile
}

func newDistCache(dir string) *distCache { return &distCache{dir: dir, ent: map[string]distCacheEnt{}} }

// file returns the manifest entry of a target (ok=false when it is not built).
func (dc *distCache) file(target string) (distFile, bool) {
	name, known := distTargets[target]
	if !known || dc == nil || dc.dir == "" {
		return distFile{}, false
	}
	p := filepath.Join(dc.dir, name)
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return distFile{}, false
	}
	dc.mu.Lock()
	defer dc.mu.Unlock()
	if e, ok := dc.ent[target]; ok && e.size == st.Size() && e.mtime.Equal(st.ModTime()) {
		return e.f, true
	}
	sha, n, err := hashFile(p)
	if err != nil || n != st.Size() {
		return distFile{}, false
	}
	f := distFile{Build: shortBuild(sha), SHA256: sha, Size: n, Built: st.ModTime().In(jst).Format(time.RFC3339), Path: "/dist/" + target}
	dc.ent[target] = distCacheEnt{size: st.Size(), mtime: st.ModTime(), f: f}
	return f, true
}

func (sv *server) manifest() manifest {
	m := manifest{App: appName, Server: sv.build, Files: map[string]distFile{}}
	for _, t := range distOrder {
		if f, ok := sv.dist.file(t); ok {
			m.Files[t] = f
		}
	}
	if f, ok := m.Files["linux-amd64"]; ok {
		m.Version = f.Build + "@" + f.Built
	}
	return m
}

func (sv *server) handleManifest(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, sv.manifest())
}

func (sv *server) handleDist(w http.ResponseWriter, r *http.Request) {
	member, target := callerOf(r), r.PathValue("target")
	name, known := distTargets[target]
	if !known {
		sv.audit.write(auditEntry{Member: member, Remote: remoteIP(r), Action: "dist-get", Status: 404, Target: "(unknown)"})
		jsonError(w, http.StatusNotFound, "unknown target (darwin-arm64, darwin-amd64, windows-amd64, linux-amd64)")
		return
	}
	df, ok := sv.dist.file(target)
	var f *os.File
	var err error
	if ok {
		f, err = os.Open(filepath.Join(sv.dist.dir, name))
	}
	if !ok || err != nil {
		sv.audit.write(auditEntry{Member: member, Remote: remoteIP(r), Action: "dist-get", Status: 404, Target: target})
		jsonError(w, http.StatusNotFound, "not built on the server")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		jsonError(w, http.StatusNotFound, "not built on the server")
		return
	}
	sv.audit.write(auditEntry{Member: member, Remote: remoteIP(r), Action: "dist-get", Size: st.Size(), Status: 200, Target: target})
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	h.Set("X-AgentsAgora-SHA256", df.SHA256) // informative; the client checks the manifest value
	w.WriteHeader(http.StatusOK)
	io.Copy(w, f)
}

func (sv *server) distRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /dist/manifest", sv.auth(sv.handleManifest))
	mux.HandleFunc("GET /dist/{target}", sv.auth(sv.handleDist))
	mux.HandleFunc("/dist/", sv.auth(func(w http.ResponseWriter, r *http.Request) {
		jsonError(w, http.StatusNotFound, "not found")
	}))
}
