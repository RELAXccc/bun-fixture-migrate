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
	// Changes are what a migration from the database to the fixture file
	// would do: an insert is a row only the file has, a delete a row only the
	// database has, and an update's old values are the database's.
	Changes []checkChange `json:"changes"`
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
		Findings: findingsJSON(cfg, res.Findings), Refusals: []checkRefusal{}, Changes: []checkChange{}}
	for _, r := range res.Refusals {
		out.Refusals = append(out.Refusals, checkRefusal{r.Model, r.Key, r.Reason})
	}
	for _, c := range res.Changes {
		out.Changes = append(out.Changes, checkChange{c.Model, string(c.Kind), jsonValues(c.Key),
			jsonValues(c.Old), jsonValues(c.New)})
	}
	return out
}
