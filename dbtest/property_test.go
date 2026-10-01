package dbtest_test

// A model-based test of the whole diff. Pairs of fixture states are drawn at
// random over a small schema that has a bit of everything the tool has to get
// right: a reference to another model, a tree whose parents can have higher
// ids than their children, a join table keyed by two references and no id,
// nulls, jsonb, a unique column whose values move from row to row, renames,
// models that leave the file, and values that read two ways. For each pair the
// old state is seeded with the real dbfixture, the change set the tool
// computes is applied with fixtureapply, and then:
//
//   - the database holds exactly what dbfixture seeds from the new file, and
//     exports as the new file;
//   - a second Apply finds every change made;
//   - Revert puts back exactly what dbfixture seeds from the old file.
//
// Half the pairs go the way generate goes, file against file through the
// generated Go source, with a database configured; the other half the way
// sync goes, the database against the file. The seed is fixed, so a failure
// is the same failure on every run, and the message says which iteration it
// was.
//
// Every pair gives each model a mode, sync, upsert or insert, and the nodes
// some insert_only columns, so what the database holds after the change set
// is the new file only where the files own it: a row upsert and insert keep,
// a value insert and insert_only leave, is the old one. A model a model that
// keeps its rows points at keeps its rows too, or a delete would fail on
// them, as it does in a deployment. The database then agrees with the new
// file as check reads it, and exports, by what the files own, as it.

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dbfixture"
)

type PtCur struct {
	bun.BaseModel `bun:"table:pt_curs"`
	ID            int64  `bun:"id,pk"`
	Code          string `bun:"code,notnull"`
}

type PtNode struct {
	bun.BaseModel `bun:"table:pt_nodes"`
	ID            int64          `bun:"id,pk"`
	Name          string         `bun:"name,notnull"`
	ParentID      *int64         `bun:"parent_id"`
	CurID         *int64         `bun:"cur_id"`
	Price         int64          `bun:"price,notnull"`
	Note          *string        `bun:"note"`
	Meta          map[string]any `bun:"meta,type:jsonb,notnull"`
	Slot          *int64         `bun:"slot"`
}

type PtLink struct {
	bun.BaseModel `bun:"table:pt_links"`
	NodeID        int64 `bun:"node_id,pk"`
	CurID         int64 `bun:"cur_id,pk"`
	Qty           int64 `bun:"qty,notnull"`
}

const ptConfig = `policy:
  renames: update
models:
  PtCur: {table: pt_curs, ref: code, key: [code]}
  PtNode:
    table: pt_nodes
    key: [name]
    references: {parent_id: PtNode, cur_id: PtCur}
  PtLink:
    table: pt_links
    key: [node_id, cur_id]
    references: {node_id: PtNode, cur_id: PtCur}
`

// ptCodes are the currencies a state draws from; a currency's id is its
// position, so it never changes. 0012 written plain is the integer 10 to YAML
// and the text 0012 to the text column, which is what the database holds and
// what every reference to that currency has to carry; 10 is there so the two
// readings meet.
var ptCodes = []string{"EUR", "USD", "0012", "10", "GBP"}

type ptNode struct {
	id       int
	name     string
	parent   int // an id, 0 for none
	cur      int // a currency id, 0 for none
	price    int
	note     string // "" for NULL
	meta     int
	slot     int  // a value of a unique column, 0 for NULL
	plainCur bool // write cur_id as a plain id rather than a template
}

type ptState struct {
	curs  []int     // currency ids, in file order
	nodes []*ptNode // in file order: every parent before its children
	links map[[2]int]int
	// omit leaves an empty model out of the file instead of writing it with
	// no rows.
	omit bool
}

// ptDraw draws the nodes' tree, currencies and links of a state over the
// given nodes, keeping their ids and names.
func ptDraw(r *rand.Rand, curs []int, nodes []*ptNode) *ptState {
	s := &ptState{curs: curs, links: map[[2]int]int{}, omit: r.Intn(2) == 0}
	r.Shuffle(len(nodes), func(i, j int) { nodes[i], nodes[j] = nodes[j], nodes[i] })
	for i, n := range nodes {
		n.parent, n.cur = 0, 0
		if i > 0 && r.Intn(3) > 0 {
			n.parent = nodes[r.Intn(i)].id
		}
		if len(curs) > 0 && r.Intn(3) > 0 {
			n.cur = curs[r.Intn(len(curs))]
		}
		n.price = r.Intn(4)
		n.note = []string{"", "x", "1.10"}[r.Intn(3)]
		n.meta = r.Intn(3)
		n.plainCur = r.Intn(2) == 0
	}
	s.nodes = nodes
	for _, n := range nodes {
		for _, c := range curs {
			if r.Intn(4) == 0 {
				s.links[[2]int{n.id, c}] = 1 + r.Intn(3)
			}
		}
	}
	return s
}

func ptCurSubset(r *rand.Rand) []int {
	var out []int
	for i := range ptCodes {
		if r.Intn(3) > 0 {
			out = append(out, i+1)
		}
	}
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// ptPair draws an old state and a new one made from it: rows deleted, rows
// added under ids and names nobody had, rows renamed to a name nobody had,
// and every other value drawn again.
func ptPair(r *rand.Rand, iteration int, renames bool) (*ptState, *ptState) {
	var oldNodes []*ptNode
	slots := r.Perm(10)
	for id := 1; id <= 8; id++ {
		if r.Intn(3) > 0 {
			n := &ptNode{id: id, name: string(rune('a' + id - 1))}
			if r.Intn(3) > 0 {
				n.slot = slots[id] + 1
			}
			oldNodes = append(oldNodes, n)
		}
	}
	old := ptDraw(r, ptCurSubset(r), oldNodes)

	// Renames come one at a time and only in some pairs: the run time cannot
	// run a set with one twice or revert it yet, and those checks should
	// still see most pairs.
	rename := -1
	if len(old.nodes) > 0 && r.Intn(4) == 0 && renames {
		rename = r.Intn(len(old.nodes))
	}
	var newNodes []*ptNode
	for i, n := range old.nodes {
		if r.Intn(6) == 0 {
			continue // deleted
		}
		c := *n
		if i == rename {
			c.name = fmt.Sprintf("r%d_%d", c.id, iteration)
		}
		newNodes = append(newNodes, &c)
	}
	for id := 9; id <= 11; id++ {
		if r.Intn(2) == 0 {
			newNodes = append(newNodes, &ptNode{id: id, name: fmt.Sprintf("n%d_%d", id, iteration)})
		}
	}
	// The unique slots move one row at a time, each to a value nobody holds
	// at that moment: one row takes what another gave up a step before, but
	// no two trade, which no order of statements could make. Nor does a row
	// take the slot of a deleted row, or a new row a slot anybody gave up: a
	// row that pointed at the deleted one, or that the row giving the slot
	// up comes to point at, would have to change its reference before and
	// its slot after that, and one UPDATE does both at once.
	held, gone := map[int]bool{}, map[int]bool{}
	for _, n := range old.nodes {
		gone[n.slot] = n.slot != 0
	}
	for _, n := range newNodes {
		held[n.slot] = n.slot != 0
	}
	for _, i := range r.Perm(len(newNodes)) {
		n := newNodes[i]
		if r.Intn(3) > 0 {
			continue
		}
		fresh := n.id > 8
		var free []int
		for v := 1; v <= 14; v++ {
			if !held[v] && !(gone[v] && fresh) && !(gone[v] && !wasHeldBy(old.nodes, newNodes, v)) {
				free = append(free, v)
			}
		}
		held[n.slot] = false
		n.slot = 0
		if r.Intn(4) > 0 && len(free) > 0 {
			n.slot = free[r.Intn(len(free))]
			held[n.slot] = true
		}
	}
	// Most currencies stay, a few come and go.
	have := map[int]bool{}
	var curs []int
	for _, id := range old.curs {
		if r.Intn(6) > 0 {
			have[id] = true
			curs = append(curs, id)
		}
	}
	for id := 1; id <= len(ptCodes); id++ {
		if !have[id] && r.Intn(4) == 0 {
			curs = append(curs, id)
		}
	}
	return old, ptDraw(r, curs, newNodes)
}

// ptModes is what a pair gives each model to own: its mode, and the nodes'
// insert_only columns.
type ptModes struct {
	cur, node, link fixturemigrate.Ownership
	insertOnly      []string
}

// ptDrawModes draws the modes of a pair. A model whose rows a mode keeps
// points only at models that keep theirs: a delete of a row a kept row
// points at fails, in a deployment as here.
func ptDrawModes(r *rand.Rand) ptModes {
	all := []fixturemigrate.Ownership{fixturemigrate.OwnSync, fixturemigrate.OwnUpsert, fixturemigrate.OwnInsert}
	m := ptModes{cur: all[r.Intn(3)], node: fixturemigrate.OwnSync, link: fixturemigrate.OwnSync}
	if m.cur != fixturemigrate.OwnSync {
		m.node = all[r.Intn(3)]
	}
	if m.node != fixturemigrate.OwnSync {
		m.link = all[r.Intn(3)]
	}
	for _, col := range []string{"price", "note"} {
		if r.Intn(4) == 0 {
			m.insertOnly = append(m.insertOnly, col)
		}
	}
	return m
}

func (m ptModes) apply(t *testing.T, cfg *fixturemigrate.Config) {
	t.Helper()
	cfg.Models["PtCur"].Mode, cfg.Models["PtNode"].Mode, cfg.Models["PtLink"].Mode = m.cur, m.node, m.link
	cfg.Models["PtNode"].InsertOnly = m.insertOnly
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
}

func (m ptModes) syncOnly() bool {
	return m.cur == fixturemigrate.OwnSync && m.node == fixturemigrate.OwnSync && m.link == fixturemigrate.OwnSync &&
		len(m.insertOnly) == 0
}

// ptOwned is the state a database seeded with old holds once a change set
// took it to next under the modes: next where the files own it, old where
// the database does.
func ptOwned(old, next *ptState, m ptModes) *ptState {
	out := &ptState{links: map[[2]int]int{}}
	out.curs = append(out.curs, next.curs...)
	if m.cur != fixturemigrate.OwnSync {
		have := map[int]bool{}
		for _, id := range next.curs {
			have[id] = true
		}
		for _, id := range old.curs {
			if !have[id] {
				out.curs = append(out.curs, id)
			}
		}
	}
	oldNodes, newNodes := map[int]*ptNode{}, map[int]bool{}
	for _, n := range old.nodes {
		oldNodes[n.id] = n
	}
	var nodes []*ptNode
	for _, n := range next.nodes {
		newNodes[n.id] = true
		c := *n
		if o, ok := oldNodes[n.id]; ok {
			if m.node == fixturemigrate.OwnInsert {
				c = *o
			}
			for _, col := range m.insertOnly {
				switch col {
				case "price":
					c.price = o.price
				case "note":
					c.note = o.note
				}
			}
		}
		nodes = append(nodes, &c)
	}
	if m.node != fixturemigrate.OwnSync {
		for _, n := range old.nodes {
			if !newNodes[n.id] {
				c := *n
				nodes = append(nodes, &c)
			}
		}
	}
	// dbfixture resolves a parent above its children.
	placed := map[int]bool{0: true}
	for len(out.nodes) < len(nodes) {
		for _, n := range nodes {
			if !placed[n.id] && placed[n.parent] {
				placed[n.id] = true
				out.nodes = append(out.nodes, n)
			}
		}
	}
	for k, q := range next.links {
		out.links[k] = q
		if o, ok := old.links[k]; ok && m.link == fixturemigrate.OwnInsert {
			out.links[k] = o
		}
	}
	if m.link != fixturemigrate.OwnSync {
		for k, q := range old.links {
			if _, ok := next.links[k]; !ok {
				out.links[k] = q
			}
		}
	}
	return out
}

// wasHeldBy reports whether the old row that held a slot is still there, so
// what it gives up is given up by an update rather than a delete.
func wasHeldBy(old, next []*ptNode, slot int) bool {
	for _, o := range old {
		if o.slot != slot {
			continue
		}
		for _, n := range next {
			if n.id == o.id {
				return true
			}
		}
	}
	return false
}

// yaml writes a state as dbfixture reads it. plain leaves the values that read
// two ways unquoted, which only the database's column types settle.
func (s *ptState) yaml(plain bool) string {
	text := func(v string) string {
		if plain {
			return v
		}
		return fmt.Sprintf("%q", v)
	}
	var b strings.Builder
	block := func(model string, n int) bool {
		if n == 0 && s.omit {
			return false
		}
		fmt.Fprintf(&b, "- model: %s\n  rows:", model)
		if n == 0 {
			b.WriteString(" []")
		}
		b.WriteString("\n")
		return true
	}
	if block("PtCur", len(s.curs)) {
		for _, id := range s.curs {
			fmt.Fprintf(&b, "    - {_id: c%d, id: %d, code: %s}\n", id, id, text(ptCodes[id-1]))
		}
	}
	if block("PtNode", len(s.nodes)) {
		for _, n := range s.nodes {
			fmt.Fprintf(&b, "    - {_id: x%d, id: %d, name: %q, price: %d, meta: {k: %d, tags: [t%d]}", n.id, n.id, n.name,
				n.price, n.meta, n.meta)
			switch {
			case n.parent == 0:
				b.WriteString(", parent_id: ~")
			default:
				fmt.Fprintf(&b, ", parent_id: '{{ $.PtNode.x%d.ID }}'", n.parent)
			}
			switch {
			case n.cur == 0:
				b.WriteString(", cur_id: ~")
			case n.plainCur:
				fmt.Fprintf(&b, ", cur_id: %d", n.cur)
			default:
				fmt.Fprintf(&b, ", cur_id: '{{ $.PtCur.c%d.ID }}'", n.cur)
			}
			if n.note == "" {
				b.WriteString(", note: ~")
			} else {
				fmt.Fprintf(&b, ", note: %s", text(n.note))
			}
			if n.slot == 0 {
				b.WriteString(", slot: ~")
			} else {
				fmt.Fprintf(&b, ", slot: %d", n.slot)
			}
			b.WriteString("}\n")
		}
	}
	keys := make([][2]int, 0, len(s.links))
	for k := range s.links {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	if block("PtLink", len(keys)) {
		for _, k := range keys {
			fmt.Fprintf(&b, "    - {node_id: '{{ $.PtNode.x%d.ID }}', cur_id: '{{ $.PtCur.c%d.ID }}', qty: %d}\n",
				k[0], k[1], s.links[k])
		}
	}
	if b.Len() == 0 {
		return "[]\n"
	}
	return b.String()
}

func TestPropertyAChangeSetTakesTheDatabaseFromOneFileToTheNext(t *testing.T) {
	db := connect(t)
	db.RegisterModel((*PtCur)(nil), (*PtNode)(nil), (*PtLink)(nil))
	ctx := context.Background()
	run(t, db, "DROP TABLE IF EXISTS pt_links, pt_nodes, pt_curs",
		"CREATE TABLE pt_curs (id bigint PRIMARY KEY, code text UNIQUE NOT NULL)",
		"CREATE TABLE pt_nodes (id bigint PRIMARY KEY, name text UNIQUE NOT NULL, "+
			"parent_id bigint REFERENCES pt_nodes, cur_id bigint REFERENCES pt_curs, price bigint NOT NULL, "+
			"note text, meta jsonb NOT NULL, slot bigint UNIQUE)",
		"CREATE TABLE pt_links (node_id bigint REFERENCES pt_nodes, cur_id bigint REFERENCES pt_curs, "+
			"qty bigint NOT NULL, PRIMARY KEY (node_id, cur_id))")
	path := filepath.Join(t.TempDir(), "c.yml")
	if err := os.WriteFile(path, []byte(ptConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := fixturemigrate.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	seed := func(text string) string {
		t.Helper()
		run(t, db, "TRUNCATE pt_links, pt_nodes, pt_curs")
		if err := dbfixture.New(db).Load(ctx, fstest.MapFS{"f.yml": {Data: []byte(text)}}, "f.yml"); err != nil {
			t.Fatalf("dbfixture cannot load a state this test drew, which is a bug in the test: %v\n%s", err, text)
		}
		return ptDump(t, db)
	}
	// The schema does not change, so it is read once; every read of the rows
	// is a read-only transaction with the session fixed, as the commands read.
	tables := schemaOf(t, db)
	read := func(fn func(tx bun.Tx)) {
		t.Helper()
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := fixturemigrate.PrepareSession(ctx, tx); err != nil {
			t.Fatal(err)
		}
		fn(tx)
	}

	iterations := 200
	if testing.Short() {
		iterations = 30
	}
	// BFM_PROPERTY_ITERATIONS runs more, or fewer.
	if n, err := strconv.Atoi(os.Getenv("BFM_PROPERTY_ITERATIONS")); err == nil && n > 0 {
		iterations = n
	}
	r := rand.New(rand.NewSource(20261001))
	var renamed, viaDB, noRerun, owned, noRevert int
	for i := 0; i < iterations; i++ {
		modes := ptDrawModes(r)
		modes.apply(t, cfg)
		if !modes.syncOnly() {
			owned++
		}
		// A rename is an update of the key, which mode insert never writes.
		old, next := ptPair(r, i, modes.node != fixturemigrate.OwnInsert)
		// Against the database as between two files, a model the file does
		// not mention has no rows, as in a fresh seed of the file.
		viaFile := i%2 == 0
		// Without the database, a value that reads two ways is refused, so
		// only the database's side gets those.
		oldText, newText := old.yaml(!viaFile), next.yaml(!viaFile)
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("iteration %d (%+v): %s\n--- old\n%s--- new\n%s", i, modes, fmt.Sprintf(format, args...),
				oldText, newText)
		}
		wantNew := seed(ptOwned(old, next, modes).yaml(!viaFile))
		wantOld := seed(oldText)

		var set fixturechange.Set
		if viaFile {
			// The way generate goes: file against file, through the Go
			// source a reviewer reads. Without a database first, which
			// has to accept every pair this test draws for it. Two notes
			// swapped between rows are a warning there, since nothing
			// says the column is not unique; the database says it, and
			// nothing it holds unique trades values.
			offline, err := fixturemigrate.Compute(cfg, fixtureSnapshot(t, cfg, oldText, "old"),
				fixtureSnapshot(t, cfg, newText, "new"))
			var warnings []fixturemigrate.Refusal
			if offline != nil {
				for _, w := range offline.Warnings {
					if !strings.Contains(w.Reason, "trade their values of") || !strings.Contains(w.Reason, "in a circle") {
						warnings = append(warnings, w)
					}
				}
			}
			if err != nil || len(offline.Refusals) != 0 || len(warnings) != 0 {
				fail("Compute without the database: %v %+v %+v", err, offline.Refusals, warnings)
			}
			// Then with one, which knows which columns are unique.
			before, head := fixtureSnapshot(t, cfg, oldText, "old"), fixtureSnapshot(t, cfg, newText, "new")
			read(func(tx bun.Tx) {
				for _, s := range []*fixturemigrate.Snapshot{before, head} {
					if err := fixturemigrate.Canonicalize(ctx, tx, cfg, s, tables); err != nil {
						fail("Canonicalize: %v", err)
					}
				}
			})
			res, err := fixturemigrate.Compute(cfg, before, head)
			if err != nil {
				fail("Compute: %v", err)
			}
			if len(res.Refusals) != 0 || len(res.Warnings) != 0 {
				fail("refused: %+v %+v", res.Refusals, res.Warnings)
			}
			if len(res.Changes) == 0 {
				set = fixturechange.Set{Name: "pt", Tables: fixturechange.Tables{}}
			} else {
				src, err := fixturemigrate.Render(cfg, "pt", "20261001000000", res)
				if err != nil {
					fail("Render: %v", err)
				}
				var ok bool
				if set, ok, err = fixturemigrate.ReadChangeSet(src); err != nil || !ok {
					fail("ReadChangeSet: %v\n%s", err, src)
				}
			}
		} else {
			// The way sync goes: the database against the file.
			viaDB++
			head := fixtureSnapshot(t, cfg, newText, "new")
			read(func(tx bun.Tx) {
				if err := fixturemigrate.Canonicalize(ctx, tx, cfg, head, tables); err != nil {
					fail("Canonicalize: %v", err)
				}
				if len(head.Findings) != 0 {
					fail("findings: %+v", head.Findings)
				}
				database, err := fixturemigrate.DatabaseSnapshot(ctx, tx, cfg, tables,
					fixturemigrate.SnapshotOptions{Columns: head.Columns, Order: head.Order})
				if err != nil {
					fail("DatabaseSnapshot: %v", err)
				}
				res, err := fixturemigrate.Compute(cfg, database, head)
				if err != nil {
					fail("Compute: %v", err)
				}
				if len(res.Refusals) != 0 || len(res.Warnings) != 0 {
					fail("refused: %+v %+v", res.Refusals, res.Warnings)
				}
				set = fixturechange.Set{Name: "pt", Tables: res.Tables, Changes: res.Changes}
			})
		}
		hasRename := false
		for _, c := range set.Changes {
			hasRename = hasRename || c.ID != ""
		}
		if hasRename {
			renamed++
		}
		// A change whose key or guard names a row the same set deletes cannot
		// run a second time yet: the run time looks the row up first, and
		// fails where it should find the change already made.
		deleted := map[fixturechange.Ref]bool{}
		for _, c := range set.Changes {
			if c.Kind == fixturechange.Delete {
				ref := cfg.Models[c.Model].Ref
				if v, ok := c.Old[ref]; ok && v.Ref == nil && !v.IsNull {
					deleted[fixturechange.Ref{Model: c.Model, Key: v.Lit}] = true
				}
			}
		}
		namesDeleted := false
		for _, c := range set.Changes {
			for _, values := range []fixturechange.Values{c.Key, c.Old} {
				for _, v := range values {
					namesDeleted = namesDeleted || (v.Ref != nil && deleted[*v.Ref])
				}
			}
		}
		if namesDeleted {
			noRerun++
		}
		describe := func() string {
			var lines []string
			for _, c := range set.Changes {
				lines = append(lines, fmt.Sprintf("  %s %s key=%v old=%v new=%v", c.Kind, c.Model, c.Key, c.Old, c.New))
			}
			return strings.Join(lines, "\n")
		}

		apply := func(step string, f func(context.Context, bun.IDB, fixturechange.Set, ...fixtureapply.Option) error) []fixtureapply.Outcome {
			t.Helper()
			var out []fixtureapply.Outcome
			if err := f(ctx, db, set, quiet(), fixtureapply.WithReport(func(o fixtureapply.Outcome) {
				out = append(out, o)
			})); err != nil {
				fail("%s: %v\n%s", step, err, describe())
			}
			return out
		}
		apply("apply", fixtureapply.Apply)
		if got := ptDump(t, db); got != wantNew {
			fail("the change set does not reproduce the new file\n got %s\nwant %s\n%s", got, wantNew, describe())
		}

		// The database agrees with the new file, as check compares them:
		// what the files do not own is no difference.
		read(func(tx bun.Tx) {
			head := fixtureSnapshot(t, cfg, newText, "new")
			if err := fixturemigrate.Canonicalize(ctx, tx, cfg, head, tables); err != nil {
				fail("Canonicalize: %v", err)
			}
			database, err := fixturemigrate.DatabaseSnapshot(ctx, tx, cfg, tables,
				fixturemigrate.SnapshotOptions{Columns: head.Columns, Order: head.Order})
			if err != nil {
				fail("DatabaseSnapshot: %v", err)
			}
			res, err := fixturemigrate.Check(cfg, database, head)
			if err != nil || len(res.Changes) != 0 || len(res.Refusals) != 0 {
				fail("check after the change set: %v %+v %+v\n%s", err, res.Changes, res.Refusals, describe())
			}
		})

		// The database exports as the new file, as far as the files own it.
		read(func(tx bun.Tx) {
			all, err := fixturemigrate.DatabaseSnapshot(ctx, tx, cfg, tables, fixturemigrate.SnapshotOptions{})
			if err != nil {
				fail("DatabaseSnapshot: %v", err)
			}
			// A model the files hold no block of is exported whole, which is
			// how one is first taken into them; under upsert or insert the
			// database keeps rows of one the new file left out, so the
			// export is held to the file with an empty block of it.
			held := *next
			held.omit = false
			if err := fixturemigrate.KeepOwned(ctx, tx, cfg, tables, fixtureSnapshot(t, cfg, held.yaml(!viaFile), "new"),
				all); err != nil {
				fail("KeepOwned: %v", err)
			}
			exported, err := fixturemigrate.Export(cfg, all, tables, nil)
			if err != nil {
				fail("Export: %v", err)
			}
			head, back := fixtureSnapshot(t, cfg, newText, "new"), fixtureSnapshot(t, cfg, string(exported), "export")
			for _, s := range []*fixturemigrate.Snapshot{head, back} {
				if err := fixturemigrate.Canonicalize(ctx, tx, cfg, s, tables); err != nil {
					fail("Canonicalize: %v", err)
				}
			}
			res, err := fixturemigrate.Compute(cfg, head, back)
			if err != nil {
				fail("Compute against the export: %v\n%s", err, exported)
			}
			if len(res.Changes) != 0 || len(res.Refusals) != 0 {
				fail("the export is not the new file: %+v %+v\n%s", res.Changes, res.Refusals, exported)
			}
		})

		// The run time does not run a rename twice, nor revert one, yet;
		// that is being fixed apart from this test.
		if hasRename {
			continue
		}
		if !namesDeleted {
			for _, o := range apply("second apply", fixtureapply.Apply) {
				if o.Index >= 0 && o.Status != fixtureapply.StatusUnchanged {
					fail("a second run changed something: %+v\n%s", o, describe())
				}
			}
		}

		// A revert puts a deleted row back from its delete's guard, which an
		// insert_only column is not in: the row comes back without it.
		if len(modes.insertOnly) > 0 {
			deletes := false
			for _, c := range set.Changes {
				deletes = deletes || (c.Model == "PtNode" && c.Kind == fixturechange.Delete)
			}
			if deletes {
				noRevert++
				continue
			}
		}
		apply("revert", fixtureapply.Revert)
		if got := ptDump(t, db); got != wantOld {
			fail("revert does not restore the old file\n got %s\nwant %s\n%s", got, wantOld, describe())
		}
	}
	t.Logf("%d pairs, %d through the database, %d with a rename (not run twice, not reverted), "+
		"%d naming a row they delete (not run twice), %d with a model the files do not own whole, "+
		"%d deleting a row with insert_only columns (not reverted)", iterations, viaDB, renamed, noRerun, owned, noRevert)
}

// ptDump is a database state as text: every row of every table, references
// by id, ordered.
func ptDump(t *testing.T, db *bun.DB) string {
	t.Helper()
	var parts []string
	for _, table := range []string{"pt_curs", "pt_nodes", "pt_links"} {
		parts = append(parts, table+":"+scan[string](t, db,
			"SELECT coalesce(string_agg(to_jsonb(t)::text, ';' ORDER BY to_jsonb(t)::text), '') FROM "+table+" t"))
	}
	return strings.Join(parts, "\n")
}
