package agenteval

import (
	"encoding/json"
	"io"
	"sort"
)

// Summary is one judge's numbers over the tasks it scored.
type Summary struct {
	// Count is how many results the judge scored.
	Count int `json:"count"`
	// Mean is the mean of the judge's values over those results.
	Mean float64 `json:"mean"`
	// Passed is how many of them the judge passed, and PassRate is
	// Passed over Count.
	Passed   int     `json:"passed"`
	PassRate float64 `json:"pass_rate"`
}

// Report is a runner's results over a suite. ByJudge averages each
// judge's scores across the tasks it scored, which is not the rule
// export.PreferScore applies when it chooses between branches of one
// session: that takes each branch's best score across judges. A
// report and a preference can therefore name different winners.
type Report struct {
	Suite    string             `json:"suite"`
	Manifest Manifest           `json:"manifest"`
	Results  []Result           `json:"results"`
	ByJudge  map[string]Summary `json:"by_judge"`
}

// Summarize computes the per-judge summaries over results.
func Summarize(results []Result) map[string]Summary {
	out := map[string]Summary{}
	for _, r := range results {
		for _, s := range r.Scores {
			sum := out[s.Judge]
			sum.Count++
			sum.Mean += s.Value
			if s.Pass {
				sum.Passed++
			}
			out[s.Judge] = sum
		}
	}
	for name, sum := range out {
		sum.Mean /= float64(sum.Count)
		sum.PassRate = float64(sum.Passed) / float64(sum.Count)
		out[name] = sum
	}
	return out
}

// Judges returns the judge names in the report, sorted.
func (r *Report) Judges() []string {
	names := make([]string, 0, len(r.ByJudge))
	for name := range r.ByJudge {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Result returns the result for a task ID.
func (r *Report) Result(task string) (*Result, bool) {
	for i := range r.Results {
		if r.Results[i].Task.ID == task {
			return &r.Results[i], true
		}
	}
	return nil, false
}

// WriteJSON writes the report as indented JSON with a trailing newline.
func (r *Report) WriteJSON(w io.Writer) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// ReadReport reads a report written by WriteJSON.
func ReadReport(rd io.Reader) (*Report, error) {
	var r Report
	if err := json.NewDecoder(rd).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}
