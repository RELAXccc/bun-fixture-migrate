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

// jsonChange is a change of a migration, from the base to the fixture files.
type jsonChange struct {
	Model string         `json:"model"`
	Kind  string         `json:"kind"`
	Key   map[string]any `json:"key"`
	Old   map[string]any `json:"old,omitempty"`
	New   map[string]any `json:"new,omitempty"`
}

func jsonChanges(changes []fixturechange.Change) []jsonChange {
	out := []jsonChange{}
	for _, c := range changes {
		out = append(out, jsonChange{c.Model, string(c.Kind), jsonValues(c.Key), jsonValues(c.Old), jsonValues(c.New)})
	}
	return out
}

// orEmpty is a list as JSON writes it: [] for none.
func orEmpty[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
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

// MarshalJSON is the result as generate -json prints it.
func (g Generated) MarshalJSON() ([]byte, error) {
	out := struct {
		Migration  string            `json:"migration"`
		DryRun     bool              `json:"dry_run"`
		Written    []string          `json:"written"`
		Base       string            `json:"base"`
		Summary    []string          `json:"summary"`
		Changes    []jsonChange      `json:"changes"`
		Findings   []ReportedFinding `json:"findings"`
		Refusals   []jsonRefusal     `json:"refusals"`
		Warnings   []jsonRefusal     `json:"warnings"`
		NotInState []string          `json:"not_in_state"`
		Problems   []string          `json:"problems"`
		LeftOut    []string          `json:"left_out"`
		Notes      []string          `json:"notes"`
		Source     string            `json:"source,omitempty"`
	}{Migration: g.ID, DryRun: g.dryRun, Written: orEmpty(g.Written), Summary: []string{},
		Changes: []jsonChange{}, Refusals: []jsonRefusal{}, Warnings: []jsonRefusal{},
		NotInState: orEmpty(g.NotInState), Problems: orEmpty(g.Problems), LeftOut: orEmpty(g.LeftOut)}
	out.Findings = reportFindings(g.cfg, append(append([]Finding{}, g.Findings...), g.Lint...))
	out.Notes = append(append([]string{}, g.Notes...), g.Warnings...)
	if g.Diff != nil {
		out.Base = g.Diff.Base
		out.Summary = orEmpty(g.Diff.Summary())
		out.Changes = jsonChanges(g.Diff.Changes)
		out.Refusals, out.Warnings = jsonRefusals(g.Diff.Refusals), jsonRefusals(g.Diff.Warnings)
	}
	if g.dryRun {
		out.Source = string(g.Source)
	}
	return json.Marshal(out)
}

// MarshalJSON is the result as baseline -json prints it.
func (b Baselined) MarshalJSON() ([]byte, error) {
	out := struct {
		State      string            `json:"state"`
		Recorded   string            `json:"recorded"`
		Written    []string          `json:"written"`
		Unchanged  bool              `json:"unchanged"`
		Respelled  bool              `json:"respelled"`
		Summary    []string          `json:"summary"`
		Refusals   []jsonRefusal     `json:"refusals"`
		Findings   []ReportedFinding `json:"findings"`
		Problems   []string          `json:"problems"`
		NotInState []string          `json:"not_in_state"`
		LeftOut    []string          `json:"left_out"`
	}{State: b.StatePath, Recorded: b.Recorded, Written: orEmpty(b.Written), Unchanged: b.Unchanged,
		Respelled: b.Respelled, Summary: []string{}, Refusals: []jsonRefusal{},
		Findings: reportFindings(b.cfg, b.Findings), Problems: orEmpty(b.Problems),
		NotInState: orEmpty(b.NotInState), LeftOut: orEmpty(b.LeftOut)}
	if b.Diff != nil {
		out.Summary = orEmpty(b.Diff.Summary())
		out.Refusals = jsonRefusals(b.Diff.Refusals)
	}
	return json.Marshal(out)
}

// MarshalJSON is the result as export -json prints it.
func (e Exported) MarshalJSON() ([]byte, error) {
	out := struct {
		Written  []string          `json:"written"`
		Files    []jsonExportFile  `json:"files"`
		Findings []ReportedFinding `json:"findings"`
		Notes    []string          `json:"notes"`
	}{Written: orEmpty(e.Written), Files: []jsonExportFile{}, Notes: orEmpty(e.Notes)}
	var cfg *Config
	if e.p != nil {
		cfg = e.p.Config
	}
	out.Findings = reportFindings(cfg, e.Findings)
	for i, f := range e.Files {
		file := jsonExportFile{Path: f.Path}
		if i < len(e.DroppedComments) {
			file.DroppedComments = e.DroppedComments[i]
		}
		out.Files = append(out.Files, file)
	}
	return json.Marshal(out)
}

type jsonExportFile struct {
	Path            string `json:"path"`
	DroppedComments int    `json:"dropped_comments"`
}
