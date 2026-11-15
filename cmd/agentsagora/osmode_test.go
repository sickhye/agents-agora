package main

import (
	"os"
	"runtime"
	"testing"
)

// unixModes is false on Windows: there Perm reports 0666 (0444 when read-only) for every
// file and Chmod only toggles the read-only flag, so mode assertions mean nothing.
const unixModes = runtime.GOOS != "windows"

const noUnixModes = "Windows has no Unix file modes (Perm is 0666/0444, Chmod only toggles read-only)"

// wrongMode reports a file whose mode is not want; always false on Windows.
func wrongMode(fi os.FileInfo, want os.FileMode) bool {
	return unixModes && fi.Mode().Perm() != want
}

// skipWithoutUnixModes skips a test whose whole point is a Unix file mode.
func skipWithoutUnixModes(t *testing.T) {
	t.Helper()
	if !unixModes {
		t.Skip(noUnixModes)
	}
}
