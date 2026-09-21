package price_test

import (
	"math"
	"os"
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
		{"small", price.Rates{}, false},
		{"gpt-5-mini", price.Rates{}, false},
		{"", price.Rates{}, false},
		{"x/", price.Rates{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got, ok := table.For(tt.model)
			if ok != tt.ok || got != tt.want {
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
