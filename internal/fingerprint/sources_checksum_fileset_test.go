package fingerprint

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-task/task/v3/internal/filepathext"
	"github.com/go-task/task/v3/internal/fsext"
	"github.com/go-task/task/v3/taskfile/ast"
)

// Discovery, checksum and the cache decision must all see the same files:
// deleting a source, restoring it or adding a hidden one must invalidate the
// cached checksum, while an unchanged set stays cached.
func TestChecksumCheckerFileSetChangesInvalidate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := filepath.Join(dir, "source.txt")
	require.NoError(t, os.WriteFile(source, []byte("content"), 0o600))

	task := &ast.Task{
		Task:    "file-set",
		Dir:     dir,
		Sources: []*ast.Glob{{Glob: "*.txt"}},
	}
	checker := NewChecksumChecker(filepath.Join(dir, ".task-cache"), false)
	isUpToDate := func() bool {
		t.Helper()
		upToDate, err := checker.IsUpToDate(task)
		require.NoError(t, err)
		return upToDate
	}

	require.False(t, isUpToDate(), "the first check should prime the cache")
	require.True(t, isUpToDate(), "unchanged sources should be cached")

	require.NoError(t, os.WriteFile(source, []byte("modified"), 0o600))
	require.False(t, isUpToDate(), "modifying a source should invalidate the cache")
	require.True(t, isUpToDate())

	// Hidden files take the fallback glob route; they must participate in the
	// checksum just like files found by the optimized route.
	hidden := filepath.Join(dir, ".hidden.txt")
	require.NoError(t, os.WriteFile(hidden, []byte("secret"), 0o600))
	require.False(t, isUpToDate(), "adding a hidden source should invalidate the cache")
	require.True(t, isUpToDate())

	require.NoError(t, os.Remove(hidden))
	require.False(t, isUpToDate(), "deleting a hidden source should invalidate the cache")
	require.True(t, isUpToDate())

	require.NoError(t, os.Remove(source))
	require.False(t, isUpToDate(), "deleting the last visible source should invalidate the cache")
	require.True(t, isUpToDate(), "no matching sources keeps the previous skip semantics")

	require.NoError(t, os.WriteFile(source, []byte("content"), 0o600))
	require.False(t, isUpToDate(), "restoring a source should invalidate the cache")
	require.True(t, isUpToDate())
}

// Moving a source to another directory changes the file set even when the
// basename and contents stay the same, so the cache must not be considered up
// to date.
func TestChecksumCheckerMoveSameNameAndContentInvalidates(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "d1"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "d2"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "d1", "f.txt"), []byte("same"), 0o600))

	task := &ast.Task{
		Task:    "move",
		Dir:     dir,
		Sources: []*ast.Glob{{Glob: "**/*.txt"}},
	}
	checker := NewChecksumChecker(filepath.Join(dir, ".task-cache"), false)
	isUpToDate := func() bool {
		t.Helper()
		upToDate, err := checker.IsUpToDate(task)
		require.NoError(t, err)
		return upToDate
	}

	require.False(t, isUpToDate())
	require.True(t, isUpToDate())

	require.NoError(t, os.Rename(
		filepath.Join(dir, "d1", "f.txt"),
		filepath.Join(dir, "d2", "f.txt"),
	))
	require.False(t, isUpToDate(), "moving a source should invalidate the cache")
	require.True(t, isUpToDate())
}

// When declared sources match no files, the first check primes the cache and
// later checks report up to date; a literal source that does not exist behaves
// the same.
func TestChecksumCheckerNoMatchingSourcesSkipSemantics(t *testing.T) {
	t.Parallel()

	for _, pattern := range []string{"*.txt", "missing.txt"} {
		t.Run(pattern, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			task := &ast.Task{
				Task:    "no-sources",
				Dir:     dir,
				Sources: []*ast.Glob{{Glob: pattern}},
			}
			checker := NewChecksumChecker(filepath.Join(dir, ".task-cache"), false)

			upToDate, err := checker.IsUpToDate(task)
			require.NoError(t, err)
			assert.False(t, upToDate, "the first check with no matched files runs the task")

			upToDate, err = checker.IsUpToDate(task)
			require.NoError(t, err)
			assert.True(t, upToDate, "later checks with no matched files skip the task")
		})
	}
}

// Both globbing routes must surface hidden files and files below hidden
// directories, so discovery, checksum and task selection never disagree.
func TestGlobsHiddenEntriesConsistentAcrossRoutes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	files := []string{
		"input/folder-a/.hidden-file-a.aaa",
		"input/folder-a/file-a.aaa",
		"input/folder-a/nested/.hidden-file-b.bbb",
		"input/folder-a/nested/file-b.aaa",
		"input/folder-a/.hidden-folder/file-c.aaa",
		"input/.hidden-root/file-d.bbb",
		"input/folder-c/file-e.aaa",
	}
	filesetWriteFiles(t, dir, files)

	tests := []struct {
		name    string
		pattern string
		fast    bool
		want    []string
	}{
		{
			name:    "optimized recursive",
			pattern: filepath.Join("input", "folder-a", "**", "*"),
			fast:    true,
			want: []string{
				"input/folder-a/.hidden-file-a.aaa",
				"input/folder-a/file-a.aaa",
				"input/folder-a/nested/.hidden-file-b.bbb",
				"input/folder-a/nested/file-b.aaa",
				"input/folder-a/.hidden-folder/file-c.aaa",
			},
		},
		{
			name:    "fallback nonrecursive",
			pattern: filepath.Join("input", "folder-a", "*"),
			fast:    false,
			want: []string{
				"input/folder-a/.hidden-file-a.aaa",
				"input/folder-a/file-a.aaa",
			},
		},
		{
			name:    "fallback wildcard root",
			pattern: filepath.Join("input", "*", "**", "*"),
			fast:    false,
			want:    files,
		},
		{
			name:    "fallback nested suffix",
			pattern: filepath.Join("input", "**", "nested", "*"),
			fast:    false,
			want: []string{
				"input/folder-a/nested/.hidden-file-b.bbb",
				"input/folder-a/nested/file-b.aaa",
			},
		},
		{
			name:    "fallback brace suffix",
			pattern: filepath.Join("input", "**", "*.{aaa,bbb}"),
			fast:    false,
			want:    files,
		},
		{
			name:    "fallback multiple recursive markers",
			pattern: filepath.Join("input", "**", "nested", "**", "*"),
			fast:    false,
			want: []string{
				"input/folder-a/nested/.hidden-file-b.bbb",
				"input/folder-a/nested/file-b.aaa",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.fast, filesetRoute(t, dir, tt.pattern), "glob route for %q", tt.pattern)

			got, err := Globs(dir, []*ast.Glob{{Glob: tt.pattern}}, false)
			require.NoError(t, err)
			assert.Equal(t, filesetAbsPaths(dir, tt.want), got)
		})
	}
}

// A hidden file found through the optimized inclusion must still be removable
// through a fallback exclusion.
func TestGlobsFallbackExclusionRemovesHiddenEntries(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	files := []string{
		"tree/keep.txt",
		"tree/skip/.visible.txt",
		"tree/skip/.hidden.txt",
	}
	filesetWriteFiles(t, dir, files)

	globs := []*ast.Glob{
		{Glob: filepath.Join("tree", "**", "*")},
		{Glob: filepath.Join("tree", "**", "skip", "**", "*"), Negate: true},
	}
	require.False(t, filesetRoute(t, dir, globs[1].Glob), "exclusion uses the fallback route")

	got, err := Globs(dir, globs, false)
	require.NoError(t, err)
	assert.Equal(t, filesetAbsPaths(dir, []string{"tree/keep.txt"}), got)
}

func filesetWriteFiles(t *testing.T, dir string, files []string) {
	t.Helper()
	for _, file := range files {
		path := filepath.Join(dir, filepath.FromSlash(file))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(file), 0o600))
	}
}

func filesetAbsPaths(dir string, files []string) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, filepath.ToSlash(filepath.Join(dir, filepath.FromSlash(file))))
	}
	sort.Strings(paths)
	return paths
}

func filesetRoute(t *testing.T, dir, pattern string) bool {
	t.Helper()
	_, fast, err := fsext.FastRecursiveGlob(filepathext.SmartJoin(dir, pattern))
	require.NoError(t, err)
	return fast
}
