//go:build !windows

package main

import (
	"os"
	"syscall"
)

// ownedByMe reports whether the file belongs to the user running the server.
func ownedByMe(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
