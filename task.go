package agenteval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/ChristopherDavenport/agenteval/replay"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// Task is one thing to send through a configuration and judge.
type Task struct {
	// ID names the task within its suite. LoadSuite defaults it to the
	// file name without its extension.
	ID string `json:"id"`
	// Instruction is the common case: one user message.
	Instruction string `json:"instruction,omitempty"`
	// Prompts is the multi-turn case: what the user says, in order,
	// one run of the loop each. It wins over Instruction when set.
	Prompts openresponses.Items `json:"prompts,omitempty"`
	// Setup is product-defined, such as a repository revision to check
	// out before the run. The runner records it and honours none of it.
	Setup map[string]string `json:"setup,omitempty"`
	// Expect is judge-defined: what the deterministic judges compare
	// the run against.
	Expect json.RawMessage `json:"expect,omitempty"`
	// Meta is free-form metadata, recorded with the run.
	Meta map[string]string `json:"meta,omitempty"`
}

// Inputs returns what the runner sends: Prompts when set, else one
// user message carrying Instruction, else nothing.
func (t Task) Inputs() openresponses.Items {
	if len(t.Prompts) > 0 {
		return t.Prompts
	}
	if t.Instruction != "" {
		return openresponses.Items{openresponses.UserText(t.Instruction)}
	}
	return nil
}

// Manifest says where a suite was loaded from and what it held: the
// location within the fs.FS and the content hash of every file read,
// keyed by path, so a result names the suite version it came from.
type Manifest struct {
	Location string            `json:"location"`
	Files    map[string]string `json:"files"`
}

// Suite is a set of tasks loaded together.
type Suite struct {
	Name     string
	Tasks    []Task
	Manifest Manifest
}

// SuiteFile is the name of the optional file in a suite directory that
// names the suite. Its content is {"name": "..."}.
const SuiteFile = "suite.json"

// LoadSuite reads a suite from a directory of fsys: every file ending
// in .json except [SuiteFile] is one task, read in name order, and
// SuiteFile, when present, names the suite. Without it the suite is
// named after the directory. The manifest records every file read.
// The fs.FS is never written.
func LoadSuite(fsys fs.FS, location string) (*Suite, error) {
	location = path.Clean(location)
	entries, err := fs.ReadDir(fsys, location)
	if err != nil {
		return nil, fmt.Errorf("agenteval: read suite %s: %w", location, err)
	}
	suite := &Suite{
		Name:     path.Base(location),
		Manifest: Manifest{Location: location, Files: map[string]string{}},
	}
	if location == "." {
		suite.Name = "."
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		p := path.Join(location, name)
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, fmt.Errorf("agenteval: read %s: %w", p, err)
		}
		suite.Manifest.Files[p] = agentsession.HashBytes(data)
		if name == SuiteFile {
			var meta struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(data, &meta); err != nil {
				return nil, fmt.Errorf("agenteval: decode %s: %w", p, err)
			}
			if meta.Name != "" {
				suite.Name = meta.Name
			}
			continue
		}
		var task Task
		if err := json.Unmarshal(data, &task); err != nil {
			return nil, fmt.Errorf("agenteval: decode task %s: %w", p, err)
		}
		if task.ID == "" {
			task.ID = strings.TrimSuffix(name, ".json")
		}
		if len(task.Inputs()) == 0 {
			return nil, fmt.Errorf("agenteval: task %s has neither an instruction nor prompts", p)
		}
		suite.Tasks = append(suite.Tasks, task)
	}
	if len(suite.Tasks) == 0 {
		return nil, fmt.Errorf("agenteval: suite %s has no tasks", location)
	}
	if err := checkIDs(suite.Tasks); err != nil {
		return nil, err
	}
	return suite, nil
}

// checkIDs rejects a duplicate task ID, which the report pairs by.
func checkIDs(tasks []Task) error {
	seen := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		if seen[t.ID] {
			return fmt.Errorf("agenteval: duplicate task id %q", t.ID)
		}
		seen[t.ID] = true
	}
	return nil
}

// The Task.Meta keys FromSession writes beside "session" and "leaf".
const (
	// LeftOutMeta names the user messages on the path that are no
	// input of a turn, in path order, each as kind=entry, the kind one
	// of agentsession's NotInput values: a resume's note, a steer or a
	// delivery drained mid-run, an item copied from another session's
	// conversation, a peer's late report, a message composed from an
	// instruction layer. A runner sending the task's prompts cannot
	// reproduce them where they stood, so they are named here rather
	// than sent as turns of their own.
	LeftOutMeta = "left_out"
	// InputsMeta is set to InputsByResponses on a path with no run
	// entries, whose turns are split at responses: there a resume's note
	// or a delivery that started a run cannot be told from an input.
	InputsMeta        = "inputs"
	InputsByResponses = "by_responses"
)

// ErrNoPrompts is returned by FromSession when the path holds no user
// message.
var ErrNoPrompts = errors.New("agenteval: session has no user prompts on its path")

// FromSession turns a recorded run into a task: the inputs that drove
// the path to the session's current leaf, in order, become Prompts, so
// the same conversation can be sent through another configuration. The
// task is named after the session and Meta records the session and the
// leaf. The inputs are read as agentsession.PathInputs reads them (RFC
// 0001, run): per run started by an input, the user messages before its
// first response. Model output, tool outputs, summaries and app-only
// entries are not prompts, and neither is a user message that started
// no turn, which is named under [LeftOutMeta].
func FromSession(s *agentsession.Session) (Task, error) {
	return FromSessionAt(s, s.Leaf())
}

// FromSessionMedia is FromSessionAt for a sidecar session (RFC 0001,
// File), whose items name their media by sidecar: references: each
// among the prompts is resolved through br, the store holding the
// session or the reader its recorder wrote the blobs with, to the data
// URL it was, as replay.ResolveMedia does, since no provider can fetch
// a reference. An inline session's prompts are as FromSessionAt gives
// them.
func FromSessionMedia(ctx context.Context, s *agentsession.Session, leaf string, br agentsession.BlobReader) (Task, error) {
	task, err := fromSession(s, leaf)
	if err != nil {
		return Task{}, err
	}
	if task.Prompts, err = replay.ResolveMedia(ctx, br, s.ID(), task.Prompts); err != nil {
		return Task{}, fmt.Errorf("agenteval: %w", err)
	}
	return task, nil
}

// FromSessionAt is FromSession over the path to leaf. A sidecar
// session's prompts that name media by sidecar: references are refused
// with session.ErrNoBlobReader, since no provider can fetch one; read
// such a session with [FromSessionMedia].
func FromSessionAt(s *agentsession.Session, leaf string) (Task, error) {
	return FromSessionMedia(context.Background(), s, leaf, nil)
}

// fromSession is FromSessionAt with the prompts as the session holds
// them.
func fromSession(s *agentsession.Session, leaf string) (Task, error) {
	path := s.Path(leaf)
	if path == nil {
		return Task{}, fmt.Errorf("agenteval: %w: %s", agentsession.ErrNoEntry, leaf)
	}
	task := Task{ID: s.ID(), Meta: map[string]string{"session": s.ID(), "leaf": leaf}}
	turns, byRuns := agentsession.PathInputs(path)
	var left []string
	for _, turn := range turns {
		for _, it := range turn.Inputs {
			if m, ok := it.Item.(*openresponses.Message); ok {
				task.Prompts = append(task.Prompts, m)
			}
		}
		for _, lo := range turn.LeftOut {
			left = append(left, lo.Kind+"="+lo.Entry.ID)
		}
	}
	if len(left) > 0 {
		task.Meta[LeftOutMeta] = strings.Join(left, " ")
	}
	if !byRuns && len(turns) > 0 {
		task.Meta[InputsMeta] = InputsByResponses
	}
	if len(task.Prompts) == 0 {
		return Task{}, ErrNoPrompts
	}
	return task, nil
}
