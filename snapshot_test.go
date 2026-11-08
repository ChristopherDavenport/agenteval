package agenteval_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/export"
	"github.com/ChristopherDavenport/agentturn"
)

// snapshotLog records, in order, each snapshot the hook took and each
// judge call, with the snapshot the judge saw through SnapshotFrom.
type snapshotLog struct {
	mu     sync.Mutex
	events []string
}

func (l *snapshotLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, fmt.Sprintf(format, args...))
}

// snapshotJudge passes every run and logs the snapshot it was handed.
func snapshotJudge(log *snapshotLog) agenteval.Judge {
	return agenteval.JudgeFunc{JudgeName: "sees-snapshot", Fn: func(ctx context.Context, _ export.Trajectory, task agenteval.Task) (agenteval.Score, error) {
		snap, ok := agenteval.SnapshotFrom(ctx)
		switch {
		case !ok:
			log.add("judge %s: none", task.ID)
		case snap.Error != "":
			log.add("judge %s: error %s", task.ID, snap.Error)
		default:
			log.add("judge %s: %s", task.ID, snap.Data)
		}
		return agenteval.Score{Value: 1, Pass: true}, nil
	}}
}

// diffOf is the snapshot the test hook returns for a task.
func diffOf(task agenteval.Task) agenteval.Snapshot {
	return agenteval.Snapshot{Data: []byte("diff --git a/" + task.ID + " b/" + task.ID + "\n"), Base: "base-" + task.ID}
}

// snapshotEntries returns the snapshot records a session holds.
func snapshotEntries(t *testing.T, store agentsession.Store, id string) []agenteval.SnapshotRecord {
	t.Helper()
	s, err := store.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []agenteval.SnapshotRecord
	for _, e := range s.Entries() {
		if c, ok := e.(*agentsession.CustomEntry); ok {
			if rec, ok := agenteval.ReadSnapshot(c); ok {
				out = append(out, rec)
			}
		}
	}
	return out
}

// TestRunnerSnapshot is #73: the hook reads each run's workspace once,
// after the run and before its judges, which see it; the result keeps
// the bytes, their digest and the base; the session keeps them after
// the run's last entry, where ResultAt reads them back.
func TestRunnerSnapshot(t *testing.T) {
	ctx := context.Background()
	store := newStableStore()
	log := &snapshotLog{}
	r := basicRunner(store)
	r.Judges = append(r.Judges, snapshotJudge(log))
	r.Snapshot = func(_ context.Context, task agenteval.Task) (agenteval.Snapshot, error) {
		log.add("snapshot %s", task.ID)
		snap := diffOf(task)
		snap.Digest = "sha256:not-the-hook's-to-set"
		return snap, nil
	}
	report, err := r.Run(ctx, basicSuite(t))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, res := range report.Results {
		want = append(want, "snapshot "+res.Task.ID, fmt.Sprintf("judge %s: %s", res.Task.ID, diffOf(res.Task).Data))
	}
	if got := strings.Join(log.events, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("events:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	for _, res := range report.Results {
		if res.Err != nil || len(res.Scores) != len(r.Judges) {
			t.Errorf("%s: err %v, %d scores", res.Task.ID, res.Err, len(res.Scores))
		}
		want := diffOf(res.Task)
		want.Digest = agentsession.HashBytes(want.Data)
		if res.Snapshot == nil || string(res.Snapshot.Data) != string(want.Data) || res.Snapshot.Base != want.Base || res.Snapshot.Digest != want.Digest || res.Snapshot.Error != "" {
			t.Errorf("%s: snapshot %+v, want %+v", res.Task.ID, res.Snapshot, want)
		}
		recs := snapshotEntries(t, store, res.SessionID)
		if len(recs) != 1 || recs[0].Target != res.Target || recs[0].Task != res.Task.ID || recs[0].Digest != want.Digest {
			t.Errorf("%s: session holds %+v", res.Task.ID, recs)
		}
		// The snapshot comes back from the session at the result's
		// target, and at the target ResultAt finds for itself.
		for _, target := range []string{res.Target, ""} {
			read, err := agenteval.ResultAt(ctx, store, res.SessionID, target)
			if err != nil {
				t.Fatal(err)
			}
			if read.Target != res.Target || read.Err != nil || read.Snapshot == nil || string(read.Snapshot.Data) != string(want.Data) || read.Snapshot.Base != want.Base || read.Snapshot.Digest != want.Digest {
				t.Errorf("%s: ResultAt(%q) = target %s err %v snapshot %+v", res.Task.ID, target, read.Target, read.Err, read.Snapshot)
			}
			if len(read.Scores) != len(res.Scores) {
				t.Errorf("%s: ResultAt(%q) read %d scores, want %d", res.Task.ID, target, len(read.Scores), len(res.Scores))
			}
		}
	}
	// The report carries the snapshot, and reads back with it.
	var b strings.Builder
	if err := report.WriteJSON(&b); err != nil {
		t.Fatal(err)
	}
	back, err := agenteval.ReadReport(strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if s := back.Results[0].Snapshot; s == nil || string(s.Data) != string(diffOf(back.Results[0].Task).Data) {
		t.Errorf("report read back with snapshot %+v", s)
	}
}

// TestRunnerSnapshotError: a hook that cannot read the workspace is
// recorded on the snapshot, on the result and in the session, and the
// run is judged as it would have been. Result.Err stays nil: it says
// the run failed, and a harness that reruns on it would pay for a run
// that did not fail.
func TestRunnerSnapshotError(t *testing.T) {
	ctx := context.Background()
	store := newStableStore()
	log := &snapshotLog{}
	r := basicRunner(store)
	r.Judges = append(r.Judges, snapshotJudge(log))
	r.Snapshot = func(context.Context, agenteval.Task) (agenteval.Snapshot, error) {
		return agenteval.Snapshot{Data: []byte("partial")}, errors.New("container gone")
	}
	report, err := r.Run(ctx, basicSuite(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range report.Results {
		if res.Reason != agentturn.ReasonDone || len(res.Scores) != len(r.Judges) {
			t.Errorf("%s: reason %s, %d scores; the run should be judged", res.Task.ID, res.Reason, len(res.Scores))
		}
		if res.Err != nil {
			t.Errorf("%s: err %v; a snapshot's error is not the run's", res.Task.ID, res.Err)
		}
		if s := res.Snapshot; s == nil || s.Error != "container gone" || s.Data != nil || s.Base != "" || s.Digest != "" {
			t.Errorf("%s: snapshot %+v", res.Task.ID, res.Snapshot)
		}
		read, err := agenteval.ResultAt(ctx, store, res.SessionID, res.Target)
		if err != nil {
			t.Fatal(err)
		}
		if read.Snapshot == nil || read.Snapshot.Error != "container gone" || read.Snapshot.Data != nil || read.Err != nil {
			t.Errorf("%s: read back snapshot %+v err %v", res.Task.ID, read.Snapshot, read.Err)
		}
	}
	if got := log.events[0]; got != "judge greet: error container gone" {
		t.Errorf("judge saw %q", got)
	}
}

// refusingSnapshots is a store that cannot append a snapshot entry.
type refusingSnapshots struct{ *stableStore }

func (s refusingSnapshots) Append(ctx context.Context, id string, e agentsession.Entry) (string, error) {
	if c, ok := e.(*agentsession.CustomEntry); ok && c.NS == agenteval.SnapshotNS {
		return "", errors.New("disk full")
	}
	return s.stableStore.Append(ctx, id, e)
}

// TestRunnerSnapshotRecordError: a snapshot the store would not take is
// kept on the result with its Error saying so, not on Result.Err.
func TestRunnerSnapshotRecordError(t *testing.T) {
	ctx := context.Background()
	store := refusingSnapshots{newStableStore()}
	r := basicRunner(store)
	r.Snapshot = func(_ context.Context, task agenteval.Task) (agenteval.Snapshot, error) {
		return diffOf(task), nil
	}
	report, err := r.Run(ctx, basicSuite(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range report.Results {
		if res.Err != nil || len(res.Scores) != len(r.Judges) {
			t.Errorf("%s: err %v, %d scores", res.Task.ID, res.Err, len(res.Scores))
		}
		if s := res.Snapshot; s == nil || s.Error != "record: disk full" || string(s.Data) != string(diffOf(res.Task).Data) || s.Digest == "" {
			t.Errorf("%s: snapshot %+v", res.Task.ID, s)
		}
	}
}

// TestRunnerNoSnapshot: without a hook nothing changes; no entry, no
// snapshot on the result or read back, none for the judges.
func TestRunnerNoSnapshot(t *testing.T) {
	ctx := context.Background()
	store := newStableStore()
	log := &snapshotLog{}
	r := basicRunner(store)
	r.Judges = append(r.Judges, snapshotJudge(log))
	report, err := r.Run(ctx, basicSuite(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range report.Results {
		if res.Err != nil || res.Snapshot != nil {
			t.Errorf("%s: err %v snapshot %+v", res.Task.ID, res.Err, res.Snapshot)
		}
		if recs := snapshotEntries(t, store, res.SessionID); len(recs) != 0 {
			t.Errorf("%s: session holds %+v", res.Task.ID, recs)
		}
		read, err := agenteval.ResultAt(ctx, store, res.SessionID, "")
		if err != nil || read.Snapshot != nil {
			t.Errorf("%s: ResultAt snapshot %+v err %v", res.Task.ID, read.Snapshot, err)
		}
	}
	for _, e := range log.events {
		if !strings.HasSuffix(e, ": none") {
			t.Errorf("judge saw %q", e)
		}
	}
	var b strings.Builder
	if err := report.WriteJSON(&b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), `"snapshot"`) {
		t.Errorf("report names a snapshot:\n%s", b.String())
	}
}

// TestRunnerSnapshotFailedRun: the workspace of a run that failed is
// read too, since that is the run a person asks about, though the run
// is not judged.
func TestRunnerSnapshotFailedRun(t *testing.T) {
	ctx := context.Background()
	store := newStableStore()
	calls := 0
	r := basicRunner(store)
	r.Config = func(agenteval.Task) agentturn.Config { return agentturn.Config{Model: failingModel{}} }
	r.Snapshot = func(_ context.Context, task agenteval.Task) (agenteval.Snapshot, error) {
		calls++
		return diffOf(task), nil
	}
	report, err := r.Run(ctx, basicSuite(t))
	if err != nil {
		t.Fatal(err)
	}
	if calls != len(report.Results) {
		t.Errorf("hook called %d times for %d runs", calls, len(report.Results))
	}
	for _, res := range report.Results {
		if res.Reason != agentturn.ReasonError || len(res.Scores) != 0 || res.Snapshot == nil || res.Snapshot.Base != diffOf(res.Task).Base {
			t.Errorf("%s: reason %s, %d scores, snapshot %+v", res.Task.ID, res.Reason, len(res.Scores), res.Snapshot)
		}
		read, err := agenteval.ResultAt(ctx, store, res.SessionID, "")
		if err != nil || read.Target != res.Target || read.Snapshot == nil {
			t.Errorf("%s: ResultAt target %s snapshot %+v err %v", res.Task.ID, read.Target, read.Snapshot, err)
		}
	}
}

// TestSnapshotGroup: each group member carries its sample's snapshot,
// in a run and when JudgeGroups reads the group back from the store.
func TestSnapshotGroup(t *testing.T) {
	ctx := context.Background()
	store := newStableStore()
	seen := func(name string, log *[]string) agenteval.GroupJudge {
		return agenteval.GroupJudgeFunc{JudgeName: name, Fn: func(_ context.Context, group []agenteval.Member, _ agenteval.Task) ([]agenteval.Score, error) {
			scores := make([]agenteval.Score, len(group))
			for k, m := range group {
				if m.Snapshot == nil {
					*log = append(*log, "none")
				} else {
					*log = append(*log, m.Snapshot.Base)
				}
				scores[k] = agenteval.Score{Value: 1, Pass: true}
			}
			return scores, nil
		}}
	}
	var inRun, fromStore []string
	r := basicRunner(store)
	r.Samples = 2
	r.GroupJudges = []agenteval.GroupJudge{seen("group-run", &inRun)}
	r.Snapshot = func(_ context.Context, task agenteval.Task) (agenteval.Snapshot, error) {
		return agenteval.Snapshot{Data: []byte("d"), Base: task.ID + "#" + task.Meta[agenteval.SampleMeta]}, nil
	}
	suite := basicSuite(t)
	if _, err := r.Run(ctx, suite); err != nil {
		t.Fatal(err)
	}
	want := "greet#1 greet#2 plain#1 plain#2"
	if got := strings.Join(inRun, " "); got != want {
		t.Errorf("in the run, members carried %s; want %s", got, want)
	}
	r.GroupJudges = append(r.GroupJudges, seen("group-store", &fromStore))
	if _, err := r.JudgeGroups(ctx, suite); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fromStore, " "); got != want {
		t.Errorf("from the store, members carried %s; want %s", got, want)
	}
}

// TestNewSnapshot: a harness that runs the agent itself keeps a
// snapshot with NewSnapshot, and ResultAt reads it back; bytes that do
// not hash to the recorded digest are reported, not trusted silently.
func TestNewSnapshot(t *testing.T) {
	ctx := context.Background()
	store := newStableStore()
	report, err := basicRunner(store).Run(ctx, basicSuite(t))
	if err != nil {
		t.Fatal(err)
	}
	res := report.Results[0]
	e, err := agenteval.NewSnapshot(agenteval.Snapshot{Data: []byte("tree"), Base: "abc123"}, res.Target, res.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if e.NS != agenteval.SnapshotNS {
		t.Errorf("ns %s", e.NS)
	}
	if _, err := store.Append(ctx, res.SessionID, e); err != nil {
		t.Fatal(err)
	}
	read, err := agenteval.ResultAt(ctx, store, res.SessionID, "")
	if err != nil {
		t.Fatal(err)
	}
	if read.Err != nil || read.Snapshot == nil || string(read.Snapshot.Data) != "tree" || read.Snapshot.Digest != agentsession.HashBytes([]byte("tree")) {
		t.Errorf("read back %+v err %v", read.Snapshot, read.Err)
	}

	// A record whose bytes were changed after it was written.
	var rec agenteval.SnapshotRecord
	if err := json.Unmarshal(e.Data, &rec); err != nil {
		t.Fatal(err)
	}
	rec.Data = []byte("other tree")
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, res.SessionID, &agentsession.CustomEntry{NS: agenteval.SnapshotNS, Data: data}); err != nil {
		t.Fatal(err)
	}
	read, err = agenteval.ResultAt(ctx, store, res.SessionID, "")
	if err != nil {
		t.Fatal(err)
	}
	if read.Err != nil || read.Snapshot == nil || !strings.HasPrefix(read.Snapshot.Error, "digest: ") || string(read.Snapshot.Data) != "other tree" {
		t.Errorf("tampered: snapshot %+v err %v", read.Snapshot, read.Err)
	}
}
