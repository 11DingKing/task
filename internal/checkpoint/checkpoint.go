// Package checkpoint implements verifiable run checkpoints that let a large
// task graph be resumed from the nodes that had fully completed instead of
// re-running it from the beginning.
//
// A checkpoint is only used when breakpoint mode is explicitly enabled. Every
// record binds the identity of the node it describes - its resolved task
// definition, its effective variables and the identity of its source files -
// so on resume nodes are skipped only when they still match the checkpoint.
package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/zeebo/xxh3"

	"github.com/go-task/task/v3/internal/fingerprint"
	"github.com/go-task/task/v3/internal/logger"
	"github.com/go-task/task/v3/taskfile/ast"
)

// ErrInterrupted is the context cause used when a run is actively stopped at
// a checkpoint by the user. Nodes stopped this way are recorded as
// interrupted rather than failed.
var ErrInterrupted = errors.New("task: interrupted at checkpoint")

// formatVersion is the version of the checkpoint file format understood by
// this package. A file with another version cannot be verified and is
// discarded.
const formatVersion = 1

// Status is the recorded outcome of a single graph node.
type Status string

const (
	// StatusCompleted means the task commands, deferred commands, declared
	// outputs and status checks all succeeded.
	StatusCompleted Status = "completed"
	// StatusInterrupted means the node was running when the run was stopped.
	StatusInterrupted Status = "interrupted"
	// StatusFailed means a command or the completion verification failed.
	StatusFailed Status = "failed"
	// StatusCancelled means the node was stopped because another node failed
	// (e.g. under failfast) without failing on its own terms.
	StatusCancelled Status = "cancelled"
)

// CallBinding identifies one root task call: its name and the variables it
// was invoked with.
type CallBinding struct {
	Task string         `json:"task"`
	Vars map[string]any `json:"vars,omitempty"`
}

// Binding is the verifiable identity of a whole run. The checkpoint is
// reusable only when it still describes the same invocation.
type Binding struct {
	Calls        []CallBinding `json:"calls"`
	Vars         map[string]any `json:"vars,omitempty"`
	Entrypoint   string        `json:"entrypoint"`
	Dir          string        `json:"dir"`
	TaskfileHash string        `json:"taskfile_hash"`
}

// Node is the persisted state of a single task node.
type Node struct {
	Status      Status    `json:"status"`
	DefHash     string    `json:"def_hash"`
	SourcesHash string    `json:"sources_hash,omitempty"`
	Subtasks    []string  `json:"subtasks,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type fileData struct {
	Version int              `json:"version"`
	Binding Binding          `json:"binding"`
	Nodes   map[string]*Node `json:"nodes"`
}

// Manager owns the checkpoint of one run. All methods are safe for concurrent
// use, as task graphs can be executed in parallel.
type Manager struct {
	path    string
	log     *logger.Logger
	mu      sync.Mutex
	nodes   map[string]*Node
	binding Binding

	// usable reports whether a verifiable checkpoint was loaded for resume.
	usable atomic.Bool

	interrupted atomic.Bool

	sourceCache sync.Map // defHash -> sourcesHash
}

// Open loads the checkpoint for the given binding. When resume is false a new,
// empty checkpoint is started (any stale file is overwritten). When resume is
// true but the file is missing, corrupt, of an unknown version or bound to a
// different invocation, the checkpoint is considered unverifiable and an
// empty, non-usable manager is returned so the run starts from scratch.
func Open(dir string, resume bool, binding Binding, log *logger.Logger) *Manager {
	m := &Manager{
		path:    filepath.Join(dir, "checkpoint", FileName(binding)),
		log:     log,
		nodes:   map[string]*Node{},
		binding: binding,
	}
	if !resume {
		return m
	}
	if err := m.load(); err != nil {
		log.VerboseOutf(logger.Yellow, "task: checkpoint cannot be verified (%v); running from scratch\n", err)
		return m
	}
	m.usable.Store(true)
	log.VerboseOutf(logger.Magenta, "task: resuming from checkpoint %s\n", m.path)
	return m
}

// Usable reports whether completed nodes may be restored from this checkpoint.
func (m *Manager) Usable() bool {
	return m.usable.Load()
}

// Node returns the persisted state of a node.
func (m *Manager) Node(key string) (*Node, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[key]
	return n, ok
}

// Completed records a node as fully completed and persists the checkpoint.
func (m *Manager) Completed(key, sourcesHash string, subtasks []string) error {
	m.mu.Lock()
	m.nodes[key] = &Node{
		Status:      StatusCompleted,
		DefHash:     key,
		SourcesHash: sourcesHash,
		Subtasks:    dedupSorted(subtasks),
		UpdatedAt:   time.Now().UTC(),
	}
	err := m.flushLocked()
	m.mu.Unlock()
	return err
}

// Record stores a non-completed outcome for a node and persists the
// checkpoint.
func (m *Manager) Record(key string, status Status) error {
	m.mu.Lock()
	m.nodes[key] = &Node{
		Status:    status,
		DefHash:   key,
		UpdatedAt: time.Now().UTC(),
	}
	err := m.flushLocked()
	m.mu.Unlock()
	return err
}

// MarkInterrupted flags the whole run as stopped at a checkpoint.
func (m *Manager) MarkInterrupted() {
	m.interrupted.Store(true)
}

// Interrupted reports whether the run was stopped at a checkpoint.
func (m *Manager) Interrupted() bool {
	return m.interrupted.Load()
}

// IsInterruptedCause reports whether ctx was cancelled by an explicit
// checkpoint interruption.
func IsInterruptedCause(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrInterrupted)
}

// Discard removes the checkpoint. It is called once the whole run succeeded,
// as a completed run no longer has any reuse for it.
func (m *Manager) Discard() error {
	if err := os.Remove(m.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Best effort: remove the directory when no checkpoints are left in it.
	_ = os.Remove(filepath.Dir(m.path))
	return nil
}

func (m *Manager) load() error {
	data, err := os.ReadFile(m.path)
	if err != nil {
		return fmt.Errorf("reading checkpoint: %w", err)
	}
	var fd fileData
	if err := json.Unmarshal(data, &fd); err != nil {
		return fmt.Errorf("parsing checkpoint: %w", err)
	}
	if fd.Version != formatVersion {
		return fmt.Errorf("unsupported checkpoint version %d", fd.Version)
	}
	if !bindingsEqual(fd.Binding, m.binding) {
		return errors.New("checkpoint belongs to a different invocation")
	}
	if fd.Nodes == nil {
		fd.Nodes = map[string]*Node{}
	}
	m.nodes = fd.Nodes
	return nil
}

// flushLocked writes the checkpoint atomically: callers must hold mu. Writing
// to a temporary file and renaming it means a killed process can never leave a
// half-written checkpoint behind, which would poison parallel resume runs.
func (m *Manager) flushLocked() error {
	fd := fileData{
		Version: formatVersion,
		Binding: m.binding,
		Nodes:   m.nodes,
	}
	data, err := json.MarshalIndent(&fd, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(m.path), ".checkpoint-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, m.path)
}

// FileName returns the per-invocation checkpoint file name.
func FileName(b Binding) string {
	type fileNameBinding struct {
		Calls      []CallBinding `json:"calls"`
		Vars       map[string]any `json:"vars,omitempty"`
		Entrypoint string        `json:"entrypoint"`
		Dir        string        `json:"dir"`
	}
	data, _ := json.Marshal(fileNameBinding{
		Calls:      b.Calls,
		Vars:       b.Vars,
		Entrypoint: b.Entrypoint,
		Dir:        b.Dir,
	})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) + ".json"
}

func bindingsEqual(a, b Binding) bool {
	if a.Entrypoint != b.Entrypoint || a.Dir != b.Dir || len(a.Calls) != len(b.Calls) {
		return false
	}
	for i := range a.Calls {
		if a.Calls[i].Task != b.Calls[i].Task ||
			!mapsEqual(a.Calls[i].Vars, b.Calls[i].Vars) {
			return false
		}
	}
	return mapsEqual(a.Vars, b.Vars)
}

func mapsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if valueKey(b[k]) != valueKey(v) {
			return false
		}
	}
	return true
}

// valueKey renders variable values for comparison. It collapses typed nil
// values (e.g. a nil []string) with untyped nil, which is what the same value
// becomes after a JSON round-trip.
func valueKey(v any) string {
	if v == nil {
		return "<nil>"
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Map, reflect.Ptr, reflect.Chan, reflect.Func, reflect.Interface:
		if rv.IsNil() {
			return "<nil>"
		}
	}
	return fmt.Sprintf("%v", v)
}

func dedupSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	sort.Strings(out)
	return slices.Compact(out)
}

// DefHash is the node identity derived from the resolved task definition and
// the effective variables. It is independent of the task's run mode and of
// its source files, which are bound separately by SourcesHash.
func DefHash(t *ast.Task) (string, error) {
	structHash, err := hashstructure.Hash(t, hashstructure.FormatV2, nil)
	if err != nil {
		return "", err
	}
	// hashstructure skips unexported fields, so the ordered maps behind
	// Vars/Env and the variables attached to deps and task-call commands do
	// not contribute to structHash. Hash them explicitly from their resolved
	// values so effective variables are genuinely bound to the node.
	supplement := struct {
		Vars    map[string]any
		Env     map[string]any
		Deps    []map[string]any
		CmdVars []map[string]any
	}{
		Vars:    t.Vars.ToCacheMap(),
		Env:     t.Env.ToCacheMap(),
		Deps:    make([]map[string]any, 0, len(t.Deps)),
		CmdVars: make([]map[string]any, 0, len(t.Cmds)),
	}
	for _, d := range t.Deps {
		if d == nil {
			continue
		}
		supplement.Deps = append(supplement.Deps, map[string]any{
			"task": d.Task,
			"vars": d.Vars.ToCacheMap(),
		})
	}
	for _, c := range t.Cmds {
		if c == nil || c.Task == "" {
			continue
		}
		supplement.CmdVars = append(supplement.CmdVars, map[string]any{
			"task": c.Task,
			"vars": c.Vars.ToCacheMap(),
		})
	}
	varsHash, err := hashstructure.Hash(supplement, hashstructure.FormatV2, nil)
	if err != nil {
		return "", err
	}
	location := ""
	name := t.Task
	if t.Location != nil {
		location = t.Location.Taskfile
		name = t.LocalName()
	}
	return fmt.Sprintf("%s:%s:%d:%d", location, name, structHash, varsHash), nil
}

// TaskfileHash binds the parsed task definitions and taskfile-level variables
// to the checkpoint. It is recorded for verification but never causes the
// whole checkpoint to be discarded: per-node hashes decide which part of the
// graph is stale.
func TaskfileHash(tf *ast.Taskfile) (string, error) {
	structHash, err := hashstructure.Hash(tf, hashstructure.FormatV2, nil)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, tf.Tasks.Len())
	taskHashes := make(map[string]uint64, tf.Tasks.Len())
	for name, task := range tf.Tasks.All(nil) {
		h, err := hashstructure.Hash(task, hashstructure.FormatV2, nil)
		if err != nil {
			return "", err
		}
		names = append(names, name)
		taskHashes[name] = h
	}
	sort.Strings(names)
	supplement := struct {
		Vars  map[string]any
		Env   map[string]any
		Tasks []map[string]any
	}{
		Vars:  tf.Vars.ToCacheMap(),
		Env:   tf.Env.ToCacheMap(),
		Tasks: make([]map[string]any, 0, len(names)),
	}
	for _, name := range names {
		task, _ := tf.Tasks.Get(name)
		supplement.Tasks = append(supplement.Tasks, map[string]any{
			"name": name,
			"hash": taskHashes[name],
			"vars": task.Vars.ToCacheMap(),
			"env":  task.Env.ToCacheMap(),
		})
	}
	suppHash, err := hashstructure.Hash(supplement, hashstructure.FormatV2, nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", structHash, suppHash), nil
}

// SourcesHash is the identity of a node's source files: their relative names
// and contents are summed. Tasks without sources have an empty identity.
// Results are memoized per manager, as the same compiled task is evaluated
// more than once while deciding whether it can be restored.
func (m *Manager) SourcesHash(t *ast.Task) (string, error) {
	key, err := DefHash(t)
	if err != nil {
		return "", err
	}
	if v, ok := m.sourceCache.Load(key); ok {
		return v.(string), nil
	}
	h, err := sourcesHash(t)
	if err != nil {
		return "", err
	}
	m.sourceCache.Store(key, h)
	return h, nil
}

func sourcesHash(t *ast.Task) (string, error) {
	if len(t.Sources) == 0 {
		return "", nil
	}
	sources, err := fingerprint.Globs(t.Dir, t.Sources, t.ShouldUseGitignore())
	if err != nil {
		return "", err
	}
	h := xxh3.New()
	buf := make([]byte, 128*1024)
	for _, f := range sources {
		// Sum the (slash-normalized) name as well, so renaming a source
		// invalidates the node even when contents would match.
		if _, err := io.CopyBuffer(h, strings.NewReader(filepath.ToSlash(f)), buf); err != nil {
			return "", err
		}
		file, err := os.Open(f)
		if err != nil {
			return "", err
		}
		_, err = io.CopyBuffer(h, file, buf)
		file.Close()
		if err != nil {
			return "", err
		}
	}
	sum := h.Sum128()
	return fmt.Sprintf("%x%x", sum.Hi, sum.Lo), nil
}
