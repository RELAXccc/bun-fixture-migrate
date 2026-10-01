package fixturemigrate

// The JSON the commands print with -json is the encoding of what the Project
// methods return, so a program reading the command's output and one calling
// the library see the same thing. Values are strings as the database spells
// them, NULL is null, and a reference is {"model": ..., "key": ...}; lists are
// [] rather than null when empty.

import (
	"encoding/json"

	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

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

type jsonRefusal struct {
	Model  string `json:"model"`
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

func jsonRefusals(refusals []Refusal) []jsonRefusal {
	out := []jsonRefusal{}
	for _, r := range refusals {
		out = append(out, jsonRefusal(r))
	}
	return out
}

// jsonCheckChange is a change between the database and the fixture files.
type jsonCheckChange struct {
	Model string         `json:"model"`
	Kind  string         `json:"kind"`
	Key   map[string]any `json:"key"`
	Old   map[string]any `json:"database,omitempty"`
	New   map[string]any `json:"file,omitempty"`
}

// MarshalJSON is the report as check -json prints it.
func (r CheckReport) MarshalJSON() ([]byte, error) {
	out := struct {
		Agree    bool              `json:"agree"`
		Findings []ReportedFinding `json:"findings"`
		Refusals []jsonRefusal     `json:"refusals"`
		Warnings []jsonRefusal     `json:"warnings"`
		Changes  []jsonCheckChange `json:"changes"`
	}{Agree: r.Agree, Findings: reportFindings(r.cfg, r.Findings), Refusals: []jsonRefusal{},
		Warnings: []jsonRefusal{}, Changes: []jsonCheckChange{}}
	if r.Diff != nil {
		out.Refusals, out.Warnings = jsonRefusals(r.Diff.Refusals), jsonRefusals(r.Diff.Warnings)
		for _, c := range r.Diff.Changes {
			out.Changes = append(out.Changes, jsonCheckChange{c.Model, string(c.Kind), jsonValues(c.Key),
				jsonValues(c.Old), jsonValues(c.New)})
		}
	}
	return json.Marshal(out)
}

// MarshalJSON is the report as sync -json prints it.
func (r SyncReport) MarshalJSON() ([]byte, error) {
	out := struct {
		Applied  bool                   `json:"applied"`
		DryRun   bool                   `json:"dry_run"`
		Findings []ReportedFinding      `json:"findings"`
		Refusals []jsonRefusal          `json:"refusals"`
		Warnings []jsonRefusal          `json:"warnings"`
		Changes  []fixtureapply.Outcome `json:"changes"`
	}{DryRun: r.DryRun, Refusals: []jsonRefusal{}, Warnings: []jsonRefusal{}, Changes: []fixtureapply.Outcome{}}
	if r.SyncResult != nil {
		out.Applied = r.Applied
		out.Findings = reportFindings(r.cfg, r.SyncResult.Findings)
		if r.Diff != nil {
			out.Refusals, out.Warnings = jsonRefusals(r.Diff.Refusals), jsonRefusals(r.Diff.Warnings)
		}
		out.Changes = append(out.Changes, r.Outcomes...)
	} else {
		out.Findings = []ReportedFinding{}
	}
	return json.Marshal(out)
}
