package main

import (
	"fmt"
	"net/url"
	"testing"
	"time"
)

// /inbox?sent=1 lists only the caller's own sends.
func TestInboxSent(t *testing.T) {
	f := setup(t)
	t0 := time.Date(2026, 11, 15, 9, 0, 0, 0, jst)
	put := func(i int, ch, from, to string, re int64) {
		if _, err := f.sv.store.appendReply(ch, from, to, "m", "", re, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	put(1, "team", "alice", "all", 0)  // #1
	put(2, "team", "bob", "dave", 0)   // #2
	put(3, "team", "alice", "all", 1)  // #3 own reply
	put(4, "general", "bob", "all", 0) // #4
	put(5, "team", "carol", "all", 3)  // #5 reply by someone else
	put(6, "team", "alice", "bob", 1)  // #6 DM sent by alice
	put(7, "team", "bob", "alice", 0)  // #7 DM to alice
	eq := func(name string, got []int64, want ...int64) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: got %v want %v", name, got, want)
		}
	}
	_, ids := inboxIDs(t, f, "alice", "sent=1")
	eq("own sends (broadcast and DM)", ids, 1, 3, 6)
	_, ids = inboxIDs(t, f, "bob", "sent=1")
	eq("another member sees only its own", ids, 2, 4, 7)
	_, ids = inboxIDs(t, f, "dave", "sent=1")
	eq("no sends", ids)
	_, ids = inboxIDs(t, f, "alice", "sent=1&thread=1")
	eq("thread with own replies", ids, 1, 3, 6)
	_, ids = inboxIDs(t, f, "alice", "sent=1&since_time="+url.QueryEscape(t0.Add(3*time.Minute).Format(time.RFC3339)))
	eq("since_time", ids, 3, 6)
	_, ids = inboxIDs(t, f, "alice", "sent=1&channel=general")
	eq("channel filter", ids)
	// another member's name never widens it
	if code, _ := inboxIDs(t, f, "alice", "sent=1&from=bob"); code != 400 {
		t.Errorf("sent+from other: code %d", code)
	}
	_, ids = inboxIDs(t, f, "alice", "sent=1&from=alice")
	eq("sent+from self", ids, 1, 3, 6)
	// the normal inbox is unchanged and sent does not record deliveries
	_, ids = inboxIDs(t, f, "alice", "")
	eq("normal inbox", ids, 4, 5, 7)
}
