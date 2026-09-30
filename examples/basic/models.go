package main

import "github.com/uptrace/bun"

// The models dbfixture loads the fixture file into. A fixture file names a
// model, not a table: dbfixture finds the table through the registered model,
// and bun-fixture-migrate through the table the configuration names for it.

type Currency struct {
	bun.BaseModel `bun:"table:currencies"`

	ID     int64 `bun:",pk"`
	Code   string
	Symbol string
}

type Plan struct {
	bun.BaseModel `bun:"table:plans"`

	ID         int64 `bun:",pk,autoincrement"`
	Name       string
	CurrencyID int64
	PriceCents int64
	Seats      int64
	Settings   map[string]any `bun:"type:jsonb"`
	// A nil pointer is written as DEFAULT, so a row without a note gets the
	// column's default. The configuration says what that is (defaults:
	// note: ~), so the tool knows what the loader writes.
	Note *string
}

type Feature struct {
	bun.BaseModel `bun:"table:features"`

	ID     int64 `bun:",pk,autoincrement"`
	PlanID int64
	Code   string
	Quota  int64
}
