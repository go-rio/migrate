// Package migrate applies schema migrations declared with Go builders and
// compiled into the binary. One declaration compiles to PostgreSQL, MySQL,
// SQLite, or single-server ClickHouse SQL, with rollbacks, dry-run plans,
// checksums, and repeatable migrations.
//
// Entry points: Add and AddRepeatable register migrations in the default
// Collection (NewCollection and WithCollection keep an explicit one); New
// wraps a caller-owned *sql.DB with one of Postgres, MySQL, SQLite, or
// ClickHouse; Migrator.Up, Rollback, Status, and Plan drive it. Irreversible
// operations require WithDown. The package owns no connections or drivers.
package migrate
