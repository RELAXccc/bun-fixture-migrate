package fixtureapply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
	"github.com/RELAXccc/bun-fixture-migrate/internal/pgerr"

	"github.com/uptrace/bun"
)

// The audit table, which a change set with an AuditTable writes one row into
// at the end of every Apply and Revert that succeeds, in the transaction that
// made its changes and under the advisory lock every change set takes. A run
// that fails rolls back, and its row with it, so the table holds only what
// was done.
//
// It answers two questions nothing else in the database does. Which changes
// did a migration make here, and which did it find made already or skip:
// bun's migrations table only says it ran. And so, what does undoing it mean
// here: Revert inverts only the changes the last Apply made, not those it
// found made through another path, whose old value this database never held,
// nor those it skipped as somebody's edit, which inverting would overwrite.
//
// The layout:
//
//	id          bigint, an identity: the order the runs committed in, since
//	            the advisory lock lets one change set write at a time
//	set_name    the change set's Name, the migration's file name
//	direction   'up' for Apply, 'down' for Revert
//	set_sha256  the change set's SetSHA256 when it ran, which status compares
//	            with the file to say it was edited after it ran here
//	applied_at  when the run wrote its row, just before it committed
//	applied_by  the role it ran as, current_user
//	outcomes    a jsonb array, one AuditOutcome per Outcome of the run
const auditTableSQL = `CREATE TABLE IF NOT EXISTS %s (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    set_name   text NOT NULL,
    direction  text NOT NULL CHECK (direction IN ('up', 'down')),
    set_sha256 text NOT NULL CHECK (set_sha256 ~ '^[0-9a-f]{64}$'),
    applied_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    applied_by text NOT NULL DEFAULT current_user,
    outcomes   jsonb NOT NULL CHECK (jsonb_typeof(outcomes) = 'array')
)`

// auditComments are what a DBA reads in \d+ of the table, and why it is there.
var auditComments = []struct{ target, text string }{
	{"TABLE %s", "Written by the fixtureapply package of bun-fixture-migrate: one row per successful run of a " +
		"fixture change set (a generated data migration) in this database, written in the transaction that made " +
		"its changes. direction is up for Apply and down for Revert. outcomes says what became of each change: " +
		"applied, unchanged (the database held it already) or skipped (with the problem, such as a row somebody " +
		"edited), so a later Revert undoes only what an Apply here applied. Rows are appended, never updated: do " +
		"not edit or delete them while the migrations they name may still be rolled back."},
	{"COLUMN %s.set_sha256", "SHA-256 of the change set as the migration file held it when it ran; " +
		"bun-fixture-migrate status compares it with the file and reports a file edited after it ran here."},
	{"COLUMN %s.outcomes", "One object per outcome of the run, in the order the changes ran: index (the " +
		"change's position in the set, -1 for one about the whole set), model, key, kind, status and problem."},
}

// Direction is which way a change set ran: Apply is up, Revert down.
type Direction string

const (
	DirectionUp   Direction = "up"
	DirectionDown Direction = "down"
)

// AuditOutcome is what the audit table keeps of an Outcome: which change, and
// what became of it. The message is left out; the log has it.
type AuditOutcome struct {
	// Index is the change's position in the set as it ran, -1 for an
	// outcome about the whole set, such as StatusUnseeded.
	Index int    `json:"index"`
	Model string `json:"model,omitempty"`
	// Key and Kind are the Outcome's: for Revert, the inverted change's.
	Key     string             `json:"key,omitempty"`
	Kind    fixturechange.Kind `json:"kind,omitempty"`
	Status  Status             `json:"status"`
	Problem Problem            `json:"problem,omitempty"`
}

// AuditRecord is one row of the audit table.
type AuditRecord struct {
	ID        int64
	Set       string
	Direction Direction
	// SetSHA256 is the SetSHA256 of the set as it ran.
	SetSHA256 string
	AppliedAt time.Time
	AppliedBy string
	Outcomes  []AuditOutcome
}

// Count is how many of the run's changes ended with status.
func (r AuditRecord) Count(status Status) int {
	n := 0
	for _, o := range r.Outcomes {
		if o.Index >= 0 && o.Status == status {
			n++
		}
	}
	return n
}

// Unseeded reports whether the run found the seed guard table empty, and so
// did nothing: the database was not seeded yet.
func (r AuditRecord) Unseeded() bool {
	for _, o := range r.Outcomes {
		if o.Index < 0 && o.Status == StatusUnseeded {
			return true
		}
	}
	return false
}

// outcomeOf is what the record says its run did with change i of the set, c as
// the set holds it now, and false when the run did not have the change. The
// record keeps the position each change had when it ran; a set edited since
// then may have moved it, so a change found at its position under another
// model, key or kind is looked for by those, and is taken only when exactly
// one outcome has them.
func (r AuditRecord) outcomeOf(i int, c fixturechange.Change) (AuditOutcome, bool) {
	key := keyLabel(c.Key)
	same := func(o AuditOutcome) bool { return o.Model == c.Model && o.Key == key && o.Kind == c.Kind }
	var found []AuditOutcome
	for _, o := range r.Outcomes {
		if o.Index == i && same(o) {
			return o, true
		}
		if o.Index >= 0 && same(o) {
			found = append(found, o)
		}
	}
	if len(found) == 1 {
		return found[0], true
	}
	return AuditOutcome{}, false
}

// Applies are the audit rows of the runs of Apply a Revert undoes, newest
// first: every 'up' row of the set after its last 'down' row.
type Applies []AuditRecord

// ran is the newest of the runs that found the database seeded, nil when
// every one of them found it empty and did nothing.
func (a Applies) ran() *AuditRecord {
	for k := range a {
		if !a[k].Unseeded() {
			return &a[k]
		}
	}
	return nil
}

// Made reports whether one of the runs made change i of the set, c as the set
// holds it now, with the outcome that says so and the run's row, or else the
// newest outcome of the change and its row. The row is nil when no run had the
// change, which a set edited after it ran here can hold.
//
// Any of them, and not only the newest: a Revert that failed under bun's
// default migrator leaves the migration pending with its changes made, and the
// Apply of the next migrate finds every one of them made already. The run
// before it is the one that made them.
func (a Applies) Made(i int, c fixturechange.Change) (made bool, outcome AuditOutcome, row *AuditRecord) {
	for k := range a {
		o, ok := a[k].outcomeOf(i, c)
		if !ok {
			continue
		}
		if o.Status == StatusApplied {
			return true, o, &a[k]
		}
		if row == nil {
			outcome, row = o, &a[k]
		}
	}
	return false, outcome, row
}

// SetSHA256 is the SHA-256, in hex, of a canonical encoding of the set: every
// field the set and its tables, policies, changes and values hold, with the
// keys of every map in order, so the same set gives the same hash in whatever
// order its maps were filled. A field at its zero value is left out, so a
// field a later version adds does not change the hash of a set that does not
// use it, and Format 0 and 1, which mean the same, hash the same. Strings are
// written as Go quotes them, so every byte counts.
//
// The audit table records it with every run, and status compares it with the
// set the migration file holds now.
func SetSHA256(set fixturechange.Set) string {
	var b strings.Builder
	canonical(&b, setFields(set))
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// canonicalMap is a map written with its keys in order.
type canonicalMap map[string]any

func setFields(set fixturechange.Set) canonicalMap {
	m := canonicalMap{}
	put := func(k string, v string) {
		if v != "" {
			m[k] = v
		}
	}
	if set.Format > 1 {
		m["format"] = set.Format
	}
	put("name", set.Name)
	put("seed_guard_table", set.SeedGuardTable)
	put("migrations_table", set.MigrationsTable)
	put("lock_timeout", set.LockTimeout)
	put("audit_table", set.AuditTable)
	if p := policyFields(set.Policy); len(p) > 0 {
		m["policy"] = p
	}
	if len(set.Tables) > 0 {
		tables := canonicalMap{}
		for model, t := range set.Tables {
			tm := canonicalMap{}
			for k, v := range map[string]string{"name": t.Name, "id": t.ID, "key": t.Key, "where": t.Where,
				"soft_delete": t.SoftDelete} {
				if v != "" {
					tm[k] = v
				}
			}
			if t.Serial {
				tm["serial"] = true
			}
			if t.Cascade {
				tm["cascade"] = true
			}
			if t.Policy != nil {
				if p := policyFields(*t.Policy); len(p) > 0 {
					tm["policy"] = p
				}
			}
			tables[model] = tm
		}
		m["tables"] = tables
	}
	if len(set.Changes) > 0 {
		changes := make([]any, 0, len(set.Changes))
		for _, c := range set.Changes {
			cm := canonicalMap{"model": c.Model, "kind": string(c.Kind)}
			if c.ID != "" {
				cm["id"] = c.ID
			}
			for k, v := range map[string]fixturechange.Values{"key": c.Key, "old": c.Old, "new": c.New} {
				if len(v) == 0 {
					continue
				}
				vm := canonicalMap{}
				for col, value := range v {
					switch {
					case value.IsNull:
						vm[col] = nil
					case value.Ref != nil:
						vm[col] = canonicalMap{"ref": []any{value.Ref.Model, value.Ref.Key}}
					default:
						vm[col] = value.Lit
					}
				}
				cm[k] = vm
			}
			changes = append(changes, cm)
		}
		m["changes"] = changes
	}
	return m
}

func policyFields(p fixturechange.Policy) canonicalMap {
	m := canonicalMap{}
	for k, v := range map[string]fixturechange.Mode{"missing_row": p.MissingRow, "changed_row": p.ChangedRow,
		"id_drift": p.IDDrift, "duplicate_key": p.DuplicateKey} {
		if v != "" {
			m[k] = string(v)
		}
	}
	return m
}

func canonical(b *strings.Builder, v any) {
	switch v := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case int:
		b.WriteString(strconv.Itoa(v))
	case string:
		b.WriteString(strconv.Quote(v))
	case []any:
		b.WriteByte('[')
		for i, e := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			canonical(b, e)
		}
		b.WriteByte(']')
	case canonicalMap:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Quote(k))
			b.WriteByte(':')
			canonical(b, v[k])
		}
		b.WriteByte('}')
	default:
		panic(fmt.Sprintf("canonical: %T", v))
	}
}

// writeAudit writes the row of a run that succeeded into the set's audit table,
// creating the table first when it is not there. It runs last in the run's
// transaction, so a run that fails writes nothing, and under the advisory
// lock, so no other change set creates the table at the same moment.
func writeAudit(ctx context.Context, tx bun.IDB, set fixturechange.Set, revert bool, outcomes []Outcome) error {
	table, err := quoteIdent(set.AuditTable)
	if err != nil {
		return err
	}
	if err := ensureAuditTable(ctx, tx, set.AuditTable, table); err != nil {
		return err
	}
	kept := make([]AuditOutcome, 0, len(outcomes))
	for _, o := range outcomes {
		kept = append(kept, AuditOutcome{Index: o.Index, Model: o.Model, Key: o.Key, Kind: o.Kind,
			Status: o.Status, Problem: o.Problem})
	}
	data, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	direction := DirectionUp
	if revert {
		direction = DirectionDown
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+table+" (set_name, direction, set_sha256, applied_at, applied_by, "+
		"outcomes) VALUES (?, ?, ?, clock_timestamp(), current_user, ?::jsonb)",
		set.Name, string(direction), SetSHA256(set), string(data)); err != nil {
		if pgerr.State(err) == pgerr.LockNotAvailable {
			return auditLocked(set, "once every change was made, recording the run in", err)
		}
		if pgerr.State(err) == pgerr.InsufficientPrivilege {
			return fmt.Errorf("%s: the role running the migration may not write into the audit table %s, so nothing "+
				"was changed: grant it SELECT and INSERT on the table, or take AuditTable out of the migration to "+
				"run it without a record: %w", set.Name, set.AuditTable, err)
		}
		return fmt.Errorf("%s: record the run in the audit table %s: %w", set.Name, set.AuditTable, err)
	}
	return nil
}

// auditLocked is the error of a run that waited for a lock another session
// held on the audit table for longer than the lock timeout.
func auditLocked(set fixturechange.Set, what string, err error) error {
	limit := "the session's lock_timeout"
	if set.LockTimeout != "" {
		limit = "the lock timeout of " + set.LockTimeout
	}
	out := Outcome{Set: set.Name, Index: -1, Status: StatusFailed, Problem: ProblemLockTimeout,
		Message: fmt.Sprintf("%s the audit table %s waited for a lock another session held on it, such as an "+
			"ALTER TABLE or a VACUUM FULL, for longer than %s, so nothing was changed; the change set runs again "+
			"on the next deploy", what, set.AuditTable, limit)}
	return &ChangeError{Outcome: out, err: fmt.Errorf("%s: %w", out.Message, err)}
}

// ensureAuditTable creates the audit table, with its comments, when it is not
// there. Creating it takes CREATE on its schema, and the comments that the
// role owns what it created, which it does.
func ensureAuditTable(ctx context.Context, tx bun.IDB, name, quoted string) error {
	exists, err := auditTableExists(ctx, tx, quoted)
	if pgerr.State(err) == pgerr.InsufficientPrivilege {
		return fmt.Errorf("the role running the migration may not use the schema of the audit table %s, so nothing "+
			"was changed: grant it USAGE on the schema, and SELECT and INSERT on the table: %w", name, err)
	}
	if err != nil {
		return fmt.Errorf("look for the audit table %s: %w", name, err)
	}
	if exists {
		return nil
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(auditTableSQL, quoted)); err != nil {
		switch pgerr.State(err) {
		case pgerr.InsufficientPrivilege:
			return fmt.Errorf("the audit table %s does not exist, and the role running the migration may not create "+
				"it, which takes CREATE on its schema, so nothing was changed. Grant the role CREATE on the schema, or "+
				"have a role that may create it run the migration once and grant this one SELECT and INSERT on the "+
				"table: %w", name, err)
		case pgerr.InvalidSchemaName:
			return fmt.Errorf("the audit table %s cannot be created, because its schema does not exist, so nothing "+
				"was changed; create the schema, or name the audit table in one that exists: %w", name, err)
		}
		return fmt.Errorf("create the audit table %s: %w", name, err)
	}
	for _, c := range auditComments {
		if _, err := tx.ExecContext(ctx, "COMMENT ON "+fmt.Sprintf(c.target, quoted)+" IS ?", c.text); err != nil {
			return fmt.Errorf("comment on the audit table %s: %w", name, err)
		}
	}
	return nil
}

// auditTableExists reports whether the audit table is there. In a schema the
// role may not use, PostgreSQL's to_regclass fails with a privilege error.
func auditTableExists(ctx context.Context, db bun.IDB, quoted string) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, "SELECT to_regclass(?) IS NOT NULL", quoted).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

const auditColumns = "id, set_name, direction, set_sha256, applied_at, applied_by, outcomes::text"

func scanAudit(rows interface{ Scan(...any) error }) (AuditRecord, error) {
	var r AuditRecord
	var direction, outcomes string
	if err := rows.Scan(&r.ID, &r.Set, &direction, &r.SetSHA256, &r.AppliedAt, &r.AppliedBy, &outcomes); err != nil {
		return r, err
	}
	r.Direction = Direction(direction)
	if err := json.Unmarshal([]byte(outcomes), &r.Outcomes); err != nil {
		return r, fmt.Errorf("the outcomes of row %d do not read: %w", r.ID, err)
	}
	return r, nil
}

// ReadAudit reads the newest row of the audit table for every change set it
// holds a row of, by the set's name: what the last run of each did in this
// database. The second result is false when the table does not exist, which is
// what a database looks like before the first change set with an audit table
// ran there, and not an error.
func ReadAudit(ctx context.Context, db bun.IDB, table string) (map[string]AuditRecord, bool, error) {
	quoted, err := quoteIdent(table)
	if err != nil {
		return nil, false, fmt.Errorf("audit table %w", err)
	}
	exists, err := auditTableExists(ctx, db, quoted)
	if err != nil || !exists {
		return nil, false, err
	}
	rows, err := db.QueryContext(ctx, "SELECT DISTINCT ON (set_name) "+auditColumns+" FROM "+quoted+
		" ORDER BY set_name, id DESC")
	if err != nil {
		return nil, true, fmt.Errorf("read the audit table %s: %w", table, err)
	}
	defer rows.Close()
	out := map[string]AuditRecord{}
	for rows.Next() {
		r, err := scanAudit(rows)
		if err != nil {
			return nil, true, fmt.Errorf("read the audit table %s: %w", table, err)
		}
		out[r.Set] = r
	}
	if err := rows.Err(); err != nil {
		return nil, true, fmt.Errorf("read the audit table %s: %w", table, err)
	}
	return out, true, rows.Close()
}

// ApplyRecords are the rows Revert follows: every 'up' row of the set in its
// audit table after the set's last 'down' row, newest first, the runs of Apply
// that made the changes now in this database.
//
// The second result is the set's newest row when that is a 'down' row: the set
// was reverted in this database and no Apply ran since, so a Revert has
// nothing to undo. It is nil, and the rows are empty, when the set has no
// AuditTable, the table does not exist, or it holds no row of the set: the set
// never ran here with one, and nothing says what Apply did.
func ApplyRecords(ctx context.Context, db bun.IDB, set fixturechange.Set) (Applies, *AuditRecord, error) {
	if set.AuditTable == "" {
		return nil, nil, nil
	}
	quoted, err := quoteIdent(set.AuditTable)
	if err != nil {
		return nil, nil, fmt.Errorf("audit table %w", err)
	}
	exists, err := auditTableExists(ctx, db, quoted)
	if err != nil || !exists {
		return nil, nil, err
	}
	// The 'up' rows after the last 'down' row, and that 'down' row when no
	// 'up' row follows it.
	rows, err := db.QueryContext(ctx, "WITH d AS (SELECT max(id) AS last_down FROM "+quoted+" WHERE set_name = ? "+
		"AND direction = 'down') SELECT "+auditColumns+" FROM "+quoted+", d WHERE set_name = ? AND "+
		"((direction = 'up' AND id > coalesce(last_down, 0)) OR (id = last_down AND NOT EXISTS (SELECT 1 FROM "+
		quoted+" u WHERE u.set_name = ? AND u.direction = 'up' AND u.id > last_down))) ORDER BY id DESC",
		set.Name, set.Name, set.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("read the audit table %s: %w", set.AuditTable, err)
	}
	defer rows.Close()
	var out Applies
	var reverted *AuditRecord
	for rows.Next() {
		r, err := scanAudit(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("read the audit table %s: %w", set.AuditTable, err)
		}
		if r.Direction == DirectionDown {
			reverted = &r
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the audit table %s: %w", set.AuditTable, err)
	}
	return out, reverted, rows.Close()
}
