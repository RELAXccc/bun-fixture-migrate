package fixturemigrate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
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
	// Hints explain a difference an update holds, by the change's index in
	// Changes and the column: where the database holds the column's default
	// and the file a null or a zero, which bun writes as DEFAULT.
	Hints map[int]map[string]string
}

// Drifted reports whether anything at all was found, warnings included.
func (c *CheckResult) Drifted() bool {
	return len(c.Changes) > 0 || len(c.Refusals) > 0 || len(c.Warnings) > 0 || len(c.Findings) > 0
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
	for i, c := range res.Changes {
		if c.Kind != fixturechange.Update {
			continue
		}
		table := database.tables[c.Model]
		if table == nil {
			table = fixture.tables[c.Model]
		}
		if table == nil {
			continue
		}
		for _, col := range sortedColumns(c.New) {
			column, ok := table.Column(col)
			if !ok {
				continue
			}
			if hint := defaultHint(column, c.Old[col], c.New[col]); hint != "" {
				if out.Hints == nil {
					out.Hints = map[int]map[string]string{}
				}
				if out.Hints[i] == nil {
					out.Hints[i] = map[string]string{}
				}
				out.Hints[i][col] = hint
			}
		}
	}
	return out, nil
}

// defaultHint says why a column a database holds its default in and the file
// a null or a zero differs, "" when that is not the case. bun writes DEFAULT,
// not the value, for a nil pointer, for a zero in a nullzero field and for a
// zero in a field with a default, on an INSERT and, since bun v1.2.17, on an
// UPDATE of a model too: whatever bun wrote the row from such a field, a seed
// or an admin UI, left the default there. It is the drift that looks the
// most mysterious, and the lints say it of the file only where the policy
// does not ignore them.
func defaultHint(column dbschema.Column, database, file fixturechange.Value) string {
	if database.IsNull || database.Ref != nil || file.Ref != nil {
		return ""
	}
	def, literal := column.LiteralDefault()
	if !literal {
		return ""
	}
	var what string
	switch {
	case file.IsNull:
		what = "a nil pointer or a nullzero field"
	default:
		zero, known := column.ZeroText()
		if !known || !sameScalar(file.Lit, zero) {
			return ""
		}
		if hazard, _ := column.ZeroIsNotDefault(); !hazard {
			return ""
		}
		what = "a zero in a field with a default or a nullzero one"
	}
	if database.Lit != def && !(numericType(column.Type) && sameScalar(database.Lit, def)) {
		return ""
	}
	return fmt.Sprintf("the column defaults to %s, and bun writes DEFAULT for %s, on INSERT and, since "+
		"v1.2.17, on UPDATE too, so a row bun wrote holds the default: write %s in the file, or drop the "+
		"column default", def, what, def)
}

// Lines is the report, one problem per line or per short block, in the order a
// person would want to read it: what is wrong with the file first, then what
// the database and the file disagree about.
func (c *CheckResult) Lines() []string {
	var out []string
	grouped := FindingsByKind(c.Findings)
	for _, kind := range []FindingKind{FindingUnknownColumn, FindingInvalidValue, FindingAmbiguousValue,
		FindingZeroDefault, FindingNullDefault, FindingDuplicateKey, FindingDuplicateID, FindingUnbackedKey,
		FindingSoftDelete} {
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
	if len(c.Warnings) > 0 {
		out = append(out, "", "Warnings, which the policy lets a migration carry on past:")
		for _, w := range c.Warnings {
			out = append(out, "  "+w.String())
		}
	}

	var del []fixturechange.Change
	var ins, upd []int
	for i, ch := range c.Changes {
		switch ch.Kind {
		case fixturechange.Insert:
			ins = append(ins, i)
		case fixturechange.Update:
			upd = append(upd, i)
		default:
			del = append(del, ch)
		}
	}
	if len(ins) > 0 {
		out = append(out, "", "In the fixture file, not in the database:")
		for _, i := range ins {
			ch := c.Changes[i]
			out = append(out, "  "+ch.Model+" "+keyLabelOf(ch.Key)+softDeletedNote(c.SoftDeleted, i))
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
		for _, i := range upd {
			ch := c.Changes[i]
			out = append(out, "  "+ch.Model+" "+keyLabelOf(ch.Key))
			for _, col := range sortedColumns(ch.New) {
				out = append(out, fmt.Sprintf("    %s: database %s, file %s",
					col, ch.Old[col].String(), ch.New[col].String()))
				if hint := c.Hints[i][col]; hint != "" {
					out = append(out, "      hint: "+hint)
				}
			}
		}
	}
	if len(out) == 0 {
		out = []string{"", "the database and " + c.Head + " agree"}
	}
	// What the configuration leaves to the database is no drift, and is
	// counted only so nobody wonders whether check saw it.
	if lines := c.LeftAloneLines(); len(lines) > 0 {
		out = append(out, "", "Left to the database by the configuration, which is no drift:")
		for _, line := range lines {
			out = append(out, "  "+line)
		}
	}
	return out[1:]
}

// softDeletedNote is what check adds to a row the fixture files hold and the
// database holds soft-deleted, by the change's index: when it was deleted,
// and that a migration brings it back.
func softDeletedNote(deleted map[int]string, i int) string {
	at, ok := deleted[i]
	if !ok {
		return ""
	}
	return " (soft-deleted there at " + at + "; a migration restores it)"
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
	case FindingUnbackedKey:
		return c.Policy.KeyIndex
	}
	return ModeError
}

// warnTo is the end of a sentence that says which policies to set to warn
// for the findings they make errors to be warnings: ", or set
// policy.zero_default and Plan's duplicate_key to warn", each named where
// its value comes from (ModeOf). "" when no policy makes any of them an
// error, an invalid value say, which only fixing it settles.
func (c *Config) warnTo(findings []Finding) string {
	var names []string
	seen := map[string]bool{}
	for _, f := range findings {
		if c.ModeOf(f) != ModeError {
			continue
		}
		var key string
		switch f.Kind {
		case FindingZeroDefault:
			key = "zero_default"
		case FindingNullDefault:
			key = "null_default"
		case FindingDuplicateKey:
			key = "duplicate_key"
		default:
			continue
		}
		name := c.policyName(f.Model, key)
		if own, ok := strings.CutPrefix(name, "the model's "); ok {
			name = f.Model + "'s " + own
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return ", or set " + joinAnd(names) + " to warn"
}

// Worst is the strictest mode any of the findings calls for, and whether any
// finding is left once the ignored ones are dropped.
func (c *Config) Worst(findings []Finding) (Mode, []Finding) {
	worst := ModeWarn
	var kept []Finding
	for _, f := range findings {
		mode := c.ModeOf(f)
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
