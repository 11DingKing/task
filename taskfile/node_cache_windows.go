//go:build windows

package taskfile

import "golang.org/x/sys/windows"

// replaceFile atomically publishes tmp as newpath. os.Rename cannot replace an
// existing destination on Windows, so use MoveFileEx with
// MOVEFILE_REPLACE_EXISTING, which swaps the file atomically.
func replaceFile(tmp, newpath string) error {
	return windows.Rename(tmp, newpath)
}
