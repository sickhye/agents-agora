package main

import (
	"os"
	"runtime"
	"time"
)

// renameOver renames tmp over dst (both in the same dir). On Unix this is one os.Rename.
// On Windows a rename onto a file fails with "Access is denied" while any process has dst
// open (os.Open does not share delete access there), e.g. `a2 watch-status` reading the
// tick file. Such readers hold the file for microseconds, so the rename is tried a few more
// times over about 100 ms before the error is returned.
func renameOver(tmp, dst string) error {
	err := os.Rename(tmp, dst)
	for i := 0; err != nil && runtime.GOOS == "windows" && i < 10; i++ {
		time.Sleep(10 * time.Millisecond)
		err = os.Rename(tmp, dst)
	}
	return err
}
