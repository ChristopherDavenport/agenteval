package agenteval_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

func TestLoadSuite(t *testing.T) {
	suite, err := agenteval.LoadSuite(os.DirFS("testdata/suites"), "basic")
	if err != nil {
		t.Fatal(err)
	}
	if suite.Name != "basic" {
		t.Errorf("name = %q", suite.Name)
	}
	if len(suite.Tasks) != 2 || suite.Tasks[0].ID != "greet" || suite.Tasks[1].ID != "plain" {
		t.Fatalf("tasks = %+v", suite.Tasks)
	}
	greet, plain := suite.Tasks[0], suite.Tasks[1]
	if greet.Instruction != "hello world" || len(greet.Prompts) != 0 || greet.Meta["kind"] != "tool" {
		t.Errorf("greet = %+v", greet)
	}
	if in := greet.Inputs(); len(in) != 1 || in[0].(*openresponses.Message).Text() != "hello world" {
		t.Errorf("greet inputs = %+v", in)
	}
	if len(plain.Prompts) != 2 || plain.Prompts[1].(*openresponses.Message).Text() != "second" || plain.Setup["tools"] != "none" {
		t.Errorf("plain = %+v", plain)
	}
	if string(plain.Expect) != `{"text": "second"}` {
		t.Errorf("plain expect = %s", plain.Expect)
	}
	m := suite.Manifest
	if m.Location != "basic" || len(m.Files) != 3 {
		t.Errorf("manifest = %+v", m)
	}
	for _, p := range []string{"basic/suite.json", "basic/greet.json", "basic/plain.json"} {
		if !strings.HasPrefix(m.Files[p], agentsession.HashPrefix) {
			t.Errorf("manifest lacks %s: %v", p, m.Files)
		}
	}
	if (agenteval.Task{}).Inputs() != nil {
		t.Error("empty task has inputs")
	}
}

func TestLoadSuiteErrors(t *testing.T) {
	tests := []struct {
		name string
		fsys fstest.MapFS
		want string
	}{
		{"missing", fstest.MapFS{}, "read suite"},
		{"empty", fstest.MapFS{"s/readme.md": {Data: []byte("x")}}, "no tasks"},
		{"bad json", fstest.MapFS{"s/a.json": {Data: []byte("{")}}, "decode task"},
		{"no prompt", fstest.MapFS{"s/a.json": {Data: []byte(`{"id":"a"}`)}}, "neither an instruction nor prompts"},
		{"duplicate", fstest.MapFS{"s/a.json": {Data: []byte(`{"id":"x","instruction":"1"}`)}, "s/b.json": {Data: []byte(`{"id":"x","instruction":"2"}`)}}, "duplicate task id"},
		{"bad suite file", fstest.MapFS{"s/suite.json": {Data: []byte("[]")}, "s/a.json": {Data: []byte(`{"instruction":"1"}`)}}, "decode s/suite.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := agenteval.LoadSuite(tt.fsys, "s")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
	// A suite at the root of the fs is named "." and its files carry
	// no directory prefix.
	suite, err := agenteval.LoadSuite(fstest.MapFS{"a.json": {Data: []byte(`{"instruction":"1"}`)}, "sub/b.json": {Data: []byte(`{"instruction":"2"}`)}}, ".")
	if err != nil {
		t.Fatal(err)
	}
	if suite.Name != "." || len(suite.Tasks) != 1 || suite.Manifest.Files["a.json"] == "" {
		t.Errorf("root suite = %+v", suite)
	}
}

func TestFromSession(t *testing.T) {
	s := loadFixture(t, "compaction")
	task, err := agenteval.FromSession(s)
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != s.ID() || task.Meta["session"] != s.ID() || task.Meta["leaf"] != s.Leaf() {
		t.Errorf("task = %+v", task)
	}
	var texts []string
	for _, p := range task.Prompts {
		texts = append(texts, p.(*openresponses.Message).Text())
	}
	// The folds' summary messages are user messages too, but they live
	// in compaction entries and are not prompts.
	if strings.Join(texts, ",") != "one,two,three" {
		t.Errorf("prompts = %v", texts)
	}
	roots := loadFixture(t, "roots")
	first := ""
	for _, leaf := range roots.Leaves() {
		if leaf != roots.Leaf() {
			first = leaf
		}
	}
	task, err = agenteval.FromSessionAt(roots, first)
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Prompts) != 1 || task.Prompts[0].(*openresponses.Message).Text() != "hello" {
		t.Errorf("first root prompts = %+v", task.Prompts)
	}
	if _, err := agenteval.FromSessionAt(roots, "nope"); !errors.Is(err, agentsession.ErrNoEntry) {
		t.Errorf("unknown leaf: %v", err)
	}
	empty := agentsession.New(agentsession.Header{})
	if _, err := agenteval.FromSession(empty); !errors.Is(err, agentsession.ErrNoEntry) {
		t.Errorf("empty session: %v", err)
	}
	if _, err := empty.Append(&agentsession.ItemEntry{Item: openresponses.AssistantText("hi")}); err != nil {
		t.Fatal(err)
	}
	if _, err := agenteval.FromSession(empty); !errors.Is(err, agenteval.ErrNoPrompts) {
		t.Errorf("no prompts: %v", err)
	}
}
