//go:build !windows

package taskfile

import "os"

// replaceFile atomically publishes tmp as newpath. On POSIX systems rename(2)
// atomically replaces the destination when both paths are on the same
// filesystem.
func replaceFile(tmp, newpath string) error {
	return os.Rename(tmp, newpath)
}
