package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The general channel is for non-work talk. The server rejects general messages, file names
// and text file contents that carry work markers. The markers are in a config file
// (default: general-deny.txt in the config dir; see examples/general-deny.txt), one per line, matched case-insensitively:
//
//	word            a whole word or phrase (word boundaries where the entry starts/ends
//	                with a letter or digit), e.g. "acme", "git.acme.example", "notes/"
//	re:REGEX        a Go RE2 regular expression
//	## Section      starts a section; its name is the class shown in the error
//	# comment       ignored
//
// The server does not start without the file (fail closed) and re-reads it when it changes.

const lintMsg = "work content goes to team"

type denyRule struct {
	re    *regexp.Regexp
	class string
	line  int
}

type denyList struct {
	path  string
	mu    sync.Mutex
	mtime time.Time
	size  int64
	rules []denyRule
}

func parseDeny(data string) ([]denyRule, error) {
	var rules []denyRule
	class := "work marker"
	sc := bufio.NewScanner(strings.NewReader(data))
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "##"):
			class = strings.TrimSpace(strings.TrimLeft(line, "#"))
			continue
		case strings.HasPrefix(line, "#"):
			continue
		}
		var expr string
		if r, ok := strings.CutPrefix(line, "re:"); ok {
			expr = r
		} else {
			expr = regexp.QuoteMeta(line)
			if isWordByte(line[0]) {
				expr = `\b` + expr
			}
			if isWordByte(line[len(line)-1]) {
				expr += `\b`
			}
		}
		re, err := regexp.Compile("(?i)" + expr)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		rules = append(rules, denyRule{re: re, class: class, line: n})
	}
	if len(rules) == 0 {
		return nil, errors.New("no rules")
	}
	return rules, sc.Err()
}

func isWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func loadDenyList(path string) (*denyList, error) {
	d := &denyList{path: path}
	if err := d.reload(); err != nil {
		return nil, fmt.Errorf("general deny list %s: %w", path, err)
	}
	return d, nil
}

// reload re-reads the file when its mtime or size changed. On an error the last good
// rules stay in force.
func (d *denyList) reload() error {
	st, err := os.Stat(d.path)
	if err != nil {
		return err
	}
	if d.rules != nil && st.ModTime().Equal(d.mtime) && st.Size() == d.size {
		return nil
	}
	b, err := os.ReadFile(d.path)
	if err != nil {
		return err
	}
	rules, err := parseDeny(string(b))
	if err != nil {
		return err
	}
	d.rules, d.mtime, d.size = rules, st.ModTime(), st.Size()
	return nil
}

// check returns an error naming the marker class when text has a work marker.
// The error never repeats the matched text (it may be a secret).
func (d *denyList) check(texts ...string) error {
	d.mu.Lock()
	if err := d.reload(); err != nil {
		fmt.Fprintln(os.Stderr, "a2: deny list reload (old rules kept):", err)
	}
	rules := d.rules
	d.mu.Unlock()
	for _, t := range texts {
		for _, r := range rules {
			if r.re.MatchString(t) {
				return fmt.Errorf("%s (general channel rule: %s, general-deny.txt line %d)", lintMsg, r.class, r.line)
			}
		}
	}
	return nil
}
