package main

import (
	"time"

	"github.com/uptrace/bun"
)

// The models dbfixture loads the fixture files into. A fixture file names a
// model, not a table: dbfixture finds the table through the registered model,
// and bun-fixture-migrate through the table fixture-migrate.yml names for it.
// Only master data has a model here; products, customers, orders and admin
// users are written by the application.

// masterModels is every model the fixture files use.
var masterModels = []any{
	(*Currency)(nil), (*Country)(nil), (*TaxRate)(nil), (*PaymentProvider)(nil),
	(*Category)(nil), (*CategorySEO)(nil), (*ShippingZone)(nil), (*ShippingMethod)(nil),
	(*MenuItem)(nil), (*SenderIdentity)(nil),
	(*AdminRole)(nil), (*AdminPermission)(nil), (*AdminRolePermission)(nil),
	(*HsCode)(nil),
}

// Currency is keyed by its ISO 4217 code, which is its primary key too.
type Currency struct {
	bun.BaseModel `bun:"table:billing.currencies"`

	Code       string `bun:",pk"`
	Name       string
	Symbol     string
	MinorUnits int16 `bun:",notnull"`
}

type Country struct {
	bun.BaseModel `bun:"table:countries"`

	ID           int64             `bun:",pk,autoincrement"`
	ISO2         string            `bun:"iso2"`
	ISO3         string            `bun:"iso3"`
	Name         map[string]string `bun:"type:jsonb,notnull"`
	CurrencyCode string
	EUMember     bool `bun:"eu_member,notnull"`
}

// TaxRate is effective-dated: a nil ValidTo is the rate in force.
type TaxRate struct {
	bun.BaseModel `bun:"table:billing.tax_rates"`

	ID        int64 `bun:",pk,autoincrement"`
	CountryID int64
	TaxClass  string `bun:"type:billing.tax_class"`
	// numeric(6,4), kept as text so no rate goes through a float.
	Rate      string    `bun:"type:numeric(6,4)"`
	ValidFrom time.Time `bun:"type:date,notnull"`
	ValidTo   time.Time `bun:"type:date,nullzero"`
}

type PaymentProvider struct {
	bun.BaseModel `bun:"table:billing.payment_providers"`

	ID                  int64 `bun:",pk,autoincrement"`
	Code                string
	Kind                string            `bun:"type:billing.provider_kind"`
	DisplayName         map[string]string `bun:"type:jsonb,notnull"`
	SupportedCurrencies []string          `bun:",array,notnull"`
	FeePercent          string            `bun:"type:numeric(5,2)"`
	Active              bool              `bun:",notnull"`
}

type Category struct {
	bun.BaseModel `bun:"table:catalog.categories"`

	ID       int64 `bun:",pk,autoincrement"`
	Slug     string
	ParentID int64 `bun:",nullzero"`
	Position int16 `bun:",notnull"`
	// The display name per locale.
	Name map[string]string `bun:"type:jsonb,notnull"`
	Tags []string          `bun:",array,notnull"`
}

// CategorySEO extends a category: its primary key is the category's id.
type CategorySEO struct {
	bun.BaseModel `bun:"table:catalog.category_seo"`

	CategoryID      int64             `bun:",pk"`
	MetaTitle       map[string]string `bun:"type:jsonb,notnull"`
	MetaDescription map[string]string `bun:"type:jsonb,nullzero"`
	NoIndex         bool              `bun:"noindex,notnull"`
}

type ShippingZone struct {
	bun.BaseModel `bun:"table:shipping_zones"`

	ID               int64 `bun:",pk,autoincrement"`
	Code             string
	Name             string
	Countries        []string `bun:",array,notnull"`
	FreeShippingOver *string  `bun:"type:numeric(10,2)"`
}

type ShippingMethod struct {
	bun.BaseModel `bun:"table:shipping_methods"`

	ID           int64 `bun:",pk,autoincrement"`
	Code         string
	ZoneID       int64
	Carrier      string
	Name         map[string]string `bun:"type:jsonb,notnull"`
	BasePrice    string            `bun:"type:numeric(10,2)"`
	CurrencyCode string
	MinDays      int16 `bun:",notnull"`
	MaxDays      int16 `bun:",notnull"`
	IsDefault    bool  `bun:",notnull"`
}

type MenuItem struct {
	bun.BaseModel `bun:"table:menu_items"`

	ID       int64 `bun:",pk,autoincrement"`
	Menu     string
	Slug     string
	Position int16             `bun:",notnull"`
	Label    map[string]string `bun:"type:jsonb,notnull"`
	URL      string            `bun:"url"`
}

type SenderIdentity struct {
	bun.BaseModel `bun:"table:sender_identities"`

	ID          int64 `bun:",pk,autoincrement"`
	Code        string
	Email       string
	DisplayName string
	ReplyTo     *string
}

// AdminRole: the built-in roles are master data; a merchant's custom roles
// share the table and its identity.
type AdminRole struct {
	bun.BaseModel `bun:"table:admin_roles"`

	ID      int64 `bun:",pk,autoincrement"`
	Code    string
	Name    string
	Builtin bool `bun:",notnull"`
}

type AdminPermission struct {
	bun.BaseModel `bun:"table:admin_permissions"`

	ID          int64 `bun:",pk,autoincrement"`
	Code        string
	Description string
}

type AdminRolePermission struct {
	bun.BaseModel `bun:"table:admin_role_permissions"`

	RoleID       int64 `bun:",pk"`
	PermissionID int64 `bun:",pk"`
}

// HsCode is a Harmonised System code: six digits, leading zeros kept.
type HsCode struct {
	bun.BaseModel `bun:"table:billing.hs_codes"`

	ID          int64 `bun:",pk,autoincrement"`
	Code        string
	Chapter     int16             `bun:",notnull"`
	Description map[string]string `bun:"type:jsonb,notnull"`
	DutyFreeEU  bool              `bun:"duty_free_eu,notnull"`
}
