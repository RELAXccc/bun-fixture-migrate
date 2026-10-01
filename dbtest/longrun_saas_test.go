package dbtest_test

// The application of examples/saas between releases, for the replay in
// longrun_test.go: tenants sign up, create custom roles, subscribe and are
// invoiced.

import (
	"fmt"
	"testing"
)

func TestLongRunningSaaSProject(t *testing.T) {
	replayExample(t, lrExample{
		name:       "saas",
		minVersion: 130000,
		minReason:  "the example's plans have gen_random_uuid() keys, which PostgreSQL has built in from 13 on",
		appData: []string{
			`SELECT coalesce(string_agg(format('%s:%s:%s', s.id, p.code, c.code), ' ' ORDER BY s.id), '')
			 FROM subscriptions s JOIN plans p ON p.id = s.plan_id JOIN currencies c ON c.id = s.currency_id`,
			`SELECT coalesce(string_agg(format('%s:%s/%s/%s/%s=%s', i.id, p.code, c.code, pp.valid_from, pp.amount, i.amount), ' ' ORDER BY i.id), '')
			 FROM invoices i JOIN plan_prices pp ON pp.id = i.plan_price_id JOIN plans p ON p.id = pp.plan_id
			 JOIN currencies c ON c.id = pp.currency_id`,
			`SELECT coalesce(string_agg(format('%s:%s:%s', u.id, r.code, coalesce(r.tenant_id::text, 'master')), ' ' ORDER BY u.id), '')
			 FROM users u JOIN roles r ON r.id = u.role_id`,
			// A tenant's custom roles and what they grant. A master migration
			// must never touch them.
			`SELECT coalesce(string_agg(format('%s:%s:%s', r.tenant_id, r.code, rp.permission_id), ' ' ORDER BY r.id, rp.permission_id), '')
			 FROM roles r LEFT JOIN role_permissions rp ON rp.role_id = r.id WHERE r.tenant_id IS NOT NULL`,
			`SELECT coalesce(string_agg(format('%s:%s', t.name, c.code), ' ' ORDER BY t.id), '')
			 FROM tenants t JOIN countries c ON c.id = t.country_id`,
		},
		appInserts: []lrInsert{
			{sql: `INSERT INTO currencies (code, name, symbol, minor_units) VALUES ('ZZZ', 'probe', 'z', 2)`},
			{sql: `INSERT INTO countries (code, name, currency_id, tax_rate) VALUES ('ZZ', 'probe', (SELECT min(id) FROM currencies), 0)`},
			{sql: `INSERT INTO features (code, name) VALUES ('probe', 'probe')`},
			{sql: `INSERT INTO permissions (code, description) VALUES ('probe', 'probe')`},
			{sql: `INSERT INTO categories (slug, name, position) VALUES ('probe', 'probe', 99)`},
			{sql: `INSERT INTO translations (locale, key, value) VALUES ('xx', 'probe', 'probe')`},
			{sql: `INSERT INTO plan_prices (plan_id, currency_id, valid_from, amount)
			 VALUES ((SELECT min(id::text)::uuid FROM plans), (SELECT min(id) FROM currencies), '2099-12-31', 1)`},
			// Where the application works, its tenants' custom roles are
			// this insert, kept rather than rolled back.
			{sql: `INSERT INTO roles (code, name) VALUES ('probe', 'probe')`, notWithTraffic: true},
		},
		traffic: saasTraffic,
	})
}

// saasTraffic is the application at work between releases: a tenant signs
// up, creates a custom role (its id comes from the sequence the master roles'
// ids share), invites users, subscribes to a plan and is invoiced at the
// plan's current price.
func saasTraffic(lr *longrun, env *lrEnv) {
	n := lr.traffics
	tenant := fmt.Sprintf("tenant-%s-%s", lr.release, env.name)
	var plans []string
	if err := env.conn.NewRaw(`SELECT p.code FROM plans p WHERE p.active
		AND EXISTS (SELECT 1 FROM plan_prices pp JOIN currencies c ON c.id = pp.currency_id
		            WHERE pp.plan_id = p.id AND c.code = 'EUR')
		ORDER BY p.code`).Scan(lr.ctx, &plans); err != nil {
		lr.t.Fatal(err)
	}
	plan := plans[n%len(plans)]
	stmts := []string{
		fmt.Sprintf(`INSERT INTO tenants (name, country_id) VALUES ('%s', (SELECT id FROM countries WHERE code = 'DE'))`, tenant),
		fmt.Sprintf(`INSERT INTO roles (tenant_id, code, name)
			VALUES ((SELECT id FROM tenants WHERE name = '%s'), 'custom-%d', 'Custom role %d')`, tenant, n, n),
		fmt.Sprintf(`INSERT INTO role_permissions (role_id, permission_id)
			SELECT r.id, p.id FROM roles r, permissions p
			WHERE r.tenant_id = (SELECT id FROM tenants WHERE name = '%s') AND p.code IN ('projects.read', 'reports.export')`, tenant),
		fmt.Sprintf(`INSERT INTO users (tenant_id, email, role_id) VALUES
			((SELECT id FROM tenants WHERE name = '%[1]s'), 'owner@%[1]s.example', (SELECT id FROM roles WHERE code = 'owner' AND tenant_id IS NULL)),
			((SELECT id FROM tenants WHERE name = '%[1]s'), 'staff@%[1]s.example', (SELECT id FROM roles WHERE tenant_id = (SELECT id FROM tenants WHERE name = '%[1]s')))`, tenant),
		fmt.Sprintf(`INSERT INTO subscriptions (tenant_id, plan_id, currency_id, seats)
			SELECT t.id, p.id, c.id, %d FROM tenants t, plans p, currencies c
			WHERE t.name = '%s' AND p.code = '%s' AND c.code = 'EUR'`, 3+n, tenant, plan),
		fmt.Sprintf(`INSERT INTO invoices (subscription_id, plan_price_id, amount)
			SELECT s.id, pp.id, pp.amount * s.seats FROM subscriptions s
			JOIN tenants t ON t.id = s.tenant_id
			JOIN LATERAL (SELECT * FROM plan_prices pp WHERE pp.plan_id = s.plan_id AND pp.currency_id = s.currency_id
			              ORDER BY pp.valid_from DESC LIMIT 1) pp ON true
			WHERE t.name = '%s'`, tenant),
	}
	for _, stmt := range stmts {
		run(lr.t, env.conn, stmt)
	}
}
