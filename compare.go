package agenteval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// Comparison is the same suite run under two configurations and judged
// the same way, paired by task.
type Comparison struct {
	Suite    string   `json:"suite"`
	Manifest Manifest `json:"manifest"`
	// A and B are the two reports. They are not written by WriteJSON;
	// the pairs carry both results of every task.
	A, B *Report `json:"-"`
	// Config is the difference between the settings B's runs started
	// under and A's, when every pair differs the same way; Uniform
	// says whether they did. Each pair carries its own.
	Config  ConfigDiff `json:"config"`
	Uniform bool       `json:"uniform"`
	Pairs   []Pair     `json:"pairs"`
	// ByJudge is B's mean minus A's, per judge scored on both sides.
	ByJudge map[string]float64 `json:"by_judge"`
}

// Pair is one task under both configurations.
type Pair struct {
	Task string  `json:"task"`
	A    *Result `json:"a"`
	B    *Result `json:"b"`
	// Delta is B's value minus A's, per judge that scored both.
	Delta map[string]float64 `json:"delta"`
	// Config is the difference between the settings B's run started
	// under and A's.
	Config ConfigDiff `json:"config"`
}

// Change is one setting under A and under B.
type Change struct {
	A any `json:"a"`
	B any `json:"b"`
}

// ConfigDiff is what differed between two runs' settings: the settings
// each run's first model call was made under, replayed from its
// session's config entries.
type ConfigDiff struct {
	Model        *Change `json:"model,omitempty"`
	Instructions *Change `json:"instructions,omitempty"`
	Reasoning    *Change `json:"reasoning,omitempty"`
	Text         *Change `json:"text,omitempty"`
	// ToolsAdded names tools B has and A lacks, ToolsRemoved the
	// reverse, and ToolsChanged tools both have under a different
	// definition; each sorted.
	ToolsAdded   []string `json:"tools_added,omitempty"`
	ToolsRemoved []string `json:"tools_removed,omitempty"`
	ToolsChanged []string `json:"tools_changed,omitempty"`
	// Extra holds passthrough members that differ, by wire name; a
	// side that lacks the member has nil.
	Extra map[string]Change `json:"extra,omitempty"`
}

// Empty reports whether nothing differed.
func (d ConfigDiff) Empty() bool {
	return d.Model == nil && d.Instructions == nil && d.Reasoning == nil && d.Text == nil &&
		len(d.ToolsAdded) == 0 && len(d.ToolsRemoved) == 0 && len(d.ToolsChanged) == 0 && len(d.Extra) == 0
}

// Compare runs the suite under a and b and pairs the results by task.
// The runners share nothing but the suite; each records its own
// sessions in its own store, which may be the same store. The
// comparison's Config is the difference between the settings the two
// runs of each task started under, read back from their sessions, so
// a comparison says how the configurations differed and not only which
// scored higher.
func Compare(ctx context.Context, suite *Suite, a, b *Runner) (*Comparison, error) {
	if a == nil || b == nil {
		return nil, errors.New("agenteval: compare needs two runners")
	}
	ra, err := a.Run(ctx, suite)
	if err != nil {
		return nil, fmt.Errorf("agenteval: compare: a: %w", err)
	}
	rb, err := b.Run(ctx, suite)
	if err != nil {
		return nil, fmt.Errorf("agenteval: compare: b: %w", err)
	}
	c := &Comparison{Suite: suite.Name, Manifest: suite.Manifest, A: ra, B: rb, Uniform: true, ByJudge: map[string]float64{}}
	counts := map[string]int{}
	for i := range ra.Results {
		pa := &ra.Results[i]
		pb, ok := rb.Result(pa.Task.ID)
		if !ok {
			continue
		}
		pair := Pair{Task: pa.Task.ID, A: pa, B: pb, Delta: map[string]float64{}}
		byJudge := map[string]float64{}
		for _, s := range pa.Scores {
			byJudge[s.Judge] = s.Value
		}
		for _, s := range pb.Scores {
			if va, ok := byJudge[s.Judge]; ok {
				d := s.Value - va
				pair.Delta[s.Judge] = d
				c.ByJudge[s.Judge] += d
				counts[s.Judge]++
			}
		}
		sa, errA := initialSettings(ctx, a.Store, pa.SessionID)
		sb, errB := initialSettings(ctx, b.Store, pb.SessionID)
		if errA == nil && errB == nil {
			pair.Config = DiffSettings(sa, sb)
		}
		if i == 0 {
			c.Config = pair.Config
		} else if !reflect.DeepEqual(c.Config, pair.Config) {
			c.Uniform = false
		}
		c.Pairs = append(c.Pairs, pair)
	}
	for name, sum := range c.ByJudge {
		c.ByJudge[name] = sum / float64(counts[name])
	}
	if !c.Uniform {
		c.Config = ConfigDiff{}
	}
	return c, nil
}

// initialSettings replays the settings the session's first model call
// was made under: the config entries on the path to the first response
// entry, or to the leaf when there is none.
func initialSettings(ctx context.Context, store agentsession.Store, sessionID string) (agentsession.Settings, error) {
	if sessionID == "" {
		return agentsession.Settings{}, errors.New("agenteval: no session")
	}
	s, err := store.Open(ctx, sessionID)
	if err != nil {
		return agentsession.Settings{}, err
	}
	for _, e := range s.Path(s.Leaf()) {
		if _, ok := e.(*agentsession.ResponseEntry); ok {
			cx, err := s.RequestContext(e.Base().ID)
			return cx.Settings, err
		}
	}
	cx, err := s.Context()
	return cx.Settings, err
}

// DiffSettings returns what differs between a and b.
func DiffSettings(a, b agentsession.Settings) ConfigDiff {
	var d ConfigDiff
	if a.Model != b.Model {
		d.Model = &Change{A: a.Model, B: b.Model}
	}
	if a.Instructions != b.Instructions {
		d.Instructions = &Change{A: a.Instructions, B: b.Instructions}
	}
	if a.Reasoning != b.Reasoning {
		d.Reasoning = &Change{A: a.Reasoning, B: b.Reasoning}
	}
	if !sameJSON(a.Text, b.Text) {
		d.Text = &Change{A: a.Text, B: b.Text}
	}
	d.ToolsAdded, d.ToolsRemoved, d.ToolsChanged = diffTools(a.Tools, b.Tools)
	for k, va := range a.Extra {
		vb, ok := b.Extra[k]
		if !ok {
			d.extra(k, Change{A: rawValue(va)})
		} else if !sameJSON(va, vb) {
			d.extra(k, Change{A: rawValue(va), B: rawValue(vb)})
		}
	}
	for k, vb := range b.Extra {
		if _, ok := a.Extra[k]; !ok {
			d.extra(k, Change{B: rawValue(vb)})
		}
	}
	return d
}

func (d *ConfigDiff) extra(k string, c Change) {
	if d.Extra == nil {
		d.Extra = map[string]Change{}
	}
	d.Extra[k] = c
}

// diffTools compares two tool lists by name.
func diffTools(a, b openresponses.Tools) (added, removed, changed []string) {
	byName := map[string]openresponses.Tool{}
	for _, t := range a {
		byName[agentsession.ToolName(t)] = t
	}
	seen := map[string]bool{}
	for _, t := range b {
		name := agentsession.ToolName(t)
		seen[name] = true
		old, ok := byName[name]
		switch {
		case !ok:
			added = append(added, name)
		case !sameJSON(old, t):
			changed = append(changed, name)
		}
	}
	for _, t := range a {
		if name := agentsession.ToolName(t); !seen[name] {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return added, removed, changed
}

// rawValue decodes a passthrough member for the diff, so the report
// shows the value rather than its bytes.
func rawValue(raw json.RawMessage) any {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

func sameJSON(a, b any) bool {
	da, errA := json.Marshal(a)
	db, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(da) == string(db)
}

// WriteJSON writes the comparison as indented JSON with a trailing
// newline. The two reports are not included; write them separately.
func (c *Comparison) WriteJSON(w io.Writer) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}
