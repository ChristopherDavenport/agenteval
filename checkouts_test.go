package agenteval_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agenteval/judge"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/export"
	"github.com/ChristopherDavenport/agentturn/session"
)

const (
	revRoot = "1111111111111111111111111111111111111111"
	revTree = "2222222222222222222222222222222222222222"
	revSide = "3333333333333333333333333333333333333333"
)

// checkoutsEnv is the env entry a harness that lists its checkouts
// records at each run start (format 0.22): the tree at cwd, a worktree
// below it and a repository outside it.
func checkoutsEnv() *agentsession.EnvEntry {
	vcs := func(rev string) *agentsession.VCS { return &agentsession.VCS{System: "git", Revision: rev} }
	return &agentsession.EnvEntry{
		CWD: "/w",
		VCS: vcs(revRoot),
		Checkouts: []agentsession.Checkout{
			{Path: ".", Repo: ".git", VCS: vcs(revRoot), Branch: "main"},
			{Path: ".worktrees/fix", Repo: ".git", VCS: vcs(revTree), Branch: "fix"},
			{Path: "/srv/side", Repo: "/srv/side/.git", VCS: vcs(revSide)},
		},
	}
}

// withCheckouts records checkoutsEnv on every run of the runner.
func withCheckouts(agenteval.Task) []session.Option {
	return []session.Option{session.WithEnv(func(context.Context) (*agentsession.EnvEntry, error) {
		return checkoutsEnv(), nil
	})}
}

// checkoutDiff is the snapshot the test hook returns for a checkout.
func checkoutDiff(task agenteval.Task, c agentsession.Checkout) agenteval.Snapshot {
	return agenteval.Snapshot{Data: []byte("diff " + task.ID + " " + c.Path + "\n"), Base: c.VCS.Revision}
}

// TestRunnerCheckoutSnapshots: with checkouts on the run's env entries,
// CheckoutSnapshot is called once for each but ".", which Snapshot
// takes, in the entry's order, with the checkout as recorded; the runner
// names each snapshot by its checkout's path, whatever the hook set;
// the judges see them, the session keeps them after the cwd one, and
// ResultAt reads both back apart.
func TestRunnerCheckoutSnapshots(t *testing.T) {
	ctx := context.Background()
	store := newStableStore()
	log := &snapshotLog{}
	r := basicRunner(store)
	r.SessionOptions = withCheckouts
	r.Snapshot = func(_ context.Context, task agenteval.Task) (agenteval.Snapshot, error) {
		log.add("snapshot %s", task.ID)
		return diffOf(task), nil
	}
	r.CheckoutSnapshot = func(_ context.Context, task agenteval.Task, c agentsession.Checkout) (agenteval.Snapshot, error) {
		log.add("checkout %s %s %s", task.ID, c.Path, c.Branch)
		snap := checkoutDiff(task, c)
		snap.Path = "not-the-hook's-to-set"
		return snap, nil
	}
	r.Judges = append(r.Judges, agenteval.JudgeFunc{JudgeName: "sees-checkouts", Fn: func(ctx context.Context, _ export.Trajectory, task agenteval.Task) (agenteval.Score, error) {
		snaps, ok := agenteval.CheckoutSnapshotsFrom(ctx)
		var paths []string
		for _, s := range snaps {
			paths = append(paths, s.Path)
		}
		log.add("judge %s: %v %s", task.ID, ok, strings.Join(paths, ","))
		return agenteval.Score{Value: 1, Pass: true}, nil
	}})
	report, err := r.Run(ctx, basicSuite(t))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, res := range report.Results {
		id := res.Task.ID
		want = append(want, "snapshot "+id, "checkout "+id+" .worktrees/fix fix", "checkout "+id+" /srv/side ", "judge "+id+": true .worktrees/fix,/srv/side")
	}
	if got := strings.Join(log.events, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("events:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	env := checkoutsEnv()
	for _, res := range report.Results {
		if res.Err != nil {
			t.Errorf("%s: err %v", res.Task.ID, res.Err)
		}
		check := func(where string, snaps []agenteval.Snapshot) {
			t.Helper()
			if len(snaps) != 2 {
				t.Fatalf("%s %s: %d checkout snapshots, want 2", where, res.Task.ID, len(snaps))
			}
			for i, c := range env.Checkouts[1:] {
				w := checkoutDiff(res.Task, c)
				if s := snaps[i]; s.Path != c.Path || string(s.Data) != string(w.Data) || s.Base != w.Base || s.Digest != agentsession.HashBytes(w.Data) || s.Error != "" {
					t.Errorf("%s %s: checkout snapshot %d = %+v, want %s", where, res.Task.ID, i, s, c.Path)
				}
			}
		}
		check("result", res.CheckoutSnapshots)
		recs := snapshotEntries(t, store, res.SessionID)
		if len(recs) != 3 || recs[0].Path != "" || recs[1].Path != ".worktrees/fix" || recs[2].Path != "/srv/side" {
			t.Errorf("%s: session holds %+v", res.Task.ID, recs)
		}
		read, err := agenteval.ResultAt(ctx, store, res.SessionID, res.Target)
		if err != nil {
			t.Fatal(err)
		}
		if read.Snapshot == nil || read.Snapshot.Path != "" || string(read.Snapshot.Data) != string(diffOf(res.Task).Data) {
			t.Errorf("%s: ResultAt cwd snapshot %+v", res.Task.ID, read.Snapshot)
		}
		check("ResultAt", read.CheckoutSnapshots)
	}

	// The report carries them, and writes them without their bytes when
	// asked.
	var b strings.Builder
	if err := report.WriteJSON(&b); err != nil {
		t.Fatal(err)
	}
	back, err := agenteval.ReadReport(strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if got := back.Results[0].CheckoutSnapshots; len(got) != 2 || got[0].Path != ".worktrees/fix" || got[0].Data == nil {
		t.Errorf("report read back with %+v", got)
	}
	b.Reset()
	if err := report.WriteJSON(&b, agenteval.WithoutSnapshotData()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), `"data"`) {
		t.Errorf("report without data carries bytes:\n%s", b.String())
	}
	back, err = agenteval.ReadReport(strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range back.Results[0].CheckoutSnapshots {
		was := report.Results[0].CheckoutSnapshots[i]
		if s.Data != nil || s.Omitted != int64(len(was.Data)) || s.Digest != was.Digest || was.Data == nil {
			t.Errorf("written without data: %+v (in memory %+v)", s, was)
		}
	}
}

// TestRunnerCheckoutSnapshotError: a hook that cannot read a checkout
// is that checkout's snapshot's Error, named by its path, and the run
// is judged as it would have been.
func TestRunnerCheckoutSnapshotError(t *testing.T) {
	ctx := context.Background()
	store := newStableStore()
	r := basicRunner(store)
	r.SessionOptions = withCheckouts
	r.CheckoutSnapshot = func(_ context.Context, task agenteval.Task, c agentsession.Checkout) (agenteval.Snapshot, error) {
		if c.Path == "/srv/side" {
			return agenteval.Snapshot{Data: []byte("kept?")}, fmt.Errorf("no such directory")
		}
		return checkoutDiff(task, c), nil
	}
	report, err := r.Run(ctx, basicSuite(t))
	if err != nil {
		t.Fatal(err)
	}
	res := report.Results[0]
	if res.Err != nil || len(res.Scores) != len(r.Judges) {
		t.Errorf("err %v, %d scores", res.Err, len(res.Scores))
	}
	if res.Snapshot != nil {
		t.Errorf("no Snapshot hook, yet a cwd snapshot %+v", res.Snapshot)
	}
	if len(res.CheckoutSnapshots) != 2 {
		t.Fatalf("checkout snapshots %+v", res.CheckoutSnapshots)
	}
	if s := res.CheckoutSnapshots[1]; s.Path != "/srv/side" || s.Error != "no such directory" || s.Data != nil || s.Digest != "" {
		t.Errorf("failed checkout snapshot %+v", s)
	}
	read, err := agenteval.ResultAt(ctx, store, res.SessionID, res.Target)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.CheckoutSnapshots) != 2 || read.CheckoutSnapshots[1].Error != "no such directory" || read.CheckoutSnapshots[0].Error != "" {
		t.Errorf("read back %+v", read.CheckoutSnapshots)
	}
}

// TestRunnerNoCheckouts: a run whose env entries list no checkouts, as
// one recorded before 0.22 or by a harness that does not list them,
// never calls CheckoutSnapshot, and an env entry listing only "." calls
// it never either.
func TestRunnerNoCheckouts(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name string
		opts func(agenteval.Task) []session.Option
	}{
		{"no env", nil},
		{"env without checkouts", func(agenteval.Task) []session.Option {
			return []session.Option{session.WithEnv(func(context.Context) (*agentsession.EnvEntry, error) {
				return &agentsession.EnvEntry{CWD: "/w"}, nil
			})}
		}},
		{"only the tree at cwd", func(agenteval.Task) []session.Option {
			return []session.Option{session.WithEnv(func(context.Context) (*agentsession.EnvEntry, error) {
				e := checkoutsEnv()
				e.Checkouts = e.Checkouts[:1]
				return e, nil
			})}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := newStableStore()
			r := basicRunner(store)
			r.SessionOptions = tt.opts
			called := 0
			r.CheckoutSnapshot = func(context.Context, agenteval.Task, agentsession.Checkout) (agenteval.Snapshot, error) {
				called++
				return agenteval.Snapshot{}, nil
			}
			report, err := r.Run(ctx, basicSuite(t))
			if err != nil {
				t.Fatal(err)
			}
			if called != 0 || report.Results[0].CheckoutSnapshots != nil {
				t.Errorf("called %d times, snapshots %+v", called, report.Results[0].CheckoutSnapshots)
			}
			if recs := snapshotEntries(t, store, report.Results[0].SessionID); len(recs) != 0 {
				t.Errorf("session holds %+v", recs)
			}
		})
	}
}

// TestNewSnapshotPaths: a harness that runs the agent itself keeps one
// snapshot per checkout with NewSnapshot. ResultAt reads one with no
// Path, or ".", as the tree at cwd, which is every snapshot written
// before Path existed, and the rest by Path, the last of each, in the
// order each Path was first written.
func TestNewSnapshotPaths(t *testing.T) {
	ctx := context.Background()
	store := newStableStore()
	report, err := basicRunner(store).Run(ctx, basicSuite(t))
	if err != nil {
		t.Fatal(err)
	}
	res := report.Results[0]
	for _, snap := range []agenteval.Snapshot{
		{Data: []byte("cwd 1")},
		{Path: "wt", Data: []byte("wt 1")},
		{Path: "/srv/side", Data: []byte("side")},
		{Path: ".", Data: []byte("cwd 2")},
		{Path: "wt", Data: []byte("wt 2")},
	} {
		e, err := agenteval.NewSnapshot(snap, res.Target, res.Task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Append(ctx, res.SessionID, e); err != nil {
			t.Fatal(err)
		}
	}
	recs := snapshotEntries(t, store, res.SessionID)
	if len(recs) != 5 || recs[3].Path != "" {
		t.Errorf("\".\" is kept as empty: %+v", recs)
	}
	read, err := agenteval.ResultAt(ctx, store, res.SessionID, "")
	if err != nil {
		t.Fatal(err)
	}
	if read.Snapshot == nil || read.Snapshot.Path != "" || string(read.Snapshot.Data) != "cwd 2" {
		t.Errorf("cwd snapshot %+v", read.Snapshot)
	}
	var got []string
	for _, s := range read.CheckoutSnapshots {
		got = append(got, s.Path+"="+string(s.Data))
	}
	if strings.Join(got, " ") != "wt=wt 2 /srv/side=side" {
		t.Errorf("checkout snapshots %v", got)
	}
}

// TestRunnerCarriesCheckouts: a fork's manifest env entry restates the
// checkouts in force at its base, as it restates the workspace, so the
// whole-snapshot env entry does not drop them; the session holding it
// was appended under 0.22's writer rules, the checkout at "." carrying
// the entry's vcs.
func TestRunnerCarriesCheckouts(t *testing.T) {
	ctx := context.Background()
	store := newStableStore()
	base := basicRunner(store)
	base.SessionOptions = withCheckouts
	first, err := base.Run(ctx, basicSuite(t))
	if err != nil {
		t.Fatal(err)
	}
	origin := first.Results[0]
	r := &agenteval.Runner{
		Store:  store,
		Config: basicConfig,
		Header: func(agenteval.Task) agentsession.Header {
			return agentsession.Header{ParentSession: origin.SessionID, Base: origin.Target}
		},
		Judges: []agenteval.Judge{judge.ToolCalled("upper")},
	}
	suite := basicSuite(t)
	suite.Tasks = suite.Tasks[:1]
	report, err := r.Run(ctx, suite)
	if err != nil {
		t.Fatal(err)
	}
	res := report.Results[0]
	if res.Err != nil {
		t.Fatalf("err = %v", res.Err)
	}
	s, err := store.Open(ctx, res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var manifest *agentsession.EnvEntry
	for _, e := range s.Path(res.Target) {
		if v, ok := e.(*agentsession.EnvEntry); ok && v.Files != nil {
			manifest = v
		}
	}
	if manifest == nil {
		t.Fatal("the fork holds no manifest env entry")
	}
	if changes := agentsession.CheckoutChanges(checkoutsEnv(), manifest); manifest.Checkouts == nil || len(changes) != 0 {
		t.Errorf("manifest checkouts %+v, changes from the base's %+v", manifest.Checkouts, changes)
	}
}
