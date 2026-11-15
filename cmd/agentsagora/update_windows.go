//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// replaceExe: Windows cannot replace a running .exe, but it can rename it, so the running
// one moves to .old first (renameAside).
func replaceExe(exe, tmp string) error { return renameAside(exe, tmp, time.Now()) }

func cleanupOld(exe string) { removeAside(exe) }

// reexec: Windows has no exec, so the new binary runs as a child with the same arguments
// and standard streams, and this process exits with its exit code. It returns only when
// the child could not start.
func reexec(args []string) {
	cmd := exec.Command(reexecPath(), args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = reexecEnv()
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		os.Exit(0)
	case errors.As(err, &ee):
		os.Exit(ee.ExitCode())
	}
	fmt.Fprintln(os.Stderr, "a2: notice: re-exec after update failed, continuing with the old build:", err)
}
