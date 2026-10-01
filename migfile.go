package fixturemigrate

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

// migrationFile is bun's pattern for a migration file name (migrate/migrations.go,
// fnameRE). The digits are the name bun records in its migrations table; the
// rest, up to the first dot, is the comment it keeps beside it.
var migrationFile = regexp.MustCompile(`^(\d{1,14})_([0-9a-z_\-]+)\.`)

// MigrationFile is one migration of a bun migrations directory.
type MigrationFile struct {
	// Name is what bun records: the digits the file name starts with.
	Name string
	// Comment is the rest of the file name.
	Comment string
	// Files are the migration's files: one .go file, or an .up.sql and a
	// .down.sql.
	Files []string
	// Fixture is the change set of a migration this tool generated, nil for
	// any other migration.
	Fixture *fixturechange.Set
}

// ID is the migration as bun prints it, "20260921120000_fixture_plan_prices".
func (m MigrationFile) ID() string { return m.Name + "_" + m.Comment }

// Migrations is what ReadMigrations found in a directory.
type Migrations struct {
	// List is every migration, in the order bun runs them.
	List []MigrationFile
	// Problems are what makes the directory unsafe to run as it stands, one
	// sentence each: two migrations bun would record under one name, a
	// generated file that no longer reads as one.
	Problems []string
}

// Fixtures is the migrations this tool generated, in the order bun runs them.
func (ms *Migrations) Fixtures() []MigrationFile {
	var out []MigrationFile
	for _, m := range ms.List {
		if m.Fixture != nil {
			out = append(out, m)
		}
	}
	return out
}

// ReadMigrations lists the migrations of a bun migrations directory: its Go
// files and its SQL files whose names bun accepts, top level only. A Go file
// that holds a fixturechange.Set is read back into Fixture.
//
// Two migrations with the same name are a problem bun only partly catches:
// Discover refuses two SQL files, but a Go migration registered under a name
// another migration already has is recorded as one of the two, so whichever
// runs second is marked applied without ever running.
func ReadMigrations(dir string) (*Migrations, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := &Migrations{}
	byID := map[string]*MigrationFile{}
	var order []string
	for _, entry := range entries {
		file := entry.Name()
		if entry.IsDir() || strings.HasSuffix(file, "_test.go") {
			continue
		}
		isGo := strings.HasSuffix(file, ".go")
		isSQL := strings.HasSuffix(file, ".up.sql") || strings.HasSuffix(file, ".down.sql")
		if !isGo && !isSQL {
			continue
		}
		match := migrationFile.FindStringSubmatch(file)
		if match == nil {
			continue
		}
		path := filepath.Join(dir, file)
		m := MigrationFile{Name: match[1], Comment: match[2], Files: []string{path}}
		if isGo {
			src, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			set, isFixture, err := ReadChangeSet(src)
			if err != nil {
				out.Problems = append(out.Problems, fmt.Sprintf(
					"%s holds a change set this tool cannot read back (%v); status and plan leave it out", file, err))
			}
			if isFixture && err == nil {
				m.Fixture = &set
			}
			// A Go file is a migration of its own.
			id := m.ID() + "\x00" + file
			byID[id] = &m
			order = append(order, id)
			continue
		}
		// The .up.sql and .down.sql of one migration share its name and comment.
		id := m.ID()
		if have, ok := byID[id]; ok {
			have.Files = append(have.Files, path)
			continue
		}
		byID[id] = &m
		order = append(order, id)
	}
	for _, id := range order {
		out.List = append(out.List, *byID[id])
	}
	sort.SliceStable(out.List, func(i, j int) bool {
		if out.List[i].Name != out.List[j].Name {
			return out.List[i].Name < out.List[j].Name
		}
		return out.List[i].Comment < out.List[j].Comment
	})
	for i := 1; i < len(out.List); i++ {
		a, b := out.List[i-1], out.List[i]
		if a.Name == b.Name {
			out.Problems = append(out.Problems, fmt.Sprintf(
				"%s and %s share the name %s: bun records them as one migration, so one of them never runs. "+
					"Give one of them a new timestamp", filepath.Base(a.Files[0]), filepath.Base(b.Files[0]), a.Name))
		}
	}
	return out, nil
}

// ReadChangeSet reads the fixturechange.Set a generated migration file holds.
// The second result is false for a Go file that holds none, which is any
// migration this tool did not write.
//
// It reads the literal, it does not run it: the file is parsed and the one
// composite literal of type fixturechange.Set is decoded from the syntax tree.
// So it understands what Render writes and what a person editing that by hand
// is likely to write -- dropping a change, fixing a value -- and says so,
// naming the position, about anything else.
//
// It also reads how the file registers the set, in either shape a version of
// this tool has written: fixtureapply.Up(set) and fixtureapply.Down(set), or
// functions that call fixtureapply.Apply and fixtureapply.Revert with it. A
// file that hands them another variable than the set it declares -- a copy of
// another migration with only the set renamed -- runs that other set under
// this file's name, while status and plan would describe this one; that is
// an error. A registration in any other shape is left alone.
func ReadChangeSet(src []byte) (fixturechange.Set, bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return fixturechange.Set{}, false, err
	}
	pkg := ""
	for _, imp := range file.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if path != "github.com/RELAXccc/bun-fixture-migrate/fixturechange" {
			continue
		}
		pkg = "fixturechange"
		if imp.Name != nil {
			pkg = imp.Name.Name
		}
	}
	if pkg == "" {
		return fixturechange.Set{}, false, nil
	}
	r := &setReader{fset: fset, pkg: pkg, funcs: map[string]*ast.FuncDecl{}}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
			r.funcs[fn.Name.Name] = fn
		}
	}
	var found []*ast.CompositeLit
	ast.Inspect(file, func(n ast.Node) bool {
		if lit, ok := n.(*ast.CompositeLit); ok && r.isType(lit.Type, "Set") {
			found = append(found, lit)
			return false
		}
		return true
	})
	switch len(found) {
	case 0:
		return fixturechange.Set{}, false, nil
	case 1:
	default:
		return fixturechange.Set{}, true, fmt.Errorf("%d change sets in one file", len(found))
	}
	set, err := r.set(found[0])
	if err != nil {
		return set, true, err
	}
	return set, true, r.registers(file, found[0])
}

// registers checks that every fixtureapply.Up, Down, Apply and Revert in the
// file is handed the variable the change set is declared as.
func (r *setReader) registers(file *ast.File, lit *ast.CompositeLit) error {
	declared := ""
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, v := range vs.Values {
				if v == ast.Expr(lit) && i < len(vs.Names) {
					declared = vs.Names[i].Name
				}
			}
		}
	}
	apply := ""
	for _, imp := range file.Imports {
		if path, _ := strconv.Unquote(imp.Path.Value); path == "github.com/RELAXccc/bun-fixture-migrate/fixtureapply" {
			apply = "fixtureapply"
			if imp.Name != nil {
				apply = imp.Name.Name
			}
		}
	}
	if declared == "" || apply == "" {
		return nil
	}
	var err error
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || err != nil {
			return err == nil
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != apply {
			return true
		}
		arg := -1
		switch sel.Sel.Name {
		case "Up", "Down":
			arg = 0
		case "Apply", "Revert":
			arg = 2
		}
		if arg < 0 || arg >= len(call.Args) {
			return true
		}
		if id, ok := call.Args[arg].(*ast.Ident); ok && id.Name != declared {
			err = r.errorf(call, "fixtureapply.%s is handed %s, and the change set this file declares is %s: bun "+
				"would run %s under this file's name", sel.Sel.Name, id.Name, declared, id.Name)
		}
		return true
	})
	return err
}

type setReader struct {
	fset *token.FileSet
	pkg  string
	// funcs are the file's functions, by name, where a large set's parts are.
	funcs map[string]*ast.FuncDecl
}

func (r *setReader) errorf(n ast.Node, format string, args ...any) error {
	return fmt.Errorf("line %d: %s", r.fset.Position(n.Pos()).Line, fmt.Sprintf(format, args...))
}

// isType reports whether expr is fixturechange.<name>.
func (r *setReader) isType(expr ast.Expr, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == r.pkg
}

// fields walks the key-value pairs of a struct literal.
func (r *setReader) fields(lit *ast.CompositeLit, fn func(key string, value ast.Expr) error) error {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return r.errorf(elt, "a field without its name")
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			return r.errorf(kv, "a field name that is not a name")
		}
		if err := fn(key.Name, kv.Value); err != nil {
			return err
		}
	}
	return nil
}

func (r *setReader) set(lit *ast.CompositeLit) (fixturechange.Set, error) {
	var set fixturechange.Set
	err := r.fields(lit, func(key string, value ast.Expr) error {
		var err error
		switch key {
		case "Format":
			set.Format, err = r.integer(value)
			if err == nil && (set.Format < 0 || set.Format > fixturechange.CurrentFormat) {
				err = r.errorf(value, "the change set is in format %d, and this version of bun-fixture-migrate reads "+
					"formats up to %d: upgrade it", set.Format, fixturechange.CurrentFormat)
			}
		case "Name":
			set.Name, err = r.str(value)
		case "SeedGuardTable":
			set.SeedGuardTable, err = r.str(value)
		case "MigrationsTable":
			set.MigrationsTable, err = r.str(value)
		case "LockTimeout":
			set.LockTimeout, err = r.str(value)
		case "Tables":
			set.Tables, err = r.tables(value)
		case "Policy":
			set.Policy, err = r.policy(value)
		case "Changes":
			set.Changes, err = r.changes(value)
		default:
			err = r.unknown(value, key)
		}
		return err
	})
	return set, err
}

func (r *setReader) composite(expr ast.Expr, typ string) (*ast.CompositeLit, error) {
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil, r.errorf(expr, "%s is not written out as a literal", typ)
	}
	if lit.Type != nil && !r.isType(lit.Type, typ) {
		return nil, r.errorf(expr, "expected a fixturechange.%s", typ)
	}
	return lit, nil
}

func (r *setReader) tables(expr ast.Expr) (fixturechange.Tables, error) {
	lit, err := r.composite(expr, "Tables")
	if err != nil {
		return nil, err
	}
	out := fixturechange.Tables{}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return nil, r.errorf(elt, "a table without its model name")
		}
		model, err := r.str(kv.Key)
		if err != nil {
			return nil, err
		}
		tlit, err := r.composite(kv.Value, "Table")
		if err != nil {
			return nil, err
		}
		var t fixturechange.Table
		err = r.fields(tlit, func(key string, value ast.Expr) error {
			var err error
			switch key {
			case "Name":
				t.Name, err = r.str(value)
			case "ID":
				t.ID, err = r.str(value)
			case "Key":
				t.Key, err = r.str(value)
			case "Serial":
				t.Serial, err = r.boolean(value)
			case "Cascade":
				t.Cascade, err = r.boolean(value)
			case "Where":
				t.Where, err = r.str(value)
			default:
				err = r.unknown(value, key)
			}
			return err
		})
		if err != nil {
			return nil, err
		}
		out[model] = t
	}
	return out, nil
}

func (r *setReader) policy(expr ast.Expr) (fixturechange.Policy, error) {
	var p fixturechange.Policy
	lit, err := r.composite(expr, "Policy")
	if err != nil {
		return p, err
	}
	err = r.fields(lit, func(key string, value ast.Expr) error {
		mode, err := r.mode(value)
		switch key {
		case "MissingRow":
			p.MissingRow = mode
		case "ChangedRow":
			p.ChangedRow = mode
		case "IDDrift":
			p.IDDrift = mode
		case "DuplicateKey":
			p.DuplicateKey = mode
		default:
			err = r.unknown(value, key)
		}
		return err
	})
	return p, err
}

func (r *setReader) mode(expr ast.Expr) (fixturechange.Mode, error) {
	if sel, ok := expr.(*ast.SelectorExpr); ok && r.isType(sel, sel.Sel.Name) {
		switch sel.Sel.Name {
		case "ModeError":
			return fixturechange.ModeError, nil
		case "ModeWarn":
			return fixturechange.ModeWarn, nil
		case "ModeIgnore":
			return fixturechange.ModeIgnore, nil
		}
	}
	s, err := r.str(expr)
	return fixturechange.Mode(s), err
}

func (r *setReader) changes(expr ast.Expr) ([]fixturechange.Change, error) {
	if call, ok := expr.(*ast.CallExpr); ok {
		return r.parts(call)
	}
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil, r.errorf(expr, "Changes is not written out as a literal")
	}
	if arr, ok := lit.Type.(*ast.ArrayType); !ok || arr.Len != nil || !r.isType(arr.Elt, "Change") {
		return nil, r.errorf(expr, "expected a []fixturechange.Change")
	}
	out := make([]fixturechange.Change, 0, len(lit.Elts))
	for _, elt := range lit.Elts {
		clit, err := r.composite(elt, "Change")
		if err != nil {
			return nil, err
		}
		var c fixturechange.Change
		err = r.fields(clit, func(key string, value ast.Expr) error {
			var err error
			switch key {
			case "Model":
				c.Model, err = r.str(value)
			case "Kind":
				c.Kind, err = r.kind(value)
			case "ID":
				c.ID, err = r.str(value)
			case "Key":
				c.Key, err = r.values(value)
			case "Old":
				c.Old, err = r.values(value)
			case "New":
				c.New, err = r.values(value)
			default:
				err = r.unknown(value, key)
			}
			return err
		})
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// parts reads the Changes of a large set: fixturechange.Concat of calls to
// functions of the same file, each of which returns a literal and does
// nothing else.
func (r *setReader) parts(call *ast.CallExpr) ([]fixturechange.Change, error) {
	if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || !r.isType(sel, "Concat") {
		return nil, r.errorf(call, "Changes is neither a literal nor fixturechange.Concat of parts")
	}
	var out []fixturechange.Change
	for _, arg := range call.Args {
		var id *ast.Ident
		if part, ok := arg.(*ast.CallExpr); ok && len(part.Args) == 0 {
			id, _ = part.Fun.(*ast.Ident)
		}
		if id == nil {
			return nil, r.errorf(arg, "a part of Changes that is not a call of a function of this file")
		}
		fn := r.funcs[id.Name]
		if fn == nil || fn.Type.Params.NumFields() != 0 || fn.Body == nil || len(fn.Body.List) != 1 {
			return nil, r.errorf(arg, "%s is not a function of this file that only returns its changes", id.Name)
		}
		ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			return nil, r.errorf(fn, "%s is not a function of this file that only returns its changes", id.Name)
		}
		if _, ok := ret.Results[0].(*ast.CompositeLit); !ok {
			return nil, r.errorf(ret, "%s does not return its changes written out as a literal", id.Name)
		}
		changes, err := r.changes(ret.Results[0])
		if err != nil {
			return nil, err
		}
		out = append(out, changes...)
	}
	return out, nil
}

func (r *setReader) kind(expr ast.Expr) (fixturechange.Kind, error) {
	if sel, ok := expr.(*ast.SelectorExpr); ok && r.isType(sel, sel.Sel.Name) {
		switch sel.Sel.Name {
		case "Insert":
			return fixturechange.Insert, nil
		case "Update":
			return fixturechange.Update, nil
		case "Delete":
			return fixturechange.Delete, nil
		}
		return "", r.errorf(expr, "unknown kind %s; the file may have been written by a newer version of "+
			"bun-fixture-migrate, which this one cannot read: upgrade it", sel.Sel.Name)
	}
	s, err := r.str(expr)
	return fixturechange.Kind(s), err
}

func (r *setReader) values(expr ast.Expr) (fixturechange.Values, error) {
	lit, err := r.composite(expr, "Values")
	if err != nil {
		return nil, err
	}
	out := fixturechange.Values{}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return nil, r.errorf(elt, "a value without its column")
		}
		col, err := r.str(kv.Key)
		if err != nil {
			return nil, err
		}
		v, err := r.value(kv.Value)
		if err != nil {
			return nil, err
		}
		if _, dup := out[col]; dup {
			return nil, r.errorf(kv, "column %q twice", col)
		}
		out[col] = v
	}
	return out, nil
}

// value reads fixturechange.Lit("x"), fixturechange.Null() or
// fixturechange.RefTo("Model", "key").
func (r *setReader) value(expr ast.Expr) (fixturechange.Value, error) {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return fixturechange.Value{}, r.errorf(expr, "a value that is not Lit, Null or RefTo")
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !r.isType(sel, sel.Sel.Name) {
		return fixturechange.Value{}, r.errorf(expr, "a value that is not Lit, Null or RefTo")
	}
	args := make([]string, 0, len(call.Args))
	for _, a := range call.Args {
		s, err := r.str(a)
		if err != nil {
			return fixturechange.Value{}, err
		}
		args = append(args, s)
	}
	switch {
	case sel.Sel.Name == "Lit" && len(args) == 1:
		return fixturechange.Lit(args[0]), nil
	case sel.Sel.Name == "Null" && len(args) == 0:
		return fixturechange.Null(), nil
	case sel.Sel.Name == "RefTo" && len(args) == 2:
		return fixturechange.RefTo(args[0], args[1]), nil
	case sel.Sel.Name != "Lit" && sel.Sel.Name != "Null" && sel.Sel.Name != "RefTo":
		return fixturechange.Value{}, r.errorf(expr, "a value that is not Lit, Null or RefTo but %s; the file may "+
			"have been written by a newer version of bun-fixture-migrate, which this one cannot read: upgrade it",
			sel.Sel.Name)
	}
	return fixturechange.Value{}, r.errorf(expr, "a value that is not Lit, Null or RefTo")
}

func (r *setReader) str(expr ast.Expr) (string, error) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", r.errorf(expr, "expected a string literal")
	}
	return strconv.Unquote(lit.Value)
}

func (r *setReader) integer(expr ast.Expr) (int, error) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, r.errorf(expr, "expected a whole number")
	}
	n, err := strconv.ParseInt(lit.Value, 0, 32)
	if err != nil {
		return 0, r.errorf(expr, "%v", err)
	}
	return int(n), nil
}

// unknown is the error for a field this version does not know. A file names
// one when somebody edited it by hand, or when a newer version wrote it: a new
// field is how the format grows, and then this version cannot read the file
// and the application's fixtureapply cannot compile it.
func (r *setReader) unknown(n ast.Node, field string) error {
	return r.errorf(n, "unknown field %s; the file may have been written by a newer version of bun-fixture-migrate, "+
		"which this one cannot read: upgrade it", field)
}

func (r *setReader) boolean(expr ast.Expr) (bool, error) {
	if id, ok := expr.(*ast.Ident); ok && (id.Name == "true" || id.Name == "false") {
		return id.Name == "true", nil
	}
	return false, r.errorf(expr, "expected true or false")
}

// CheckPackage looks at the Go package a migration is about to be written into
// and says what would stop the file compiling there: an error for a package
// clause other than pkg, a warning when no file declares the migrator
// variable the generated file registers with.
func CheckPackage(dir, pkg, migrator string) (warnings []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	declared, files := false, 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files++
		if file.Name.Name != pkg {
			return nil, fmt.Errorf("%s is package %s and the configuration says package %s: "+
				"the generated file would not compile there, set package in the configuration",
				filepath.Join(dir, name), file.Name.Name, pkg)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				for _, id := range spec.(*ast.ValueSpec).Names {
					if id.Name == migrator {
						declared = true
					}
				}
			}
		}
	}
	if files > 0 && !declared {
		warnings = append(warnings, fmt.Sprintf("no file in %s declares the variable %s the generated file "+
			"registers with; set migrator in the configuration if yours has another name", dir, migrator))
	}
	return warnings, nil
}

// Applied is one row of bun's migrations table.
type Applied struct {
	Name       string
	GroupID    int64
	MigratedAt time.Time
}

// ReadApplied reads bun's migrations table, by migration name. The second
// result is false when the table does not exist, which is what a database the
// migrator never ran against looks like, and not an error.
//
// The name is used as bun uses it, unquoted, so PostgreSQL folds it to lower
// case exactly as it did for the migrator; it has been checked to be a plain,
// optionally schema-qualified identifier.
func ReadApplied(ctx context.Context, db bun.IDB, table string) (map[string]Applied, bool, error) {
	if _, err := quoteQualified(table); err != nil {
		return nil, false, err
	}
	quoted := table
	var exists bool
	if err := db.QueryRowContext(ctx, "SELECT to_regclass(?) IS NOT NULL", quoted).Scan(&exists); err != nil {
		return nil, false, fmt.Errorf("look for %s: %w", table, err)
	}
	if !exists {
		return nil, false, nil
	}
	rows, err := db.QueryContext(ctx, "SELECT name, group_id, migrated_at FROM "+quoted+" ORDER BY id")
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", table, err)
	}
	defer rows.Close()
	out := map[string]Applied{}
	for rows.Next() {
		var a Applied
		if err := rows.Scan(&a.Name, &a.GroupID, &a.MigratedAt); err != nil {
			return nil, true, fmt.Errorf("read %s: %w", table, err)
		}
		out[a.Name] = a
	}
	if err := rows.Err(); err != nil {
		return nil, true, fmt.Errorf("read %s: %w", table, err)
	}
	return out, true, rows.Close()
}
