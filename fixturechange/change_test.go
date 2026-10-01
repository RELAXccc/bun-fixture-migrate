package fixturechange

import "testing"

// A model's table can carry its own policy, and only the fields it sets
// replace the set's: translations at changed_row warn, prices at error.
func TestPolicyForTakesTheTablesFieldsOverTheSets(t *testing.T) {
	set := Set{
		Policy: Policy{MissingRow: ModeError, ChangedRow: ModeError, IDDrift: ModeError, DuplicateKey: ModeError},
		Tables: Tables{
			"Price":       {Name: "prices", ID: "id"},
			"Translation": {Name: "translations", ID: "id", Policy: &Policy{ChangedRow: ModeWarn}},
			"Empty":       {Name: "empties", ID: "id", Policy: &Policy{}},
		},
	}
	if got := set.PolicyFor("Price"); got != set.Policy {
		t.Fatalf("a table without a policy runs under the set's: %+v", got)
	}
	want := set.Policy
	want.ChangedRow = ModeWarn
	if got := set.PolicyFor("Translation"); got != want {
		t.Fatalf("PolicyFor(Translation) = %+v, want %+v", got, want)
	}
	if got := set.PolicyFor("Empty"); got != set.Policy {
		t.Fatalf("an empty table policy leaves every field to the set: %+v", got)
	}
	if got := set.PolicyFor("Unknown"); got != set.Policy {
		t.Fatalf("a model without a table runs under the set's: %+v", got)
	}
}
