// Package agenteval evaluates Go agents over Open Responses: tasks and
// suites loaded from an fs.FS, a runner that sends each task through an
// agentturn configuration and records the run as an agentsession, a
// judge contract, and scores written back into the session as outcome
// entries. The replay package serves a recorded session as a model and
// as tools, so hooks, transforms and fronts are tested against real
// traffic offline; the judge package holds the deterministic judges and
// the rubric judge; price costs a run; the harbor nested module reads a
// Harbor task and a verifier reward.
//
// A run is an ordinary session: the recorder writes it, the exporter
// renders it, and every score is an outcome entry whose target is the
// last entry of the run it judges. The report is a value over those
// sessions and holds no fact they lack.
//
//	suite, _ := agenteval.LoadSuite(os.DirFS("evals"), "smoke")
//	r := &agenteval.Runner{
//		Store:  store,
//		Config: func(agenteval.Task) agentturn.Config { return cfg },
//		Judges: []agenteval.Judge{judge.Contains("text"), judge.ToolCalled("bash")},
//	}
//	report, _ := r.Run(ctx, suite)
//	report.WriteJSON(os.Stdout)
package agenteval
