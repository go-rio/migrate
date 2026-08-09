// Package migrate applies schema migrations declared with Go builders.
// Declarations compile to PostgreSQL, MySQL, SQLite, or single-server
// ClickHouse SQL and support rollbacks, dry-run plans, checksums, and
// repeatable migrations.
//
// Irreversible operations require WithDown. The package owns no connections
// or drivers; New accepts a caller-owned *sql.DB.
package migrate
