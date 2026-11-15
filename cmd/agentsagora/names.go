package main

// Member names and renames.
//
// memberChannels (paths.go) keeps its fixed keys ("base names"); they never change in the
// code. A rename only writes member-names.json in the config dir:
//
//	{"names":   {"<base>": "<current name>"},      // only renamed members
//	 "aliases": {"<old name>": "<current name>"},  // every earlier name
//	 "renames": [{"old": "...", "new": "...", "at": "...", "by": "..."}]}
//
// Everything that compares names (auth, recipients, inbox, members, view) works on current
// names; stored messages keep the name they were written with and are shown with the
// current name (nameTable.canon). A message sent to an old name is delivered to the member
// that now has it, and the sender gets a one-line notice. The token hash moves to the new
// key in tokens.json, so the same token (and every token file) keeps working.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"
)

const (
	namesFile     = "member-names.json"
	maxMemberName = 40
)

var memberNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

type renameRec struct {
	Old string `json:"old"`
	New string `json:"new"`
	At  string `json:"at"`
	By  string `json:"by"`
}

// nameTable is an immutable snapshot; take one per request with currentNames().
type nameTable struct {
	Names   map[string]string `json:"names"`
	Aliases map[string]string `json:"aliases"`
	Renames []renameRec       `json:"renames,omitempty"`
}

func namesPath() string { return filepath.Join(configDir(), namesFile) }

var namesCache struct {
	mu    sync.Mutex
	path  string
	mtime time.Time
	size  int64
	ok    bool
	nt    nameTable
}

func emptyNames() nameTable {
	return nameTable{Names: map[string]string{}, Aliases: map[string]string{}}
}

func readNames(path string) (nameTable, error) {
	nt := emptyNames()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nt, nil
	}
	if err != nil {
		return nt, err
	}
	if err := json.Unmarshal(b, &nt); err != nil {
		return emptyNames(), fmt.Errorf("%s: %w", path, err)
	}
	if nt.Names == nil {
		nt.Names = map[string]string{}
	}
	if nt.Aliases == nil {
		nt.Aliases = map[string]string{}
	}
	return nt, nil
}

// currentNames returns the name table, re-read when the file changed (a rename made on the
// command line takes effect in a running server). A broken file keeps the last good table.
func currentNames() nameTable {
	p := namesPath()
	c := &namesCache
	c.mu.Lock()
	defer c.mu.Unlock()
	st, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		c.path, c.ok, c.nt = p, true, emptyNames()
		c.mtime, c.size = time.Time{}, 0
		return c.nt
	}
	if err == nil && c.ok && c.path == p && st.ModTime().Equal(c.mtime) && st.Size() == c.size {
		return c.nt
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "a2: names:", err)
		return c.nt
	}
	nt, err := readNames(p)
	if err != nil {
		fmt.Fprintln(os.Stderr, "a2: names:", err)
		if c.path != p {
			return emptyNames()
		}
		return c.nt
	}
	c.path, c.ok, c.nt, c.mtime, c.size = p, true, nt, st.ModTime(), st.Size()
	return nt
}

// current is the current name of a base member.
func (nt nameTable) current(base string) string {
	if n, ok := nt.Names[base]; ok {
		return n
	}
	return base
}

// baseOf returns the base key of a current member name.
func (nt nameTable) baseOf(name string) (string, bool) {
	for b, n := range nt.Names {
		if n == name {
			return b, true
		}
	}
	if _, ok := memberChannels[name]; ok {
		if _, renamed := nt.Names[name]; !renamed {
			return name, true
		}
	}
	return "", false
}

// canon maps an old name to the current one; other names are returned unchanged.
func (nt nameTable) canon(name string) string {
	if n, ok := nt.Aliases[name]; ok {
		return n
	}
	return name
}

// channels returns the channels of a current member name (nil for old names and non-members).
func (nt nameTable) channels(name string) []string {
	b, ok := nt.baseOf(name)
	if !ok {
		return nil
	}
	chs := memberChannels[b]
	if slices.Contains(generalOnlyMembers, b) {
		return slices.DeleteFunc(slices.Clone(chs), func(c string) bool { return !isPublic(c) })
	}
	return chs
}

// showMsg returns m with from and to shown under the current names.
func (nt nameTable) showMsg(m Message) Message {
	m.From, m.To = nt.canon(m.From), nt.canon(m.To)
	return m
}

func checkMemberName(n string) error {
	if len(n) == 0 || len(n) > maxMemberName || !memberNameRe.MatchString(n) {
		return fmt.Errorf("bad member name %q: lowercase letters, digits and single hyphens, like alice-admin, at most %d characters", n, maxMemberName)
	}
	if n == allTarget || n == viewerName || n == systemSender {
		return fmt.Errorf("member name %q is reserved", n)
	}
	return nil
}

var renameMu sync.Mutex // one rename at a time inside a process

// renameMember renames the current member old to new: name table first, then the token
// hash in tokens.json (the same token keeps working). by is "local-cli" or "viewer".
func renameMember(old, new, by string, now time.Time) error {
	if err := checkMemberName(new); err != nil {
		return errCode(400, "%s", err.Error())
	}
	renameMu.Lock()
	defer renameMu.Unlock()
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	nt, err := readNames(namesPath())
	if err != nil {
		return err
	}
	base, ok := nt.baseOf(old)
	if !ok {
		if cur, isOld := nt.Aliases[old]; isOld {
			return errCode(400, "%s was renamed to %s; rename %s", old, cur, cur)
		}
		return errCode(400, "unknown member %s", old)
	}
	if slices.Contains(generalOnlyMembers, base) {
		return errCode(400, "%s has a fixed name (general-only relay); it cannot be renamed", old)
	}
	if old == new {
		return errCode(400, "%s already has that name", old)
	}
	if _, taken := nt.baseOf(new); taken {
		return errCode(409, "member name %s is in use", new)
	}
	if cur, isOld := nt.Aliases[new]; isOld && cur != old {
		return errCode(409, "member name %s is an old name of %s", new, cur)
	}
	if _, isBase := memberChannels[new]; isBase && new != base {
		return errCode(409, "member name %s is reserved for another member", new)
	}
	// every old name of this member (and old itself) now points to new; new is no alias.
	for a, cur := range nt.Aliases {
		if cur == old {
			nt.Aliases[a] = new
		}
	}
	nt.Aliases[old] = new
	delete(nt.Aliases, new)
	if new == base {
		delete(nt.Names, base)
	} else {
		nt.Names[base] = new
	}
	nt.Renames = append(nt.Renames, renameRec{Old: old, New: new, At: now.In(jst).Format(time.RFC3339), By: by})
	b, _ := json.MarshalIndent(nt, "", "  ")
	if err := writeFile0600(namesPath(), append(b, '\n')); err != nil {
		return err
	}
	hashes, err := readHashes(tokensPath())
	if err != nil {
		return err
	}
	if h, ok := hashes[old]; ok {
		delete(hashes, old)
		hashes[new] = h
		if err := writeHashes(hashes); err != nil {
			return err
		}
	}
	return nil
}

// cmdTokenRename: a2 token rename <old> <new> (on the server machine; the running server
// picks it up without a restart).
func cmdTokenRename(args []string) error {
	if len(args) != 2 {
		return usageError("token rename needs <old> <new>")
	}
	old, new := args[0], args[1]
	if old == viewerName {
		return usageError("viewer cannot be renamed")
	}
	if err := renameMember(old, new, "local-cli", time.Now()); err != nil {
		return err
	}
	auditLocal("member-rename", new, old+"->"+new)
	fmt.Printf("renamed %s -> %s (same token; old name still delivers with a notice)\n", old, new)
	return nil
}

// memberNames is the list of current member names of the server, in table order.
func (sv *server) memberNames(nt nameTable) []string {
	out := make([]string, 0, len(sv.members))
	for _, b := range sv.members {
		out = append(out, nt.current(b))
	}
	return out
}

// isMember reports whether name is a current member name of the server.
func (sv *server) isMember(nt nameTable, name string) bool {
	b, ok := nt.baseOf(name)
	return ok && slices.Contains(sv.members, b)
}
