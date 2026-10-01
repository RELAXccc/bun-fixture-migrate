package main

import (
	"encoding/json"
	"io"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// jsonValues writes values so a program can tell them apart: a literal is a
// string, NULL is null, and a reference is {"model": ..., "key": ...}. The
// text form of the report cannot tell the literal "NULL" from a NULL.
func jsonValues(v fixturechange.Values) map[string]any {
	if len(v) == 0 {
		return nil
	}
	out := make(map[string]any, len(v))
	for col, value := range v {
		switch {
		case value.IsNull:
			out[col] = nil
		case value.Ref != nil:
			out[col] = map[string]string{"model": value.Ref.Model, "key": value.Ref.Key}
		default:
			out[col] = value.Lit
		}
	}
	return out
}

type checkReport struct {
	// Agree is what check's exit code says: true for 0. A finding the
	// policy makes a warning is listed and leaves it true.
	Agree    bool           `json:"agree"`
	Findings []checkFinding `json:"findings"`
	Refusals []checkRefusal `json:"refusals"`
	// Warnings are differences the policy lets a migration carry on past,
	// such as a renumbered row under id_drift: warn.
	Warnings []checkRefusal `json:"warnings"`
	// Changes are what a migration from the database to the fixture file
	// would do: an insert is a row only the file has, a delete a row only the
	// database has, and an update's old values are the database's.
	Changes []checkChange `json:"changes"`
	// LeftAlone counts what the configuration gives to the database, mode
	// upsert and insert and insert_only columns, which is no drift.
	LeftAlone []checkLeftAlone `json:"left_alone"`
}

type checkLeftAlone struct {
	Model   string         `json:"model"`
	Mode    string         `json:"mode"`
	Rows    int            `json:"rows"`
	Changed int            `json:"changed"`
	Columns map[string]int `json:"columns,omitempty"`
}

type checkFinding struct {
	Kind string `json:"kind"`
	// Level is what the policy makes of the kind: "error" or "warn".
	Level  string `json:"level"`
	Model  string `json:"model"`
	Row    string `json:"row,omitempty"`
	Detail string `json:"detail"`
}

type checkRefusal struct {
	Model  string `json:"model"`
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

type checkChange struct {
	Model string         `json:"model"`
	Kind  string         `json:"kind"`
	Key   map[string]any `json:"key"`
	Old   map[string]any `json:"database,omitempty"`
	New   map[string]any `json:"file,omitempty"`
	// Hints say, per column, why a value differs where check can tell.
	Hints map[string]string `json:"hints,omitempty"`
}

// findingsJSON is findings as a program reads them, each with the level the
// policy gives its kind.
func findingsJSON(cfg *fixturemigrate.Config, findings []fixturemigrate.Finding) []checkFinding {
	out := []checkFinding{}
	for _, f := range findings {
		out = append(out, checkFinding{Kind: string(f.Kind), Level: string(cfg.FindingMode(f.Kind)),
			Model: f.Model, Row: f.Row, Detail: f.Detail})
	}
	return out
}

func checkJSON(cfg *fixturemigrate.Config, res *fixturemigrate.CheckResult) checkReport {
	out := checkReport{Agree: res.Agree(cfg),
		Findings: findingsJSON(cfg, res.Findings), Refusals: []checkRefusal{}, Warnings: []checkRefusal{},
		Changes: []checkChange{}, LeftAlone: []checkLeftAlone{}}
	for _, r := range res.Refusals {
		out.Refusals = append(out.Refusals, checkRefusal{r.Model, r.Key, r.Reason})
	}
	for _, w := range res.Warnings {
		out.Warnings = append(out.Warnings, checkRefusal{w.Model, w.Key, w.Reason})
	}
	for i, c := range res.Changes {
		out.Changes = append(out.Changes, checkChange{c.Model, string(c.Kind), jsonValues(c.Key),
			jsonValues(c.Old), jsonValues(c.New), res.Hints[i]})
	}
	for _, a := range res.LeftAlone {
		out.LeftAlone = append(out.LeftAlone, checkLeftAlone{a.Model, string(a.Mode), a.Rows, a.Changed, a.Columns})
	}
	return out
}
