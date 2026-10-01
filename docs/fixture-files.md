# Fixture files

How the tool reads a `dbfixture` file, how it treats each kind of value, and what it refuses. The rule
behind all of it: a file this tool accepts is a file `dbfixture` loads, and it means to the tool what
it means to `dbfixture` and PostgreSQL. Each rule below is checked against the real loader in
`dbtest`.

- [Templates, anchors and order](#templates-anchors-and-order)
- [Values](#values)
  - [Dates and times](#dates-and-times)
  - [JSON and jsonb](#json-and-jsonb)
  - [Lengths, domains and constraints](#lengths-domains-and-constraints)
  - [What export writes](#what-export-writes)
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
  migration; `ignore` the column. A template of text and string constants is the exception: it
  evaluates to the same text anywhere, and `'{{ "Hello {{ name }}" }}'` is how a file stores a value
  holding the delimiters. It reads as `Hello {{ name }}`.
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
- A template copying a field other than the id stores what the field holds as Go's `fmt` prints it.
  That is the value for a field of a string or an integer column, and nothing a file can write for
  any other: a `float64` of 100000000 prints as `1e+08`, a `time.Time` as
  `2026-01-01 10:00:00 +0000 UTC`, a nil pointer as `<nil>`. So a copy of a null, of a mapping or
  sequence, or of a column of another type is refused; without the database, which says the column's
  type, a change carrying a copy is. An id written as a template is refused too: the tool reads the
  id as the row's own value.
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
| `2026-03-04` | `2026-03-04` | a date; in a `timestamptz` column midnight UTC, which is what a `time.Time` field holds |
| `~`, `null` | NULL (and see `null_default`) | NULL; in `json` and `jsonb` [a finding](#json-and-jsonb) |
| `!!binary SGk=` | the text it encodes, `Hi` | the text it encodes |
| `1.5` in an integer column | | a finding: an integer field holds `1`, a string field is refused |
| a mapping `{sso: true}` or a sequence | | in `json` or `jsonb` the JSON document; in an array column the array, and a sequence of sequences an `invalid value`, because bun cannot write a nested slice into an array column and `dbfixture` fails to load it; in `bytea` the bytes of a sequence of byte values, the only YAML a `[]byte` field loads |
| an alias `*name` | the value it names | the value it names |

A null inside a sequence, `[a, ~, b]`, is left out by a `[]string` or `[]int64` field and kept by a
`[]*string` one, and no column type says which the model has: it is an `ambiguous value` finding,
and a change carrying it is refused, unless `array_nulls: keep` in the policy or on the model says
its array fields keep a null. An alias of a template is refused too: `dbfixture` evaluates a
template only where it is written, and would store the text of one reached through an alias.

The tool keeps both readings of such a value and lets the column decide: a column of a string type,
a domain over one, an enum, or an array of any of them takes the value as written, everything else
the resolved one. A domain is its base type throughout: a domain over integer is a number, one over
`jsonb` a JSON document, and its default is the column's when the column has none. The column's type comes from the database, so **without one** (`generate` with no
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

### Dates and times

The Go field decides what a date or a time becomes, and the tool cannot see it: bun writes a
`time.Time` in UTC, cut to microseconds, and PostgreSQL reads a string in the `TimeZone` and
`DateStyle` of the session that seeds. A value is accepted when it is one value either way; when it
is not, it is an `invalid value` finding that names both and says how to write the one you mean.
`check`, `generate` with a database, and `sync` read every value of a `date`, `timestamp`,
`timestamptz`, `time` or `timetz` column, or of an array of them, under other session settings too.

| In the file | Column | |
| --- | --- | --- |
| `2026-01-01 10:00:00`, `2026-01-01T10:00:00Z`, `"2026-01-01T10:00:00Z"` | `timestamp` | 10:00 |
| `2026-01-01T10:00:00+02:00`, quoted or not | `timestamptz` | that instant |
| `2026-01-01 10:00:00`, unquoted | `timestamptz` | 10:00 UTC: the column is taken to be written from a `time.Time`, to which yaml.v3 hands a timestamp without a zone in UTC |
| `2026-01-01`, unquoted | `timestamptz` | midnight UTC, for the same reason, in an array too |
| `2026-01-01`, `2026-01-01T10:00:00+02:00` | `date` | the 1st |
| `"10:00:00"`, `"10:00:00+02"` | `time`, `timetz` | that time; `time` drops the offset whoever writes it |
| `"infinity"` | any of them | infinity, which only a string field or a type that reads it holds |
| `2026-01-01T10:00:00+02:00`, quoted or not | `timestamp` | refused: 08:00 through a `time.Time`, 10:00 through a string |
| `2026-01-01T23:30:00-05:00` | `date` | refused: the 2nd through a `time.Time`, the 1st through a string |
| `2026-01-01T10:00:00.1234567Z` | any | refused: bun cuts to `.123456`, PostgreSQL rounds to `.123457` |
| `"2026-01-01 10:00:00"`, `"2026-01-01"`, `"10:00"` | `timestamptz`, `timetz` | refused: a string is read in the seeding session's time zone |
| `"01/02/2026"` | any | refused: January or February by `DateStyle` |
| `"now"`, `"today"`, `"tomorrow"` | any | refused: a different value every day |

Write a date as `2026-01-02`, a `timestamp` without an offset, a `timestamptz` or `timetz` with one,
and at most six fractional digits. `export` writes every value that way.

### JSON and jsonb

A mapping or sequence in a `json` or `jsonb` column is what a `map[string]any`, `[]any` or `any` field
makes of it, as `encoding/json` marshals that, at the top of the column as inside it: a key as it is
written (`017: x` is the key `"017"`), a number as a `float64` (`0.1234567890123456789` is stored as
`0.12345678901234568`, an integer beyond 64 bits the same way), a timestamp as the `time.Time` yaml.v3
makes of it (`2026-01-01` is `"2026-01-01T00:00:00Z"`, an offset is kept), `!!binary` as the text it
encodes. So `[2026-01-01T10:00:00+02:00]` is `["2026-01-01T10:00:00+02:00"]` there, where a
`timestamptz[]` column holds that instant. Two documents are compared as `jsonb` compares them, with
every number written canonically, so `{"a": 1.0}` written by SQL and `{a: 1}` in the file agree. A
migration writes a document, and an array, in one spelling, compact with the keys sorted, whether
`generate` read the database or not, so one edit of the file is one migration. A
YAML merge key `<<` inside a mapping is merged as yaml.v3 merges it: a key the mapping writes itself
wins over a merged one, and of several mappings merged, the first.

A scalar is read the same way: a timestamp, a date and a float are what an `any` field makes of them
(`2026-01-01T10:00:00+02:00` is the JSON string `"2026-01-01T10:00:00+02:00"`, `2026-01-01` is
`"2026-01-01T00:00:00Z"`, `0.1234567890123456789` is `0.12345678901234568`). A string is the document
a string field hands bun when it is JSON (`'{"a": 1}'`, `"1.5"`), and the JSON string an `any` field
makes of it when it is not (`hello` is `"hello"`).

`~` is the JSON null to a map, slice or `any` field and SQL NULL, or the column default, to a nil
pointer or a `nullzero` field, so in a column without a default it is a `null against a default`
finding under `policy.null_default`. Set the policy to `warn` if your models write NULL there, and the
tool reads `~` as NULL. For the JSON null, leave the column out of the row and give the model
`defaults: {settings: 'null'}`.

### Lengths, domains and constraints

`check`, `generate` with a database, and `sync` cast every value of the file to its column's type,
domain and length included, and hold it against what an `INSERT` would do, which is not always what
a cast does:

- A value longer than `varchar(n)` or `char(n)` is an `invalid value`, unless what is past the length
  is spaces: an `INSERT` drops those, and so does the migration, so `"abc    "` is `abc  ` in a
  `varchar(5)`. `char(n)` is compared without its padding, in an array too.
- A bit string has to be exactly as long as `bit(n)`, and no longer than `bit varying(n)`; a cast
  would pad or cut it without a word.
- Any error PostgreSQL gives casting a value is that value's `invalid value`, with its message: a
  domain's `CHECK`, a syntax error in an `hstore`, `ltree` or `tsquery`, an enum label that does not
  exist. A domain's `NOT NULL` makes the column one that takes no NULL.
- A table's `CHECK` constraint that names one column is evaluated against each value of it, and a
  value it refuses is an `invalid value`. A constraint over several columns, a unique index, a foreign
  key and a trigger are not evaluated: `plan` runs the migration and reports what they refuse, and
  without it the deploy fails.
- Two natural keys that differ as text but are one value to the key's type, `Go` and `GO` in a
  `citext` column, are a `duplicate key`. A key that changes only that way between two states, `go` in
  the database and `Go` in the file, is one row under a new spelling, which a fresh seed stores: a
  rename, under `policy.renames`, even when the file has no ids to say so.

### What export writes

Every value is written so that `dbfixture` loads it back through the field a bun model has for the
column's type, and the tool reads it back as the same value; the export is parsed back with yaml.v3
and held against the database before anything is written, and a difference fails it.

- Numbers in decimal, `NaN` and `Infinity` of a float as `.nan`, `.inf`, `-.inf`. A `numeric` `NaN` or
  `Infinity` is refused: a string field and a `float64` load different spellings.
- Instants in RFC 3339 UTC, dates as dates. `infinity` is written as `"infinity"`, with a comment
  that a `time.Time` cannot hold it.
- `bytea` as the sequence of its bytes, `[72, 105]`, which is what a `[]byte` field loads.
- `json` and `jsonb` documents as flow mappings and sequences, numbers canonical. A number with more
  digits than a `float64` holds, a top-level string that is itself JSON, and the JSON null are
  refused: no spelling reads back as itself. A model with `defaults: {settings: 'null'}` gets its JSON
  nulls left out of the rows instead, which is what a map field leaves there. SQL NULL is `~`,
  refused unless `policy.null_default` is `warn`, because a map field loads `~` as the JSON null.
- Arrays as sequences. An array of more than one dimension is refused: a file can only write it as a
  sequence of sequences, which `dbfixture` fails to load, because bun cannot write a nested slice into
  an array column; put the column in `ignore`. So is an array whose lower bound is not 1,
  `[0:1]={7,8}`, and, unless the model's `array_nulls` is `keep`, an array holding a NULL element,
  which a `[]string` or `[]int64` field would load without it.
- Text double-quoted, everything YAML would refuse or fold escaped: control characters, DEL, the C1
  range, NEL, U+2028 and U+2029, U+FFFE. Text that `dbfixture` would evaluate as a template,
  `Hello {{ name }}`, is written as a template whose only action is that text as a string literal,
  `'{{ "Hello {{ name }}" }}'`, which evaluates to it.

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

A column of a `key_any_of` group that a row leaves out is NULL unless `defaults` says otherwise: the
group's columns are the references a row sets one of, and the database holds NULL in the others. So a
row setting none of them is keyed by NULL, as the database reads it.

`scaffold` fills `defaults` from the column defaults. `~` is right for a column added to a table
later, which holds NULL in the rows written before it.

A row that leaves out a column other rows of its model write, with no `defaults` entry for it, is
not inserted by a migration: `dbfixture` stores the field's zero there, or NULL, or the column's
default, depending on the model, and every comparison with the database would refuse the row
afterwards. Write the column, or say in `defaults` what leaving it out means.

The comparison stays literal: the tool never substitutes a column default for a value the file
writes. A value bun would not write as it stands (a zero into a column with a default, a null into
a column with a default) is a fault in the file, and the lint says so.

When comparing against a database, only the columns the fixture files mention are read. A column no
fixture row writes is not master data, and a difference in it is not drift.

Every configured model is compared, though. A model the files hold no block of has no rows in a fresh
seed, so `check` reports its rows as in the database only and `sync` deletes them, under the model's
`deletes`, as `generate` does when a block leaves the files. Its rows are read by their key alone.

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
- **A reference to a ref value more than one row holds**, such as two categories called
  `Accessories` under different parents: a migration finds the row a reference names by that value
  alone. Make the ref column unique.
- **Two rows sharing one id**, which two branches each adding the next id leave behind: `dbfixture`
  cannot load the file, so it is a `duplicate id` finding, and an insert writing that id is
  refused. Neither row is taken for a rename of the other.
- **Rows whose natural key is not unique**, once the group changes. Two rows with one key cannot be
  told apart by a `WHERE` clause. An unchanged duplicate group is left alone; `check` and `export`
  list every one.
- **A column set on one side only**, with no `defaults` entry.
- **A model the configuration does not list.** A model nobody visits is a change that silently does
  not happen.
- **A generated column** in the file, and **an explicit id** in a `GENERATED ALWAYS AS IDENTITY`
  column: PostgreSQL refuses to write either.
- **A null in a NOT NULL column without a default**, an `invalid value`: bun writes a plain field's
  zero there instead, and a pointer field, like a migration, fails the insert.
- **A value only the column's type can settle**, such as `1.10` or `017`, in a change computed
  without a database; see [values](#values).
- **A value the column cannot take as `dbfixture` writes it**, an `invalid value`: one PostgreSQL
  refuses to cast, one too long for the column, one a single-column `CHECK` refuses, a fraction in
  an integer column. See [lengths, domains and constraints](#lengths-domains-and-constraints).
- **A value whose stored value the model or the server decides**: a date or time a `time.Time` and a
  string field store differently, or the seeding session reads ([dates and times](#dates-and-times)),
  and `~` in a `json` or `jsonb` column ([JSON](#json-and-jsonb)).
- **A model whose table is a view** or a materialized view: only a table holds master data.

The answer is always the same: configure what the tool cannot know, or write that one migration by
hand and `baseline -force`. See the [runbook](production.md#generate-refused-a-change).

## Limitations

- PostgreSQL 12 or later, plainly. The catalog queries, the sequence handling and the typed
  comparisons are written for it.
- Only `{{ $.Model.row.Field }}` templates are understood, and the field is mapped to a column by
  bun's default naming. A template naming a field whose column is spelled otherwise is an error, not
  a guess.
- A model's `id` is the row's own value, never a reference: a primary key that also points at another
  model, a plan's limits keyed by the plan, is refused in the configuration. Leave `id` out for such a
  table, which is then read without one, and keep the column in `key` and `references`.
- A structured value (mapping or sequence) is supported in `json`, `jsonb`, array and `bytea` columns,
  and not as a reference. A mapping in an `hstore` column, which a `map[string]string` field loads, is
  an `invalid value`.
- A `json` column is written in `jsonb`'s spelling: equal as JSON, but not byte for byte what bun
  writes.
- A top-level string that is itself JSON, in a `json` or `jsonb` column, is taken as that document,
  which is what a string field stores; an `any` field stores it as a JSON string.
- A sequence in a `json` or `jsonb` column is read as an `[]any` field reads it. A `[]string` field
  stores every element as the text it is written as, so `[1, 2026-01-01]` is `["1", "2026-01-01"]`
  through one: quote the elements of such a field.
- Without a database the JSON reading of a value is not known to apply: a date, a timestamp's offset
  and a float of more digits than a `float64` holds are compared as a `date`, `timestamptz` or
  `numeric` column reads them, which is what they are wherever they are not JSON.
- The two readings of a date or time are compared in scalar columns. Inside an array a date or time is
  what a `[]time.Time` field makes of it, and only the seeding session is checked.
- Keys equal under their type are found for the type's own equality; a column's nondeterministic
  collation is not considered.
- A `CHECK` over several columns, a unique index, a foreign key and a trigger are checked by `plan`,
  not by `check` or `generate`.
- Two `bytea` readings meet in a sequence: a quoted string that is a JSON array of byte values, in a
  `bytea` column, is taken for the sequence.
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
