// Package price costs a run. A [Table] maps model names to [Rates] in
// US dollars per million tokens, loaded from an fs.FS so a product
// keeps its prices beside its suites; [Hook] shapes a table for
// export.Options.Cost and agenteval.Runner.Cost, so the report and the
// exported document agree on one number. Prices change and belong with
// the thing that compares runs, not in the wire package or the record.
package price

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"

	"github.com/ChristopherDavenport/openresponses"
)

// Rates are one model's prices in US dollars per million tokens: Input
// for uncached prompt tokens, Cached for prompt tokens served from the
// cache, Output for completion tokens.
type Rates struct {
	Input  float64 `json:"input"`
	Cached float64 `json:"cached"`
	Output float64 `json:"output"`
}

// Cost prices a usage under the rates with the formula ATIF defines:
// (prompt - cached) x input + cached x cached + completion x output.
func (r Rates) Cost(u openresponses.Usage) float64 {
	cached := u.InputTokensDetails.CachedTokens
	uncached := u.InputTokens - cached
	if uncached < 0 {
		uncached = 0
	}
	return (float64(uncached)*r.Input + float64(cached)*r.Cached + float64(u.OutputTokens)*r.Output) / 1e6
}

// Table maps model names to rates. A name may carry a provider prefix,
// "provider/model"; [Table.For] tries the name as given and then the
// part after the first slash, so one table serves a configuration that
// names its model either way.
type Table map[string]Rates

// Load reads a table from a JSON file in fsys: an object of model name
// to {"input", "cached", "output"}.
func Load(fsys fs.FS, path string) (Table, error) {
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, fmt.Errorf("price: read %s: %w", path, err)
	}
	t, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("price: %s: %w", path, err)
	}
	return t, nil
}

// Parse decodes a table from its JSON form.
func Parse(data []byte) (Table, error) {
	var t Table
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("price: table is null")
	}
	for name, r := range t {
		if name == "" {
			return nil, fmt.Errorf("price: empty model name")
		}
		if r.Input < 0 || r.Cached < 0 || r.Output < 0 {
			return nil, fmt.Errorf("price: %s: negative rate", name)
		}
	}
	return t, nil
}

// For returns the rates for a model: the name as given, else the name
// after its first slash.
func (t Table) For(model string) (Rates, bool) {
	if r, ok := t[model]; ok {
		return r, true
	}
	if _, rest, ok := strings.Cut(model, "/"); ok && rest != "" {
		if r, ok := t[rest]; ok {
			return r, true
		}
	}
	return Rates{}, false
}

// Cost prices one call. It reports false for a model the table lacks.
func (t Table) Cost(model string, u openresponses.Usage) (float64, bool) {
	r, ok := t.For(model)
	if !ok {
		return 0, false
	}
	return r.Cost(u), true
}

// Hook returns the table's Cost as the function export.Options.Cost
// and agenteval.Runner.Cost take.
func Hook(t Table) func(model string, usage openresponses.Usage) (float64, bool) {
	return t.Cost
}
