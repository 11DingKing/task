package checkpoint_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-task/task/v3/internal/checkpoint"
	"github.com/go-task/task/v3/internal/logger"
	"github.com/go-task/task/v3/taskfile/ast"
)

func testLogger() *logger.Logger {
	var buf bytes.Buffer
	return &logger.Logger{Stdout: &buf, Stderr: &buf}
}

func newTask(cmds ...string) *ast.Task {
	t := &ast.Task{
		Task:     "build",
		Location: &ast.Location{Taskfile: "/repo/Taskfile.yml"},
		Vars:     ast.NewVars(),
		Env:      ast.NewVars(),
	}
	for _, c := range cmds {
		t.Cmds = append(t.Cmds, &ast.Cmd{Cmd: c})
	}
	return t
}

func TestDefHashBindsDefinitionAndVars(t *testing.T) {
	t.Parallel()

	t1 := newTask("echo {{.FOO}}")
	t1.Vars.Set("FOO", ast.Var{Value: "bar"})
	t2 := newTask("echo {{.FOO}}")
	t2.Vars.Set("FOO", ast.Var{Value: "bar"})
	t3 := newTask("echo {{.FOO}}")
	t3.Vars.Set("FOO", ast.Var{Value: "baz"})
	t4 := newTask("echo {{.FOO}} && echo changed")
	t4.Vars.Set("FOO", ast.Var{Value: "bar"})

	h1, err := checkpoint.DefHash(t1)
	require.NoError(t, err)
	h2, err := checkpoint.DefHash(t2)
	require.NoError(t, err)
	h3, err := checkpoint.DefHash(t3)
	require.NoError(t, err)
	h4, err := checkpoint.DefHash(t4)
	require.NoError(t, err)

	assert.Equal(t, h1, h2, "identical resolved definition and vars must hash equal")
	assert.NotEqual(t, h1, h3, "effective variables must be bound to the node hash")
	assert.NotEqual(t, h1, h4, "task commands must be bound to the node hash")
}

func TestDefHashBindsDepAndSubtaskVars(t *testing.T) {
	t.Parallel()

	t1 := newTask()
	t1.Deps = []*ast.Dep{{Task: "compile", Vars: ast.NewVars()}}
	t1.Deps[0].Vars.Set("MODE", ast.Var{Value: "debug"})

	t2 := newTask()
	t2.Deps = []*ast.Dep{{Task: "compile", Vars: ast.NewVars()}}
	t2.Deps[0].Vars.Set("MODE", ast.Var{Value: "release"})

	h1, err := checkpoint.DefHash(t1)
	require.NoError(t, err)
	h2, err := checkpoint.DefHash(t2)
	require.NoError(t, err)
	assert.NotEqual(t, h1, h2, "dependency call variables must be bound")

	t3 := newTask()
	t3.Cmds = []*ast.Cmd{{Task: "compile", Vars: ast.NewVars()}}
	t3.Cmds[0].Vars.Set("MODE", ast.Var{Value: "debug"})
	t4 := newTask()
	t4.Cmds = []*ast.Cmd{{Task: "compile", Vars: ast.NewVars()}}
	t4.Cmds[0].Vars.Set("MODE", ast.Var{Value: "release"})
	h3, err := checkpoint.DefHash(t3)
	require.NoError(t, err)
	h4, err := checkpoint.DefHash(t4)
	require.NoError(t, err)
	assert.NotEqual(t, h3, h4, "subtask call variables must be bound")
}

func TestSourcesHashChangesWithFileContentsAndNames(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one"), 0o644))

	task := newTask("cat a.txt")
	task.Dir = dir
	task.Sources = []*ast.Glob{{Glob: "*.txt"}}

	binding := checkpoint.Binding{Entrypoint: filepath.Join(dir, "Taskfile.yml"), Dir: dir}
	newManager := func() *checkpoint.Manager {
		return checkpoint.Open(dir, false, binding, testLogger())
	}

	h1, err := newManager().SourcesHash(task)
	require.NoError(t, err)
	require.NotEmpty(t, h1)

	// Same content -> stable across managers/runs.
	h2, err := newManager().SourcesHash(task)
	require.NoError(t, err)
	assert.Equal(t, h1, h2)

	// Changed content -> new identity.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two"), 0o644))
	h3, err := newManager().SourcesHash(task)
	require.NoError(t, err)
	assert.NotEqual(t, h1, h3)

	// Renamed source with same content -> new identity.
	require.NoError(t, os.Rename(filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")))
	h4, err := newManager().SourcesHash(task)
	require.NoError(t, err)
	assert.NotEqual(t, h3, h4)
}

func TestManagerRoundTripAndStatuses(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	binding := checkpoint.Binding{
		Calls:      []checkpoint.CallBinding{{Task: "default", Vars: map[string]any{"MODE": "dev"}}},
		Entrypoint: filepath.Join(dir, "Taskfile.yml"),
		Dir:        dir,
	}

	writer := checkpoint.Open(dir, false, binding, testLogger())
	require.False(t, writer.Usable())
	require.NoError(t, writer.Completed("node-a", "src-a", nil))
	require.NoError(t, writer.Record("node-b", checkpoint.StatusFailed))

	// Reopen for resume: records must survive and only completed nodes count.
	reader := checkpoint.Open(dir, true, binding, testLogger())
	require.True(t, reader.Usable(), "a valid checkpoint for the same invocation must be usable")

	na, ok := reader.Node("node-a")
	require.True(t, ok)
	assert.Equal(t, checkpoint.StatusCompleted, na.Status)
	assert.Equal(t, "src-a", na.SourcesHash)

	nb, ok := reader.Node("node-b")
	require.True(t, ok)
	assert.Equal(t, checkpoint.StatusFailed, nb.Status)
}

func TestManagerRejectsUnverifiableCheckpoints(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	binding := checkpoint.Binding{
		Calls:      []checkpoint.CallBinding{{Task: "default"}},
		Entrypoint: filepath.Join(dir, "Taskfile.yml"),
		Dir:        dir,
	}

	writer := checkpoint.Open(dir, false, binding, testLogger())
	require.NoError(t, writer.Completed("node-a", "", nil))

	// Different invocation (other root variables) -> fresh run.
	other := binding
	other.Vars = map[string]any{"MODE": "prod"}
	m := checkpoint.Open(dir, true, other, testLogger())
	assert.False(t, m.Usable())
	_, ok := m.Node("node-a")
	assert.False(t, ok)

	// Same binding but corrupt file -> fresh run.
	path := filepath.Join(dir, "checkpoint", checkpoint.FileName(binding))
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))
	corrupt := checkpoint.Open(dir, true, binding, testLogger())
	assert.False(t, corrupt.Usable())
}

func TestManagerDiscardOnSuccess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	binding := checkpoint.Binding{
		Calls:      []checkpoint.CallBinding{{Task: "default"}},
		Entrypoint: filepath.Join(dir, "Taskfile.yml"),
		Dir:        dir,
	}
	m := checkpoint.Open(dir, false, binding, testLogger())
	require.NoError(t, m.Completed("node-a", "", nil))

	require.NoError(t, m.Discard())
	// A successful run leaves nothing to resume from.
	reopened := checkpoint.Open(dir, true, binding, testLogger())
	assert.False(t, reopened.Usable())
}

func TestSubtasksRecordedInNode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	binding := checkpoint.Binding{
		Calls:      []checkpoint.CallBinding{{Task: "default"}},
		Entrypoint: filepath.Join(dir, "Taskfile.yml"),
		Dir:        dir,
	}
	m := checkpoint.Open(dir, false, binding, testLogger())
	require.NoError(t, m.Completed("parent", "", []string{"child-b", "child-a", "child-a"}))

	n, ok := m.Node("parent")
	require.True(t, ok)
	assert.Equal(t, []string{"child-a", "child-b"}, n.Subtasks, "subtask keys must be de-duplicated and sorted")
}
