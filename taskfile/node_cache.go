package taskfile

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const remoteCacheDir = "remote"

type CacheNode struct {
	*baseNode
	source RemoteNode
}

func NewCacheNode(source RemoteNode, dir string) *CacheNode {
	return &CacheNode{
		baseNode: &baseNode{
			dir: filepath.Join(dir, remoteCacheDir),
		},
		source: source,
	}
}

func (node *CacheNode) Read() ([]byte, error) {
	return os.ReadFile(node.Location())
}

// Commit stores a freshly downloaded remote Taskfile behind a single commit
// boundary: the complete content, its checksum and the freshness timestamp are
// each staged in a temporary file and atomically moved into place. The
// timestamp is published last because its presence and expiry is what marks an
// entry as usable. If anything fails, the staged files are discarded and the
// previous entry is left intact (or no entry at all), so a subsequent run
// re-requests the remote file instead of reusing partial content.
func (node *CacheNode) Commit(data []byte, sum string, t time.Time) error {
	if err := node.CreateCacheDir(); err != nil {
		return err
	}
	if err := writeFileAtomic(node.Location(), data); err != nil {
		return fmt.Errorf("caching %q: %w", node.source.Location(), err)
	}
	if err := writeFileAtomic(node.checksumPath(), []byte(sum)); err != nil {
		return fmt.Errorf("caching checksum for %q: %w", node.source.Location(), err)
	}
	if err := writeFileAtomic(node.timestampPath(), []byte(t.Format(time.RFC3339))); err != nil {
		return fmt.Errorf("caching timestamp for %q: %w", node.source.Location(), err)
	}
	return nil
}

// writeFileAtomic writes data to path via a temporary file in the same
// directory followed by an atomic rename. Readers of path can therefore only
// ever observe the previous complete contents or the new complete contents,
// never a file that is still being written. The temporary file is removed if
// any step fails.
func writeFileAtomic(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".task-cache-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()

	if _, err = f.Write(data); err != nil {
		return err
	}
	// Flush the data to disk before publishing the file.
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Chmod(0o644); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// Atomically publish the staged file over any previous complete file.
	return replaceFile(tmp, path)
}

func (node *CacheNode) ReadTimestamp() time.Time {
	b, err := os.ReadFile(node.timestampPath())
	if err != nil {
		return time.Time{}.UTC()
	}
	timestamp, err := time.Parse(time.RFC3339, string(b))
	if err != nil {
		return time.Time{}.UTC()
	}
	return timestamp.UTC()
}

func (node *CacheNode) ReadChecksum() string {
	b, _ := os.ReadFile(node.checksumPath())
	return string(b)
}

func (node *CacheNode) CreateCacheDir() error {
	if err := os.MkdirAll(node.dir, 0o755); err != nil {
		return err
	}
	return nil
}

func (node *CacheNode) ChecksumPrompt(checksum string) string {
	cachedChecksum := node.ReadChecksum()
	switch {

	// If the checksum doesn't exist, prompt the user to continue
	case cachedChecksum == "":
		return taskfileUntrustedPrompt

	// If there is a cached hash, but it doesn't match the expected hash, prompt the user to continue
	case cachedChecksum != checksum:
		return taskfileChangedPrompt

	default:
		return ""
	}
}

func (node *CacheNode) Location() string {
	return node.filePath("yaml")
}

func (node *CacheNode) checksumPath() string {
	return node.filePath("checksum")
}

func (node *CacheNode) timestampPath() string {
	return node.filePath("timestamp")
}

func (node *CacheNode) filePath(suffix string) string {
	return filepath.Join(node.dir, fmt.Sprintf("%s.%s", node.source.CacheKey(), suffix))
}

func checksum(b []byte) string {
	h := sha256.New()
	h.Write(b)
	return fmt.Sprintf("%x", h.Sum(nil))
}
