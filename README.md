# migrate

[![Doc](https://pkg.go.dev/badge/github.com/go-rio/migrate.svg)](https://pkg.go.dev/github.com/go-rio/migrate)
[![Go](https://img.shields.io/github/go-mod/go-version/go-rio/migrate)](https://go.dev/)
[![Release](https://img.shields.io/github/release/go-rio/migrate.svg)](https://github.com/go-rio/migrate/releases)
[![Test](https://github.com/go-rio/migrate/actions/workflows/test.yml/badge.svg)](https://github.com/go-rio/migrate/actions/workflows/test.yml)
[![License](https://img.shields.io/github/license/go-rio/migrate)](https://opensource.org/license/MIT)

Database schema migrations written as Go code and compiled into your binary: no SQL files to ship, no CLI to install, no third-party dependencies.

A migration declares its changes once on a fluent schema builder. One declaration produces:

- Dialect-specific SQL for PostgreSQL, MySQL, SQLite, and single-server ClickHouse.
- Automatic rollback with no hand-written down migration.
- A reviewable dry-run plan.
- A checksum that detects migrations edited after they ran.

```go
func init() {
	migrate.Add("20260708100000_create_users", func(s *migrate.Schema) {
		s.Create("users", func(t *migrate.Table) {
			t.ID()
			t.String("email").Unique()
			t.String("name", 100)
			t.Enum("role", "admin", "member").Default("member")
			t.ForeignID("team_id").Constrained().CascadeOnDelete()
			t.Timestamps()
		})
	})
}
```

```go
db, err := sql.Open("pgx", dsn) // any database/sql driver you already use
...
m, err := migrate.New(db, migrate.Postgres)
...
if err := m.Up(ctx); err != nil {
	log.Fatal(err)
}
```

## Features

| Feature | Behavior |
|---|---|
| Migrations are code | One Go file per migration, self-registered in `init`; `go build` packages the full history into the binary. A broken migration is a compile error. |
| Automatic rollbacks | `Rollback` reverses recorded operations in reverse order (e.g. an added index drops before its column). Information-discarding operations — dropping tables/columns, raw SQL, Go functions — need an explicit `WithDown`, else fail with `ErrIrreversible`. |
| Explicit failure state | On Postgres and SQLite, the migration row is written atomically with the migration and a failure rolls everything back. MySQL names any prefix committed by implicit DDL. ClickHouse executes directly on one connection and names the prefix that may already be effective. Failed runs do not add a new migration row; there is no `force` flag. |
| Serialized execution | Postgres and MySQL use session advisory locks; SQLite uses its single writer plus record-first bookkeeping. ClickHouse deliberately fails with `ErrLockUnsupported` unless the deploy system serializes execution and the caller explicitly passes `WithoutLock`. |
| Tamper detection | Each applied migration records a checksum of its compiled SQL. On drift, `Up` warns (or fails under `WithStrictChecksum`), `Status` reports it, and `Repair` re-records after review. |
| Repeatable migrations | `AddRepeatable` re-runs views, functions, triggers, and reference data when their declaration changes (see below). |
| Reviewable | `Plan` renders pending SQL against a live database; `Collection.SQL` renders a collection offline with no database; `PlanRollback` previews a rollback. |
| Dialect types | Compiles to each engine's explicit types: `JSONB`, `TIMESTAMPTZ`, identity columns on Postgres; `DATETIME(6)`, native `ENUM` on MySQL; `DateTime64`, `JSON`, `UUID`, and numbered enums on ClickHouse. Unsupported operations fail at compile time, not silently. |
| Batch history | Each `Up` is one batch; `Rollback`, `RollbackBatch`, `Reset`, and `Baseline` manage history (see below). |
| Zero dependencies | Only `database/sql` and the standard library; pass in your own `*sql.DB` and driver. |

## Installation

Requires Go 1.27 or newer.

```bash
go get github.com/go-rio/migrate
```

> Moved from `github.com/libtnb/migrate` in v0.5.0 — part of the
> [go-rio](https://github.com/go-rio) family alongside the
> [rio](https://github.com/go-rio/rio) ORM. Releases up to v0.4.0 remain
> installable from the old path; new releases ship here only. The advisory
> lock namespace was renamed with the module, so avoid running migrations
> concurrently from pre-v0.5.0 and post-v0.5.0 binaries during a rolling
> upgrade.

### Using the rio connection

`migrate` stays independent of the ORM and owns no connection pool. Pass the
underlying `*sql.DB` from an injected `*rio.DB`:

```go
rdb, err := riopostgres.Open(dsn)
if err != nil {
	return err
}
defer rdb.Close()

m, err := migrate.New(rdb.Unwrap(), migrate.Postgres)
if err != nil {
	return err
}
return m.Up(ctx)
```

The explicit `Unwrap` matters: a migration takes a dedicated `*sql.Conn` so
its advisory lock, DDL, and bookkeeping stay on the same database session.
Postgres's native rio channel also supplies a `database/sql` view from
`Unwrap` for pool-agnostic operations such as migrations. `migrate.New` does
not take ownership of the pool and never closes it.

## Writing migrations

Conventional layout: a `migrations` package, one file per migration, imported for effect from `main`.

```
app/
├── main.go
└── migrations/
    ├── 20260708100000_create_users.go
    ├── 20260712093000_create_posts.go
    └── 20260801154500_backfill_display_names.go
```

```go
// main.go
import _ "app/migrations"
```

Names order lexically and are recorded in the database, so start them with a sortable timestamp. Registration panics on duplicate or malformed names at init time.

### Columns

```go
s.Create("articles", func(t *migrate.Table) {
	t.ID()                              // auto-incrementing 64-bit primary key
	                                    // (t.Integer("id").AutoIncrement() for other widths)
	t.String("slug", 80).Unique()       // VARCHAR(80) + unique index
	t.Text("body")                      // unbounded text
	t.Integer("views").Default(0)
	t.Decimal("rating", 3, 1).Nullable()
	t.Boolean("published").Default(false)
	t.JSON("meta").Nullable()           // JSONB / JSON / TEXT
	t.UUID("public_id").DefaultExpr("gen_random_uuid()")
	t.Enum("state", "draft", "live")    // native ENUM or CHECK constraint
	t.TimestampTz("published_at").Nullable()
	t.Timestamps()                      // created_at, updated_at
	t.SoftDeletes()                     // deleted_at
	t.Column("tags", "text[]")          // any dialect type, verbatim
})
```

Go 1.27 generic methods let declarations reuse application scalar types
without conversions:

```go
type State string

const (
	StateDraft State = "draft"
	StateLive  State = "live"
)

t.Enum("state", StateDraft, StateLive).Default(StateDraft)
```

`Default` accepts defined boolean, string, integer, unsigned integer, and
floating-point types and renders them as escaped SQL literals. A nullable
column needs no explicit `DEFAULT NULL`; use `Nullable()` without `Default`.
Database functions and other SQL expressions stay explicit through
`DefaultExpr`.

Columns are `NOT NULL` unless declared `.Nullable()`. Modifiers chain:

- Portable where supported: `.Default(v)`, `.DefaultExpr(expr)`, `.UseCurrent()`, `.Nullable()`, `.Comment(...)`.
- Relational engines: `.Unique()`, `.Index()`, `.Primary()`, `.AutoIncrement()`; ClickHouse rejects these with a concrete alternative.
- MySQL and ClickHouse integer declarations: `.Unsigned()`.
- Generated columns: `.StoredAs(expr)`, `.VirtualAs(expr)`.
- MySQL and ClickHouse alterations: `.After(...)`, `.First()`.
- MySQL only: `.UseCurrentOnUpdate()`.

Table names may be schema-qualified: `s.Create("analytics.events", ...)` renders `"analytics"."events"`, with conventional constraint names inside the schema.

CHECK constraints must be named; anonymous checks are rejected (an unnamed constraint cannot be dropped later).

```go
t.Check("orders_price_positive", "price > 0")   // in Create or Table
t.DropCheck("orders_price_positive")            // reverses an added check
```

`.AutoIncrement()` makes any integer column the database-generated primary key, in each engine's form:

| Engine | Form |
|---|---|
| Postgres | identity column, `GENERATED BY DEFAULT AS IDENTITY` (not legacy serial) |
| MySQL | `AUTO_INCREMENT` |
| SQLite | `INTEGER PRIMARY KEY AUTOINCREMENT` |
| ClickHouse | unsupported — generate a UUID or integer ID in the application |

`t.ID()` is shorthand for `t.BigInteger("id").Unsigned().AutoIncrement()`.

### ClickHouse tables

ClickHouse support targets one ClickHouse 26.0+ server and one local database
endpoint. It never emits or manages `ON CLUSTER`, Keeper, ZooKeeper, replicated
databases, `Distributed` tables, or multi-replica consistency. The deploy
system must guarantee that only one migrator runs at a time.

Without `WithoutLock`, `Up`, every rollback/reset method, `Baseline`, `Repair`,
and `Fresh` return an error matching `ErrLockUnsupported` before business DDL.
`Status`, `Plan`, rollback plans, and offline `Collection.SQL` do not require
the option.

Every ClickHouse `Schema.Create` must declare its storage engine and sorting
key explicitly. The sorting key controls physical order and the sparse primary
index; it is not a uniqueness constraint. See ClickHouse's
[`CREATE TABLE`](https://clickhouse.com/docs/reference/statements/create/table)
and [`MergeTree`](https://clickhouse.com/docs/reference/engines/table-engines/mergetree-family/mergetree)
documentation.

```go
c.Add("20260809100000_create_events", func(s *migrate.Schema) {
	s.Create("events", func(t *migrate.Table) {
		t.UUID("id")
		t.String("tenant_id")
		t.TimestampTz("occurred_at")
		t.JSON("payload")
		t.Check("events_tenant_not_empty", "tenant_id != ''")
		t.ClickHouseEngine(
			"MergeTree() " +
				"PARTITION BY toYYYYMM(occurred_at) " +
				"ORDER BY (tenant_id, occurred_at)",
		)
	})
})

m, err := migrate.New(
	db,
	migrate.ClickHouse,
	migrate.WithCollection(c),
	migrate.WithoutLock(), // only after the deploy system serialized execution
)
```

`ClickHouseEngine` is the trusted, deterministic fragment after `ENGINE =`.
It may contain engine parameters, `PARTITION BY`, `ORDER BY`, a ClickHouse
`PRIMARY KEY`, `SAMPLE BY`, TTL, and storage-level `SETTINGS`. Do not include
`ENGINE =`, a table-level `COMMENT`, or a trailing semicolon. `t.Comment` is
rendered after the complete storage fragment. Empty and duplicate declarations,
and calls from `Schema.Table`, are declaration errors. PostgreSQL, MySQL, and
SQLite ignore this ClickHouse-only setting, so a shared migration may carry
dialect-specific declarations without changing their SQL.

ClickHouse column mapping:

| Declaration | ClickHouse type |
|---|---|
| `String`, `Text`, `Binary` | `String` (`String` length is not a constraint) |
| `Char(n)` | `FixedString(n)` |
| `TinyInteger`, `SmallInteger`, `Integer`, `BigInteger` | `Int8`, `Int16`, `Int32`, `Int64` |
| Integer + `Unsigned()` | `UInt8`, `UInt16`, `UInt32`, `UInt64` |
| `Boolean` | `Bool` |
| `Decimal(p,s)` | `Decimal(p,s)` |
| `Float`, `Double` | `Float32`, `Float64` |
| `Date`, `Time` | `Date`, `Time` |
| `DateTime`, `Timestamp` | `DateTime64(6)` |
| `TimestampTz` | `DateTime64(6, 'UTC')` |
| `JSON`, `UUID` | `JSON`, `UUID` |
| `Enum` | stable positive numbering in `Enum8` up to 127 values, then `Enum16` up to 32767 |
| `Column(name, sqlType)` | the type verbatim |

`Nullable()` wraps the type in `Nullable(T)`. Defaults, `now64(6)`,
`MATERIALIZED`/`ALIAS`, column comments, table comments, and named `CHECK`
constraints compile natively. `Change()` uses `MODIFY COLUMN`; `After` and
`First` apply to both added and modified columns.

Relational uniqueness cannot be inferred safely. ClickHouse therefore rejects
`ID`/`AutoIncrement`, relational `Primary`/`Unique`, every relational index
builder or index drop/rename, foreign keys, `UseCurrentOnUpdate`, `Using`, and
primary-key alterations. Keep an unconstrained `ForeignID` as a normal
`UInt64`; put sorting and sparse-primary-key clauses in `ClickHouseEngine`;
choose a MergeTree family engine or application logic for deduplication. Use
explicit `Schema.Exec("ALTER TABLE ... ADD INDEX ... TYPE ...")` plus
`WithDown` for a data-skipping index. ClickHouse also rejects `Recreate`
because there is no transaction spanning copy, drop, and rename.

### Indexes and foreign keys

```go
t.Index("a", "b")                 // articles_a_b_index
t.Unique("slug").Name("custom")   // custom name
t.Primary("a", "b")               // composite primary key

t.ForeignID("user_id").Constrained()              // → users.id, inferred
t.ForeignID("category_id").Constrained().NullOnDelete()
t.Foreign("code").References("regions", "code")   // existing column, explicit
```

Names follow `{table}_{columns}_{index|unique|foreign}`, so dropping by columns (`t.DropIndex("a", "b")`, `t.DropForeign("user_id")`) reconstructs the created name.

Indexes take modifiers; unsupported combinations fail at compile time with advice, never silently:

```go
t.Unique("name").Where("deleted_at IS NULL")  // partial: live rows only (Postgres, SQLite)
t.Index("payload").Using("gin")               // index method (Postgres); btree/hash on MySQL
t.Unique("email").Include("name")             // covering index (Postgres 11+)
t.Unique("email").NullsNotDistinct()          // one NULL at most (Postgres 15+)
t.Index("user_id").Concurrently()             // online build (Postgres) — see below

t.IndexExpr("users_email_lower_index", "lower(email)")   // expression index, name required
t.UniqueExpr("users_email_lower_unique", "lower(email)")
t.FullText("title", "body")                   // MySQL FULLTEXT; Postgres: IndexExpr + gin
t.Spatial("location")                         // MySQL SPATIAL geometry index
```

`.Where` on a unique index is the soft-delete pattern: a deleted row releases its name while two live rows still cannot share one. `.Concurrently()` builds without blocking writes and therefore cannot run inside a transaction — declare the migration `WithoutTransaction()`, which compile enforces; rolling it back drops the index concurrently too. MySQL and SQLite build indexes online anyway and ignore the flag.

### Altering tables

```go
migrate.Add("20260801120000_polish_users", func(s *migrate.Schema) {
	s.Table("users", func(t *migrate.Table) {
		t.String("nickname", 50).Nullable().Index()
		t.RenameColumn("name", "full_name")
		t.DropColumn("legacy_flags")
	})
	s.Rename("groups", "teams")
})
```

Each change compiles to its own statement and reverses individually; the migration runs in one transaction where the engine allows. Also: `t.RenameIndex(from, to)`, `t.DropIndex`/`DropUnique`/`DropFullText`/`DropSpatial`/`DropForeign` (by columns, via conventional names), `t.DropPrimary()`, and table comments via `t.Comment(...)`.

To alter a column, restate its complete target definition and mark it `.Change()`:

```go
s.Table("users", func(t *migrate.Table) {
	t.String("name", 500).Nullable().Change()               // widen + allow NULL
	t.Integer("age").Change().Using("age::integer")         // Postgres cast for old rows
})
```

MySQL and ClickHouse compile a `Change` to `MODIFY COLUMN`; Postgres compiles `ALTER COLUMN` statements for the type (with the optional `USING` conversion), nullability and default — it is a restatement, so an omitted default drops any existing one. SQLite cannot alter columns: use `Recreate`. Index modifiers, primary keys, auto-increment and generated expressions cannot be restated. Changing a column discards its previous definition, so rolling back needs `WithDown`.

### Rebuilding tables

`Recreate` handles what `ALTER TABLE` cannot (on SQLite, any constraint change): declare the full target table and the migrator rebuilds it around the data (create temporary → copy rows → capture triggers → drop old → rename → rebuild indexes → recreate triggers), within the migration's transaction on Postgres and SQLite, so a failure leaves the original untouched. MySQL refuses to compile `Recreate` because implicit DDL commits open a crash window. ClickHouse rejects it because no multi-statement transaction can protect the copy/drop/rename sequence.

```go
s.Recreate("users", func(t *migrate.Table) {
	t.ID()
	t.String("email").Unique()                                 // the new constraint
	t.Integer("logins").Default(0).SkipCopy()                  // brand-new column
})
```

- `Recreate` requires its transaction; combining it with `WithoutTransaction` is a compile-time error.
- On Postgres, a table referenced by other tables' foreign keys or by views cannot be rebuilt; the drop is refused and the transaction rolls back cleanly. Use native `ALTER` there.

Columns copy by name. `SkipCopy` marks columns absent from the old table; `CopyFrom` substitutes a SELECT expression, renaming and retyping a column in one rebuild:

```go
t.Integer("age").CopyFrom("CAST(age AS INTEGER)")
```

Conventional constraint and index names come out for the *final* table name, so later `DropUnique`/`DropForeign` still resolve. `Recreate` discards the old definition, so rolling back needs a `WithDown` (usually another `Recreate` for the previous shape).

SQLite caveat: with `PRAGMA foreign_keys=ON` and child rows referencing the table, run on a connection with enforcement off (off by default in SQLite and most drivers).

Triggers (created via `Exec`, since the builder does not declare them) are captured and recreated verbatim after the rename, which `DROP TABLE` would otherwise remove. A trigger the new shape breaks fails the replay and rolls back; drop it with `Exec` before the `Recreate` and declare its successor after.

There is no `Change()` for redefining a column type in place; SQLite cannot without rebuilding. Use SQL — reviewable in `Plan` and checksummed:

```go
migrate.Add("20260812100000_widen_amounts",
	func(s *migrate.Schema) {
		s.Exec(`ALTER TABLE orders ALTER COLUMN amount TYPE NUMERIC(12, 2)`)
	},
	migrate.WithDown(func(s *migrate.Schema) {
		s.Exec(`ALTER TABLE orders ALTER COLUMN amount TYPE NUMERIC(8, 2)`)
	}),
)
```

### Raw SQL and data migrations

```go
migrate.Add("20260805090000_backfill",
	func(s *migrate.Schema) {
		s.Exec(`UPDATE users SET plan = 'free' WHERE plan IS NULL`)
		s.Run(func(ctx context.Context, db migrate.DB) error {
			// arbitrary Go: batched backfills, API lookups, encoding changes
			_, err := db.ExecContext(ctx, `UPDATE users SET score = score * 10`)
			return err
		})
	},
	migrate.WithDown(func(s *migrate.Schema) {
		s.Exec(`UPDATE users SET score = score / 10`)
	}),
)
```

`Run` receives the migration's transaction when present and its dedicated
`*sql.Conn` on ClickHouse or a `WithoutTransaction` migration; `migrate.DB` is
satisfied by `*sql.Tx`, `*sql.Conn`, and `*sql.DB`. Statements that cannot run
in a transaction (e.g. `CREATE INDEX CONCURRENTLY`) set
`migrate.WithoutTransaction()`:

```go
migrate.Add("20260810110000_index_events",
	func(s *migrate.Schema) {
		s.Exec(`CREATE INDEX CONCURRENTLY events_at_index ON events (at)`)
	},
	migrate.WithoutTransaction(),
	migrate.WithDown(func(s *migrate.Schema) {
		s.Exec(`DROP INDEX events_at_index`)
	}),
)
```

### Repeatable migrations

A versioned migration runs once; a repeatable migration runs whenever its compiled SQL differs from the last recorded value. Use it for views, stored functions, triggers, and reference data.

```go
migrate.AddRepeatable("active_users_view", func(s *migrate.Schema) {
	s.Exec(`CREATE OR REPLACE VIEW active_users AS
	        SELECT * FROM users WHERE deleted_at IS NULL`)
})
```

- Change the declaration and deploy; the next `Up` re-runs it, after all versioned migrations, in name order.
- Declarations must be idempotent: `CREATE OR REPLACE`, or `DROP ... IF EXISTS` + `CREATE` on SQLite (no `OR REPLACE`).
- `Status` shows a changed repeatable as drifted-pending; `Plan` renders what would re-run; rollbacks leave repeatables untouched (`Reset` forgets their records, so a fresh `Up` re-runs them).
- A `Run` body is invisible to the checksum; edit SQL, not Go, to trigger a re-run.
- Postgres refuses to roll back a versioned migration whose table a live repeatable view depends on; drop the dependent object first.

## Running

| Call | Effect |
|---|---|
| `m.Up(ctx)` | apply all pending migrations as one batch |
| `m.Rollback(ctx, 1)` | undo the most recently applied migration |
| `m.Rollback(ctx, n)` | undo the n most recent migrations |
| `m.RollbackBatch(ctx)` | undo the latest batch — everything the last `Up` applied |
| `m.Reset(ctx)` | undo everything |
| `m.Status(ctx)` | applied / pending / drifted / unregistered, per migration |
| `m.Plan(ctx)` / `m.PlanRollback(ctx, n)` / `m.PlanRollbackBatch(ctx)` | the SQL that would run, without running it |
| `m.Baseline(ctx)` | mark migrations applied without executing (existing databases) |
| `m.Repair(ctx)` | re-record versioned checksums after a reviewed change (repeatables stay due) |
| `m.Fresh(ctx)` | **development only**: drop every table, re-run everything |

Run `Up` at startup where the dialect provides its lock, or from a dedicated
deploy step. ClickHouse requires the latter to serialize execution externally:

```go
m, err := migrate.New(db, migrate.Postgres,
	migrate.WithLogger(slog.Default()),
	migrate.WithLockTimeout(2*time.Minute), // wait for a sibling deploy
)
```

Options: `WithCollection` (explicit collection instead of the global registry), `WithTable` (records table name), `WithoutLock`, `WithLockTimeout`, `WithStrictChecksum`, `WithLogger`, `WithClock`.

## Safety analysis

Some operations are safe on an empty database but dangerous on a loaded one: dropping a column live code still reads, adding a `NOT NULL` column to a populated table, building an index that blocks writes. The migrator detects these before executing anything.

| Mode | Behavior |
|---|---|
| `SafetyWarn` (default) | logs each finding through `WithLogger` and proceeds |
| `WithSafety(migrate.SafetyStrict)` | `Up` fails with `ErrUnsafe` before executing, listing every finding across the run; wire it into CI |
| `Assured()` | marks a reviewed migration so the analysis skips it |

`Plan` attaches findings to each planned migration. Creating tables never warns; raw engine fragments and `Exec`/`Run` are manual-review boundaries and are not parsed. The analysis covers declarative operations: destructive drops, backward-incompatible renames, `NOT NULL` additions without defaults, and Postgres index/foreign-key builds that lock large tables. On ClickHouse it explains that old rows read a new non-`Nullable` column's zero value, warns about type-change rewrite cost, and notes that a newly added `CHECK` does not validate historical rows.

## Transactions and engines

| | PostgreSQL | MySQL | SQLite | ClickHouse |
|---|---|---|---|---|
| Execution mode | full migration transaction | transaction protects DML; DDL commits implicitly | full migration transaction | no multi-statement transaction; direct dedicated connection |
| Built-in lock | `pg_advisory_lock`, session-level | `GET_LOCK`, session-level | single-writer transaction plus record-first bookkeeping | none; default writes fail with `ErrLockUnsupported` |
| Constraint changes | full support | full support | compile-time error with guidance | named `CHECK` add/drop only |

Each migration uses the strongest mode the dialect provides. On MySQL the
transaction still protects data statements, but a mid-migration failure may
leave earlier DDL in effect. On ClickHouse statements run strictly in order on
one `*sql.Conn`; `database/sql.Begin` is never called. The history insert is
last and forces `async_insert = 0`; repeatable updates, repairs, and rollback or
reset deletes use `mutations_sync = 1`. A duplicate history `version` is a hard
error because it proves the external serialization guarantee was violated.

ClickHouse failures name the failing statement and the successfully executed
prefix that may already be effective. No new history row is added, and no
rollback is claimed. Inspect and reconcile the actual schema and history before
retrying or baselining. Rollback/reset failures use the same rule for reverse
operations. There is intentionally no dirty-state `force` API. This deliberately
does not rely on ClickHouse's experimental
[multi-statement transactions](https://clickhouse.com/docs/concepts/features/operations/insert/transactions),
which do not cover ordinary table DDL.

Session-level advisory locks do not survive transaction-pooling proxies (PgBouncer in transaction mode). Point the migrator at the database directly or through a session-mode pool.

## Design notes

- **Declarations are data.** Nothing runs while declaring; declarations must be deterministic — derive nothing from the clock or environment.
- **Forward-first.** Rollbacks come free for structural operations; irreversibility is an explicit, typed error.
- **No CLI.** Status, plans, and baselines are method calls, not a separate tool or login wall.
- **Convention with overrides.** Inferred parent tables, conventional constraint names, and portable column types each take an explicit override (`Constrained("people")`, `.Name("...")`, `t.Column(name, "any type")`, `s.Exec("any SQL")`).

## License

go-rio/migrate is released under the [MIT License](LICENSE), © 2026-now
TreeNewBee.
