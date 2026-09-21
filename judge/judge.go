// Package judge holds the judges: three deterministic ones that read a
// trajectory against the task's Expect, and [Rubric], an agent that
// reads the trajectory and answers in a fixed schema. The contract they
// implement, agenteval.Judge, lives in the root package.
package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession/export"
	"github.com/ChristopherDavenport/openresponses"
)

// FinalText returns the text of the last assistant message in the
// trajectory's context, or "".
func FinalText(t export.Trajectory) string {
	items := t.Context.Items
	for i := len(items) - 1; i >= 0; i-- {
		if m, ok := items[i].(*openresponses.Message); ok && m.Role == openresponses.RoleAssistant {
			return m.Text()
		}
	}
	return ""
}

// Calls returns the function calls in the trajectory's context, in
// order. A call folded away by a compaction is not among them.
func Calls(t export.Trajectory) []*openresponses.FunctionCall {
	var out []*openresponses.FunctionCall
	for _, item := range t.Context.Items {
		if c, ok := item.(*openresponses.FunctionCall); ok {
			out = append(out, c)
		}
	}
	return out
}

// Expect decodes the task's Expect, or the member of it named by field
// when field is not empty, into v. It reports false when the task has
// no Expect or no such member.
func Expect(task agenteval.Task, field string, v any) (bool, error) {
	raw := task.Expect
	if len(raw) == 0 {
		return false, nil
	}
	if field != "" {
		var members map[string]json.RawMessage
		if err := json.Unmarshal(raw, &members); err != nil {
			return false, fmt.Errorf("judge: expect of task %s is not an object: %w", task.ID, err)
		}
		var ok bool
		if raw, ok = members[field]; !ok {
			return false, nil
		}
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return true, fmt.Errorf("judge: expect of task %s: %w", task.ID, err)
	}
	return true, nil
}

// verdict builds a pass-or-fail score.
func verdict(name string, pass bool, reason string) agenteval.Score {
	s := agenteval.Score{Judge: name, Pass: pass, Reason: reason}
	if pass {
		s.Value = 1
	}
	return s
}

// Exact passes when the final assistant text equals the expected
// string: Expect itself when field is empty, else Expect's member of
// that name. A task with no such expectation fails with a reason that
// says so.
func Exact(field string) agenteval.Judge {
	return agenteval.JudgeFunc{JudgeName: "exact", Fn: func(_ context.Context, t export.Trajectory, task agenteval.Task) (agenteval.Score, error) {
		var want string
		ok, err := Expect(task, field, &want)
		if err != nil {
			return agenteval.Score{}, err
		}
		if !ok {
			return verdict("exact", false, "task has no expected text"), nil
		}
		got := FinalText(t)
		if got == want {
			return verdict("exact", true, "final text equals the expected text"), nil
		}
		return verdict("exact", false, fmt.Sprintf("final text %q is not %q", got, want)), nil
	}}
}

// Contains passes when the final assistant text contains every
// expected string: Expect itself when field is empty, else Expect's
// member of that name, either a string or an array of strings.
func Contains(field string) agenteval.Judge {
	return agenteval.JudgeFunc{JudgeName: "contains", Fn: func(_ context.Context, t export.Trajectory, task agenteval.Task) (agenteval.Score, error) {
		want, ok, err := expectStrings(task, field)
		if err != nil {
			return agenteval.Score{}, err
		}
		if !ok {
			return verdict("contains", false, "task has no expected strings"), nil
		}
		got := FinalText(t)
		var missing []string
		for _, w := range want {
			if !strings.Contains(got, w) {
				missing = append(missing, w)
			}
		}
		if len(missing) == 0 {
			return verdict("contains", true, fmt.Sprintf("final text contains %d expected string(s)", len(want))), nil
		}
		return verdict("contains", false, fmt.Sprintf("final text lacks %q", missing)), nil
	}}
}

// expectStrings reads an expectation that is one string or a list of them.
func expectStrings(task agenteval.Task, field string) ([]string, bool, error) {
	var raw json.RawMessage
	ok, err := Expect(task, field, &raw)
	if err != nil || !ok {
		return nil, ok, err
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, true, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, true, fmt.Errorf("judge: expect of task %s: want a string or an array of strings: %w", task.ID, err)
	}
	return many, true, nil
}

// ToolCalled passes when a call to the named tool appears in the
// trajectory's context. The judge is named "tool_called:<name>".
func ToolCalled(name string) agenteval.Judge {
	judge := "tool_called:" + name
	return agenteval.JudgeFunc{JudgeName: judge, Fn: func(_ context.Context, t export.Trajectory, _ agenteval.Task) (agenteval.Score, error) {
		n := 0
		for _, c := range Calls(t) {
			if c.Name == name {
				n++
			}
		}
		if n > 0 {
			return verdict(judge, true, fmt.Sprintf("%s was called %d time(s)", name, n)), nil
		}
		return verdict(judge, false, name+" was not called"), nil
	}}
}
