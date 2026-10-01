package fixturemigrate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// CheckResult is what the database and the fixture file disagree about.
type CheckResult struct {
	// Result is the diff itself, database on the left and file on the right.
	// An insert in it is a row the file has and the database does not.
	*Result
	// Findings are everything both snapshots turned up on their own: a natural
	// key two rows share, a zero written against a column default, a column
	// the table does not have.
	Findings []Finding
}

// Drifted reports whether anything at all was found.
func (c *CheckResult) Drifted() bool {
	return len(c.Changes) > 0 || len(c.Refusals) > 0 || len(c.Findings) > 0
}

// Agree reports whether the database and the fixture files agree as far as
// the policy goes: no difference, nothing generate would refuse, and no
// finding the policy makes an error. A finding the policy makes a warning is
// reported, and is not disagreement: that is what a warning is for.
func (c *CheckResult) Agree(cfg *Config) bool {
	if len(c.Changes) > 0 || len(c.Refusals) > 0 {
		return false
	}
	mode, _ := cfg.Worst(c.Findings)
	return mode != ModeError
}

// Check compares a database snapshot with a fixture-file snapshot. It writes
// nothing and decides nothing; the report is for a person.
//
// It is the same diff the generator runs, read the other way round. That is on
// purpose: a check that answers a different question from the generator is a
// check that passes while the generator is about to do something else.
func Check(cfg *Config, database, fixture *Snapshot) (*CheckResult, error) {
	res, err := Compute(cfg, database, fixture)
	if err != nil {
		return nil, err
	}
	out := &CheckResult{Result: res}
	out.Findings = append(out.Findings, database.Findings...)
	out.Findings = append(out.Findings, fixture.Findings...)
	return out, nil
}

// Lines is the report, one problem per line or per short block, in the order a
// person would want to read it: what is wrong with the file first, then what
// the database and the file disagree about.
func (c *CheckResult) Lines() []string {
	var out []string
	grouped := FindingsByKind(c.Findings)
	for _, kind := range []FindingKind{FindingUnknownColumn, FindingInvalidValue, FindingZeroDefault,
		FindingNullDefault, FindingDuplicateKey} {
		list := grouped[kind]
		if len(list) == 0 {
			continue
		}
		out = append(out, "", strings.ToUpper(string(kind)[:1])+string(kind)[1:]+":")
		for _, f := range list {
			out = append(out, "  "+f.String())
		}
	}
	if len(c.Refusals) > 0 {
		out = append(out, "", "Cannot be migrated as it stands:")
		for _, r := range c.Refusals {
			out = append(out, "  "+r.String())
		}
	}

	var ins, upd, del []fixturechange.Change
	for _, ch := range c.Changes {
		switch ch.Kind {
		case fixturechange.Insert:
			ins = append(ins, ch)
		case fixturechange.Update:
			upd = append(upd, ch)
		default:
			del = append(del, ch)
		}
	}
	if len(ins) > 0 {
		out = append(out, "", "In the fixture file, not in the database:")
		for _, ch := range ins {
			out = append(out, "  "+ch.Model+" "+keyLabelOf(ch.Key))
		}
	}
	if len(del) > 0 {
		out = append(out, "", "In the database, not in the fixture file:")
		for _, ch := range del {
			out = append(out, "  "+ch.Model+" "+keyLabelOf(ch.Key))
		}
	}
	if len(upd) > 0 {
		out = append(out, "", "Different in the database and the fixture file:")
		for _, ch := range upd {
			out = append(out, "  "+ch.Model+" "+keyLabelOf(ch.Key))
			for _, col := range sortedColumns(ch.New) {
				out = append(out, fmt.Sprintf("    %s: database %s, file %s",
					col, ch.Old[col].String(), ch.New[col].String()))
			}
		}
	}
	if len(out) == 0 {
		return []string{"the database and " + c.Head + " agree"}
	}
	return out[1:]
}

func keyLabelOf(key fixturechange.Values) string {
	cols := sortedColumns(key)
	parts := make([]string, 0, len(cols))
	for _, col := range cols {
		parts = append(parts, col+"="+key[col].String())
	}
	return strings.Join(parts, ",")
}

// FindingMode is the policy that governs a kind of finding, so the caller knows
// whether it should fail.
func (c *Config) FindingMode(kind FindingKind) Mode {
	switch kind {
	case FindingZeroDefault:
		return c.Policy.ZeroDefault
	case FindingNullDefault:
		return c.Policy.NullDefault
	case FindingDuplicateKey:
		return c.Policy.DuplicateKey
	}
	return ModeError
}

// Worst is the strictest mode any of the findings calls for, and whether any
// finding is left once the ignored ones are dropped.
func (c *Config) Worst(findings []Finding) (Mode, []Finding) {
	worst := ModeWarn
	var kept []Finding
	for _, f := range findings {
		mode := c.FindingMode(f.Kind)
		if mode == ModeIgnore {
			continue
		}
		kept = append(kept, f)
		if mode == ModeError {
			worst = ModeError
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Kind < kept[j].Kind })
	return worst, kept
}
