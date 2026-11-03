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
// cache, Output for completion tokens. CacheWrite is the rate for
// prompt tokens written to the provider's prompt cache, and
// CacheWriteByTTL the rate for each time the provider keeps a write,
// keyed as the usage keys its split ("5m", "1h"), since Claude bills a
// five-minute write and a one-hour write differently. Zero and absent
// mean the table has no write rate, and writes are priced as Input.
type Rates struct {
	Input           float64            `json:"input"`
	Cached          float64            `json:"cached"`
	Output          float64            `json:"output"`
	CacheWrite      float64            `json:"cache_write,omitempty"`
	CacheWriteByTTL map[string]float64 `json:"cache_write_by_ttl,omitempty"`
}

// Cost prices a usage under the rates with the formula ATIF defines,
// (prompt - cached) x input + cached x cached + completion x output,
// with the cache writes openresponses docs/extensions.md section 5
// defines taken out of the uncached prompt and priced at the write
// rate, as its Readers rule asks: each TTL's writes at that TTL's rate
// when the usage splits them, else at CacheWrite, else at Input. Usage
// that reports no writes, and a table with no write rate, price as the
// formula alone does.
func (r Rates) Cost(u openresponses.Usage) float64 {
	cached := u.InputTokensDetails.CachedTokens
	written, writeCost := r.writes(u.InputTokensDetails)
	uncached := u.InputTokens - cached - written
	if uncached < 0 {
		uncached = 0
	}
	return (float64(uncached)*r.Input + float64(cached)*r.Cached + writeCost + float64(u.OutputTokens)*r.Output) / 1e6
}

// writes returns the prompt tokens the usage reports written to the
// cache and their price in dollars per million: each TTL of a valid
// split at its own rate, and any writes the split does not cover at
// the flat write rate.
func (r Rates) writes(d openresponses.InputTokensDetails) (int, float64) {
	total, _ := d.CacheWriteTokens()
	split, ok := d.CacheWriteTokensByTTL()
	if !ok {
		return total, float64(total) * r.writeRate("")
	}
	sum, cost := 0, 0.0
	for ttl, n := range split {
		sum += n
		cost += float64(n) * r.writeRate(ttl)
	}
	if rest := total - sum; rest > 0 {
		sum += rest
		cost += float64(rest) * r.writeRate("")
	}
	return sum, cost
}

// writeRate is the rate for a write kept for ttl: the TTL's own, else
// the flat write rate, else the input rate.
func (r Rates) writeRate(ttl string) float64 {
	if v, ok := r.CacheWriteByTTL[ttl]; ok && ttl != "" && v > 0 {
		return v
	}
	if r.CacheWrite > 0 {
		return r.CacheWrite
	}
	return r.Input
}

// Table maps model names to rates. A name may carry a provider prefix,
// "provider/model"; [Table.For] tries the name as given and then the
// part after the first slash, so one table serves a configuration that
// names its model either way.
type Table map[string]Rates

// Load reads a table from a JSON file in fsys: an object of model name
// to {"input", "cached", "output"}, with "cache_write" and
// "cache_write_by_ttl" for a model whose provider bills a write to its
// prompt cache:
//
//	{
//	  "claude-sonnet-4-5": {"input": 3, "cached": 0.3, "output": 15,
//	    "cache_write": 3.75, "cache_write_by_ttl": {"5m": 3.75, "1h": 6}}
//	}
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
		if r.Input < 0 || r.Cached < 0 || r.Output < 0 || r.CacheWrite < 0 {
			return nil, fmt.Errorf("price: %s: negative rate", name)
		}
		for ttl, v := range r.CacheWriteByTTL {
			if v < 0 {
				return nil, fmt.Errorf("price: %s: negative cache write rate for %s", name, ttl)
			}
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
