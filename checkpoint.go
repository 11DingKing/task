package task

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/go-task/task/v3/internal/checkpoint"
	"github.com/go-task/task/v3/internal/fingerprint"
	"github.com/go-task/task/v3/internal/logger"
	"github.com/go-task/task/v3/taskfile/ast"
)

// checkpointEnabled reports whether this run should record into and restore
// from a checkpoint. The feature is strictly opt-in and stays out of the way
// of dry runs, summaries and watch mode.
func (e *Executor) checkpointEnabled() bool {
	return (e.Breakpoint || e.Resume) &&
		!e.Dry &&
		!e.Watch &&
		!e.Summary &&
		e.Taskfile != nil &&
		e.Taskfile.Tasks.Len() > 0
}

// setupCheckpoint opens (or starts) the checkpoint bound to this invocation.
func (e *Executor) setupCheckpoint(calls []*Call) *checkpoint.Manager {
	binding := checkpoint.Binding{
		Calls:        make([]checkpoint.CallBinding, 0, len(calls)),
		Vars:         e.InvocationVars,
		Entrypoint:   e.Entrypoint,
		Dir:          e.Dir,
		TaskfileHash: e.taskfileCheckpointHash(),
	}
	for _, call := range calls {
		var vars map[string]any
		if call.Vars != nil {
			vars = call.Vars.ToCacheMap()
		}
		binding.Calls = append(binding.Calls, checkpoint.CallBinding{
			Task: call.Task,
			Vars: vars,
		})
	}
	m := checkpoint.Open(e.TempDir.Fingerprint, e.Resume, binding, e.Logger)
	if e.interrupted.Load() {
		m.MarkInterrupted()
	}
	return m
}

func (e *Executor) taskfileCheckpointHash() string {
	h, err := checkpoint.TaskfileHash(e.Taskfile)
	if err != nil {
		e.Logger.VerboseErrf(logger.Yellow, "task: unable to hash taskfile for checkpoint: %v\n", err)
		return ""
	}
	return h
}

// verifyCheckpointCompletion certifies the completion conditions required
// before a node may be recorded as completed: the declared outputs exist and
// the task status commands, if any, report up-to-date. The task and deferred
// commands have already finished successfully at this point.
func (e *Executor) verifyCheckpointCompletion(ctx context.Context, t *ast.Task) error {
	for _, g := range t.Generates {
		if g.Negate {
			continue
		}
		matches, err := fingerprint.Globs(t.Dir, []*ast.Glob{g}, false)
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			return fmt.Errorf(`task: %q did not generate expected output %q`, t.Name(), g.Glob)
		}
	}

	if len(t.Status) > 0 {
		upToDate, err := fingerprint.NewStatusChecker(e.Logger).IsUpToDate(ctx, t)
		if err != nil {
			return err
		}
		if !upToDate {
			return fmt.Errorf(`task: %q status check failed after execution`, t.Name())
		}
	}
	return nil
}

// classifyNodeOutcome maps the error returned by a node execution to the
// checkpoint status recorded for it.
func (e *Executor) classifyNodeOutcome(ctx context.Context, err error) checkpoint.Status {
	if e.checkpoint.Interrupted() || checkpoint.IsInterruptedCause(ctx) {
		// The interruption may have reached us through the context only
		// (e.g. before the manager was flagged); make sure the run as a whole
		// is marked interrupted so it finalizes gracefully.
		e.checkpoint.MarkInterrupted()
		return checkpoint.StatusInterrupted
	}
	// A command that failed on its own terms (non-zero exit or timeout) is a
	// genuine failure even when failfast happens to cancel the context around
	// it at the same time.
	if isCommandFailure(err) {
		return checkpoint.StatusFailed
	}
	if ctx.Err() != nil {
		return checkpoint.StatusCancelled
	}
	return checkpoint.StatusFailed
}

// cpRestorable decides whether a node can be skipped by restoring it from the
// checkpoint. It is restorable only when this invocation loaded a usable
// checkpoint, none of its direct dependencies had to re-run, the node was
// recorded as completed with the same definition, variables and sources, the
// outputs it declares still exist, and every task it invokes as a command is
// restorable too (so a change in a called task invalidates the caller).
func (e *Executor) cpRestorable(t *ast.Task, key string, depsReran bool, seen map[string]bool) bool {
	cp := e.checkpoint
	if cp == nil || !cp.Usable() || depsReran {
		return false
	}
	if v, ok := seen[key]; ok {
		// A key encountered twice in the recursion is either a shared
		// dependency (memoized answer) or a cycle (never completed, so not
		// restorable).
		return v
	}
	seen[key] = false

	node, ok := cp.Node(key)
	if !ok || node.Status != checkpoint.StatusCompleted {
		return false
	}

	sources, err := cp.SourcesHash(t)
	if err != nil {
		e.Logger.VerboseErrf(logger.Yellow, "task: cannot verify sources of %q for checkpoint: %v\n", t.Name(), err)
		return false
	}
	if sources != node.SourcesHash {
		return false
	}

	for _, g := range t.Generates {
		if g.Negate {
			continue
		}
		matches, err := fingerprint.Globs(t.Dir, []*ast.Glob{g}, false)
		if err != nil || len(matches) == 0 {
			return false
		}
	}

	for _, c := range t.Cmds {
		if c == nil || c.Task == "" {
			continue
		}
		sub, err := e.CompiledTask(&Call{Task: c.Task, Vars: c.Vars, Silent: c.Silent, Indirect: true})
		if err != nil {
			return false
		}
		subKey, err := checkpoint.DefHash(sub)
		if err != nil {
			return false
		}
		if !slices.Contains(node.Subtasks, subKey) {
			return false
		}
		if !e.cpRestorable(sub, subKey, false, seen) {
			return false
		}
	}

	seen[key] = true
	return true
}

// subtaskRecorder collects the identities of tasks invoked through the
// commands of the node being executed. They are folded into the node's
// checkpoint record so a change in a called task invalidates the caller.
type subtaskRecorder struct {
	mu        sync.Mutex
	collected []string
}

func (r *subtaskRecorder) add(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.collected = append(r.collected, key)
}

func (r *subtaskRecorder) keys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.collected)
}

type subtaskRecorderCtxKey struct{}

func withSubtaskRecorder(ctx context.Context) (context.Context, *subtaskRecorder) {
	r := &subtaskRecorder{}
	return context.WithValue(ctx, subtaskRecorderCtxKey{}, r), r
}

func subtaskRecorderFromContext(ctx context.Context) *subtaskRecorder {
	r, _ := ctx.Value(subtaskRecorderCtxKey{}).(*subtaskRecorder)
	return r
}

// withoutSubtaskRecorder detaches the recorder carried by ctx. Dependency
// edges are accounted for separately (via the deps-reran flag) and must not
// be recorded as command-invoked subtasks.
func withoutSubtaskRecorder(ctx context.Context) context.Context {
	if subtaskRecorderFromContext(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, subtaskRecorderCtxKey{}, (*subtaskRecorder)(nil))
}
