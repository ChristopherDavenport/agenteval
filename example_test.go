package agenteval_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
)

// A product whose tools work in a git checkout per run keeps the change
// each run made against the commit the checkout started from, so the
// run can be judged again once the checkout is gone: a judge applies
// the diff to that commit in a fresh checkout. The checkout is found
// for the task where the product's Build put it, and the commit is the
// task's own setup.
func ExampleRunner_snapshot() {
	r := &agenteval.Runner{
		Store: agentsession.NewMemoryStore(),
		Snapshot: func(ctx context.Context, t agenteval.Task) (agenteval.Snapshot, error) {
			sample, _ := strconv.Atoi(t.Meta[agenteval.SampleMeta])
			base := t.Setup["commit"]
			// -N adds the files the run created to the index with no
			// content, so the diff carries them.
			cmd := exec.CommandContext(ctx, "sh", "-c", `git add -A -N . && git diff --binary "$0"`, base)
			cmd.Dir = filepath.Join("runs", agenteval.SampleName(t.ID, sample))
			diff, err := cmd.Output()
			return agenteval.Snapshot{Data: diff, Base: base}, err
		},
	}
	_ = r
}
