package task_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-task/task/v3"
	"github.com/go-task/task/v3/internal/checkpoint"
)

func writeCheckpointTaskfile(t *testing.T, dir, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Taskfile.yml"), []byte(content), 0o644))
}

func runCheckpointed(
	t *testing.T,
	dir string,
	resume bool,
	invocationVars map[string]any,
	ctx context.Context,
) (string, error) {
	t.Helper()
	var out SyncBuffer
	opts := []task.ExecutorOption{
		task.WithDir(dir),
		task.WithStdout(&out),
		task.WithStderr(&out),
		task.WithColor(false),
	}
	if resume {
		opts = append(opts, task.WithResume(true))
	} else {
		opts = append(opts, task.WithBreakpoint(true))
	}
	if invocationVars != nil {
		opts = append(opts, task.WithInvocationVars(invocationVars))
	}
	e := task.NewExecutor(opts...)
	require.NoError(t, e.Setup())
	err := e.Run(ctx, &task.Call{Task: "default"})
	return out.buf.String(), err
}

func checkpointFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".task", "checkpoint", "*.json"))
	require.NoError(t, err)
	return matches
}

// interruptWhen cancels ctx with the checkpoint interruption cause once the
// marker file shows up, simulating the user pressing Ctrl-C at a known point.
func interruptWhen(t *testing.T, marker string, cancel context.CancelCauseFunc) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			cancel(checkpoint.ErrInterrupted)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Errorf (unlike Fatalf) is safe to call from a non-test goroutine.
	t.Errorf("marker file %q never appeared", marker)
	cancel(checkpoint.ErrInterrupted)
}

const interruptResumeTaskfile = `version: '3'
tasks:
  default:
    deps: [c]
  c:
    deps: [a, b]
    cmds:
      - echo running-c
  a:
    cmds:
      - echo running-a
      # Do not arm the interrupt marker until b has finished, so the test is
      # deterministic about which node is completed and which is interrupted.
      - 'while [ ! -f .b.done ]; do sleep 0.02; done'
      - echo ready > .a.ready
      - 'if [ -f .go ]; then echo a-done; else sleep 30; fi'
  b:
    cmds:
      - echo running-b
      - echo done > .b.done
`

func TestCheckpointInterruptAndResume(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeCheckpointTaskfile(t, dir, interruptResumeTaskfile)

	ctx, cancel := context.WithCancelCause(t.Context())
	go interruptWhen(t, filepath.Join(dir, ".a.ready"), cancel)

	out, err := runCheckpointed(t, dir, false, nil, ctx)
	require.NoError(t, err, "a run stopped at a checkpoint must finish gracefully")
	assert.Contains(t, out, "running-a")
	assert.Contains(t, out, "running-b")
	assert.NotContains(t, out, "running-c")
	require.Len(t, checkpointFiles(t, dir), 1, "progress must be persisted")

	// Remove the gate and resume: b is restored, a and its successor c run.
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".go"), []byte("1"), 0o644))
	out, err = runCheckpointed(t, dir, true, nil, t.Context())
	require.NoError(t, err)
	assert.Contains(t, out, `Task "b" restored from checkpoint`)
	assert.NotContains(t, out, "task: [b] echo running-b", "restored nodes must not execute again")
	assert.Contains(t, out, "a-done")
	assert.Contains(t, out, "running-c", "successors of the interrupted node must re-execute")
	assert.Empty(t, checkpointFiles(t, dir), "a successful run must discard the checkpoint")
}

func TestCheckpointSuccessfulRunLeavesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeCheckpointTaskfile(t, dir, `version: '3'
tasks:
  default:
    cmds:
      - echo done
`)

	out, err := runCheckpointed(t, dir, false, nil, t.Context())
	require.NoError(t, err)
	assert.Contains(t, out, "done")
	assert.Empty(t, checkpointFiles(t, dir))

	// Executions without breakpoint mode are completely unaffected: not even
	// the checkpoint directory is created.
	dir2 := t.TempDir()
	writeCheckpointTaskfile(t, dir2, `version: '3'
tasks:
  default:
    cmds:
      - echo done
`)
	var out2 bytes.Buffer
	e := task.NewExecutor(
		task.WithDir(dir2),
		task.WithStdout(&out2),
		task.WithStderr(&out2),
	)
	require.NoError(t, e.Setup())
	require.NoError(t, e.Run(t.Context(), &task.Call{Task: "default"}))
	_, statErr := os.Stat(filepath.Join(dir2, ".task", "checkpoint"))
	assert.True(t, os.IsNotExist(statErr))
}

func TestCheckpointFailedNodeRerunsOnResume(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeCheckpointTaskfile(t, dir, `version: '3'
tasks:
  default:
    deps: [a, b]
  a:
    cmds:
      - echo running-a
      - 'test -f .go'
  b:
    cmds:
      - echo running-b
`)

	out, err := runCheckpointed(t, dir, false, nil, t.Context())
	require.Error(t, err, "a genuinely failed run must still report the error")
	assert.Contains(t, out, "running-a")
	assert.Contains(t, out, "running-b")
	require.Len(t, checkpointFiles(t, dir), 1)

	require.NoError(t, os.WriteFile(filepath.Join(dir, ".go"), []byte("1"), 0o644))
	out, err = runCheckpointed(t, dir, true, nil, t.Context())
	require.NoError(t, err)
	assert.Contains(t, out, `Task "b" restored from checkpoint`)
	assert.Contains(t, out, "test -f .go", "the previously failed node must re-execute")
	assert.Empty(t, checkpointFiles(t, dir))
}

func TestCheckpointFailfastCancelledNodesRerun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeCheckpointTaskfile(t, dir, `version: '3'
tasks:
  default:
    deps: [a, b]
    failfast: true
  a:
    cmds:
      - 'if [ -f .go ]; then echo a-done; else sleep 30; fi'
  b:
    cmds:
      - echo running-b
      - 'test -f .go'
`)

	// b fails immediately; failfast cancels a while it sleeps. No signal is
	// involved, so the failed/cancelled distinction is exercised directly.
	_, err := runCheckpointed(t, dir, false, nil, t.Context())
	require.Error(t, err, "failfast failure must not be masked")
	require.Len(t, checkpointFiles(t, dir), 1)

	require.NoError(t, os.WriteFile(filepath.Join(dir, ".go"), []byte("1"), 0o644))
	out, err := runCheckpointed(t, dir, true, nil, t.Context())
	require.NoError(t, err)
	assert.Contains(t, out, "a-done", "the cancelled node must re-execute")
	assert.Contains(t, out, "running-b", "the failed node must re-execute")
	assert.NotContains(t, out, `Task "b" restored from checkpoint`)
}

func TestCheckpointDefinitionChangeInvalidatesAffectedPart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// slow depends on the branch under test, so when it arms the marker the
	// whole a/b/c branch has already completed.
	writeCheckpointTaskfile(t, dir, `version: '3'
tasks:
  default:
    deps: [slow]
  slow:
    deps: [c]
    cmds:
      - echo slow > .slow.ready
      - 'if [ -f .go ]; then echo slow-done; else sleep 30; fi'
  c:
    deps: [a, b]
    cmds:
      - echo running-c
  a:
    cmds:
      - echo running-a
  b:
    cmds:
      - echo running-b
`)

	ctx, cancel := context.WithCancelCause(t.Context())
	go interruptWhen(t, filepath.Join(dir, ".slow.ready"), cancel)

	_, err := runCheckpointed(t, dir, false, nil, ctx)
	require.NoError(t, err)

	// Change only task a; give slow a way to finish.
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".go"), []byte("1"), 0o644))
	writeCheckpointTaskfile(t, dir, `version: '3'
tasks:
  default:
    deps: [slow]
  slow:
    deps: [c]
    cmds:
      - echo slow > .slow.ready
      - 'if [ -f .go ]; then echo slow-done; else sleep 30; fi'
  c:
    deps: [a, b]
    cmds:
      - echo running-c
  a:
    cmds:
      - echo running-a-v2
  b:
    cmds:
      - echo running-b
`)

	out, err := runCheckpointed(t, dir, true, nil, t.Context())
	require.NoError(t, err)
	assert.Contains(t, out, "running-a-v2", "changed node must re-execute")
	assert.Contains(t, out, "running-c", "its successor must re-execute")
	assert.Contains(t, out, "slow-done", "the interrupted node must re-execute")
	assert.Contains(t, out, `Task "b" restored from checkpoint`, "unaffected branch stays cached")
	assert.NotContains(t, out, "task: [b] echo running-b")
}

func TestCheckpointSourceChangeInvalidatesNode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeCheckpointTaskfile(t, dir, `version: '3'
tasks:
  default:
    deps: [slow]
  slow:
    deps: [a, b]
    cmds:
      - echo slow > .slow.ready
      - 'if [ -f .go ]; then echo slow-done; else sleep 30; fi'
  a:
    sources:
      - src.txt
    generates:
      - a.out
    cmds:
      - cp src.txt a.out
  b:
    cmds:
      - echo running-b
`)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src.txt"), []byte("one"), 0o644))

	ctx, cancel := context.WithCancelCause(t.Context())
	go interruptWhen(t, filepath.Join(dir, ".slow.ready"), cancel)
	_, err := runCheckpointed(t, dir, false, nil, ctx)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(dir, "a.out"))

	// Source changes: a must re-run even though its definition did not.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src.txt"), []byte("two"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".go"), []byte("1"), 0o644))
	out, err := runCheckpointed(t, dir, true, nil, t.Context())
	require.NoError(t, err)
	assert.Contains(t, out, "task: [a] cp src.txt a.out")
	assert.Contains(t, out, `Task "b" restored from checkpoint`)
	content, err := os.ReadFile(filepath.Join(dir, "a.out"))
	require.NoError(t, err)
	assert.Equal(t, "two", string(content))
}

func TestCheckpointMissingOutputIsNotCompleted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeCheckpointTaskfile(t, dir, `version: '3'
tasks:
  default:
    cmds:
      - echo hi
    generates:
      - missing.txt
`)

	_, err := runCheckpointed(t, dir, false, nil, t.Context())
	require.Error(t, err)
	require.Len(t, checkpointFiles(t, dir), 1)

	// The node must have been recorded as failed, not completed.
	data, err := os.ReadFile(checkpointFiles(t, dir)[0])
	require.NoError(t, err)
	assert.Contains(t, string(data), `"status": "failed"`)
	assert.NotContains(t, string(data), `"status": "completed"`)
}

func TestCheckpointDifferentInvocationStartsFresh(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeCheckpointTaskfile(t, dir, `version: '3'
tasks:
  default:
    deps: [slow]
  slow:
    deps: [b]
    cmds:
      - echo slow > .slow.ready
      - 'if [ -f .go ]; then echo slow-done; else sleep 30; fi'
  b:
    cmds:
      - echo running-b
`)

	ctx, cancel := context.WithCancelCause(t.Context())
	go interruptWhen(t, filepath.Join(dir, ".slow.ready"), cancel)
	_, err := runCheckpointed(t, dir, false, map[string]any{"MODE": "dev"}, ctx)
	require.NoError(t, err)

	// Resume with different CLI variables: checkpoint cannot be verified.
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".go"), []byte("1"), 0o644))
	out, err := runCheckpointed(t, dir, true, map[string]any{"MODE": "prod"}, t.Context())
	require.NoError(t, err)
	assert.NotContains(t, out, "restored from checkpoint")
	assert.Contains(t, out, "task: [b] echo running-b", "every node must run from scratch")
}

func TestCheckpointSubtaskChangeInvalidatesCaller(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeCheckpointTaskfile(t, dir, `version: '3'
tasks:
  default:
    deps: [slow]
  slow:
    deps: [chain]
    cmds:
      - echo slow > .slow.ready
      - 'if [ -f .go ]; then echo slow-done; else sleep 30; fi'
  chain:
    cmds:
      - task: sub
      - echo chain-done
  sub:
    cmds:
      - echo sub-v1
`)

	ctx, cancel := context.WithCancelCause(t.Context())
	go interruptWhen(t, filepath.Join(dir, ".slow.ready"), cancel)
	_, err := runCheckpointed(t, dir, false, nil, ctx)
	require.NoError(t, err)

	// Only the subtask definition changes; the caller's definition does not.
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".go"), []byte("1"), 0o644))
	writeCheckpointTaskfile(t, dir, `version: '3'
tasks:
  default:
    deps: [slow]
  slow:
    deps: [chain]
    cmds:
      - echo slow > .slow.ready
      - 'if [ -f .go ]; then echo slow-done; else sleep 30; fi'
  chain:
    cmds:
      - task: sub
      - echo chain-done
  sub:
    cmds:
      - echo sub-v2
`)

	out, err := runCheckpointed(t, dir, true, nil, t.Context())
	require.NoError(t, err)
	assert.Contains(t, out, "sub-v2", "the changed subtask must re-execute")
	assert.Contains(t, out, "chain-done", "its caller must re-execute too")
}

func TestCheckpointDeferredSubtaskChangeInvalidatesCaller(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeCheckpointTaskfile(t, dir, `version: '3'
tasks:
  default:
    deps: [slow]
  slow:
    deps: [chain]
    cmds:
      - echo slow > .slow.ready
      - 'if [ -f .go ]; then echo slow-done; else sleep 30; fi'
  chain:
    cmds:
      - echo chain-done
      - defer:
          task: cleanup
  cleanup:
    cmds:
      - echo cleanup-v1
`)

	ctx, cancel := context.WithCancelCause(t.Context())
	go interruptWhen(t, filepath.Join(dir, ".slow.ready"), cancel)
	_, err := runCheckpointed(t, dir, false, nil, ctx)
	require.NoError(t, err)

	// The teardown task's definition changes; its caller must re-run.
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".go"), []byte("1"), 0o644))
	writeCheckpointTaskfile(t, dir, `version: '3'
tasks:
  default:
    deps: [slow]
  slow:
    deps: [chain]
    cmds:
      - echo slow > .slow.ready
      - 'if [ -f .go ]; then echo slow-done; else sleep 30; fi'
  chain:
    cmds:
      - echo chain-done
      - defer:
          task: cleanup
  cleanup:
    cmds:
      - echo cleanup-v2
`)

	out, err := runCheckpointed(t, dir, true, nil, t.Context())
	require.NoError(t, err)
	assert.Contains(t, out, "cleanup-v2", "changed deferred subtask must re-execute")
	assert.Contains(t, out, "chain-done", "its caller must re-execute")
	assert.Contains(t, out, "slow-done")
}

func TestCheckpointDeferredCommandRunsOnInterrupt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeCheckpointTaskfile(t, dir, `version: '3'
tasks:
  default:
    cmds:
      - cmd: echo working > .work
      - defer: echo cleaned > .cleanup
      - sleep 30
`)

	ctx, cancel := context.WithCancelCause(t.Context())
	go interruptWhen(t, filepath.Join(dir, ".work"), cancel)
	out, err := runCheckpointed(t, dir, false, nil, ctx)
	require.NoError(t, err)
	assert.Contains(t, out, "stopped at checkpoint")
	require.FileExists(t, filepath.Join(dir, ".cleanup"),
		"teardown commands must run before the node is left behind")

	// A node interrupted before its commands finished is never completed.
	data, err := os.ReadFile(checkpointFiles(t, dir)[0])
	require.NoError(t, err)
	assert.Contains(t, string(data), `"status": "interrupted"`)
	assert.NotContains(t, string(data), `"status": "completed"`)
}
