package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// String of syncBuf (ws_test.go) is the whole captured log.
func (s *syncBuf) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// fakeHook is a local TLS stand-in for the incoming webhook; it records the posted lines.
type fakeHook struct {
	srv    *httptest.Server
	mu     sync.Mutex
	lines  []string
	status int
	url    string // the "webhook URL" written to the file
}

const fakeHookPath = "/services/T0TEST/B0TEST/SECRETPATHxyz"

func newFakeHook(t *testing.T) *fakeHook {
	fs := &fakeHook{status: 200}
	fs.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Text string `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		fs.mu.Lock()
		defer fs.mu.Unlock()
		if r.Method != "POST" || r.URL.Path != fakeHookPath || r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(400)
			return
		}
		if fs.status == 200 {
			fs.lines = append(fs.lines, b.Text)
		}
		w.WriteHeader(fs.status)
	}))
	t.Cleanup(fs.srv.Close)
	fs.url = fs.srv.URL + fakeHookPath
	return fs
}

func (fs *fakeHook) got() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]string(nil), fs.lines...)
}

// liveFixture is a server with a clean liveness state, a webhook file pointing at a fake
// the webhook, a captured log, and the start well in the past (grace over).
func liveFixture(t *testing.T) (*fixture, *fakeHook, *syncBuf) {
	t.Helper()
	f := setup(t)
	fs := newFakeHook(t)
	if err := os.WriteFile(filepath.Join(configDir(), webhookFile), []byte(fs.url+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := &syncBuf{}
	wireLive(f.sv, fs, log)
	return f, fs, log
}

func wireLive(sv *server, fs *fakeHook, log *syncBuf) {
	sv.live.mu.Lock()
	defer sv.live.mu.Unlock()
	sv.live.log = log
	c := fs.srv.Client()
	c.Timeout = hookTimeout
	c.CheckRedirect = sv.live.client.CheckRedirect
	sv.live.client = c
	sv.live.started = time.Now().Add(-time.Hour)
	sv.live.m = map[string]*liveMember{} // drop contacts made by the fixture's own requests
}

func countWith(lines []string, sub string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func TestLiveAlertOnceAndRecovery(t *testing.T) {
	f, fs, log := liveFixture(t)
	if code, _ := send(t, f, "alice", "team", "dave", "secret plan for dave"); code != 200 {
		t.Fatal(code)
	}
	f.sv.live.mu.Lock()
	f.sv.live.m = map[string]*liveMember{}
	f.sv.live.mu.Unlock()
	t0 := time.Now()
	f.sv.live.touch("dave", t0)
	f.sv.liveTick(t0.Add(9 * time.Minute))
	if n := len(fs.got()); n != 0 {
		t.Fatalf("alert before 10 min: %v", fs.got())
	}
	f.sv.liveTick(t0.Add(10*time.Minute + time.Second))
	got := fs.got()
	want := "a2 liveness: dave no response — last seen " + t0.In(jst).Format("15:04") + ", undelivered 1"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("alert = %q, want %q", got, want)
	}
	for i := 2; i < 6; i++ { // still down: no repeat
		f.sv.liveTick(t0.Add(time.Duration(i) * 10 * time.Minute))
	}
	if n := countWith(fs.got(), "no response"); n != 1 {
		t.Fatalf("alerts per incident = %d, want 1: %v", n, fs.got())
	}
	back := t0.Add(61 * time.Minute)
	f.sv.live.touch("dave", back)
	f.sv.liveTick(back.Add(30 * time.Second))
	got = fs.got()
	if len(got) != 2 || got[1] != "a2 liveness: dave back "+back.In(jst).Format("15:04") {
		t.Fatalf("recovery = %q", got)
	}
	f.sv.liveTick(back.Add(2 * time.Minute))
	if len(fs.got()) != 2 {
		t.Fatalf("repeat after recovery: %q", fs.got())
	}
	for _, l := range fs.got() {
		if strings.Contains(l, "secret plan") {
			t.Fatalf("message text left the machine: %q", l)
		}
	}
	if strings.Contains(log.String(), "SECRETPATH") {
		t.Fatalf("URL in log: %s", log)
	}
}

func TestLiveNoAlertWhileWSOpen(t *testing.T) {
	f, fs, _ := liveFixture(t)
	t0 := time.Now()
	f.sv.live.touch("dave", t0)
	if !f.sv.ws.acquire("dave") {
		t.Fatal("acquire")
	}
	f.sv.liveTick(t0.Add(30 * time.Minute))
	f.sv.liveTick(t0.Add(60 * time.Minute))
	if len(fs.got()) != 0 {
		t.Fatalf("alert with an open /ws: %q", fs.got())
	}
	if lv := f.sv.liveOf(currentNames(), "dave", t0.Add(60*time.Minute)); !lv.alive {
		t.Fatal("ws open but not alive")
	}
	f.sv.ws.release("dave")
	// last contact is now the last tick with the socket open (t0+60m)
	f.sv.liveTick(t0.Add(69 * time.Minute))
	if len(fs.got()) != 0 {
		t.Fatalf("alert 9 min after the socket closed: %q", fs.got())
	}
	f.sv.liveTick(t0.Add(71 * time.Minute))
	if n := countWith(fs.got(), "dave no response"); n != 1 {
		t.Fatalf("no alert after socket gone: %q", fs.got())
	}
}

func TestLiveRestartKeepsIncident(t *testing.T) {
	f, fs, log := liveFixture(t)
	t0 := time.Now()
	f.sv.live.touch("bob", t0)
	f.sv.liveTick(t0.Add(11 * time.Minute))
	if countWith(fs.got(), "bob no response") != 1 {
		t.Fatalf("no alert: %q", fs.got())
	}
	st, err := os.Stat(filepath.Join(stateDir(), livenessFile))
	if err != nil || wrongMode(st, 0o600) {
		t.Fatalf("state file: %v %v", st, err)
	}
	restartFixture(t, f)
	wireLiveKeep(f.sv, fs, log)
	f.sv.liveTick(t0.Add(30 * time.Minute))
	if n := countWith(fs.got(), "bob no response"); n != 1 {
		t.Fatalf("restart re-alerted: %q", fs.got())
	}
	f.sv.live.touch("bob", t0.Add(31*time.Minute))
	f.sv.liveTick(t0.Add(32 * time.Minute))
	if countWith(fs.got(), "bob back") != 1 {
		t.Fatalf("restart forgot the incident (no recovery): %q", fs.got())
	}
}

// wireLiveKeep is wireLive without clearing the loaded state.
func wireLiveKeep(sv *server, fs *fakeHook, log *syncBuf) {
	m := sv.live.m
	wireLive(sv, fs, log)
	sv.live.m = m
}

func TestLiveStartupGrace(t *testing.T) {
	f, fs, _ := liveFixture(t)
	t0 := time.Now()
	f.sv.live.touch("carol", t0)
	f.sv.live.started = t0.Add(9 * time.Minute) // restart while carol was already quiet
	f.sv.liveTick(t0.Add(11 * time.Minute))     // 2 min after start
	f.sv.liveTick(t0.Add(11*time.Minute + 59*time.Second))
	if len(fs.got()) != 0 {
		t.Fatalf("alert inside the startup grace: %q", fs.got())
	}
	f.sv.liveTick(t0.Add(12*time.Minute + time.Second)) // grace over
	if countWith(fs.got(), "carol no response") != 1 {
		t.Fatalf("no alert after the grace: %q", fs.got())
	}
}

func TestLiveNeverSeenSkipped(t *testing.T) {
	f, fs, _ := liveFixture(t)
	t0 := time.Now()
	f.sv.live.mu.Lock()
	f.sv.live.m["carol"] = &liveMember{Last: t0.Add(-25 * time.Hour)} // last alive over 24h ago
	f.sv.live.mu.Unlock()
	f.sv.liveTick(t0.Add(time.Hour))
	f.sv.liveTick(t0.Add(2 * time.Hour))
	if len(fs.got()) != 0 {
		t.Fatalf("alert for a member never seen or not seen in 24h: %q", fs.got())
	}
	if lv := f.sv.liveOf(currentNames(), "frank", t0); lv.seen {
		t.Fatal("never-seen member shown as seen")
	}
}

func TestLiveWebhookFileChecks(t *testing.T) {
	f, fs, log := liveFixture(t)
	hook := filepath.Join(configDir(), webhookFile)
	t0 := time.Now()
	if unixModes { // a 0644 file is refused (no Unix modes on Windows)
		f.sv.live.touch("dave", t0)
		if err := os.Chmod(hook, 0o644); err != nil {
			t.Fatal(err)
		}
		f.sv.liveTick(t0.Add(11 * time.Minute))
		f.sv.live.touch("carol", t0)
		f.sv.live.touch("carol", t0.Add(12*time.Minute))
		f.sv.liveTick(t0.Add(23 * time.Minute))
		if len(fs.got()) != 0 {
			t.Fatalf("sent with a 0644 webhook file: %q", fs.got())
		}
		if n := strings.Count(log.String(), "liveness: webhook file not 0600, alerts off"); n != 1 {
			t.Fatalf("mode log %d times: %s", n, log)
		}
	}
	// missing file: feature off, one line
	os.Remove(hook)
	if u, why := readWebhook(hook); u != "" || !strings.Contains(why, "no alert-webhook file") {
		t.Fatalf("missing file: %q %q", u, why)
	}
	// symlink to a 0600 file: refused. Windows creates symlinks only with Developer Mode
	// or admin rights, so the check is skipped there when the link cannot be made.
	real := filepath.Join(t.TempDir(), "real")
	os.WriteFile(real, []byte(fs.url), 0o600)
	if err := os.Symlink(real, hook); err == nil {
		if u, why := readWebhook(hook); u != "" || !strings.Contains(why, "not a regular file") {
			t.Fatalf("symlink: %q %q", u, why)
		}
	} else if runtime.GOOS != "windows" {
		t.Fatal(err)
	}
	os.Remove(hook)
	// http (not https) URL: refused
	os.WriteFile(hook, []byte(strings.Replace(fs.url, "https://", "http://", 1)), 0o600)
	if u, why := readWebhook(hook); u != "" || !strings.Contains(why, "no https URL") {
		t.Fatalf("http URL: %q %q", u, why)
	}
	// good file: on, and the server never changed it
	os.WriteFile(hook, []byte(fs.url+"\n"), 0o600)
	if u, why := readWebhook(hook); u != fs.url || why != "" {
		t.Fatalf("good file refused: %q", why)
	}
	if strings.Contains(log.String(), "SECRETPATH") || strings.Contains(log.String(), fs.srv.URL) {
		t.Fatalf("URL in log: %s", log)
	}
}

func TestLiveFailureLogsNoURLAndRetriesOnce(t *testing.T) {
	f, fs, log := liveFixture(t)
	fs.mu.Lock()
	fs.status = 500
	fs.mu.Unlock()
	t0 := time.Now()
	f.sv.live.touch("dave", t0)
	f.sv.liveTick(t0.Add(11 * time.Minute))
	if !strings.Contains(log.String(), "webhook post failed (status 500), retry next minute") {
		t.Fatalf("log: %s", log)
	}
	fs.mu.Lock()
	fs.status = 200
	fs.mu.Unlock()
	f.sv.liveTick(t0.Add(12 * time.Minute)) // the retry goes out
	f.sv.liveTick(t0.Add(13 * time.Minute))
	if n := countWith(fs.got(), "dave no response"); n != 1 {
		t.Fatalf("retry: %q", fs.got())
	}
	// a dead endpoint: tried twice, then dropped
	f.sv.live.touch("carol", t0)
	f.sv.live.touch("carol", t0.Add(13*time.Minute))
	fs.srv.Close()
	f.sv.liveTick(t0.Add(24 * time.Minute))
	f.sv.liveTick(t0.Add(25 * time.Minute))
	f.sv.liveTick(t0.Add(26 * time.Minute))
	l := log.String()
	if !strings.Contains(l, "webhook post failed (request failed), retry next minute") || !strings.Contains(l, "webhook post failed (request failed), line dropped") {
		t.Fatalf("log: %s", l)
	}
	if strings.Count(l, "line dropped") != 1 {
		t.Fatalf("more than one retry: %s", l)
	}
	if strings.Contains(l, "SECRETPATH") || strings.Contains(l, fs.srv.URL) || strings.Contains(l, "127.0.0.1") {
		t.Fatalf("URL in log: %s", l)
	}
}

func TestLiveMembersShowsAlive(t *testing.T) {
	f, _, _ := liveFixture(t)
	code, out := f.req(t, "GET", "/members", f.tokens["dave"], nil)
	if code != 200 {
		t.Fatal(code)
	}
	seen := map[string]any{}
	for _, m := range out["members"].([]any) {
		mm := m.(map[string]any)
		seen[mm["name"].(string)] = mm["alive"]
		if mm["name"] == "dave" && mm["last_seen"] == nil {
			t.Fatalf("dave has no last_seen: %v", mm)
		}
	}
	if seen["dave"] != true {
		t.Fatalf("dave (the caller) not alive: %v", seen)
	}
	if seen["carol"] != nil {
		t.Fatalf("never-seen carol has an alive field: %v", seen)
	}
}
