//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// replaceExe renames tmp (same dir) over exe in one step: other processes see the old or
// the new file, never a partial one; a running process keeps its old image.
func replaceExe(exe, tmp string) error { return os.Rename(tmp, exe) }

func cleanupOld(exe string) {}

// reexec replaces this process with the (new) binary at the same path and the same
// arguments. It returns only when the exec failed.
func reexec(args []string) {
	p := reexecPath()
	err := syscall.Exec(p, append([]string{os.Args[0]}, args...), reexecEnv())
	fmt.Fprintln(os.Stderr, "a2: notice: re-exec after update failed, continuing with the old build:", err)
}
