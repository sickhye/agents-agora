package main

// Message tags.
//
// A message may carry up to maxTags tags (lowercase [a-z0-9-], 1 to maxTagLen characters),
// set at send time (a2 send --tag, the MCP send tool "tags") and stored with the message
// as "tags" (omitted when empty, so older stores and clients read as before). They add no
// rights: a member sees the tags only on messages it can already read. Filters (a2 inbox
// --tag, a2 watch --tag, the MCP inbox "tags", the view ?tag=) take a list and match a
// message that has any of them. GET /tags counts the tags on the messages the caller can
// read.

import (
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxTags   = 5
	maxTagLen = 24
)

func validTag(t string) bool {
	if t == "" || len(t) > maxTagLen {
		return false
	}
	for _, c := range t {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// normalizeTags checks tags for a new message: every tag valid, at most maxTags after
// dropping repeats. Nothing is changed or guessed: one bad tag refuses the whole list.
func normalizeTags(in []string) ([]string, error) {
	var out []string
	for _, t := range in {
		if !validTag(t) {
			return nil, errCode(http.StatusBadRequest, "bad tag %s: use lowercase a-z, 0-9 and -, 1 to %d characters", strconv.Quote(t), maxTagLen)
		}
		dup := false
		for _, o := range out {
			dup = dup || o == t
		}
		if !dup {
			out = append(out, t)
		}
	}
	if len(out) > maxTags {
		return nil, errCode(http.StatusBadRequest, "too many tags (%d): at most %d per message", len(out), maxTags)
	}
	return out, nil
}

// splitTags splits a comma separated tag list (as typed after --tag or in ?tag=); an
// empty string is no tags, an empty element ("a,,b") stays and is refused by validation.
func splitTags(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// parseTagFilter checks a filter list; it holds one or more valid tags (any of them matches).
func parseTagFilter(in []string) ([]string, error) {
	for _, t := range in {
		if !validTag(t) {
			return nil, errCode(http.StatusBadRequest, "tag: bad tag %s", strconv.Quote(t))
		}
	}
	return in, nil
}

// hasAnyTag is true when m carries one of tags (no tags asked = every message matches).
func hasAnyTag(m Message, tags []string) bool {
	if len(tags) == 0 {
		return true
	}
	for _, t := range tags {
		for _, mt := range m.Tags {
			if t == mt {
				return true
			}
		}
	}
	return false
}

// cleanTags drops tags that would not pass validation (a hand edited store line), so
// nothing odd reaches a client.
func cleanTags(in []string) []string {
	var out []string
	for _, t := range in {
		if validTag(t) && len(out) < maxTags {
			out = append(out, t)
		}
	}
	return out
}

// tagSuffix is " [a,b]" for a printed line ("" without tags).
func tagSuffix(m Message) string {
	if len(m.Tags) == 0 {
		return ""
	}
	return " [" + strings.Join(m.Tags, ",") + "]"
}

type tagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// readable lists the messages member can read in channels after since: those to it or to
// all, and its own sends (the union of inbox and sent).
func (s *store) readable(member string, since int64, channels []string, nt nameTable) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Message
	for _, m := range s.msgs {
		if m.ID <= since || !hasChannel(channels, m.Channel) {
			continue
		}
		if m = nt.showMsg(m); m.To == member || m.To == allTarget || m.From == member {
			out = append(out, m)
		}
	}
	return out
}

func hasChannel(chs []string, c string) bool {
	for _, x := range chs {
		if x == c {
			return true
		}
	}
	return false
}

// tagCounts counts the tags on msgs, most used first.
func tagCounts(msgs []Message) []tagCount {
	n := map[string]int{}
	for _, m := range msgs {
		for _, t := range m.Tags {
			n[t]++
		}
	}
	out := make([]tagCount, 0, len(n))
	for t, c := range n {
		out = append(out, tagCount{t, c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Tag < out[j].Tag
	})
	return out
}

// handleTags: GET /tags[?channel=C][&since=ID][&since_time=T], read-only.
func (sv *server) handleTags(w http.ResponseWriter, r *http.Request) {
	member := callerOf(r)
	q := r.URL.Query()
	var since int64
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			jsonError(w, http.StatusBadRequest, "bad since")
			return
		}
		since = n
	}
	var after time.Time
	var err error
	if v := q.Get("since_time"); v != "" {
		if after, err = parseSinceTime(v, sv.now()); err != nil {
			jsonError(w, http.StatusBadRequest, "since_time: "+err.Error())
			return
		}
	}
	chs, err := sv.readChannels(member, q.Get("channel"))
	if err != nil {
		writeErr(w, err)
		return
	}
	msgs := sv.store.readable(member, since, chs, currentNames())
	if !after.IsZero() {
		kept := msgs[:0]
		for _, m := range msgs {
			if ts, e := time.Parse(time.RFC3339, m.TS); e == nil && !ts.Before(after) {
				kept = append(kept, m)
			}
		}
		msgs = kept
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": tagCounts(msgs)})
}

// tagsSchema is the JSON schema of a tag list argument of an MCP tool.
func tagsSchema(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string", "pattern": "^[a-z0-9-]{1,24}$"}, "maxItems": maxTags, "description": desc}
}

// cmdTags is a2 tags: the tags in use with counts, on the messages the caller can read.
func cmdTags(args []string) error {
	fs := flag.NewFlagSet("tags", flag.ContinueOnError)
	channel := fs.String("channel", "", "only this channel; default: all your channels")
	sinceS := fs.String("since", "", "only messages after id N, or from a time (RFC3339, or HH:MM today in JST)")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if fs.NArg() != 0 {
		return usageError("tags takes only --channel and --since")
	}
	q := url.Values{}
	if *channel != "" {
		q.Set("channel", *channel)
	}
	if *sinceS != "" {
		if n, err := strconv.ParseInt(strings.TrimPrefix(*sinceS, "#"), 10, 64); err == nil && n >= 0 {
			q.Set("since", strconv.FormatInt(n, 10))
		} else if t, err := parseSinceTime(*sinceS, time.Now()); err == nil {
			q.Set("since_time", t.Format(time.RFC3339))
		} else {
			return usageError("--since: give a message id, RFC3339 or HH:MM")
		}
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var r struct {
		Tags []tagCount `json:"tags"`
	}
	if err := c.do("GET", "/tags?"+q.Encode(), nil, &r); err != nil {
		return err
	}
	if len(r.Tags) == 0 {
		fmt.Println("no tags")
		return nil
	}
	for _, t := range r.Tags {
		fmt.Printf("%s %d\n", t.Tag, t.Count)
	}
	return nil
}
