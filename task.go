package task

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"

	"golang.org/x/sync/errgroup"
	"mvdan.cc/sh/v3/interp"

	"github.com/go-task/task/v3/errors"
	"github.com/go-task/task/v3/internal/checkpoint"
	"github.com/go-task/task/v3/internal/env"
	"github.com/go-task/task/v3/internal/execext"
	"github.com/go-task/task/v3/internal/logger"
	"github.com/go-task/task/v3/internal/output"
	"github.com/go-task/task/v3/internal/slicesext"
	"github.com/go-task/task/v3/internal/sort"
	"github.com/go-task/task/v3/internal/summary"
	"github.com/go-task/task/v3/internal/templater"
	"github.com/go-task/task/v3/taskfile/ast"
)

const (
	// MaximumTaskCall is the max number of times a task can be called.
	// This exists to prevent infinite loops on cyclic dependencies
	MaximumTaskCall = 1000
)

// MatchingTask represents a task that matches a given call. It includes the
// task itself and a list of wildcards that were matched.
type MatchingTask struct {
	Task      *ast.Task
	Wildcards []string
}

// Run runs Task
func (e *Executor) Run(ctx context.Context, calls ...*Call) error {
	// check if given tasks exist
	for _, call := range calls {
		task, err := e.GetTask(call)
		if err != nil {
			if _, ok := err.(*errors.TaskNotFoundError); ok {
				if _, err := e.ListTasks(ListOptions{ListOnlyTasksWithDescriptions: true}); err != nil {
					return err
				}
			}
			return err
		}

		if task.Internal {
			if _, ok := err.(*errors.TaskNotFoundError); ok {
				if _, err := e.ListTasks(ListOptions{ListOnlyTasksWithDescriptions: true}); err != nil {
					return err
				}
			}
			return &errors.TaskInternalError{TaskName: call.Task}
		}
	}

	if e.Summary {
		for i, c := range calls {
			compiledTask, err := e.FastCompiledTask(c)
			if err != nil {
				return nil
			}
			summary.PrintSpaceBetweenSummaries(e.Logger, i)
			summary.PrintTask(e.Logger, compiledTask)
		}
		return nil
	}

	// Prompt for all required vars from deps upfront (parallel execution)
	if err := e.promptDepsVars(calls); err != nil {
		return err
	}

	regularCalls, watchCalls, err := e.splitRegularAndWatchCalls(calls...)
	if err != nil {
		return err
	}

	var cp *checkpoint.Manager
	if e.checkpointEnabled() && len(watchCalls) == 0 {
		cp = e.setupCheckpoint(regularCalls)
		e.checkpoint = cp
	}

	g := &errgroup.Group{}
	if e.Failfast {
		g, ctx = errgroup.WithContext(ctx)
	}
	for _, c := range regularCalls {
		if e.Parallel {
			g.Go(func() error { return e.RunTask(ctx, c) })
		} else {
			if err := e.RunTask(ctx, c); err != nil {
				return e.runFinished(cp, err)
			}
		}
	}
	if err := g.Wait(); err != nil {
		return e.runFinished(cp, err)
	}
	if err := e.runFinished(cp, nil); err != nil {
		return err
	}

	if len(watchCalls) > 0 {
		return e.watchTasks(watchCalls...)
	}

	return nil
}

// runFinished finalizes a run that used a checkpoint. A run stopped at a
// checkpoint ends successfully: its progress has been persisted and the user
// is expected to continue with resume. A failed run keeps the checkpoint so
// its failed, interrupted and cancelled nodes can be re-executed on resume. A
// fully successful run discards the checkpoint, as it has no further reuse.
func (e *Executor) runFinished(cp *checkpoint.Manager, runErr error) error {
	if cp == nil {
		return runErr
	}
	if runErr != nil {
		if cp.Interrupted() {
			e.Logger.Outf(logger.Yellow,
				"task: run stopped at checkpoint; run again with --resume to continue from the completed tasks\n")
			return nil
		}
		return runErr
	}
	if err := cp.Discard(); err != nil {
		e.Logger.VerboseErrf(logger.Yellow, "task: unable to remove checkpoint: %v\n", err)
	}
	return nil
}

func (e *Executor) splitRegularAndWatchCalls(calls ...*Call) (regularCalls []*Call, watchCalls []*Call, err error) {
	for _, c := range calls {
		t, err := e.GetTask(c)
		if err != nil {
			return nil, nil, err
		}

		if e.Watch || t.Watch {
			watchCalls = append(watchCalls, c)
		} else {
			regularCalls = append(regularCalls, c)
		}
	}
	return regularCalls, watchCalls, err
}

// RunTask runs a task by its name
func (e *Executor) RunTask(ctx context.Context, call *Call) error {
	_, err := e.runTask(ctx, call)
	return err
}

// runTask compiles the task call and runs its node. The returned bool reports
// whether the node was skipped (up-to-date, restored from a checkpoint, or not
// applicable to the current run), so callers can tell whether a dependency
// actually executed.
func (e *Executor) runTask(ctx context.Context, call *Call) (bool, error) {
	// Inject prompted vars into call if available
	if e.promptedVars != nil {
		if call.Vars == nil {
			call.Vars = ast.NewVars()
		}
		for name, v := range e.promptedVars.All() {
			// Only inject if not already set in call
			if _, ok := call.Vars.Get(name); !ok {
				call.Vars.Set(name, v)
			}
		}
	}

	t, err := e.FastCompiledTask(call)
	if err != nil {
		return false, err
	}
	if !shouldRunOnCurrentPlatform(t.Platforms) {
		e.Logger.VerboseOutf(logger.Yellow, `task: %q not for current platform - ignored\n`, call.Task)
		return true, nil
	}

	// Check required vars early (before template compilation) if we can't prompt.
	// This gives a clear "missing required variables" error instead of a template error.
	if !e.canPrompt() {
		if err := e.areTaskRequiredVarsSet(t); err != nil {
			return false, err
		}
	}

	t, err = e.CompiledTask(call)
	if err != nil {
		return false, err
	}

	// Check if condition after CompiledTask so dynamic variables are resolved
	if strings.TrimSpace(t.If) != "" {
		if err := execext.RunCommand(ctx, &execext.RunCommandOptions{
			Command: t.If,
			Dir:     t.Dir,
			Env:     env.Get(t),
		}); err != nil {
			e.Logger.VerboseOutf(logger.Yellow, "task: if condition not met - skipped: %q\n", call.Task)
			return true, nil
		}
	}

	// Prompt for missing required vars after if check (avoid prompting if task won't run)
	prompted, err := e.promptTaskVars(t, call)
	if err != nil {
		return false, err
	}
	if prompted {
		// Recompile with the new vars
		t, err = e.FastCompiledTask(call)
		if err != nil {
			return false, err
		}
	}

	if err := e.areTaskRequiredVarsSet(t); err != nil {
		return false, err
	}

	if err := e.areTaskRequiredVarsAllowedValuesSet(t); err != nil {
		return false, err
	}

	if !e.Watch && atomic.AddInt32(e.taskCallCount[t.Task], 1) >= MaximumTaskCall {
		return false, &errors.TaskCalledTooManyTimesError{
			TaskName:        t.Task,
			MaximumTaskCall: MaximumTaskCall,
		}
	}

	release := e.acquireConcurrencyLimit()
	defer release()

	restored, err := e.runNode(ctx, t, call)
	if err != nil {
		return false, &errors.TaskRunError{TaskName: t.Name(), Err: err}
	}
	return restored, nil
}

// runNode executes one graph node (a single compiled task invocation) and
// reports whether it was restored from the checkpoint instead of executing.
// Only the caller that actually runs the node (the "leader") records its
// outcome; callers joined onto a duplicate execution share that outcome.
func (e *Executor) runNode(ctx context.Context, t *ast.Task, call *Call) (bool, error) {
	cp := e.checkpoint

	var nodeKey string
	if cp != nil {
		key, err := checkpoint.DefHash(t)
		if err != nil {
			return false, err
		}
		nodeKey = key
		// Record this invocation on the enclosing node's command-subtask
		// recorder (dependency calls run without a recorder).
		if recorder := subtaskRecorderFromContext(ctx); recorder != nil {
			recorder.add(nodeKey)
		}
	}

	var subtasks *subtaskRecorder
	// restored is set by the execution closure on this caller's behalf.
	restored := false
	leader, joinedRestored, err := e.startExecution(ctx, t, func(ctx context.Context) error {
		e.Logger.VerboseErrf(logger.Magenta, "task: %q started\n", call.Task)

		bodyCtx := ctx
		if cp != nil {
			bodyCtx, subtasks = withSubtaskRecorder(ctx)
		}

		depsReran, err := e.runDeps(withoutSubtaskRecorder(bodyCtx), t)
		if err != nil {
			return err
		}

		// Resume: skip the node only when it is still consistent with the
		// checkpoint and none of its dependencies had to be re-executed.
		// --force/--force-all opts out of restore just as it opts out of the
		// up-to-date check below.
		if cp != nil && !(e.ForceAll || (!call.Indirect && e.Force)) {
			seen := make(map[string]bool)
			if e.cpRestorable(t, nodeKey, depsReran, seen) {
				restored = true
				if e.Verbose || (!call.Silent && !t.IsSilent() && !e.Taskfile.Silent && !e.Silent) {
					name := t.Name()
					if e.OutputStyle.Name == "prefixed" {
						name = t.Prefix
					}
					e.Logger.Errf(logger.Magenta, "task: Task %q restored from checkpoint\n", name)
				}
				return nil
			}
		}

		skipFingerprinting := e.ForceAll || (!call.Indirect && e.Force)
		ctx = bodyCtx
		if !skipFingerprinting {
			if err := ctx.Err(); err != nil {
				return err
			}

			preCondMet, err := e.areTaskPreconditionsMet(ctx, t)
			if err != nil {
				return err
			}

			upToDate, err := e.fingerprinter().UpToDate(ctx, t)
			if err != nil {
				return err
			}

			if upToDate && preCondMet {
				if e.Verbose || (!call.Silent && !t.IsSilent() && !e.Taskfile.Silent && !e.Silent) {
					name := t.Name()
					if e.OutputStyle.Name == "prefixed" {
						name = t.Prefix
					}
					e.Logger.Errf(logger.Magenta, "task: Task %q is up to date\n", name)
				}
				// Nothing was executed in this run, so the node must not be
				// recorded as newly completed; its dependents did not have
				// to wait for a re-execution either.
				restored = true
				return nil
			}
		}

		for _, p := range t.Prompt {
			if p != "" && !e.Dry {
				if err := e.Logger.Prompt(logger.Yellow, p, "n", "y", "yes"); errors.Is(err, logger.ErrNoTerminal) {
					return &errors.TaskCancelledNoTerminalError{TaskName: call.Task}
				} else if errors.Is(err, logger.ErrPromptCancelled) {
					return &errors.TaskCancelledByUserError{TaskName: call.Task}
				} else if err != nil {
					return err
				}
			}
		}

		if err := e.mkdir(t); err != nil {
			e.Logger.Errf(logger.Red, "task: cannot make directory %q: %v\n", t.Dir, err)
		}

		var deferredExitCode uint8

		for i := range t.Cmds {
			if t.Cmds[i].Defer {
				// ctx carries the node's subtask recorder; runDeferred keeps
				// its values while detaching from cancellation so teardown is
				// not cut short by the interruption that triggered it.
				defer e.runDeferred(ctx, t, call, i, t.Vars, &deferredExitCode)
				continue
			}

			if err := e.runCommand(ctx, t, call, i); err != nil {
				if err2 := e.statusOnError(t); err2 != nil {
					e.Logger.VerboseErrf(logger.Yellow, "task: error cleaning status on error: %v\n", err2)
				}

				if t.IgnoreError && isCommandFailure(err) {
					e.Logger.VerboseErrf(logger.Yellow, "task: task error ignored: %v\n", err)
					continue
				}

				e.Logger.VerboseErrf(logger.Red, "task: %q failed: %v\n", call.Task, err)

				if exitCode, ok := errors.AsType[interp.ExitStatus](err); ok {
					deferredExitCode = uint8(exitCode)
				} else if _, ok := errors.AsType[*errors.TaskTimeoutError](err); ok {
					deferredExitCode = errors.TimeoutExitCode
				}

				return err
			}
		}
		e.Logger.VerboseErrf(logger.Magenta, "task: %q finished\n", call.Task)
		return nil
	})
	if !leader {
		// Callers joined onto a duplicate execution share its outcome; the
		// leader is responsible for recording the checkpoint.
		restored = joinedRestored
	}
	if err != nil {
		if cp != nil && leader {
			if recErr := cp.Record(nodeKey, e.classifyNodeOutcome(ctx, err)); recErr != nil {
				e.Logger.VerboseErrf(logger.Yellow, "task: unable to persist checkpoint: %v\n", recErr)
			}
		}
		return false, err
	}
	if cp != nil && leader && !restored {
		// Task commands and deferred commands have both finished. The node is
		// only recorded once the declared outputs exist and its status check
		// passes, so a later resume never skips a half-finished node.
		if err := e.verifyCheckpointCompletion(ctx, t); err != nil {
			if recErr := cp.Record(nodeKey, e.classifyNodeOutcome(ctx, err)); recErr != nil {
				e.Logger.VerboseErrf(logger.Yellow, "task: unable to persist checkpoint: %v\n", recErr)
			}
			return false, err
		}
		var subKeys []string
		if subtasks != nil {
			subKeys = subtasks.keys()
		}
		sources, srcErr := cp.SourcesHash(t)
		if srcErr != nil {
			// Unknown source identity means the record cannot be verified
			// later; do not block the run, but leave the node unrecorded so
			// a resume re-executes it.
			e.Logger.VerboseErrf(logger.Yellow, "task: cannot bind sources to checkpoint for %q: %v\n", t.Name(), srcErr)
			return restored, nil
		}
		if err := cp.Completed(nodeKey, sources, subKeys); err != nil {
			e.Logger.VerboseErrf(logger.Yellow, "task: unable to persist checkpoint: %v\n", err)
		}
	}
	return restored, nil
}

func (e *Executor) mkdir(t *ast.Task) error {
	if t.Dir == "" {
		return nil
	}

	mutex := e.mkdirMutexMap[t.Task]
	mutex.Lock()
	defer mutex.Unlock()

	if _, err := os.Stat(t.Dir); os.IsNotExist(err) {
		if err := os.MkdirAll(t.Dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// runDeps runs the dependencies of a task. It also reports whether any of
// them actually executed in this run (as opposed to being up-to-date or
// restored from a checkpoint); a re-executed dependency invalidates the
// current node for checkpoint restore.
func (e *Executor) runDeps(ctx context.Context, t *ast.Task) (bool, error) {
	g := &errgroup.Group{}
	if e.Failfast || t.Failfast {
		g, ctx = errgroup.WithContext(ctx)
	}

	reacquire := e.releaseConcurrencyLimit()
	defer reacquire()

	var reexecuted atomic.Bool

	for _, d := range t.Deps {
		g.Go(func() error {
			depCtx := ctx
			var timeout *errors.TaskTimeoutError
			if d.Timeout > 0 {
				timeout = &errors.TaskTimeoutError{TaskName: d.Task, Timeout: d.Timeout}
				var cancel context.CancelFunc
				depCtx, cancel = context.WithTimeoutCause(ctx, d.Timeout, timeout)
				defer cancel()
			}

			restored, err := e.runTask(depCtx, &Call{Task: d.Task, Vars: d.Vars, Silent: d.Silent, Indirect: true})
			if err != nil && timedOut(depCtx, timeout) {
				return timeout
			}
			if err == nil && !restored {
				reexecuted.Store(true)
			}
			return err
		})
	}

	// Wait first: reading the flag in the same return expression would
	// evaluate it before g.Wait has joined the dependency goroutines.
	err := g.Wait()
	return reexecuted.Load(), err
}

func (e *Executor) runDeferred(parent context.Context, t *ast.Task, call *Call, i int, vars *ast.Vars, deferredExitCode *uint8) {
	// Teardown keeps running even when the run was interrupted: start from a
	// context that inherits parent's values (e.g. the subtask recorder) but
	// none of its cancellation.
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()

	cmd := t.Cmds[i]
	cache := &templater.Cache{Vars: vars}
	extra := map[string]any{}

	if deferredExitCode != nil && *deferredExitCode > 0 {
		extra["EXIT_CODE"] = fmt.Sprintf("%d", *deferredExitCode)
	}

	// Resolve template with secrets masked for logging
	cmd.LogCmd = templater.MaskSecretsWithExtra(cmd.Cmd, vars, extra)
	cmd.Cmd = templater.ReplaceWithExtra(cmd.Cmd, cache, extra)
	cmd.Task = templater.ReplaceWithExtra(cmd.Task, cache, extra)
	cmd.If = templater.ReplaceWithExtra(cmd.If, cache, extra)
	cmd.Vars = templater.ReplaceVarsWithExtra(cmd.Vars, cache, extra)

	if err := e.runCommand(ctx, t, call, i); err != nil {
		e.Logger.VerboseErrf(logger.Yellow, "task: ignored error in deferred cmd: %s\n", err.Error())
	}
}

func (e *Executor) runCommand(ctx context.Context, t *ast.Task, call *Call, i int) error {
	cmd := t.Cmds[i]

	// In place before the if condition, which would otherwise run unbounded.
	var timeout *errors.TaskTimeoutError
	if cmd.Timeout > 0 {
		timeout = &errors.TaskTimeoutError{TaskName: t.Name(), Timeout: cmd.Timeout}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, cmd.Timeout, timeout)
		defer cancel()
	}

	// Check if condition for any command type
	if strings.TrimSpace(cmd.If) != "" {
		if err := execext.RunCommand(ctx, &execext.RunCommandOptions{
			Command: cmd.If,
			Dir:     t.Dir,
			Env:     env.Get(t),
		}); err != nil {
			if timedOut(ctx, timeout) {
				return timeout
			}
			e.Logger.VerboseOutf(logger.Yellow, "task: [%s] if condition not met - skipped\n", t.Name())
			return nil
		}
	}

	switch {
	case cmd.Task != "":
		reacquire := e.releaseConcurrencyLimit()
		defer reacquire()

		err := e.RunTask(ctx, &Call{Task: cmd.Task, Vars: cmd.Vars, Silent: cmd.Silent, Indirect: true})
		if err != nil && timedOut(ctx, timeout) {
			err = timeout
		}
		if cmd.IgnoreError && isCommandFailure(err) {
			e.Logger.VerboseErrf(logger.Yellow, "task: [%s] task error ignored: %v\n", t.Name(), err)
			return nil
		}
		return err
	case cmd.Cmd != "":
		if !shouldRunOnCurrentPlatform(cmd.Platforms) {
			e.Logger.VerboseOutf(logger.Yellow, "task: [%s] %s not for current platform - ignored\n", t.Name(), cmd.LogCmd)
			return nil
		}

		if e.Verbose || (!call.Silent && !cmd.Silent && !t.IsSilent() && !e.Taskfile.Silent && !e.Silent) {
			e.Logger.Errf(logger.Green, "task: [%s] %s\n", t.Name(), cmd.LogCmd)
		}

		if e.Dry {
			return nil
		}

		outputWrapper := e.Output
		if t.Interactive {
			outputWrapper = output.Interleaved{}
		}
		vars, err := e.Compiler.FastGetVariables(t, call)
		outputTemplater := &templater.Cache{Vars: vars}
		if err != nil {
			return fmt.Errorf("task: failed to get variables: %w", err)
		}
		stdOut, stdErr, closer := outputWrapper.WrapWriter(e.Stdout, e.Stderr, t.Prefix, outputTemplater)

		err = execext.RunCommand(ctx, &execext.RunCommandOptions{
			Command:   cmd.Cmd,
			Dir:       t.Dir,
			Env:       env.Get(t),
			PosixOpts: slicesext.UniqueJoin(e.Taskfile.Set, t.Set, cmd.Set),
			BashOpts:  slicesext.UniqueJoin(e.Taskfile.Shopt, t.Shopt, cmd.Shopt),
			Stdin:     e.Stdin,
			Stdout:    stdOut,
			Stderr:    stdErr,
		})
		if closeErr := closer(err); closeErr != nil {
			e.Logger.Errf(logger.Red, "task: unable to close writer: %v\n", closeErr)
		}
		if err != nil && timedOut(ctx, timeout) {
			err = timeout
		}
		if cmd.IgnoreError && isCommandFailure(err) {
			e.Logger.VerboseErrf(logger.Yellow, "task: [%s] command error ignored: %v\n", t.Name(), err)
			return nil
		}
		return err
	default:
		return nil
	}
}

// isCommandFailure reports whether the command failed on its own terms - a
// non-zero exit status or its timeout - rather than Task failing to run it.
func isCommandFailure(err error) bool {
	if _, ok := errors.AsType[interp.ExitStatus](err); ok {
		return true
	}
	_, ok := errors.AsType[*errors.TaskTimeoutError](err)
	return ok
}

// timedOut reports whether ctx was cancelled by the given timeout rather than by
// an inherited deadline, which a derived context reports as its own.
func timedOut(ctx context.Context, timeout *errors.TaskTimeoutError) bool {
	return timeout != nil && errors.Is(context.Cause(ctx), timeout)
}

// executionState is the outcome of a task execution, shared with the callers
// that join it. err is written before done is closed; read it only once closed.
type executionState struct {
	done     chan struct{}
	err      error
	restored bool
}

// startExecution deduplicates concurrent executions of the same task. It
// reports whether this caller was the leader that ran the task and whether
// the execution was skipped (up-to-date or restored from a checkpoint).
func (e *Executor) startExecution(
	ctx context.Context,
	t *ast.Task,
	execute func(ctx context.Context) error,
) (leader bool, restored bool, err error) {
	h, err := e.GetHash(t)
	if err != nil {
		return false, false, err
	}

	if h == "" || t.Watch {
		return true, restored, execute(ctx)
	}

	e.executionHashesMutex.Lock()

	if other, ok := e.executionHashes[h]; ok {
		e.executionHashesMutex.Unlock()
		e.Logger.VerboseErrf(logger.Magenta, "task: skipping execution of task: %s\n", h)

		// Release our execution slot to avoid blocking other tasks while we wait
		reacquire := e.releaseConcurrencyLimit()
		defer reacquire()

		// A finished execution wins even if our context is done: there is
		// nothing left to wait for, and select would otherwise pick at random.
		select {
		case <-other.done:
			return false, other.restored, other.err
		default:
		}

		select {
		case <-other.done:
			// Its outcome is ours. Returning nil would hide an execution that
			// failed, or that another caller's timeout killed.
			return false, other.restored, other.err
		case <-ctx.Done():
			// We did not start it, so we can only stop waiting. Report the cause
			// so that our own timeout surfaces as one.
			return false, false, context.Cause(ctx)
		}
	}

	state := &executionState{done: make(chan struct{})}
	e.executionHashes[h] = state
	e.executionHashesMutex.Unlock()

	defer close(state.done)
	state.err = execute(ctx)
	state.restored = restored
	return true, restored, state.err
}

// FindMatchingTasks returns a list of tasks that match the given call. A task
// matches a call if its name is equal to the call's task name, or one of aliases, or if it matches
// a wildcard pattern. The function returns a list of MatchingTask structs, each
// containing a task and a list of wildcards that were matched.
// If multiple tasks match due to aliases, a TaskNameConflictError is returned.
func (e *Executor) FindMatchingTasks(call *Call) ([]*MatchingTask, error) {
	if call == nil {
		return nil, nil
	}
	var matchingTasks []*MatchingTask
	// If there is a direct match, return it
	if task, ok := e.Taskfile.Tasks.Get(call.Task); ok {
		matchingTasks = append(matchingTasks, &MatchingTask{Task: task, Wildcards: nil})
		return matchingTasks, nil
	}
	var aliasedTasks []string
	for task := range e.Taskfile.Tasks.Values(nil) {
		if slices.Contains(task.Aliases, call.Task) {
			aliasedTasks = append(aliasedTasks, task.Task)
			matchingTasks = append(matchingTasks, &MatchingTask{Task: task, Wildcards: nil})
		}
	}

	if len(aliasedTasks) == 1 {
		return matchingTasks, nil
	}

	// If we found multiple tasks
	if len(aliasedTasks) > 1 {
		return nil, &errors.TaskNameConflictError{
			Call:      call.Task,
			TaskNames: aliasedTasks,
		}
	}

	// Attempt a wildcard match
	for _, value := range e.Taskfile.Tasks.All(nil) {
		if match, wildcards := value.WildcardMatch(call.Task); match {
			matchingTasks = append(matchingTasks, &MatchingTask{
				Task:      value,
				Wildcards: wildcards,
			})
		}
	}
	return matchingTasks, nil
}

// GetTask will return the task with the name matching the given call from the taskfile.
// If no task is found, it will search for tasks with a matching alias.
// If multiple tasks contain the same alias or no matches are found an error is returned.
func (e *Executor) GetTask(call *Call) (*ast.Task, error) {
	// Search for a matching task
	matchingTasks, err := e.FindMatchingTasks(call)
	if err != nil {
		return nil, err
	}

	if len(matchingTasks) > 0 {
		if call.Vars == nil {
			call.Vars = ast.NewVars()
		}
		call.Vars.Set("MATCH", ast.Var{Value: matchingTasks[0].Wildcards})
		return matchingTasks[0].Task, nil
	}

	// If we found no tasks
	didYouMean := ""
	if !e.DisableFuzzy {
		e.fuzzyModelOnce.Do(e.setupFuzzyModel)
		if e.fuzzyModel != nil {
			didYouMean = e.fuzzyModel.SpellCheck(call.Task)
		}
	}
	return nil, &errors.TaskNotFoundError{
		TaskName:   call.Task,
		DidYouMean: didYouMean,
	}
}

type FilterFunc func(task *ast.Task) bool

func (e *Executor) GetTaskList(filters ...FilterFunc) ([]*ast.Task, error) {
	tasks := make([]*ast.Task, 0, e.Taskfile.Tasks.Len())

	// Create an error group to wait for each task to be compiled
	var g errgroup.Group

	// Sort the tasks
	if e.TaskSorter == nil {
		e.TaskSorter = sort.AlphaNumericWithRootTasksFirst
	}

	// Filter tasks based on the given filter functions
	for task := range e.Taskfile.Tasks.Values(e.TaskSorter) {
		var shouldFilter bool
		for _, filter := range filters {
			if filter(task) {
				shouldFilter = true
			}
		}
		if !shouldFilter {
			tasks = append(tasks, task)
		}
	}

	// Compile the list of tasks
	for i := range tasks {
		g.Go(func() error {
			compiledTask, err := e.CompiledTaskForTaskList(&Call{Task: tasks[i].Task})
			if err != nil {
				return err
			}
			tasks[i] = compiledTask
			return nil
		})
	}

	// Wait for all the go routines to finish
	if err := g.Wait(); err != nil {
		return nil, err
	}

	return tasks, nil
}

// FilterOutNoDesc removes all tasks that do not contain a description.
func FilterOutNoDesc(task *ast.Task) bool {
	return task.Desc == ""
}

// FilterOutInternal removes all tasks that are marked as internal.
func FilterOutInternal(task *ast.Task) bool {
	return task.Internal
}

func shouldRunOnCurrentPlatform(platforms []*ast.Platform) bool {
	if len(platforms) == 0 {
		return true
	}
	for _, p := range platforms {
		if (p.OS == "" || p.OS == runtime.GOOS) && (p.Arch == "" || p.Arch == runtime.GOARCH) {
			return true
		}
	}
	return false
}
