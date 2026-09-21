package agenteval_test

import (
	"context"
	"flag"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
)

var update = flag.Bool("update", false, "rewrite golden files and fixtures")

// epoch is the clock every stable store starts from.
var epoch = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

// stableStore wraps a memory store and assigns every session and entry
// a fixed ID and time, so a session recorded through it, and a report
// over it, are byte-stable and can be golden files.
type stableStore struct {
	*agentsession.MemoryStore
	mu       sync.Mutex
	sessions int
	entries  map[string]int
}

func newStableStore() *stableStore {
	return &stableStore{MemoryStore: agentsession.NewMemoryStore(), entries: map[string]int{}}
}

func (s *stableStore) Create(ctx context.Context, h agentsession.Header) (*agentsession.Session, error) {
	s.mu.Lock()
	s.sessions++
	n := s.sessions
	s.mu.Unlock()
	if h.ID == "" {
		h.ID = fmt.Sprintf("01995b2a-0000-7000-8000-%012d", n)
	}
	if h.CreatedAt.IsZero() {
		h.CreatedAt = epoch.Add(time.Duration(n) * time.Hour)
	}
	return s.MemoryStore.Create(ctx, h)
}

func (s *stableStore) Append(ctx context.Context, sessionID string, e agentsession.Entry) (string, error) {
	s.mu.Lock()
	s.entries[sessionID]++
	n := s.entries[sessionID]
	s.mu.Unlock()
	b := e.Base()
	if b.ID == "" {
		b.ID = fmt.Sprintf("e%07d", n)
	}
	if b.Timestamp.IsZero() {
		b.Timestamp = epoch.Add(time.Duration(n) * time.Second)
	}
	return s.MemoryStore.Append(ctx, sessionID, e)
}

func (s *stableStore) List(ctx context.Context, f agentsession.ListFilter) iter.Seq2[agentsession.Summary, error] {
	return s.MemoryStore.List(ctx, f)
}

var _ agentsession.Store = (*stableStore)(nil)

type upperArgs struct {
	Text string `json:"text"`
}

// upper is the tool the fixtures call: the echo adapter calls the first
// function tool offered with the user's text as each required argument.
var upper = agenttool.New("upper", "Uppercase the text", func(_ context.Context, a upperArgs) (string, error) {
	return strings.ToUpper(a.Text), nil
})

// echoConfig is the configuration the fixtures were recorded under.
func echoConfig() agentturn.Config {
	return agentturn.Config{
		Model:        &echo.Adapter{},
		ModelName:    "echo/echo-1",
		Instructions: "Be brief.",
		Tools:        []agenttool.Tool{upper},
	}
}

func loadFixture(t *testing.T, name string) *agentsession.Session {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "sessions", name+".jsonl"))
	if err != nil {
		t.Fatalf("%v (run go test . -update to create it)", err)
	}
	defer f.Close()
	s, err := agentsession.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func writeSession(t *testing.T, s *agentsession.Session, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := agentsession.Write(f, s); err != nil {
		t.Fatal(err)
	}
}

// entryTypes renders a session's entries as one line for assertions:
// item entries by their item type, or role for messages, with a star
// on model output.
func entryTypes(s *agentsession.Session) string {
	var parts []string
	for _, e := range s.Entries() {
		typ := e.EntryType()
		if it, ok := e.(*agentsession.ItemEntry); ok {
			typ = "item:" + it.Item.ItemType()
			if m, ok := it.Item.(*openresponses.Message); ok {
				typ = "item:" + string(m.Role)
			}
			if it.ResponseID != "" {
				typ += "*"
			}
		}
		parts = append(parts, typ)
	}
	return strings.Join(parts, " ")
}

// verifyAll checks every response entry's hash against the rebuilt
// request and returns how many there were.
func verifyAll(t *testing.T, s *agentsession.Session) int {
	t.Helper()
	n := 0
	for _, e := range s.Entries() {
		if _, ok := e.(*agentsession.ResponseEntry); ok {
			n++
			if err := s.Verify(e.Base().ID); err != nil {
				t.Errorf("verify %s: %v", e.Base().ID, err)
			}
		}
	}
	return n
}
