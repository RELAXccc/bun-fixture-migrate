package fixturemigrate

import (
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// These pin the reading of a fixture file to dbfixture's own. A file this tool
// accepts has to be a file dbfixture loads, and a reference has to name the row
// dbfixture would have bound it to; dbtest/dbfixture_test.go shows each of
// these against the real loader.

func snapErr(t *testing.T, text string) error {
	t.Helper()
	_, err := FixtureSnapshot(testConfig(t), doc(t, text), "fixture.yml")
	return err
}

func planRef(t *testing.T, s *Snapshot, plan string) fixturechange.Value {
	t.Helper()
	for _, e := range s.Entries["Plan"] {
		if e.Cells["name"].Lit == plan {
			return e.Cells["currency_id"]
		}
	}
	t.Fatalf("no plan %q", plan)
	return fixturechange.Value{}
}

// A row without "_id" is registered as "pk" and its primary key.
func TestARowWithoutAnAnchorIsNamedByItsPrimaryKey(t *testing.T) {
	text := `- model: Currency
  rows:
    - id: 7
      code: EUR
      symbol: "E"
- model: Plan
  rows:
    - _id: team
      id: 2
      name: team
      currency_id: '{{ $.Currency.pk7.ID }}'
`
	got := planRef(t, snap(t, testConfig(t), text, "fixture.yml"), "team")
	if got.Ref == nil || got.Ref.Model != "Currency" || got.Ref.Key != "EUR" {
		t.Fatalf("got %+v", got)
	}
	// A row with an anchor is registered under the anchor only.
	withAnchor := strings.Replace(text, "    - id: 7\n", "    - _id: eur\n      id: 7\n", 1)
	if err := snapErr(t, withAnchor); err == nil || !strings.Contains(err.Error(), "names no row") {
		t.Fatalf("pk7 is not a name for a row that has an _id: %v", err)
	}
}

// dbfixture resolves a template while it loads the file, against the rows it
// has inserted so far.
func TestAReferenceToARowFurtherDownIsAnError(t *testing.T) {
	err := snapErr(t, `- model: Plan
  rows:
    - _id: team
      id: 2
      name: team
      currency_id: '{{ $.Currency.eur.ID }}'
- model: Currency
  rows:
    - _id: eur
      id: 1
      code: EUR
      symbol: "E"
`)
	if err == nil || !strings.Contains(err.Error(), "further down") {
		t.Fatalf("expected the forward reference to be refused, got %v", err)
	}
}

// Two rows can share an anchor; each template names the latest one above it.
func TestTheLatestRowOfAnAnchorIsTheOneNamed(t *testing.T) {
	s := snap(t, testConfig(t), `- model: Currency
  rows:
    - _id: main
      id: 1
      code: EUR
      symbol: "E"
- model: Plan
  rows:
    - _id: early
      id: 1
      name: early
      currency_id: '{{ $.Currency.main.ID }}'
- model: Currency
  rows:
    - _id: main
      id: 2
      code: USD
      symbol: "$"
- model: Plan
  rows:
    - _id: late
      id: 2
      name: late
      currency_id: '{{ $.Currency.main.ID }}'
`, "fixture.yml")
	if got := planRef(t, s, "early"); got.Ref == nil || got.Ref.Key != "EUR" {
		t.Fatalf("early: %+v", got)
	}
	if got := planRef(t, s, "late"); got.Ref == nil || got.Ref.Key != "USD" {
		t.Fatalf("late: %+v", got)
	}
}

// dbfixture only evaluates "{{ " ... " }}" with the spaces. Without them the
// text is not a template, and in a reference column it names no id.
func TestATemplateNeedsDbfixturesDelimiters(t *testing.T) {
	err := snapErr(t, strings.Replace(base, `'{{ $.Currency.eur.ID }}'`, `'{{$.Currency.eur.ID}}'`, 1))
	if err == nil || !strings.Contains(err.Error(), "no row of Currency") {
		t.Fatalf("expected the unspaced template to be read as text, got %v", err)
	}
}

// A template this tool cannot evaluate never reaches the database as written,
// so it can neither be compared nor written into a migration.
func TestATemplateThisToolCannotEvaluateIsAnError(t *testing.T) {
	text := strings.Replace(base, "      seats: 10\n", "      seats: 10\n      created_at: '{{ now }}'\n", 1)
	err := snapErr(t, text)
	if err == nil || !strings.Contains(err.Error(), "put created_at in ignore") {
		t.Fatalf("expected the template to be refused, got %v", err)
	}
	cfg := testConfig(t)
	cfg.Models["Plan"].Ignore = []string{"created_at"}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	if _, err := FixtureSnapshot(cfg, doc(t, text), "fixture.yml"); err != nil {
		t.Fatalf("an ignored column is not read at all: %v", err)
	}
}

// bun writes DEFAULT for a zero only in an autoincrement field, so a zero is
// "no id" in a serial model and an id like any other elsewhere.
func TestAZeroIDIsAnIDUnlessTheModelIsSerial(t *testing.T) {
	cfg := testConfig(t)
	text := `- model: Currency
  rows:
    - {id: 0, code: XXX}
- model: Plan
  rows:
    - {id: 0, name: free, currency_id: 0}
- model: Feature
  rows:
    - {id: 0, plan_id: 0, code: api}
`
	s := snap(t, cfg, text, "fixture.yml")
	if got := s.Entries["Currency"][0].ID; got != "0" {
		t.Fatalf("a zero in a model that is not serial is its id, got %q", got)
	}
	if got := s.Entries["Feature"][0].ID; got != "" {
		t.Fatalf("a zero in a serial model is left to the sequence, got %q", got)
	}
	// And a reference holding 0 names the row whose id is 0, where there is
	// one.
	if got := planRef(t, s, "free"); got.Ref == nil || got.Ref.Key != "XXX" {
		t.Fatalf("expected Currency XXX, got %+v", got)
	}
	if got := s.Entries["Feature"][0].Cells["plan_id"]; got.Ref == nil || got.Ref.Key != "free" {
		t.Fatalf("expected Plan free, got %+v", got)
	}
}

// A reference carries its row's ref value, and 0012 there is the integer 10
// or the text 0012 depending on the ref column's type. Both readings travel
// with the reference, and the ref column is the one whose type decides,
// whether the reference is a template or a plain id.
func TestAReferenceKeepsBothReadingsOfItsRow(t *testing.T) {
	cfg := testConfig(t)
	text := `- model: Currency
  rows:
    - {_id: odd, id: 1, code: 0012}
    - {_id: ten, id: 2, code: "10"}
- model: Plan
  rows:
    - {id: 1, name: a, currency_id: '{{ $.Currency.odd.ID }}', note: '{{ $.Currency.odd.Code }}'}
    - {id: 2, name: b, currency_id: 1}
`
	s := snap(t, cfg, text, "fixture.yml")
	for _, e := range s.Entries["Plan"] {
		if ref := e.Cells["currency_id"].Ref; ref == nil || ref.Key != "10" {
			t.Fatalf("expected the resolved reading, got %+v", e.Cells["currency_id"])
		}
		if e.AsWritten["currency_id"] != "0012" || e.from["currency_id"] != (source{"Currency", "code"}) {
			t.Fatalf("expected the reading as written and its column, got %q %+v", e.AsWritten, e.from)
		}
	}
	// A template copying a field hands on what that field holds, so the
	// field's type decides there too.
	a := s.Entries["Plan"][0]
	if a.Cells["note"].Lit != "10" || a.AsWritten["note"] != "0012" || a.from["note"] != (source{"Currency", "code"}) {
		t.Fatalf("got %+v %q %+v", a.Cells["note"], a.AsWritten, a.from)
	}
	// Until a type decides, 0012 and "10" are not known to be one key, so
	// neither is reported as a duplicate of the other.
	if len(s.Findings) != 0 {
		t.Fatalf("expected no finding, got %+v", s.Findings)
	}

	// And a change that needs to know is refused without the database.
	res := computeWith(t, cfg, text, text+"    - {id: 3, name: c, currency_id: '{{ $.Currency.odd.ID }}'}\n")
	if len(res.Changes) != 0 || len(res.Refusals) != 1 ||
		!strings.Contains(res.Refusals[0].Reason, "currency_id points at the Currency whose code is written 0012") {
		t.Fatalf("expected the insert to be refused, got %+v / %+v", res.Changes, res.Refusals)
	}
	// So is a reference whose row only changed its spelling.
	res = computeWith(t, cfg, text, strings.Replace(text, "code: 0012", "code: 012", 1))
	for _, r := range res.Refusals {
		if r.Model == "Plan" && strings.Contains(r.Reason, "written 0012 before and 012 after") {
			return
		}
	}
	t.Fatalf("expected the respelled reference to be refused, got %+v / %+v", res.Changes, res.Refusals)
}

// A template copying a field that is itself a template copies whatever
// dbfixture made of that one, which this tool does not follow.
func TestACopyOfATemplateIsRefused(t *testing.T) {
	err := snapErr(t, `- model: Currency
  rows:
    - {_id: eur, id: 1, code: EUR}
- model: Plan
  rows:
    - {_id: a, id: 1, name: a, currency_id: '{{ $.Currency.eur.ID }}'}
    - {id: 2, name: b, currency_id: '{{ $.Currency.eur.ID }}', note: '{{ $.Plan.a.CurrencyID }}'}
`)
	if err == nil || !strings.Contains(err.Error(), "which is itself a template") {
		t.Fatalf("expected the copy to be refused, got %v", err)
	}
}

// A key value whose spelling alone changed is the same key in a numeric
// column and a rename in a text one, and says so.
func TestARespelledKeyIsNeitherARenameNorNothing(t *testing.T) {
	text := "- model: Currency\n  rows:\n    - {_id: odd, id: 1, code: 0012}\n"
	res := computeWith(t, testConfig(t), text, strings.Replace(text, "code: 0012", "code: 012", 1))
	if len(res.Changes) != 0 || len(res.Refusals) != 1 ||
		!strings.Contains(res.Refusals[0].Reason, "code is written 0012 before and 012 after") {
		t.Fatalf("expected one refusal about the spelling, got %+v / %+v", res.Changes, res.Refusals)
	}
}

// An alias is the value it names, scalar or not, null included.
func TestAnAliasReadsAsTheValueItNames(t *testing.T) {
	d := doc(t, `- model: Plan
  rows:
    - {name: a, note: &n "shared", seats: &z ~, tags: &t [x, *n, 017]}
    - {name: b, note: *n, seats: *z, tags: *t}
`)
	got := d[0].Rows[1]
	if c := got["note"]; c.Structured || c.IsNull || c.Text != "shared" || c.Tag != "!!str" {
		t.Fatalf("note: %+v", c)
	}
	if c := got["seats"]; !c.IsNull {
		t.Fatalf("seats: %+v", c)
	}
	if c := got["tags"]; !c.Structured || c.Text != `["x","shared",15]` || c.StringText != `["x","shared","017"]` {
		t.Fatalf("tags: %+v", c)
	}
}

// dbfixture evaluates a template only in a scalar tagged !!str, and an alias
// has no tag: it would store the template's text. That is refused.
func TestAnAliasOfATemplateIsRefused(t *testing.T) {
	_, err := ParseDoc([]byte(`- model: Plan
  rows:
    - {name: a, currency_id: &c '{{ $.Currency.eur.ID }}'}
    - {name: b, currency_id: *c}
`))
	if err == nil || !strings.Contains(err.Error(), "Plan.currency_id: line 4: *c stands for {{ $.Currency.eur.ID }}") {
		t.Fatalf("expected the alias to be refused, got %v", err)
	}
}

// A template of text and string constants evaluates to that text whatever
// dbfixture evaluates it against: it is how a file stores a value holding
// "{{ " and " }}", and it reads as that value.
func TestATemplateOfStringConstantsIsItsText(t *testing.T) {
	text := replace(t, base, "      seats: 1\n", "      seats: 1\n      note: '{{ \"Hello {{ name }}\" }}, and {{ `{{ more }}` }}'\n")
	s := snap(t, testConfig(t), replace(t, text, "      seats: 10\n", "      seats: 10\n      greeting: '{{ $.Plan.free.Note }}'\n"),
		"fixture.yml")
	free, team := s.Entries["Plan"][0], s.Entries["Plan"][1]
	if got := free.Cells["note"].Lit; got != "Hello {{ name }}, and {{ more }}" {
		t.Fatalf("got %q", got)
	}
	if got := team.Cells["greeting"].Lit; got != "Hello {{ name }}, and {{ more }}" {
		t.Fatalf("a copy of it is the same text, got %q", got)
	}
	if _, ok := literalTemplate(`{{ "a" | printf "%s" }}`); ok {
		t.Fatal("a pipeline is not a constant")
	}
}

// A row with more than one fault is refused for the same one on every run:
// the first column by name, not whichever a map hands out first.
func TestARowWithTwoFaultsIsRefusedForTheSameOneEveryTime(t *testing.T) {
	text := replace(t, base, "      seats: 10\n",
		"      seats: 10\n      zz_note: '{{ now }}'\n      aa_note: '{{ $.Currency.gbp.ID }}'\n")
	first := snapErr(t, text)
	if first == nil || !strings.Contains(first.Error(), "aa_note") {
		t.Fatalf("expected aa_note to be named, got %v", first)
	}
	for i := 0; i < 50; i++ {
		if err := snapErr(t, text); err == nil || err.Error() != first.Error() {
			t.Fatalf("run %d: %v, not %v", i, err, first)
		}
	}
}

// dbfixture copies a field by printing it with fmt, which prints a nil
// pointer as <nil> and a plain field's zero as "" or 0, and a map or a slice
// as fmt does: a copy of a null or of a structured value is refused, and so
// is an id written as a template.
func TestACopyOfANullOrAStructureIsRefused(t *testing.T) {
	for _, tc := range []struct{ note, want string }{
		{"note: ~", "copies note, which is null in that row"},
		{"note: [a]", "copies note, which is a mapping or a sequence in that row"},
	} {
		err := snapErr(t, `- model: Currency
  rows:
    - {_id: eur, id: 1, code: EUR}
- model: Plan
  rows:
    - {_id: a, id: 1, name: a, currency_id: '{{ $.Currency.eur.ID }}', `+tc.note+`}
    - {id: 2, name: b, currency_id: '{{ $.Currency.eur.ID }}', note: '{{ $.Plan.a.Note }}'}
`)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: expected the copy to be refused, got %v", tc.note, err)
		}
	}
	err := snapErr(t, `- model: Currency
  rows:
    - {_id: eur, id: 1, code: EUR}
    - {id: '{{ $.Currency.eur.ID }}', code: USD}
`)
	if err == nil || !strings.Contains(err.Error(), "Currency.id is {{ $.Currency.eur.ID }}, a template") {
		t.Errorf("expected the template id to be refused, got %v", err)
	}
}

// What a copy stores depends on the Go type of the field it copies, which
// only the column's type tells: without the database a change carrying one
// is refused, with it a copy of a string or an integer column is the value
// and a copy of any other is an invalid value.
func TestACopyOfAFieldIsDecidedByItsColumnsType(t *testing.T) {
	cfg := testConfig(t)
	text := `- model: Currency
  rows:
    - {_id: eur, id: 1, code: EUR, symbol: "E"}
- model: Plan
  rows:
    - {_id: a, id: 1, name: a, currency_id: '{{ $.Currency.eur.ID }}', seats: 3, note: x}
`
	next := text + "    - {id: 2, name: b, currency_id: '{{ $.Currency.eur.ID }}', seats: 3, note: '{{ $.Plan.a.Seats }}'}\n"
	res := computeWith(t, cfg, text, next)
	if len(res.Changes) != 0 || len(res.Refusals) != 1 ||
		!strings.Contains(res.Refusals[0].Reason, "note copies seats of a Plan row, and dbfixture stores") {
		t.Fatalf("expected the insert to be refused, got %+v / %+v", res.Changes, res.Refusals)
	}

	tables := testTables()
	tables["public.plans"].Columns = append(tables["public.plans"].Columns,
		dbschema.Column{Name: "rate", Position: 8, Type: "float8", FullType: "double precision", Nullable: true})
	for _, tc := range []struct {
		field   string
		settled bool
	}{{"Seats", true}, {"Name", true}, {"Rate", false}} {
		text := strings.Replace(next, "Plan.a.Seats", "Plan.a."+tc.field, 1)
		text = strings.Replace(text, "seats: 3, note: x", "seats: 3, note: x, rate: 100000000", 1)
		s := snap(t, cfg, text, "fixture.yml")
		settleCopies(cfg, s, tables)
		b := s.Entries["Plan"][1]
		if _, open := b.copied["note"]; open {
			t.Errorf("%s: the copy is still open", tc.field)
		}
		var found bool
		for _, f := range s.Findings {
			found = found || (f.Kind == FindingInvalidValue && strings.Contains(f.Detail, "note copies rate of a Plan row, a double precision column"))
		}
		if found == tc.settled {
			t.Errorf("%s: findings %+v", tc.field, s.Findings)
		}
	}
}

// A copy of a bool field is what fmt prints of it, true or false, whatever
// spelling of it the file loads into the field; a copy of a uuid field is
// the uuid, which a uuid column reads as one value from a string field and a
// uuid type alike, and any other column only when the file writes it the way
// a uuid type prints it. Before, both were an invalid value, the reviewer's
// owner_active and owner_ext copies of a user.
func TestACopyOfABoolOrAUUIDField(t *testing.T) {
	cfg := &Config{Models: map[string]*Model{
		"User": {Table: "users"},
		"Org":  {Table: "orgs", References: map[string]string{"owner_id": "User"}},
	}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	col := func(pos int, name, typ, full string) dbschema.Column {
		return dbschema.Column{Name: name, Position: pos, Type: typ, FullType: full, Category: map[string]string{
			"text": "S", "bool": "B", "uuid": "U", "int8": "N", "float8": "N"}[typ]}
	}
	tables := map[string]*dbschema.Table{
		"public.users": {Schema: "public", Name: "users", Columns: []dbschema.Column{col(1, "id", "int8", "bigint"),
			col(2, "name", "text", "text"), col(3, "active", "bool", "boolean"), col(4, "ext", "uuid", "uuid")}},
		"public.orgs": {Schema: "public", Name: "orgs", Columns: []dbschema.Column{col(1, "id", "int8", "bigint"),
			col(2, "name", "text", "text"), col(3, "owner_id", "int8", "bigint"),
			col(4, "owner_active", "bool", "boolean"), col(5, "owner_label", "text", "text"),
			col(6, "owner_ext", "uuid", "uuid"), col(7, "owner_ext_text", "text", "text")}},
	}
	for _, tc := range []struct {
		active, ext string
		want        map[string]string // column -> value, or "!" + a finding's text
	}{
		{"true", "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", map[string]string{"owner_active": "true", "owner_label": "true",
			"owner_ext": "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", "owner_ext_text": "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"}},
		{"yes", "A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11", map[string]string{"owner_active": "true", "owner_label": "true",
			"owner_ext": "A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11",
			"owner_ext_text": "!owner_ext_text copies ext of a User row, a uuid column, and dbfixture copies what " +
				"that field holds as fmt prints it: A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11 from a string field"}},
		{"Off", "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", map[string]string{"owner_active": "false", "owner_label": "false"}},
		{"t", "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", map[string]string{
			"owner_label": "!owner_label copies active of a User row, a boolean column, and t does not load into a bool field"}},
	} {
		text := "- model: User\n  rows:\n    - {_id: smith, id: 1, name: smith, active: " + tc.active + ", ext: " + tc.ext + "}\n" +
			"- model: Org\n  rows:\n    - {id: 1, name: o, owner_id: '{{ $.User.smith.ID }}', " +
			"owner_active: '{{ $.User.smith.Active }}', owner_label: '{{ $.User.smith.Active }}', " +
			"owner_ext: '{{ $.User.smith.Ext }}', owner_ext_text: '{{ $.User.smith.Ext }}'}\n"
		s := snap(t, cfg, text, "fixture.yml")
		settleCopies(cfg, s, tables)
		org := s.Entries["Org"][0]
		if len(org.copied) != 0 {
			t.Errorf("%s/%s: copies still open: %v", tc.active, tc.ext, org.copied)
		}
		for column, want := range tc.want {
			if finding, ok := strings.CutPrefix(want, "!"); ok {
				var found bool
				for _, f := range s.Findings {
					found = found || f.Kind == FindingInvalidValue && strings.Contains(f.Detail, finding)
				}
				if !found {
					t.Errorf("%s/%s: expected %q, got %+v", tc.active, tc.ext, finding, s.Findings)
				}
				continue
			}
			if got := org.Cells[column].Lit; got != want {
				t.Errorf("%s/%s: %s is %q, want %q", tc.active, tc.ext, column, got, want)
			}
			for _, f := range s.Findings {
				if strings.HasPrefix(f.Detail, column+" ") {
					t.Errorf("%s/%s: %s", tc.active, tc.ext, f.Detail)
				}
			}
		}
	}
}
