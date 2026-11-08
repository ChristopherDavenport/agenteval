package agenteval

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ChristopherDavenport/agentsession"
)

// SnapshotNS is the namespace of the custom entry that keeps the
// workspace a run left, whose data is a [SnapshotRecord]. The runner
// writes one after each run when [Runner.Snapshot] is set; a harness
// that runs the agent itself writes one with [NewSnapshot].
const SnapshotNS = "agenteval:snapshot"

// Snapshot is the workspace a run left, as a product read it when the
// run ended, so the run can be judged again, or read by a person, once
// the workspace is gone. The library does not decide how a workspace is
// read or what the bytes are: for a git working tree they are most
// often the change against the commit the task started from, as `git
// add -A -N . && git diff --binary <base>` prints it, which a judge
// applies to Base in a fresh workspace.
type Snapshot struct {
	// Data is the workspace's state in the product's own form. Empty is
	// a snapshot of a workspace the run left as it found it, when the
	// product's form says so, as an empty diff does.
	Data []byte `json:"data,omitempty"`
	// Base is what Data applies to, in the product's words: a commit, an
	// image digest, a task's starting tree. The library does not read
	// it.
	Base string `json:"base,omitempty"`
	// Digest is the SHA-256 of Data in the session format's notation,
	// "sha256:<hex>" (agentsession.HashBytes). The runner and
	// [NewSnapshot] set it from Data, replacing one the hook returned,
	// and [ResultAt] checks it.
	Digest string `json:"digest,omitempty"`
	// Error is why the workspace could not be read: the hook's error.
	// Data, Base and Digest are then empty.
	Error string `json:"error,omitempty"`
}

// sealed returns the snapshot as it is kept: its digest set from Data,
// or, for one whose Error is set, the error alone.
func (s Snapshot) sealed() Snapshot {
	if s.Error != "" {
		return Snapshot{Error: s.Error}
	}
	s.Digest = agentsession.HashBytes(s.Data)
	return s
}

// SnapshotRecord is the data of a [SnapshotNS] custom entry: the
// snapshot, with the run it was taken of, named as an outcome names
// the run it scores: the entry its scores target, and the task and
// sample of [OutcomeDetails].
type SnapshotRecord struct {
	Target string `json:"target"`
	Task   string `json:"task"`
	Sample int    `json:"sample,omitempty"`
	Snapshot
}

// NewSnapshot builds the custom entry that keeps a snapshot of the run
// whose last entry is target, for task, in [SnapshotNS] with its digest
// set from Data. The entry is not appended. A harness that runs the
// agent itself, and writes its scores with [NewOutcome], appends it to
// the run's session after target, where [ResultAt] reads it back.
func NewSnapshot(snap Snapshot, target, task string) (*agentsession.CustomEntry, error) {
	return newSnapshot(snap, target, task, 0)
}

// newSnapshot is NewSnapshot for a sample of the task.
func newSnapshot(snap Snapshot, target, task string, sample int) (*agentsession.CustomEntry, error) {
	data, err := json.Marshal(SnapshotRecord{Target: target, Task: task, Sample: sample, Snapshot: snap.sealed()})
	if err != nil {
		return nil, err
	}
	return &agentsession.CustomEntry{NS: SnapshotNS, Data: data}, nil
}

// ReadSnapshot reads a snapshot record out of a custom entry. It
// reports false for an entry in another namespace or one whose data is
// not a record. The digest is not checked; [ResultAt] checks it.
func ReadSnapshot(e *agentsession.CustomEntry) (SnapshotRecord, bool) {
	if e.NS != SnapshotNS {
		return SnapshotRecord{}, false
	}
	var rec SnapshotRecord
	if err := json.Unmarshal(e.Data, &rec); err != nil || rec.Target == "" {
		return SnapshotRecord{}, false
	}
	return rec, true
}

// snapshotAt returns the last snapshot the session holds of the run
// whose last entry is target, for task and sample when matchTask is
// set, or nil when it holds none. A snapshot whose bytes do not hash to
// its digest is returned as recorded, with an error saying so.
func snapshotAt(s *agentsession.Session, target, task string, sample int, matchTask bool) (*Snapshot, error) {
	var found *Snapshot
	for _, e := range s.Entries() {
		c, ok := e.(*agentsession.CustomEntry)
		if !ok {
			continue
		}
		rec, ok := ReadSnapshot(c)
		if !ok || rec.Target != target || matchTask && (rec.Task != task || rec.Sample != sample) {
			continue
		}
		found = &rec.Snapshot
	}
	if found == nil || found.Error != "" {
		return found, nil
	}
	if got := agentsession.HashBytes(found.Data); got != found.Digest {
		return found, fmt.Errorf("agenteval: snapshot of %s: data hashes to %s, not its digest %s", target, got, found.Digest)
	}
	return found, nil
}

// snapshotKey is the context key a judge reads the run's snapshot by.
type snapshotKey struct{}

// SnapshotFrom returns the snapshot of the run a judge is judging, as
// [Runner.Snapshot] took it, when the runner took one: the runner
// calls each judge with a context carrying it. A group judge, which
// judges several runs at once, reads each run's on [Member.Snapshot]. A
// snapshot whose Error is set is a workspace that could not be read.
// The snapshot is the result's; a judge reads it and does not change
// it.
func SnapshotFrom(ctx context.Context) (*Snapshot, bool) {
	s, ok := ctx.Value(snapshotKey{}).(*Snapshot)
	return s, ok && s != nil
}

// withSnapshot returns ctx carrying snap for the judges, or ctx itself
// when there is none.
func withSnapshot(ctx context.Context, snap *Snapshot) context.Context {
	if snap == nil {
		return ctx
	}
	return context.WithValue(ctx, snapshotKey{}, snap)
}
