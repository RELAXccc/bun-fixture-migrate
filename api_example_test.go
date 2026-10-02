package fixturemigrate_test

// How a program runs the commands from Go. The examples need a project on
// disk and a PostgreSQL to talk to, so they compile and are not run.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// A project is the configuration and the fixture files it names, with paths
// relative to the configuration, as the command reads them.
func ExampleLoadProject() {
	project, err := fixturemigrate.LoadProject("fixture-migrate.yml")
	if err != nil {
		log.Fatal(err)
	}
	files, err := project.Files()
	if err != nil {
		log.Fatal(err)
	}
	for i, f := range files {
		fmt.Printf("%s: %d bytes\n", project.FixturePaths()[i], len(f.Data))
	}
	fmt.Println("migrations in", project.OutDir(), "state in", project.StatePath())
}

// A test, or a job on a schedule, that fails when the database and the
// fixture files disagree, as check exits 3.
func ExampleProject_Check() {
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(os.Getenv("DATABASE_URL")))), pgdialect.New())
	defer db.Close()

	project, err := fixturemigrate.LoadProject("fixture-migrate.yml")
	if err != nil {
		log.Fatal(err)
	}
	report, err := project.Check(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	if !report.Agree {
		log.Fatalf("the database and the fixture files disagree:\n%s", strings.Join(report.Lines(), "\n"))
	}
}

// A development server brings its database to the fixture files when it
// starts, keeping everything else in it.
func ExampleProject_Sync() {
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(os.Getenv("DATABASE_URL")))), pgdialect.New())
	defer db.Close()

	project, err := fixturemigrate.LoadProject("fixture-migrate.yml")
	if err != nil {
		log.Fatal(err)
	}
	report, err := project.Sync(ctx, db, fixturemigrate.SyncOptions{Logf: log.Printf})
	var refused *fixturemigrate.RefusedError
	if errors.As(err, &refused) {
		// A rename, a duplicate key: what a migration written by hand is for.
		log.Fatalf("%v\n%v %v", err, refused.Refusals, refused.Findings)
	}
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("%d changes, applied: %v", len(report.Diff.Changes), report.Applied)
}

// An admin tool takes the edits made in production into the fixture files.
// The role only reads: Export runs in a READ ONLY transaction.
func ExampleProject_Export() {
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(os.Getenv("PRODUCTION_READONLY_DSN")))),
		pgdialect.New())
	defer db.Close()

	project, err := fixturemigrate.LoadProject("fixture-migrate.yml")
	if err != nil {
		log.Fatal(err)
	}
	exported, err := project.Export(ctx, db, fixturemigrate.ExportOptions{})
	if errors.Is(err, fixturemigrate.ErrFindings) {
		// The files would not load back as the database: a zero or a null
		// bun replaces with the column's default.
		for _, f := range exported.Findings {
			log.Println(f.Kind, f)
		}
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
	written, err := exported.Write()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("wrote", written)
}

// A CI program writes the migration for an edit of the fixture files, after
// it has looked at what it would write. Without a database it works offline.
func ExampleProject_Generate() {
	ctx := context.Background()
	project, err := fixturemigrate.LoadProject("fixture-migrate.yml")
	if err != nil {
		log.Fatal(err)
	}
	g, err := project.Generate(ctx, nil, fixturemigrate.GenerateOptions{Name: "plan prices"})
	var refused *fixturemigrate.RefusedError
	if errors.As(err, &refused) {
		// A rename, a finding the policy makes an error, or, with
		// errors.Is(err, fixturemigrate.ErrLineage), a migration generated on
		// another branch: the message says what to do.
		log.Fatalf("%v\n%v %v\n%s", err, refused.Refusals, refused.Findings, strings.Join(refused.Problems, "\n"))
	}
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(strings.Join(g.Diff.Summary(), "\n"))
	for _, w := range g.Warnings {
		fmt.Println("warning:", w)
	}
	// Nothing changed is nothing to write, but for a state file that takes
	// the files' new spelling.
	written, err := g.Write()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("wrote", written)
}

// A gate in CI, status -offline: a fixture edit that came without its
// migration fails it.
func ExampleProject_Status() {
	project, err := fixturemigrate.LoadProject("fixture-migrate.yml")
	if err != nil {
		log.Fatal(err)
	}
	report, err := project.Status(context.Background(), nil, fixturemigrate.StatusOptions{})
	if err != nil {
		log.Fatal(err)
	}
	for _, line := range report.Uncovered {
		fmt.Println("not migrated:", line)
	}
	if len(report.Failures) > 0 {
		log.Fatal(strings.Join(report.Failures, "; "))
	}
}

// Adopting the tool on a project whose databases hold the fixture files
// already: record them as what the migrations leave a database holding.
func ExampleProject_Baseline() {
	ctx := context.Background()
	project, err := fixturemigrate.LoadProject("fixture-migrate.yml")
	if err != nil {
		log.Fatal(err)
	}
	b, err := project.Baseline(ctx, nil, fixturemigrate.BaselineOptions{})
	if errors.Is(err, fixturemigrate.ErrUnmigrated) {
		// The files hold changes no migration makes: generate one first.
		log.Fatal(err)
	}
	if err != nil {
		log.Fatal(err)
	}
	if b.Unchanged {
		return
	}
	if _, err := b.Write(); err != nil {
		log.Fatal(err)
	}
}

// A program that builds its own pipeline reads the way the commands do: one
// snapshot, nothing written, the session's spelling of values fixed.
func ExampleReadOnly() {
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(os.Getenv("DATABASE_URL")))), pgdialect.New())
	defer db.Close()

	project, err := fixturemigrate.LoadProject("fixture-migrate.yml")
	if err != nil {
		log.Fatal(err)
	}
	err = fixturemigrate.ReadOnly(ctx, db, func(tx bun.Tx) error {
		applied, exists, err := fixturemigrate.ReadApplied(ctx, tx, project.Config.MigrationsTable)
		fmt.Println(len(applied), "migrations applied; the table exists:", exists)
		return err
	})
	if err != nil {
		log.Fatal(err)
	}
}
