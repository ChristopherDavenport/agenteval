// Package harbor reads what Harbor, the harness behind Terminal-Bench,
// writes and expects, into agenteval's shapes.
//
// The direction that carries the score is [Reward]: a Harbor verifier
// writes one number to reward.txt or an object of labelled numbers to
// reward.json, and a labelled number is exactly an agenteval.Score.
//
// [Load] goes the other way and carries less: a Harbor task is an
// instruction, a task.toml, an environment, an oracle solution and a
// verifier, and only the instruction and the table survive into a
// Task. The Dockerfile, the network policy, the resource requests, the
// MCP servers, the steps and the test script do not. A task loaded
// this way is the prompt only; running it faithfully means running it
// under Harbor, and Load honours nothing it copies into Setup.
package harbor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/ChristopherDavenport/agenteval"
)

// File names Harbor uses.
const (
	InstructionFile = "instruction.md"
	TaskFile        = "task.toml"
	RewardJSON      = "reward.json"
	RewardText      = "reward.txt"
)

// DefaultLabel names the score read from reward.txt, which carries no
// label of its own. It is also the key Harbor reports a trial's reward
// under.
const DefaultLabel = "reward"

// ErrNoReward is returned by Reward when the directory holds neither
// reward file.
var ErrNoReward = errors.New("harbor: no reward file")

// Option configures [Reward].
type Option func(*rewardOptions)

type rewardOptions struct {
	pass func(label string, value float64) bool
}

// WithPass sets how a reward's value becomes a score's Pass. The
// default passes a value of at least 1, the 0-or-1 convention of
// Harbor's own test scripts; the format itself allows any finite
// number.
func WithPass(fn func(label string, value float64) bool) Option {
	return func(o *rewardOptions) { o.pass = fn }
}

// Reward reads a verifier's output from dir in fsys, which is
// /logs/verifier inside the container and the trial's verifier
// directory on the host: reward.json, an object of labelled numbers,
// each becoming a Score named by its key; else reward.txt, one number,
// a Score named [DefaultLabel]. The JSON wins when both exist, as it
// does for Harbor. Values are the verifier's own and unbounded; each
// score's Details records which file it came from.
func Reward(fsys fs.FS, dir string, opts ...Option) ([]agenteval.Score, error) {
	o := rewardOptions{pass: func(_ string, v float64) bool { return v >= 1 }}
	for _, opt := range opts {
		opt(&o)
	}
	if data, err := fs.ReadFile(fsys, path.Join(dir, RewardJSON)); err == nil {
		return fromJSON(data, o)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("harbor: read %s: %w", path.Join(dir, RewardJSON), err)
	}
	data, err := fs.ReadFile(fsys, path.Join(dir, RewardText))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w in %s", ErrNoReward, dir)
		}
		return nil, fmt.Errorf("harbor: read %s: %w", path.Join(dir, RewardText), err)
	}
	return fromText(data, o)
}

func fromJSON(data []byte, o rewardOptions) ([]agenteval.Score, error) {
	var rewards map[string]json.RawMessage
	if err := json.Unmarshal(data, &rewards); err != nil {
		return nil, fmt.Errorf("harbor: %s: %w", RewardJSON, err)
	}
	if len(rewards) == 0 {
		return nil, fmt.Errorf("harbor: %s holds no rewards", RewardJSON)
	}
	labels := make([]string, 0, len(rewards))
	for label := range rewards {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	var out []agenteval.Score
	for _, label := range labels {
		// The decoder has already refused anything that is not JSON, so
		// what is left to check is that the member is a number and not
		// a string that holds one.
		raw := string(rewards[label])
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || strings.HasPrefix(raw, "\"") || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("harbor: %s: %s is not a finite number: %s", RewardJSON, label, raw)
		}
		out = append(out, score(label, v, RewardJSON, o))
	}
	return out, nil
}

func fromText(data []byte, o rewardOptions) ([]agenteval.Score, error) {
	text := strings.TrimSpace(string(data))
	v, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil, fmt.Errorf("harbor: %s is not a finite number: %q", RewardText, text)
	}
	return []agenteval.Score{score(DefaultLabel, v, RewardText, o)}, nil
}

func score(label string, v float64, file string, o rewardOptions) agenteval.Score {
	details, _ := json.Marshal(map[string]any{"file": file, "value": v})
	return agenteval.Score{
		Judge:   label,
		Value:   v,
		Pass:    o.pass(label, v),
		Reason:  fmt.Sprintf("the verifier wrote %s to %s", strconv.FormatFloat(v, 'g', -1, 64), file),
		Details: details,
	}
}

// Load reads a Harbor task directory into a Task: instruction.md as
// Instruction, task.toml's [task].name as ID, [task] and [metadata]
// into Meta under the keys task.* and metadata.*, and every other
// table of task.toml into Setup under its own name, keys flattened
// with dots and values rendered as strings, lists and tables of
// tables as JSON. A missing task.toml names the task after the
// directory. What Setup holds is recorded with the run and honoured
// by nothing here.
//
// Both tables are prefixed because [task] is a package description
// with name, version, authors and keywords, while [metadata] is a
// free-form dict[str, Any] that invites the same words: unprefixed,
// a task carrying either key in both tables lost one of the two, and
// which one it lost depended on Go's map iteration order.
func Load(fsys fs.FS, dir string) (agenteval.Task, error) {
	instruction, err := fs.ReadFile(fsys, path.Join(dir, InstructionFile))
	if err != nil {
		return agenteval.Task{}, fmt.Errorf("harbor: read %s: %w", path.Join(dir, InstructionFile), err)
	}
	task := agenteval.Task{ID: path.Base(dir), Instruction: strings.TrimRight(string(instruction), "\n")}
	if task.Instruction == "" {
		return agenteval.Task{}, fmt.Errorf("harbor: %s is empty", path.Join(dir, InstructionFile))
	}
	data, err := fs.ReadFile(fsys, path.Join(dir, TaskFile))
	if errors.Is(err, fs.ErrNotExist) {
		return task, nil
	}
	if err != nil {
		return agenteval.Task{}, fmt.Errorf("harbor: read %s: %w", path.Join(dir, TaskFile), err)
	}
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return agenteval.Task{}, fmt.Errorf("harbor: decode %s: %w", path.Join(dir, TaskFile), err)
	}
	task.Meta = map[string]string{}
	task.Setup = map[string]string{}
	for key, value := range doc {
		switch key {
		case "task":
			if t, ok := value.(map[string]any); ok {
				if name, _ := t["name"].(string); name != "" {
					task.ID = name
				}
				flatten(task.Meta, "task", t)
			}
		case "metadata":
			if m, ok := value.(map[string]any); ok {
				flatten(task.Meta, "metadata", m)
			}
		default:
			flatten(task.Setup, key, value)
		}
	}
	if len(task.Meta) == 0 {
		task.Meta = nil
	}
	if len(task.Setup) == 0 {
		task.Setup = nil
	}
	return task, nil
}

// flatten renders a decoded TOML value into out under prefix: a table
// recurses with dotted keys, a scalar is formatted, and a list or an
// array of tables is JSON.
func flatten(out map[string]string, prefix string, value any) {
	key := func(k string) string {
		if prefix == "" {
			return k
		}
		return prefix + "." + k
	}
	switch v := value.(type) {
	case map[string]any:
		for k, sub := range v {
			flatten(out, key(k), sub)
		}
	case string:
		out[prefix] = v
	case bool:
		out[prefix] = strconv.FormatBool(v)
	case int64:
		out[prefix] = strconv.FormatInt(v, 10)
	case float64:
		out[prefix] = strconv.FormatFloat(v, 'g', -1, 64)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			out[prefix] = fmt.Sprint(v)
			return
		}
		out[prefix] = string(data)
	}
}
