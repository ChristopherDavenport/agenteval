package agenteval

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

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

// ErrNoPrompts is returned by FromSession when the path holds no user
// message.
var ErrNoPrompts = errors.New("agenteval: session has no user prompts on its path")

// FromSession turns a recorded run into a task: the user messages on
// the path to the session's current leaf, in order, become Prompts, so
// the same conversation can be sent through another configuration. The
// task is named after the session and Meta records the session and the
// leaf. Model output, tool outputs, summaries and app-only entries are
// not prompts and are left out.
func FromSession(s *agentsession.Session) (Task, error) {
	return FromSessionAt(s, s.Leaf())
}

// FromSessionAt is FromSession over the path to leaf.
func FromSessionAt(s *agentsession.Session, leaf string) (Task, error) {
	path := s.Path(leaf)
	if path == nil {
		return Task{}, fmt.Errorf("agenteval: %w: %s", agentsession.ErrNoEntry, leaf)
	}
	task := Task{ID: s.ID(), Meta: map[string]string{"session": s.ID(), "leaf": leaf}}
	for _, e := range path {
		item, ok := e.(*agentsession.ItemEntry)
		if !ok || item.ResponseID != "" {
			continue
		}
		if m, ok := item.Item.(*openresponses.Message); ok && m.Role == openresponses.RoleUser {
			task.Prompts = append(task.Prompts, m)
		}
	}
	if len(task.Prompts) == 0 {
		return Task{}, ErrNoPrompts
	}
	return task, nil
}
