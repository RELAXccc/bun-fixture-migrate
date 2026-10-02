package dbtest_test

// The application of examples/commerce between releases, for the replay in
// longrun_test.go: merchandisers add products to the category tree,
// customers order them, shipped and paid by the master data's methods and
// taxed at its rates, and the merchant gives its staff custom roles.

import (
	"fmt"
	"testing"
)

func TestLongRunningCommerceProject(t *testing.T) {
	replayExample(t, lrExample{
		name: "commerce",
		appData: []string{
			// Master rows by id: a deploy renames and moves them, and must
			// never take the application's rows to another one.
			`SELECT coalesce(string_agg(format('%s:%s:%s:%s', p.sku, p.category_id, p.tax_class, p.currency_code), ' ' ORDER BY p.id), '')
			 FROM catalog.products p`,
			`SELECT coalesce(string_agg(format('%s:%s', c.email, c.country_id), ' ' ORDER BY c.id), '')
			 FROM customers c`,
			`SELECT coalesce(string_agg(format('%s:%s:%s:%s:%s', o.id, o.customer_id, o.currency_code, o.shipping_method_id, o.payment_provider_id), ' ' ORDER BY o.id), '')
			 FROM orders o`,
			// The rate a line was taxed at, by what it is: a deploy closes a
			// rate and opens the next, and an order keeps the one it had.
			`SELECT coalesce(string_agg(format('%s:%s:%s/%s/%s/%s/%s', l.id, l.product_id, c.iso2, r.tax_class, r.valid_from, r.rate, l.unit_price), ' ' ORDER BY l.id), '')
			 FROM order_lines l JOIN billing.tax_rates r ON r.id = l.tax_rate_id JOIN countries c ON c.id = r.country_id`,
			`SELECT coalesce(string_agg(format('%s:%s', u.email, u.role_id), ' ' ORDER BY u.id), '')
			 FROM admin_users u`,
			// The merchant's custom roles and what they grant. A master
			// migration must never touch them.
			`SELECT coalesce(string_agg(format('%s:%s:%s', r.id, r.code, rp.permission_id), ' ' ORDER BY r.id, rp.permission_id), '')
			 FROM admin_roles r LEFT JOIN admin_role_permissions rp ON rp.role_id = r.id WHERE NOT r.builtin`,
		},
		appInserts: []lrInsert{
			{sql: `INSERT INTO countries (iso2, iso3, name, currency_code, eu_member) VALUES ('ZZ', 'ZZZ', '{}', 'EUR', false)`},
			{sql: `INSERT INTO billing.tax_rates (country_id, tax_class, rate, valid_from, valid_to)
			 VALUES ((SELECT min(id) FROM countries), 'zero', 0, '2099-01-01', '2099-02-01')`},
			{sql: `INSERT INTO billing.payment_providers (code, kind, display_name, supported_currencies, fee_percent, active)
			 VALUES ('probe', 'card', '{}', '{EUR}', 0, false)`},
			{sql: `INSERT INTO catalog.categories (slug, position, name, tags) VALUES ('probe', 99, '{}', '{}')`},
			{sql: `INSERT INTO shipping_zones (code, name, countries) VALUES ('probe', 'probe', '{}')`},
			{sql: `INSERT INTO shipping_methods (code, zone_id, carrier, name, base_price, currency_code, min_days, max_days, is_default)
			 VALUES ('probe', (SELECT min(id) FROM shipping_zones), 'probe', '{}', 0, 'EUR', 1, 1, false)`},
			{sql: `INSERT INTO menu_items (menu, slug, position, label, url) VALUES ('probe', 'probe', 1, '{}', '/')`},
			{sql: `INSERT INTO sender_identities (code, email, display_name) VALUES ('probe', 'probe@probe.example', 'probe')`},
			{sql: `INSERT INTO loyalty_tiers (code, name, min_points, discount_percent) VALUES ('probe', '{}', 0, 0)`, table: "loyalty_tiers"},
			{sql: `INSERT INTO admin_permissions (code, description) VALUES ('probe', 'probe')`},
			{sql: `INSERT INTO admin_roles (code, name, builtin) VALUES ('probe', 'probe', false)`},
		},
		traffic: commerceTraffic,
	})
}

// commerceTraffic is the shop at work between releases: a merchandiser adds
// a product to a leaf of the category tree, a customer in Germany orders it,
// shipped by a domestic method and paid with a provider that takes euros,
// taxed at the rate in force, and the merchant creates a custom role for new
// staff.
func commerceTraffic(lr *longrun, env *lrEnv) {
	n := lr.traffics
	tag := fmt.Sprintf("%s-%s", lr.release, env.name)
	pick := func(query string) string {
		var all []string
		if err := env.conn.NewRaw(query).Scan(lr.ctx, &all); err != nil {
			lr.t.Fatalf("%s %s: %s: %v", lr.release, env.name, query, err)
		}
		if len(all) == 0 {
			lr.t.Fatalf("%s %s: nothing for the traffic in %s", lr.release, env.name, query)
		}
		return all[n%len(all)]
	}
	category := pick(`SELECT c.slug::text FROM catalog.categories c
		WHERE NOT EXISTS (SELECT 1 FROM catalog.categories k WHERE k.parent_id = c.id) ORDER BY c.slug`)
	method := pick(`SELECT m.code FROM shipping_methods m JOIN shipping_zones z ON z.id = m.zone_id
		WHERE 'DE' = ANY (z.countries) ORDER BY m.code`)
	provider := pick(`SELECT code FROM billing.payment_providers WHERE active AND 'EUR' = ANY (supported_currencies) ORDER BY code`)
	stmts := []string{
		fmt.Sprintf(`INSERT INTO catalog.products (sku, category_id, tax_class, price, currency_code)
			VALUES ('SKU-%s', (SELECT id FROM catalog.categories WHERE slug = '%s'), '%s', %d.99, 'EUR')`,
			tag, category, []string{"standard", "reduced"}[n%2], 10+n),
		fmt.Sprintf(`INSERT INTO customers (email, country_id) VALUES ('buyer-%s@mail.example', (SELECT id FROM countries WHERE iso2 = 'DE'))`, tag),
		fmt.Sprintf(`INSERT INTO orders (customer_id, currency_code, shipping_method_id, payment_provider_id)
			SELECT c.id, 'EUR', m.id, p.id FROM customers c, shipping_methods m, billing.payment_providers p
			WHERE c.email = 'buyer-%s@mail.example' AND m.code = '%s' AND p.code = '%s'`, tag, method, provider),
		fmt.Sprintf(`INSERT INTO order_lines (order_id, product_id, quantity, unit_price, tax_rate_id)
			SELECT o.id, p.id, %d, p.price, r.id FROM orders o
			JOIN customers c ON c.id = o.customer_id
			JOIN catalog.products p ON p.sku = 'SKU-%s'
			JOIN billing.tax_rates r ON r.country_id = c.country_id AND r.tax_class = p.tax_class AND r.valid_to IS NULL
			WHERE c.email = 'buyer-%s@mail.example'`, 1+n%3, tag, tag),
		fmt.Sprintf(`INSERT INTO admin_roles (code, name, builtin) VALUES ('custom-%d', 'Custom role %d', false)`, n, n),
		fmt.Sprintf(`INSERT INTO admin_role_permissions (role_id, permission_id)
			SELECT r.id, p.id FROM admin_roles r, admin_permissions p
			WHERE r.code = 'custom-%d' AND p.code IN ('orders.read', 'customers.read')`, n),
		fmt.Sprintf(`INSERT INTO admin_users (email, role_id) VALUES
			('staff-%[1]s@shop.example', (SELECT id FROM admin_roles WHERE code = 'custom-%[2]d')),
			('manager-%[1]s@shop.example', (SELECT id FROM admin_roles WHERE code = 'manager'))`, tag, n),
	}
	for _, stmt := range stmts {
		run(lr.t, env.conn, stmt)
	}
}
