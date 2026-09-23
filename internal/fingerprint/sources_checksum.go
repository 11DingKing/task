package fingerprint

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zeebo/xxh3"

	"github.com/go-task/task/v3/internal/filepathext"
	"github.com/go-task/task/v3/taskfile/ast"
)

// ChecksumChecker validates if a task is up to date by calculating its source
// files checksum
type ChecksumChecker struct {
	tempDir string
	dry     bool
}

func NewChecksumChecker(tempDir string, dry bool) *ChecksumChecker {
	return &ChecksumChecker{
		tempDir: tempDir,
		dry:     dry,
	}
}

func (checker *ChecksumChecker) IsUpToDate(t *ast.Task) (bool, error) {
	if len(t.Sources) == 0 {
		return false, nil
	}

	checksumFile := checker.checksumFilePath(t)

	data, _ := os.ReadFile(checksumFile)
	oldHash := strings.TrimSpace(string(data))

	newHash, err := checker.checksum(t)
	if err != nil {
		return false, nil
	}

	if !checker.dry && oldHash != newHash {
		_ = os.MkdirAll(filepathext.SmartJoin(checker.tempDir, "checksum"), 0o755)
		if err = os.WriteFile(checksumFile, []byte(newHash+"\n"), 0o644); err != nil {
			return false, err
		}
	}

	if len(t.Generates) > 0 {
		// For each specified 'generates' field, check whether the files actually exist
		for _, g := range t.Generates {
			// Exclusion patterns don't represent output files; skip them.
			if g.Negate {
				continue
			}
			generates, err := glob(t.Dir, g.Glob)
			if os.IsNotExist(err) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if len(generates) == 0 {
				return false, nil
			}
		}
	}

	return oldHash == newHash, nil
}

func (checker *ChecksumChecker) Value(t *ast.Task) (any, error) {
	return checker.checksum(t)
}

func (checker *ChecksumChecker) OnError(t *ast.Task) error {
	if len(t.Sources) == 0 {
		return nil
	}
	return os.Remove(checker.checksumFilePath(t))
}

func (*ChecksumChecker) Kind() string {
	return "checksum"
}

// readerOnly hides any WriterTo/ReaderFrom implementation of the wrapped
// reader, forcing io.CopyBuffer to use the caller-provided buffer.
type readerOnly struct{ io.Reader }

func (c *ChecksumChecker) checksum(t *ast.Task) (string, error) {
	sources, err := Globs(t.Dir, t.Sources, t.ShouldUseGitignore())
	if err != nil {
		return "", err
	}

	h := xxh3.New()
	buf := make([]byte, 128*1024)
	for _, f := range sources {
		// Hash the file's identity (its path relative to the task directory)
		// in addition to its contents, and separate the two with NUL bytes.
		// That way adding, deleting, renaming or moving a source changes the
		// checksum even when basenames or contents coincide, so a changed file
		// set always invalidates the cached result.
		if _, err := io.WriteString(h, "\x00"+sourceName(t.Dir, f)+"\x00"); err != nil {
			return "", err
		}
		file, err := os.Open(f)
		if err != nil {
			return "", err
		}
		// Wrap the file in a plain io.Reader so io.CopyBuffer cannot take the
		// (*os.File).WriteTo fast path, which ignores buf and allocates a fresh
		// 32KiB buffer for every file. Reusing buf keeps this loop allocation-free.
		if _, err = io.CopyBuffer(h, readerOnly{file}, buf); err != nil {
			file.Close()
			return "", err
		}
		file.Close()
		if _, err := io.WriteString(h, "\x00"); err != nil {
			return "", err
		}
	}

	hash := h.Sum128()
	return fmt.Sprintf("%x%x", hash.Hi, hash.Lo), nil
}

// sourceName identifies a source independently of where the task directory
// lives, so the checksum survives moving a checkout but still changes when the
// file moves within it. Slash-normalized because glob results use forward
// slashes on every platform.
func sourceName(dir, f string) string {
	rel, err := filepath.Rel(filepath.FromSlash(dir), filepath.FromSlash(f))
	if err != nil {
		return filepath.ToSlash(f)
	}
	return filepath.ToSlash(rel)
}

func (checker *ChecksumChecker) checksumFilePath(t *ast.Task) string {
	return filepath.Join(checker.tempDir, "checksum", normalizeFilename(t.Name()))
}

var checksumFilenameRegexp = regexp.MustCompile("[^[:alnum:]]")

// replaces invalid characters on filenames with "-"
func normalizeFilename(f string) string {
	return checksumFilenameRegexp.ReplaceAllString(f, "-")
}
