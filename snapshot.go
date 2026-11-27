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
//
// A run whose tools worked in more than the tree at its env entry's
// cwd, as one that made a git worktree does, records the checkouts it
// worked in on its env entries (RFC 0001, env, checkouts, since 0.22),
// and a product keeps one snapshot per checkout, each naming its
// checkout by Path. A snapshot with no Path is of the tree at cwd, as
// every snapshot written before Path existed is read.
type Snapshot struct {
	// Path is the checkout the snapshot is of, as the run's env entry
	// names it in checkouts: relative to cwd, or absolute. Empty is the
	// tree at cwd, which "." also names; a snapshot is kept with "."
	// written as empty, so the two read as one. The library does not
	// read the checkout; a product rebuilding a run's workspace applies
	// each snapshot to the checkout its Path names, at the revision the
	// run's env entry records for it.
	Path string `json:"path,omitempty"`
	// Data is the workspace's state in the product's own form. Empty is
	// a snapshot of a workspace the run left as it found it, when the
	// product's form says so, as an empty diff does.
	Data []byte `json:"data,omitempty"`
	// Base is what Data applies to, in the product's words: a commit, an
	// image digest, a task's starting tree. The library does not read
	// it.
	Base string `json:"base,omitempty"`
	// Size is the size in bytes of the workspace state the product
	// read, set where it keeps less than it read: a change over the
	// product's bound, kept with Error and no Data, says here how large
	// it was. The library does not read it.
	Size int64 `json:"size,omitempty"`
	// Omitted is the number of bytes of Data a report written
	// [WithoutSnapshotData] left out. Where the snapshot was appended to
	// the session, the session holds them and [ResultAt] reads them
	// back; where the append failed, nothing holds them. It is set only
	// in such a report, never in a session, so a reader tells bytes the
	// report left out from bytes the product never kept, which Size and
	// Error describe.
	Omitted int64 `json:"omitted,omitempty"`
	// Digest is the SHA-256 of Data in the session format's notation,
	// "sha256:<hex>" (agentsession.HashBytes). The runner and
	// [NewSnapshot] set it from Data, replacing one the hook returned,
	// and [ResultAt] checks it.
	Digest string `json:"digest,omitempty"`
	// Error is why the snapshot could not be taken or kept: one the
	// product set, such as a change over its bound, kept with Base and
	// Size and with Data and Digest empty; the hook's error, with the
	// rest then empty; "record: " and the
	// store's error when the runner could not append the snapshot to the
	// session, the snapshot kept on the result alone; or, from
	// [ResultAt], "digest: " and why the recorded bytes do not hash to
	// the recorded digest, the record kept as it was. It is never the
	// run's error: a run whose workspace could not be read ran as it
	// did, so Result.Err does not carry it.
	Error string `json:"error,omitempty"`
}

// sealed returns the snapshot as it is kept: its digest set from Data,
// or, for one whose Error is set, the error with its Path, Base and
// Size, Data and Digest dropped. A Path of "." is kept as empty, since
// both name the tree at cwd.
func (s Snapshot) sealed() Snapshot {
	if s.Path == "." {
		s.Path = ""
	}
	if s.Error != "" {
		return Snapshot{Path: s.Path, Base: s.Base, Size: s.Size, Error: s.Error}
	}
	s.Omitted = 0
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
// the run's session after target, where [ResultAt] reads it back. A
// run that worked in several checkouts gets one entry per checkout,
// each snapshot's Path naming its checkout as the run's env entry
// does; [ResultAt] reads the one with no Path into [Result.Snapshot]
// and the rest into [Result.CheckoutSnapshots].
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

// snapshotAt returns the last snapshot of the tree at cwd the session
// holds of the run whose last entry is target, for task and sample when
// matchTask is set, or nil when it holds none: one with no Path, or
// "." , as every snapshot written before Path existed is. A snapshot
// whose bytes do not hash to its digest is returned as recorded, its
// Error saying so.
func snapshotAt(s *agentsession.Session, target, task string, sample int, matchTask bool) *Snapshot {
	cwd, _ := snapshotsAt(s, target, task, sample, matchTask)
	return cwd
}

// checkoutSnapshotsAt returns the snapshots of the run's other
// checkouts, as snapshotAt reads the cwd one: the last the session
// holds for each Path, in the order each Path was first written, nil
// when it holds none.
func checkoutSnapshotsAt(s *agentsession.Session, target, task string, sample int, matchTask bool) []Snapshot {
	_, others := snapshotsAt(s, target, task, sample, matchTask)
	return others
}

// snapshotsAt reads every snapshot of the run whose last entry is
// target: the last of the tree at cwd, and the last of each other
// checkout by Path, in the order each Path was first written. Each is
// checked against its digest.
func snapshotsAt(s *agentsession.Session, target, task string, sample int, matchTask bool) (*Snapshot, []Snapshot) {
	var cwd *Snapshot
	var others []Snapshot
	at := map[string]int{}
	for _, e := range s.Entries() {
		c, ok := e.(*agentsession.CustomEntry)
		if !ok {
			continue
		}
		rec, ok := ReadSnapshot(c)
		if !ok || rec.Target != target || matchTask && (rec.Task != task || rec.Sample != sample) {
			continue
		}
		snap := rec.Snapshot
		if snap.Path == "" || snap.Path == "." {
			snap.Path = ""
			cwd = &snap
			continue
		}
		if i, seen := at[snap.Path]; seen {
			others[i] = snap
			continue
		}
		at[snap.Path] = len(others)
		others = append(others, snap)
	}
	if cwd != nil {
		checkDigest(cwd)
	}
	for i := range others {
		checkDigest(&others[i])
	}
	return cwd, others
}

// checkDigest sets the Error of a snapshot whose bytes do not hash to
// its recorded digest, leaving the record as it was.
func checkDigest(snap *Snapshot) {
	if snap.Error != "" {
		return
	}
	if got := agentsession.HashBytes(snap.Data); got != snap.Digest {
		snap.Error = fmt.Sprintf("digest: data hashes to %s, not its digest %s", got, snap.Digest)
	}
}

// snapshotKey is the context key a judge reads the run's snapshot by.
type snapshotKey struct{}

// checkoutSnapshotsKey is the context key a judge reads the snapshots
// of the run's other checkouts by.
type checkoutSnapshotsKey struct{}

// CheckoutSnapshotsFrom returns the snapshots of the checkouts the run
// worked in other than the tree at cwd, as [Runner.CheckoutSnapshot]
// took them, one per checkout by Path: the runner calls each judge with
// a context carrying them. It reports false when there are none. A
// group judge reads each run's on [Member.CheckoutSnapshots]. The
// snapshots are the result's; a judge reads them and does not change
// them.
func CheckoutSnapshotsFrom(ctx context.Context) ([]Snapshot, bool) {
	s, ok := ctx.Value(checkoutSnapshotsKey{}).([]Snapshot)
	return s, ok && len(s) > 0
}

// withCheckoutSnapshots returns ctx carrying snaps for the judges, or
// ctx itself when there are none.
func withCheckoutSnapshots(ctx context.Context, snaps []Snapshot) context.Context {
	if len(snaps) == 0 {
		return ctx
	}
	return context.WithValue(ctx, checkoutSnapshotsKey{}, snaps)
}

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
