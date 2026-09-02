# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.13.0] - 2026-09-02

### Added

- `Index.Desc(columns...)` indexes the named columns descending on PostgreSQL,
  MySQL (8.0+), and SQLite: the key shape a mixed-direction `ORDER BY`, such
  as rio's keyset paging, needs. A column outside the index or an expression
  index fails at compile time.

## [0.12.1] - 2026-09-02

### Added

- `CONTRIBUTING.md`, `CHANGELOG.md`, `llms.txt`, and compile-only examples
  for `New`, `Migrator.Up`, and `Add`.

### Changed

- README restructured: summary, getting started, then one specification
  section holding the operation catalog, dialect notes, migration file
  conventions, and every warning.
- Doc comments on exported identifiers state constraints and error cases;
  the package comment names the entry points.

## [0.12.0] - 2026-08-31

### Added

- `Table.UniqueConstraint` declares a named table-level `UNIQUE` constraint,
  the form `ON CONFLICT ON CONSTRAINT` can reference (`Unique` stays a
  unique index). Inline in `Create`, `ALTER TABLE ADD CONSTRAINT` in `Table`
  (MySQL `DROP CONSTRAINT` needs 8.0.19+); SQLite alters through `Recreate`,
  which carries the constraint through the rebuild under its original name;
  ClickHouse rejects it and points at `ReplacingMergeTree`.
- `Table.DropConstraint` drops any named table constraint; it is
  irreversible without `WithDown`.

### Fixed

- A table-level `Primary` may include the auto-incrementing column on
  PostgreSQL and MySQL: the column renders as a plain identity and the
  composite constraint takes over. SQLite still rejects the combination
  because the rowid alias must be the sole primary key.

## [0.11.0] - 2026-08-31

### Changed

- Doc comments and the README state contracts only; the README follows the
  project-standard shape.

### Fixed

- `IndexExpr` and `UniqueExpr` keys render verbatim on PostgreSQL and SQLite
  instead of being wrapped in parentheses, so operator classes, `COLLATE`,
  and `DESC` survive; parenthesize PostgreSQL arithmetic yourself. MySQL
  keeps the mandatory per-part parentheses.

## [0.10.0] - 2026-08-30

### Changed

- Checksums use a type-tagged, length-prefixed encoding: pointer arguments
  hash by value, `1` and `"1"` differ, statement boundaries cannot fold into
  argument bytes, and argument types the encoding cannot pin down fail
  instead of hashing lossily. Records written by earlier versions report
  drift once; run `Repair` after reviewing `Status`.
- Failure notes never understate commits: a `WithoutTransaction` migration
  on a transactional dialect names its committed prefix, and a failed MySQL
  migration that ran a Go function no longer claims the database is
  unchanged. `Schema.Run` documents that DDL belongs in the builders.

### Fixed

- `Rollback` and `Reset` on SQLite use the same record-first arbitration as
  `Up`: the record `DELETE` runs first inside the transaction, and zero
  affected rows means another migrator already rolled the migration back,
  so its down statements never run twice.

## [0.9.1] - 2026-08-20

### Changed

- The `go` directive is `1.27.0` now that Go 1.27 has shipped.
- MySQL advisory-lock names are computed client-side (SHA-256 of the
  database and records table) and passed to `GET_LOCK` and `RELEASE_LOCK`
  as plain strings.
- PostgreSQL `Recreate` names the rebuilt table's `NOT NULL` constraints
  after the final table name.

## [0.9.0] - 2026-08-09

### Added

- ClickHouse dialect (`migrate.ClickHouse`) for a single server. Every
  `Create` declares its storage fragment with `Table.ClickHouseEngine`;
  `Nullable`, defaults, `now64(6)`, `MATERIALIZED`/`ALIAS` generated
  columns, comments, named `CHECK` constraints, `Enum8`/`Enum16`,
  `Unsigned`, and `After`/`First` compile natively. `ID`/`AutoIncrement`,
  `Primary`, `Unique`, indexes, foreign keys, `UseCurrentOnUpdate`, `Using`,
  and `Recreate` fail at compile time with the alternative named.
- `ErrLockUnsupported`: ClickHouse has no in-database migration lock, so
  writing methods fail until the deploy system serializes runs and
  `WithoutLock` is set.
- Failures on a dialect without a migration transaction name the failed
  statement and the possibly-effective prefix.

## [0.8.0] - 2026-08-09

### Changed

- **Breaking:** `Column.Default` takes a type parameter constrained by
  `DefaultLiteral`, so a default is checked against the scalar kinds a
  column can hold instead of being accepted as `any` and rejected at render
  time.
- **Breaking:** `Table.Enum` takes a `~string` type parameter, so defined
  string types pass through without a conversion at every call site.
- **Breaking:** Go 1.27 is required.

## [0.7.0] - 2026-07-17

### Added

- Index modifiers: `Where` (partial), `Using` (method), `Include`
  (covering), `NullsNotDistinct`, and `Concurrently` (online build on
  PostgreSQL; the migration must declare `WithoutTransaction`, and the
  rollback drops concurrently too).
- `Table.IndexExpr` and `Table.UniqueExpr` expression indexes with an
  explicit name; `Table.FullText` and `Table.Spatial` with conventional
  names and `DropFullText`/`DropSpatial` counterparts.
- `Column.Change` restates a column: `MODIFY COLUMN` on MySQL, per-clause
  `ALTER COLUMN` with optional `Using` on PostgreSQL, refused on SQLite in
  favor of `Recreate`; irreversible without `WithDown`.

### Changed

- Unsupported dialect and index-modifier combinations fail compilation with
  advice instead of silently weakening the index.
- Safety analysis warns on `Change` and recommends `Concurrently` on
  PostgreSQL.

## [0.6.1] - 2026-07-11

### Changed

- README rewritten in reference style; badges normalized.
- The integration suite drops the tables it creates up front, so two
  consecutive runs against the same database pass.

## [0.6.0] - 2026-07-10

### Fixed

- `Recreate` captures a table's triggers before `DROP TABLE` and replays
  them after the rename on SQLite and PostgreSQL; a replay failure rolls the
  migration back. Recreate migrations compiled by earlier versions report
  checksum drift once; `Repair` accepts it.
- `Fresh` drops a schema-qualified records table too and verifies the
  recreated table starts empty instead of reporting a wiped database as
  fully migrated.
- The MySQL implicit-commit note names the exact committed prefix and only
  appears when the executed prefix contains DDL; a pure-DML failure reports
  the clean rollback it got.
- Adding a `NOT NULL` generated column no longer triggers the safety finding
  that demands a default.
- Two migrators running `Up` concurrently on SQLite no longer race into
  driver errors mid-schema: the bookkeeping `INSERT` runs first inside the
  transaction, so the loser hits the records table's primary key and rolls
  back before touching any DDL.

## [0.5.0] - 2026-07-09

### Changed

- **Breaking:** the module moved to `github.com/go-rio/migrate`. The
  advisory-lock namespaces followed the rename, so binaries built before and
  after it do not exclude each other when migrating the same database
  concurrently.

## [0.4.0] - 2026-07-08

### Changed

- **Breaking:** `Rollback(ctx, n)` takes an explicit positive step count;
  the batch-wide rollback is `RollbackBatch(ctx)`. `PlanRollback(ctx, n)`
  and `PlanRollbackBatch(ctx)` follow the same split. `migrate.Steps` and
  `migrate.RollbackOption` are removed.

## [0.3.3] - 2026-07-08

### Changed

- The records-table name limit rises to 128 bytes to fit schema-qualified
  names.

### Fixed

- `Baseline` rejects more than one target and a repeatable migration as its
  bound.
- Declaring the same column twice in `Create` or `Recreate` is a declaration
  error instead of an engine-specific runtime failure.

## [0.3.2] - 2026-07-08

### Fixed

- A non-positive step count no longer behaves like `Reset` and roll back
  baselined rows; it is rejected before anything loads.
- `Recreate` combined with `WithoutTransaction` is a compile-time error
  instead of running copy, drop, and rename statement by statement.
- The docs and the safety finding say that PostgreSQL refuses to rebuild a
  table referenced by foreign keys or views.

## [0.3.1] - 2026-07-08

### Changed

- `Recreate` fails at compile time on MySQL: implicit DDL commits leave no
  transaction around the copy, drop, and rename sequence. Use `Schema.Table`
  or `Exec` there.

## [0.3.0] - 2026-07-08

### Added

- Schema-qualified table names, including the records table.
- `StoredAs` and `VirtualAs` generated columns.
- `Check(name, expr)` and `DropCheck(name)` named `CHECK` constraints;
  anonymous checks are rejected.
- `CopyFrom(expr)` substitutes a `SELECT` expression in `Recreate`'s row
  copy.

### Fixed

- `Rollback` no longer selects batch-0 baseline rows once they become the
  highest remaining batch; only `Reset` reverses them.
- `Recreate` on PostgreSQL no longer collides with the live table's
  primary-key backing index and advances the identity sequence past the
  copied rows.
- `Repair` skips repeatable records, so it cannot cancel a pending re-run.
- SQLite `ADD COLUMN` with a `STORED` generated column, `UseCurrent`, or
  `DefaultExpr` fails at compile time with the alternative named;
  `AUTOINCREMENT` on a raw-typed column requires `INTEGER`.
- `Primary()` combined with `Nullable()` is a declaration error.
- Cross-schema `Rename` is refused on PostgreSQL and SQLite; `RenameIndex`
  qualifies the source index with the table's schema.
- MySQL `GET_LOCK` timeouts round up to whole seconds; the deferred advisory
  unlock runs under a bounded context.

## [0.2.0] - 2026-07-08

### Added

- `AddRepeatable`: migrations that re-run whenever their compiled SQL
  changes, always after versioned migrations; rollbacks skip them and
  `Reset` forgets their records.
- Safety analysis: `SafetyWarn` logs findings, `SafetyStrict` refuses the
  whole run before executing anything, `Assured()` marks a reviewed
  migration, and `Plan` attaches findings to each entry.
- `Schema.Recreate` rebuilds a table around its rows for what `ALTER TABLE`
  cannot do; `SkipCopy` marks columns the old table lacks.
- `Fresh` drops every table and re-runs all migrations.

### Changed

- SQLite constraint-change errors point at `Recreate`.

## [0.1.0] - 2026-07-08

### Added

- Initial release: migrations declared with Go builders and compiled into
  the binary for PostgreSQL, MySQL, and SQLite, with automatic rollbacks,
  offline dry-run plans, checksums, session advisory locks, batch rollbacks,
  `Baseline`, `Repair`, and no third-party dependencies.

[Unreleased]: https://github.com/go-rio/migrate/compare/v0.13.0...HEAD
[0.13.0]: https://github.com/go-rio/migrate/compare/v0.12.1...v0.13.0
[0.12.1]: https://github.com/go-rio/migrate/compare/v0.12.0...v0.12.1
[0.12.0]: https://github.com/go-rio/migrate/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/go-rio/migrate/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/go-rio/migrate/compare/v0.9.1...v0.10.0
[0.9.1]: https://github.com/go-rio/migrate/compare/v0.9.0...v0.9.1
[0.9.0]: https://github.com/go-rio/migrate/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/go-rio/migrate/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/go-rio/migrate/compare/v0.6.1...v0.7.0
[0.6.1]: https://github.com/go-rio/migrate/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/go-rio/migrate/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/go-rio/migrate/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/go-rio/migrate/compare/v0.3.3...v0.4.0
[0.3.3]: https://github.com/go-rio/migrate/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/go-rio/migrate/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/go-rio/migrate/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/go-rio/migrate/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/go-rio/migrate/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/go-rio/migrate/releases/tag/v0.1.0
