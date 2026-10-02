package fixturemigrate

import (
	"strings"
	"testing"
)

// The database's own ids of an ids: database model are nobody's business: an
// export writes none, and names the rows by their anchors.
func TestExportWritesNoIDTheDatabaseGives(t *testing.T) {
	cfg := ownedConfig(t, func(cfg *Config) { cfg.Models["Plan"].IDs = IDsDatabase })
	s := snap(t, cfg, base, "db")
	out, err := Export(cfg, s, testTables(), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	plans := text[strings.Index(text, "- model: Plan"):strings.Index(text, "- model: Feature")]
	if strings.Contains(plans, "\n      id: ") {
		t.Fatalf("the plans carry an id:\n%s", plans)
	}
	if !strings.Contains(text, "{{ $.Plan.team.ID }}") {
		t.Fatalf("references go through anchors:\n%s", text)
	}
}
