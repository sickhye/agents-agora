//go:build windows

package main

import "os"

// ownedByMe: Windows has no Unix owner; the user profile ACL guards the config dir.
func ownedByMe(os.FileInfo) bool { return true }
