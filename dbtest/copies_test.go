package dbtest_test

import (
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/uptrace/bun"
)

// CpUUID is a uuid type as github.com/google/uuid declares it: sixteen bytes
// with a String method, a Scan and a Value.
type CpUUID [16]byte

func (u CpUUID) String() string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:])
}

func (u *CpUUID) Scan(src any) error {
	var s string
	switch v := src.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return fmt.Errorf("scan %T into a uuid", src)
	}
	b, err := hex.DecodeString(strings.NewReplacer("-", "", "{", "", "}", "").Replace(s))
	if err != nil || len(b) != 16 {
		return fmt.Errorf("%q is no uuid", s)
	}
	copy(u[:], b)
	return nil
}

func (u CpUUID) Value() (driver.Value, error) { return u.String(), nil }

type CpUser struct {
	bun.BaseModel `bun:"table:cp_users"`
	ID            int64       `bun:"id,pk"`
	Name          string      `bun:"name,notnull"`
	Active        bool        `bun:"active,notnull"`
	Ext           string      `bun:"ext,type:uuid,notnull"`
	Ext2          CpUUID      `bun:"ext2,type:uuid,notnull"`
	Ext3          pgtype.UUID `bun:"ext3,type:uuid,notnull"`
}

type CpOrg struct {
	bun.BaseModel `bun:"table:cp_orgs"`
	ID            int64  `bun:"id,pk"`
	Name          string `bun:"name,notnull"`
	OwnerActive   bool   `bun:"owner_active,notnull"`
	OwnerLabel    string `bun:"owner_label,notnull"`
	OwnerExt      string `bun:"owner_ext,type:uuid,notnull"`
	OwnerExt2     string `bun:"owner_ext2,notnull"`
	OwnerExt3     string `bun:"owner_ext3,notnull"`
}

// Templates copying a bool and a uuid field, as the reviewer's project has
// them, seeded by dbfixture: the tool reads what fmt printed into the row, a
// bool as true or false and a uuid as the uuid, through a string field, a
// google/uuid-like type and pgx's, and a migration and a sync store what a
// fresh seed of the new file does. Before, every copy was an invalid value.
func TestCopiesOfABoolAndAUUIDField(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{
		"CpUser": {Table: "cp_users"},
		"CpOrg":  {Table: "cp_orgs"},
	}, "cp_users",
		[]string{"DROP TABLE IF EXISTS cp_orgs", "DROP TABLE IF EXISTS cp_users",
			"CREATE TABLE cp_users (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, active bool NOT NULL, " +
				"ext uuid NOT NULL, ext2 uuid NOT NULL, ext3 uuid NOT NULL)",
			"CREATE TABLE cp_orgs (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, owner_active bool NOT NULL, " +
				"owner_label text NOT NULL, owner_ext uuid NOT NULL, owner_ext2 text NOT NULL, owner_ext3 text NOT NULL)"},
		`SELECT string_agg(concat_ws('|', name, owner_active, owner_label, owner_ext, owner_ext2, owner_ext3), E'\n' ORDER BY name) FROM cp_orgs`,
		(*CpUser)(nil), (*CpOrg)(nil))
	file := func(active, ext string) string {
		return "- model: CpUser\n  rows:\n    - {_id: smith, id: 1, name: smith, active: " + active + ", ext: " + ext +
			", ext2: " + ext + ", ext3: " + ext + "}\n" +
			"- model: CpOrg\n  rows:\n    - {id: 1, name: o, owner_active: '{{ $.CpUser.smith.Active }}', " +
			"owner_label: '{{ $.CpUser.smith.Active }}', owner_ext: '{{ $.CpUser.smith.Ext }}', " +
			"owner_ext2: '{{ $.CpUser.smith.Ext2 }}', owner_ext3: '{{ $.CpUser.smith.Ext3 }}'}\n"
	}
	v1 := file("yes", "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11")
	v2 := file("false", "b1eebc99-9c0b-4ef8-bb6d-6bb9bd380a12")
	if got := l.seed(v1); got != "o|t|true|a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11|a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11|"+
		"a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11" {
		t.Fatalf("dbfixture stored %s", got)
	}
	l.check(v1)
	l.fidelity(v1, v2)

	// The uuid written in upper case is the same value to a uuid column,
	// but a string field prints it so and a uuid type in lower case: into
	// a text column only the model says which, so it is a finding.
	upper := file("true", "A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11")
	l.refused(upper, "owner_ext2 copies ext2 of a CpUser row, a uuid column",
		"owner_ext3 copies ext3 of a CpUser row, a uuid column")
	if got := l.findings(upper); strings.Contains(got, "owner_ext copies") || strings.Contains(got, "owner_active") ||
		strings.Contains(got, "owner_label") {
		t.Fatalf("a uuid and a bool column take these copies:\n%s", got)
	}
}
