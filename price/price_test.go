package price_test

import (
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ChristopherDavenport/agenteval/price"
	"github.com/ChristopherDavenport/openresponses"
)

func usage(in, cached, out int) openresponses.Usage {
	return openresponses.Usage{InputTokens: in, OutputTokens: out, TotalTokens: in + out, InputTokensDetails: openresponses.InputTokensDetails{CachedTokens: cached}}
}

func TestLoadAndLookup(t *testing.T) {
	table, err := price.Load(os.DirFS("testdata"), "prices.json")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		model string
		want  price.Rates
		ok    bool
	}{
		{"gpt-5", price.Rates{Input: 1.25, Cached: 0.125, Output: 10}, true},
		{"openai/gpt-5", price.Rates{Input: 1.25, Cached: 0.125, Output: 10}, true},
		{"acme/small", price.Rates{Input: 0.1, Cached: 0.01, Output: 0.4}, true},
		{"claude", price.Rates{Input: 3, Cached: 0.3, Output: 15, CacheWrite: 3.75, CacheWriteByTTL: map[string]float64{"5m": 3.75, "1h": 6}}, true},
		{"small", price.Rates{}, false},
		{"gpt-5-mini", price.Rates{}, false},
		{"", price.Rates{}, false},
		{"x/", price.Rates{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got, ok := table.For(tt.model)
			if ok != tt.ok || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("For(%q) = %+v, %v; want %+v, %v", tt.model, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestCost(t *testing.T) {
	table := price.Table{"m": {Input: 2, Cached: 0.5, Output: 8}}
	tests := []struct {
		name  string
		model string
		usage openresponses.Usage
		want  float64
		ok    bool
	}{
		{"plain", "m", usage(1_000_000, 0, 500_000), 2 + 4, true},
		{"cached", "m", usage(1_000_000, 600_000, 0), 0.4*2 + 0.6*0.5, true},
		{"provider", "p/m", usage(1_000_000, 0, 0), 2, true},
		{"zero", "m", usage(0, 0, 0), 0, true},
		{"cached beyond input", "m", usage(10, 20, 0), 20 * 0.5 / 1e6, true},
		{"unknown", "n", usage(1, 0, 1), 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := table.Cost(tt.model, tt.usage)
			if ok != tt.ok || math.Abs(got-tt.want) > 1e-12 {
				t.Errorf("Cost = %v, %v; want %v, %v", got, ok, tt.want, tt.ok)
			}
			hook := price.Hook(table)
			if h, hok := hook(tt.model, tt.usage); hok != ok || h != got {
				t.Errorf("Hook disagrees: %v, %v", h, hok)
			}
		})
	}
}

// writes is a usage that wrote to the prompt cache: the count alone,
// or split by TTL when byTTL is not nil.
func writes(in, cached, out, written int, byTTL map[string]int) openresponses.Usage {
	u := usage(in, cached, out)
	if byTTL != nil {
		u.InputTokensDetails.SetCacheWriteTokensByTTL(byTTL)
	} else {
		u.InputTokensDetails.SetCacheWriteTokens(written)
	}
	return u
}

// TestCacheWrites is #68: a table with a cache-write rate prices the
// tokens written to the prompt cache at it, by TTL when the usage
// splits them, and the rest of the uncached input at the input rate
// (openresponses docs/extensions.md section 5, Readers). A table with
// none prices them as input, as before.
func TestCacheWrites(t *testing.T) {
	claude := price.Rates{Input: 3, Cached: 0.3, Output: 15, CacheWrite: 3.75, CacheWriteByTTL: map[string]float64{"5m": 3.75, "1h": 6}}
	plain := price.Rates{Input: 3, Cached: 0.3, Output: 15}
	tests := []struct {
		name  string
		rates price.Rates
		usage openresponses.Usage
		want  float64
	}{
		// The providers study's s6: 10 uncached, 5,000 read and 1,000
		// written to the one-hour cache, 20 out, billed $0.007830.
		{"one-hour write", claude, writes(6010, 5000, 20, 0, map[string]int{"1h": 1000}), 0.007830},
		{"split", claude, writes(6010, 5000, 20, 0, map[string]int{"5m": 400, "1h": 600}), (10*3 + 5000*0.3 + 400*3.75 + 600*6 + 20*15) / 1e6},
		{"count alone", claude, writes(6010, 5000, 20, 1000, nil), (10*3 + 5000*0.3 + 1000*3.75 + 20*15) / 1e6},
		{"a TTL the table lacks", claude, writes(6010, 5000, 20, 0, map[string]int{"24h": 1000}), (10*3 + 5000*0.3 + 1000*3.75 + 20*15) / 1e6},
		{"no write rate", plain, writes(6010, 5000, 20, 0, map[string]int{"1h": 1000}), 0.004830},
		{"no writes", claude, usage(6010, 5000, 20), (1010*3 + 5000*0.3 + 20*15) / 1e6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rates.Cost(tt.usage); math.Abs(got-tt.want) > 1e-12 {
				t.Errorf("Cost = %.9f, want %.9f", got, tt.want)
			}
		})
	}
	table, err := price.Parse([]byte(`{"claude":{"input":3,"cached":0.3,"output":15,"cache_write":3.75,"cache_write_by_ttl":{"5m":3.75,"1h":6}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := table.Cost("anthropic/claude", writes(6010, 5000, 20, 0, map[string]int{"1h": 1000})); !ok || math.Abs(got-0.007830) > 1e-12 {
		t.Errorf("table Cost = %.9f, %v", got, ok)
	}
	if _, err := price.Parse([]byte(`{"m":{"cache_write_by_ttl":{"1h":-1}}}`)); err == nil {
		t.Error("a negative write rate parsed")
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, data, want string
	}{
		{"not json", "{", "unexpected end"},
		{"null", "null", "null"},
		{"negative", `{"m":{"input":-1}}`, "negative rate"},
		{"empty name", `{"":{"input":1}}`, "empty model name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := price.Parse([]byte(tt.data)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
	if _, err := price.Load(fstest.MapFS{}, "missing.json"); err == nil || !strings.Contains(err.Error(), "missing.json") {
		t.Errorf("missing file: %v", err)
	}
	if _, err := price.Load(fstest.MapFS{"p.json": {Data: []byte("[]")}}, "p.json"); err == nil || !strings.Contains(err.Error(), "p.json") {
		t.Errorf("bad file: %v", err)
	}
}
