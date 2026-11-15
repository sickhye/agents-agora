package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

func hashToken(tok string) string {
	s := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(s[:])
}

// readHashes reads tokens.json: {"member": "sha256 hex"}. A missing file is an empty map.
func readHashes(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// writeFile0600 writes data to path.new in the same dir with mode 0600 and renames it.
func writeFile0600(path string, data []byte) error {
	tmp := path + ".new"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func writeHashes(hashes map[string]string) error {
	b, _ := json.MarshalIndent(hashes, "", "  ")
	return writeFile0600(tokensPath(), append(b, '\n'))
}

// cmdToken: token new|rotate|revoke.
func cmdToken(args []string) error {
	if len(args) == 0 {
		return usageError("token: new, rotate, revoke or rename")
	}
	sub := args[0]
	switch sub {
	case "new", "rotate":
	case "revoke":
		return cmdTokenRevoke(args[1:])
	case "rename":
		return cmdTokenRename(args[1:])
	default:
		return usageError("token: new, rotate, revoke or rename")
	}
	fs := flag.NewFlagSet("token "+sub, flag.ContinueOnError)
	out := fs.String("out", "", "token file path (default ~/.config/agentsagora/token-<member>)")
	force := fs.Bool("force", false, "replace an existing token file and hash")
	membersFlag := fs.String("members", "", "allowed members, comma separated (default: the whole member table)")
	if err := fs.Parse(args[1:]); err != nil {
		return usageError(err.Error())
	}
	if fs.NArg() != 1 {
		return usageError("token " + sub + " needs exactly one member")
	}
	member := fs.Arg(0)
	if member == systemSender { // the server's notice sender (sendcheck.go), never a token
		return usageError(member + " is reserved for server notices")
	}
	if member != viewerName {
		nt := currentNames()
		if base, ok := nt.baseOf(member); !ok || !slices.Contains(parseMembers(*membersFlag), base) {
			if cur := nt.canon(member); cur != member {
				return usageError(member + " was renamed to " + cur)
			}
			return usageError("unknown member " + member)
		}
	}
	rotate := sub == "rotate"
	if rotate {
		h, err := readHashes(tokensPath())
		if err != nil {
			return err
		}
		if _, ok := h[member]; !ok {
			return fmt.Errorf("member %s has no token to rotate; use token new", member)
		}
	}
	path, err := tokenNew(member, *out, *force || rotate)
	if err != nil {
		return err
	}
	auditLocal("token-"+sub, member)
	fmt.Println(path)
	return nil
}

// tokenNew makes a token for member, writes it to a 0600 file and stores its hash.
// It returns the token file path. The token itself never goes to stdout.
func tokenNew(member, out string, force bool) (string, error) {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return "", err
	}
	if out == "" {
		out = filepath.Join(configDir(), "token-"+member)
	}
	out = expandHome(out)
	hashes, err := readHashes(tokensPath())
	if err != nil {
		return "", err
	}
	if _, ok := hashes[member]; ok && !force {
		return "", fmt.Errorf("member %s already has a token; --force replaces it (the old token stops working)", member)
	}
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	tok := "sc_" + hex.EncodeToString(raw)
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(out, flags, 0o600)
	if err != nil {
		return "", err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return "", err
	}
	if _, err := f.WriteString(tok + "\n"); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	hashes[member] = hashToken(tok)
	if err := writeHashes(hashes); err != nil {
		return "", err
	}
	if member == viewerName && force { // a replaced viewer token ends every saved viewer session
		if err := clearViewSessions(); err != nil {
			return "", err
		}
	}
	return out, nil
}

// cmdTokenRevoke removes a member's token hash (the token stops working at once) and deletes
// the default token file if it is still there.
func cmdTokenRevoke(args []string) error {
	if len(args) != 1 {
		return usageError("token revoke needs exactly one member")
	}
	member := args[0]
	if member != viewerName && currentNames().channels(member) == nil {
		return usageError("unknown member " + member)
	}
	if err := tokenRevoke(member); err != nil {
		return err
	}
	auditLocal("token-revoke", member)
	fmt.Println("revoked", member)
	return nil
}

func tokenRevoke(member string) error {
	hashes, err := readHashes(tokensPath())
	if err != nil {
		return err
	}
	if _, ok := hashes[member]; !ok {
		return fmt.Errorf("member %s has no token", member)
	}
	delete(hashes, member)
	if err := writeHashes(hashes); err != nil {
		return err
	}
	if member == viewerName { // a revoked viewer token ends every saved viewer session
		if err := clearViewSessions(); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(configDir(), "token-"+member)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// tokenStore maps members to token hashes and reloads tokens.json when it changes,
// so a new, rotated or revoked token takes effect without a server restart.
type tokenStore struct {
	path    string
	members []string
	mu      sync.Mutex
	mtime   time.Time
	size    int64
	hashes  map[string][]byte // member -> sha256 of the token
	viewer  []byte            // sha256 of the viewer token; never in hashes
	renames int               // renames seen at the last load (names.go)
}

func newTokenStore(path string, members []string) (*tokenStore, error) {
	ts := &tokenStore{path: path, members: members}
	if err := ts.reload(); err != nil {
		return nil, err
	}
	return ts, nil
}

func (ts *tokenStore) reload() error {
	st, err := os.Stat(ts.path)
	if errors.Is(err, os.ErrNotExist) {
		ts.hashes, ts.viewer = map[string][]byte{}, nil
		ts.mtime, ts.size = time.Time{}, 0
		return nil
	}
	if err != nil {
		return err
	}
	if ts.hashes != nil && st.ModTime().Equal(ts.mtime) && st.Size() == ts.size && len(currentNames().Renames) == ts.renames {
		return nil
	}
	m, err := readHashes(ts.path)
	if err != nil {
		return err
	}
	hs := map[string][]byte{}
	var viewer []byte
	nt := currentNames()
	for mem, h := range m {
		raw, err := hex.DecodeString(h)
		if err != nil || len(raw) != sha256.Size {
			continue
		}
		if mem == viewerName {
			viewer = raw
			continue
		}
		// keys are current names; a key still under an old name (rename in progress) maps
		// to the current one.
		cur := nt.canon(mem)
		if base, ok := nt.baseOf(cur); ok && slices.Contains(ts.members, base) {
			if _, dup := hs[cur]; !dup || mem == cur {
				hs[cur] = raw
			}
		}
	}
	ts.hashes, ts.viewer, ts.mtime, ts.size, ts.renames = hs, viewer, st.ModTime(), st.Size(), len(nt.Renames)
	return nil
}

// viewerHash returns the current viewer token hash, or nil when viewer has no token. Sessions keep
// the hash they logged in with, so a rotated or revoked viewer token ends them.
func (ts *tokenStore) viewerHash() []byte {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if err := ts.reload(); err != nil {
		fmt.Fprintln(os.Stderr, "a2: token reload:", err)
	}
	return ts.viewer
}

// isViewer reports (in constant time) whether tok is the viewer token. Member tokens never match.
func (ts *tokenStore) isViewer(tok string) bool {
	h := ts.viewerHash()
	if tok == "" || h == nil {
		return false
	}
	sum := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare(sum[:], h) == 1
}

// member returns the member for a bearer token, or "" if the token is unknown.
// It compares against every stored hash in constant time, without an early exit.
func (ts *tokenStore) member(tok string) string {
	if tok == "" {
		return ""
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if err := ts.reload(); err != nil {
		fmt.Fprintln(os.Stderr, "a2: token reload:", err)
	}
	sum := sha256.Sum256([]byte(tok))
	found := ""
	for mem, h := range ts.hashes {
		if subtle.ConstantTimeCompare(sum[:], h) == 1 {
			found = mem
		}
	}
	return found
}

func (ts *tokenStore) hasToken() map[string]bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	_ = ts.reload()
	out := map[string]bool{}
	for m := range ts.hashes {
		out[m] = true
	}
	return out
}
