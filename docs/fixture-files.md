# Fixture files

How the tool reads a `dbfixture` file, how it treats each kind of value, and what it refuses. The rule
behind all of it: a file this tool accepts is a file `dbfixture` loads, and it means to the tool what
it means to `dbfixture` and PostgreSQL. Each rule below is checked against the real loader in
`dbtest`.

- [Templates, anchors and order](#templates-anchors-and-order)
- [Values](#values)
- [Columns a row leaves out](#columns-a-row-leaves-out)
- [What it refuses](#what-it-refuses)
- [Limitations](#limitations)

## Templates, anchors and order

- A reference is `{{ $.Model.row.Field }}`, with the spaces: `dbfixture` evaluates only values
  holding `{{ ` and ` }}`, so `{{$.Model.row.ID}}` is text to both. The anchor and the field must
  be identifiers, as Go's templates require.
- A row is named by its `_id`, and a row without one by `pk` and its primary key:
  `{{ $.Plan.pk3.ID }}`.
- Templates resolve in file order, against the rows above them: a reference to a row further down is
  refused, because `dbfixture` cannot load it. When two rows share an anchor, each template names the
  latest one above it.
- Several files loaded with one `fixture.Load` share one scope of anchors, in load order.
- Any other template, `{{ now }}` or a function call, is evaluated by `dbfixture` when it loads the
  file, so the database never holds its text. It is refused rather than compared or written into a
  migration; `ignore` the column.
- A reference column (`references:`) holding a plain id resolves through the row of the file that
  declares that id. `~` is NULL. `0` or `""` point at nothing and stay literals, unless a row has
  that id: a zero is "no id" only in a `serial` model, where bun leaves it to the sequence.
- A reference names its row by that row's ref value as the database holds it, which is the ref
  column's to decide: `code: 0012` is the integer 10 in a `bigint` column and the text `0012` in a
  `text` one, and every reference to that row carries the same. So does a template copying a field,
  `{{ $.Currency.eur.Code }}`: it hands on what the field holds, which the field's type decides.
  Without the database, a change that depends on which one it is is refused, as a
  [value](#values) is. A ref column that is itself a template, and a copy of a field that is, are
  refused: write the value.
- `export` writes anchors from the natural key (prefixed with `r` when the key starts with a digit)
  and every reference as a template.

## Values

A value means what `dbfixture` makes of it, which depends on two things: the YAML type it was
written with, and the Go field it lands in. yaml.v3 hands a **string** field a plain scalar exactly
as written, and any other field the value it resolves to:

| In the file | In a string, enum or `[]string` column | In any other column |
| --- | --- | --- |
| `"01234"`, `' spaced '` | exactly that text | exactly that text, cast to the column's type |
| `01234`, `0x1F`, `1_000` | `01234`, `0x1F`, `1_000` | the integer YAML makes of it: 668, 31, 1000; of any size |
| `1.10`, `1e3`, `.5` | `1.10`, `1e3`, `.5` | the exact decimal 1.1, 1000, 0.5, never rounded through a float |
| `.inf`, `.nan` | `.inf`, `.nan` | `Infinity`, `NaN` |
| `True`, `false` | `True`, `false` | a boolean. `yes` and `on` are strings in YAML 1.2 |
| `2026-03-04 10:00:00` | `2026-03-04 10:00:00` | that instant, in UTC; unquoted, because a quoted one decodes into a `time.Time` only in RFC 3339 |
| `2026-03-04` | `2026-03-04` | a date |
| `~`, `null` | NULL (and see `null_default`) | NULL |
| a mapping `{sso: true}` or a sequence | | in `json` or `jsonb` the JSON document; in an array column the array |

The tool keeps both readings of such a value and lets the column decide: a column of a string type,
a domain over one, an enum, or an array of any of them takes the value as written, everything else
the resolved one. The column's type comes from the database, so **without one** (`generate` with no
`database` configured or with `-no-lint`, `status -offline`, `baseline`) a change that carries a
value whose two readings differ is refused with a reason, and so is a value only respelled (`1.10`
before, `1.1` after), which is a change in a text column and none in a numeric one. To have neither
question arise, quote a value meant as text, and write any other the way it resolves: `1.1`, `15`,
`true`, `2026-01-01T10:00:00Z`. `export` writes every value that way.

**With a database at hand** (`check`, `export`, `generate` when one is configured, `sync`), every
value of the file is cast to its column's type by PostgreSQL itself, in a session with fixed
`TimeZone`, `DateStyle`, `IntervalStyle`, `extra_float_digits` and `bytea_output`. Equality is then
PostgreSQL's: `1.50` equals `1.5` in a `numeric` column and not in a `text` one, two spellings of one
instant are one timestamp, and key order in a `jsonb` document does not matter. A value the column
cannot hold is an `invalid value` finding before anything is generated.

**At run time** a migration compares through the column's type too: `json` through `jsonb`, arrays
as their type, and the few types without an equality operator (`point`, `xml`) through their text.

**Without a database**, values are compared as the YAML type says: two spellings of one integer or
one decimal are equal, a string only equals the same string, and a value the column's type would
have to settle is refused, as above.

## Columns a row leaves out

A column a row does not mention is "not set". Two rows leaving it out are equal; one row setting it
and the other leaving it out is refused, because the tool would have to invent what the missing one
means. `defaults:` says what it means:

```yaml
defaults:
  quota: "0"     # a row without quota stands for 0
  note: ~        # a row without note stands for NULL
```

`scaffold` fills `defaults` from the column defaults. `~` is right for a column added to a table
later, which holds NULL in the rows written before it.

The comparison stays literal: the tool never substitutes a column default for a value the file
writes. A value bun would not write as it stands (a zero into a column with a default, a null into
a column with a default) is a fault in the file, and the lint says so.

When comparing against a database, only the columns the fixture files mention are read. A column no
fixture row writes is not master data, and a difference in it is not drift.

## What it refuses

Each ends up in the output with the model, the row and a reason; `generate` writes nothing unless
`-allow-partial`, and exits 2.

- **A rename**: the same id under a different natural key. An insert plus a delete is not a rename:
  rows elsewhere point at the old one, and so does whatever knows the old name outside the database.
  With `policy.renames: update` it becomes what it is, an `UPDATE` of the key columns guarded by the
  id, placed first in the migration. Two rows swapping names is still refused: one has to be parked
  under a third name first.
- **A renumbered primary key**: the same natural key under a different id, under `policy.id_drift`.
- **A delete of a model marked `deletes: refuse`.** Whether the rows pointing at it should go with
  it, be repointed or block the delete is a decision about your data.
- **Rows whose natural key is not unique**, once the group changes. Two rows with one key cannot be
  told apart by a `WHERE` clause. An unchanged duplicate group is left alone; `check` and `export`
  list every one.
- **A column set on one side only**, with no `defaults` entry.
- **A model the configuration does not list.** A model nobody visits is a change that silently does
  not happen.
- **A generated column** in the file, and **an explicit id** in a `GENERATED ALWAYS AS IDENTITY`
  column: PostgreSQL refuses to write either.
- **A value only the column's type can settle**, such as `1.10` or `017`, in a change computed
  without a database; see [values](#values).

The answer is always the same: configure what the tool cannot know, or write that one migration by
hand and `baseline -force`. See the [runbook](production.md#generate-refused-a-change).

## Limitations

- PostgreSQL 12 or later, plainly. The catalog queries, the sequence handling and the typed
  comparisons are written for it.
- Only `{{ $.Model.row.Field }}` templates are understood, and the field is mapped to a column by
  bun's default naming. A template naming a field whose column is spelled otherwise is an error, not
  a guess.
- A structured value (mapping or sequence) is supported in `json`, `jsonb` and array columns, and not
  as a reference.
- Values never become part of the SQL the tool writes: they are passed as arguments, which bun quotes
  and escapes, and PostgreSQL casts them to the column's type. `where` is your SQL, used as written;
  every other identifier comes from the catalog or the configuration, checked and quoted.
- A delete is guarded by the whole old row including its id, so on a database whose ids drifted the
  delete is reported as drift rather than deleting the wrong row.
- `status` and `plan` read the top level of the migrations directory, which is where a bun migrations
  package keeps them, and read a generated file back from its syntax: a change set built by code
  rather than written as a literal is reported, not guessed at.
- `export` and `check` read whole tables into memory. They are built for master data, hundreds or
  thousands of rows, not for a data warehouse.
- `dbfixture`'s `WithBeforeInsert` hooks and template functions change rows as they load; the tool
  cannot see what they do.
