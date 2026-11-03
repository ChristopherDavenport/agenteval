// Command agenteval reads what evaluation runs left behind: Harbor jobs,
// stored sessions and the reports over them. It imports a Harbor job's
// rewards onto the agent's sessions, reports stored runs, compares two
// reports from their sessions, and exports a run as ATIF. It adds no
// format: a report is a value derived from the sessions, and compare
// reads the stores again rather than trusting the report files.
//
// Running a task, building its environment, running the agent under
// its timeouts and its verifier, stays with Harbor or the product: a
// suite needs a Go configuration no generic binary has, and Harbor
// already runs tasks.
//
//	agenteval import [-sessions agent/sessions] [-out FILE] JOB
//	agenteval report -store DIR [-suite NAME] [-price FILE] [-out FILE] [REF|SESSION[@ENTRY]]...
//	agenteval compare (-a-store DIR | -a-job DIR) (-b-store DIR | -b-job DIR) [-sessions agent/sessions] [-price FILE] [-out FILE] REPORT_A REPORT_B
//	agenteval export -store DIR [-price FILE] [-out FILE] REF|SESSION[@ENTRY]
//
// A store is a jsonl session store directory. A job is a Harbor job
// directory, each trial's sessions in the store under its directory
// that -sessions names. A price file is a price table (see the price
// package).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agenteval/harbor"
	"github.com/ChristopherDavenport/agenteval/price"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/export"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/openresponses"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage:
  agenteval import [-sessions agent/sessions] [-out FILE] JOB
  agenteval report -store DIR [-suite NAME] [-price FILE] [-out FILE] [REF|SESSION[@ENTRY]]...
  agenteval compare (-a-store DIR | -a-job DIR) (-b-store DIR | -b-job DIR) [-sessions agent/sessions] [-price FILE] [-out FILE] REPORT_A REPORT_B
  agenteval export -store DIR [-price FILE] [-out FILE] REF|SESSION[@ENTRY]
`

// errUsage is a command line the command cannot read.
var errUsage = errors.New("bad usage")

// run runs one command line and returns the exit status: 0, 1 for a
// failure, 2 for a command line it cannot read.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmds := map[string]func(context.Context, []string, io.Writer, io.Writer) error{
		"import":  importJob,
		"report":  report,
		"compare": compare,
		"export":  exportRun,
	}
	cmd, ok := cmds[args[0]]
	if !ok {
		if args[0] == "help" || args[0] == "-h" || args[0] == "-help" {
			fmt.Fprint(stdout, usage)
			return 0
		}
		fmt.Fprintf(stderr, "agenteval: unknown command %q\n%s", args[0], usage)
		return 2
	}
	if err := cmd(ctx, args[1:], stdout, stderr); err != nil {
		if errors.Is(err, errUsage) || errors.Is(err, flag.ErrHelp) {
			if !errors.Is(err, flag.ErrHelp) {
				fmt.Fprintf(stderr, "agenteval %s: %v\n", args[0], err)
			}
			fmt.Fprint(stderr, usage)
			return 2
		}
		fmt.Fprintf(stderr, "agenteval %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

// flags returns a flag set for a command that reports its errors as
// values.
func flags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parse parses args, a usage error for a bad flag.
func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	return nil
}

// output writes v as indented JSON to the file named, or stdout.
func output(path string, stdout io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if path == "" {
		_, err = stdout.Write(data)
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// prices loads the price table a -price flag names, nil for none.
func prices(path string) (func(string, openresponses.Usage) (float64, bool), error) {
	if path == "" {
		return nil, nil
	}
	t, err := price.Load(os.DirFS(filepath.Dir(path)), filepath.Base(path))
	if err != nil {
		return nil, err
	}
	return price.Hook(t), nil
}

// readStore opens a jsonl store for reading: it takes no lock and
// creates nothing.
func readStore(dir string) (*jsonl.Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: no store", errUsage)
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	return jsonl.Open(dir, jsonl.WithReadOnly())
}

// trialStores opens the stores of a Harbor job's trials, each the jsonl
// store at the trial directory's sessions path, once each, and closes
// them all.
type trialStores struct {
	job, sessions string
	readOnly      bool
	open          map[string]*jsonl.Store
}

func (t *trialStores) store(trial string) (agentsession.Store, error) {
	if s, ok := t.open[trial]; ok {
		return s, nil
	}
	dir := filepath.Join(t.job, trial, filepath.FromSlash(t.sessions))
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("the trial's sessions: %w", err)
	}
	var opts []jsonl.Option
	if t.readOnly {
		opts = append(opts, jsonl.WithReadOnly())
	}
	s, err := jsonl.Open(dir, opts...)
	if err != nil {
		return nil, err
	}
	if t.open == nil {
		t.open = map[string]*jsonl.Store{}
	}
	t.open[trial] = s
	return s, nil
}

func (t *trialStores) close() error {
	var errs []error
	for _, s := range t.open {
		errs = append(errs, s.Close())
	}
	return errors.Join(errs...)
}

// importJob writes a Harbor job's rewards onto its trials' sessions and
// prints the job as a report.
func importJob(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	fs := flags("import", stderr)
	sessions := fs.String("sessions", "agent/sessions", "the jsonl store `path` under each trial directory")
	out := fs.String("out", "", "write the report to `file` rather than stdout")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("%w: import takes one job directory", errUsage)
	}
	job := fs.Arg(0)
	stores := &trialStores{job: job, sessions: *sessions}
	defer func() { err = errors.Join(err, stores.close()) }()
	r, err := harbor.ImportJob(ctx, os.DirFS(job), stores.store)
	if err != nil {
		return err
	}
	return output(*out, stdout, r)
}

// target resolves a command-line run: a ref the store holds, or a
// session with an optional @entry.
func target(ctx context.Context, store agentsession.Store, arg string) (agentsession.RefTarget, error) {
	if id, entry, ok := strings.Cut(arg, "@"); ok {
		return agentsession.RefTarget{Session: id, Entry: entry}, nil
	}
	if refs, ok := store.(agentsession.RefStore); ok && agentsession.ValidRefName(arg) == nil {
		t, err := refs.ResolveRef(ctx, arg)
		switch {
		case err == nil:
			return t, nil
		case !errors.Is(err, agentsession.ErrNoRef):
			return agentsession.RefTarget{}, fmt.Errorf("ref %s: %w", arg, err)
		}
	}
	return agentsession.RefTarget{Session: arg}, nil
}

// report prints a report over stored runs: those named, or every
// top-level session in the store.
func report(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flags("report", stderr)
	dir := fs.String("store", "", "the jsonl session store `directory`")
	suite := fs.String("suite", "", "the report's suite `name`")
	pricePath := fs.String("price", "", "price the runs with the price table in `file`")
	out := fs.String("out", "", "write the report to `file` rather than stdout")
	if err := parse(fs, args); err != nil {
		return err
	}
	cost, err := prices(*pricePath)
	if err != nil {
		return err
	}
	store, err := readStore(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	names := fs.Args()
	if len(names) == 0 {
		for sum, err := range store.List(ctx, agentsession.ListFilter{}) {
			if err != nil {
				return err
			}
			if sum.Header.ParentSession == "" {
				names = append(names, sum.Header.ID)
			}
		}
	}
	r := &agenteval.Report{Suite: *suite}
	for _, name := range names {
		t, err := target(ctx, store, name)
		if err != nil {
			return err
		}
		res, err := agenteval.ResultAt(ctx, store, t.Session, t.Entry, agenteval.WithCost(cost))
		if err != nil {
			return err
		}
		if res.Task.ID == "" {
			res.Task.ID = name
		}
		r.Results = append(r.Results, *res)
	}
	r.ByJudge = agenteval.Summarize(r.Results)
	return output(*out, stdout, r)
}

// readReport reads a report file.
func readReport(path string) (*agenteval.Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := agenteval.ReadReport(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// side is where one report's sessions are: one store, or a Harbor
// job's trial stores.
func side(name, store, job, sessions string, closers *[]func() error) (func(agenteval.Result) (agentsession.Store, error), error) {
	switch {
	case store != "" && job != "":
		return nil, fmt.Errorf("%w: -%s-store and -%s-job are exclusive", errUsage, name, name)
	case store != "":
		s, err := readStore(store)
		if err != nil {
			return nil, err
		}
		*closers = append(*closers, s.Close)
		return agenteval.OneStore(s), nil
	case job != "":
		t := &trialStores{job: job, sessions: sessions, readOnly: true}
		*closers = append(*closers, t.close)
		return harbor.Resolver(t.store), nil
	}
	return nil, fmt.Errorf("%w: -%s-store or -%s-job is required", errUsage, name, name)
}

// compare prints the comparison of two reports, each read again from
// its sessions.
func compare(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	fs := flags("compare", stderr)
	aStore := fs.String("a-store", "", "the jsonl store `directory` holding report A's sessions")
	aJob := fs.String("a-job", "", "the Harbor job `directory` report A was imported from")
	bStore := fs.String("b-store", "", "the jsonl store `directory` holding report B's sessions")
	bJob := fs.String("b-job", "", "the Harbor job `directory` report B was imported from")
	sessions := fs.String("sessions", "agent/sessions", "the jsonl store `path` under each trial directory of a job")
	pricePath := fs.String("price", "", "price the runs with the price table in `file`")
	out := fs.String("out", "", "write the comparison to `file` rather than stdout")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("%w: compare takes two report files", errUsage)
	}
	cost, err := prices(*pricePath)
	if err != nil {
		return err
	}
	var closers []func() error
	defer func() {
		for _, c := range closers {
			err = errors.Join(err, c())
		}
	}()
	resolveA, err := side("a", *aStore, *aJob, *sessions, &closers)
	if err != nil {
		return err
	}
	resolveB, err := side("b", *bStore, *bJob, *sessions, &closers)
	if err != nil {
		return err
	}
	a, err := readReport(fs.Arg(0))
	if err != nil {
		return err
	}
	b, err := readReport(fs.Arg(1))
	if err != nil {
		return err
	}
	c, err := agenteval.CompareReports(ctx, a, b, resolveA, resolveB, agenteval.WithCost(cost))
	if err != nil {
		return err
	}
	return output(*out, stdout, c)
}

// exportRun prints a stored run as an ATIF document, its subagents'
// sessions embedded from the same store.
func exportRun(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flags("export", stderr)
	dir := fs.String("store", "", "the jsonl session store `directory`")
	pricePath := fs.String("price", "", "price the run with the price table in `file`")
	out := fs.String("out", "", "write the document to `file` rather than stdout")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("%w: export takes one run", errUsage)
	}
	cost, err := prices(*pricePath)
	if err != nil {
		return err
	}
	store, err := readStore(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	t, err := target(ctx, store, fs.Arg(0))
	if err != nil {
		return err
	}
	s, err := store.Read(ctx, t.Session)
	if err != nil {
		return err
	}
	entry := t.Entry
	if entry == "" {
		entry = s.Leaf()
	}
	tr, err := export.At(s, entry)
	if err != nil {
		return err
	}
	doc, err := export.ToATIF(tr, export.Options{Cost: cost, Subsessions: func(id string) (*agentsession.Session, error) {
		child, err := store.Read(ctx, id)
		if errors.Is(err, agentsession.ErrNoSession) {
			return nil, nil
		}
		return child, err
	}})
	if err != nil {
		return err
	}
	return output(*out, stdout, doc)
}
