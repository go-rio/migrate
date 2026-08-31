package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// DB is the database/sql surface available to Run functions.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var (
	_ DB = (*sql.Tx)(nil)
	_ DB = (*sql.DB)(nil)
	_ DB = (*sql.Conn)(nil)
)

// Schema records migration operations without touching the database.
// Declarations must be deterministic because they are re-run for planning,
// checksums, application, and rollback.
type Schema struct {
	ops  []operation
	errs []error
}

func (s *Schema) record(op operation) {
	s.ops = append(s.ops, op)
}

func (s *Schema) errf(format string, a ...any) {
	s.errs = append(s.errs, fmt.Errorf(format, a...))
}

func (s *Schema) requireTable(method, table string) {
	if table == "" {
		s.errf("%s declares an empty table name", method)
	}
}

// Create declares a new table; rollback drops it.
func (s *Schema) Create(table string, fn func(*Table)) {
	s.requireTable("Create", table)
	def := &tableDef{name: table}
	t := &Table{table: table, create: def, allowClickHouseEngine: true}
	if fn != nil {
		fn(t)
	}
	if len(def.columns) == 0 {
		s.errf("Create(%q) declares no columns", table)
	}
	s.record(&createTable{def: def})
}

// Table declares reversible alterations to an existing table.
func (s *Schema) Table(table string, fn func(*Table)) {
	s.requireTable("Table", table)
	alter := &alterTable{table: table}
	t := &Table{table: table, alter: alter}
	if fn != nil {
		fn(t)
	}
	if len(alter.changes) == 0 {
		s.errf("Table(%q) declares no changes", table)
	}
	s.record(alter)
}

// Recreate rebuilds a table while preserving rows and triggers. Columns copy
// by name unless CopyFrom or SkipCopy says otherwise. PostgreSQL and SQLite
// run the rebuild transactionally; MySQL and ClickHouse reject it.
//
// Recreate is irreversible without WithDown. PostgreSQL also rejects rebuilds
// blocked by dependent foreign keys or views.
func (s *Schema) Recreate(table string, fn func(*Table)) {
	s.requireTable("Recreate", table)
	def := &tableDef{name: table}
	t := &Table{table: table, create: def}
	if fn != nil {
		fn(t)
	}
	if len(def.columns) == 0 {
		s.errf("Recreate(%q) declares no columns", table)
	}
	s.record(&recreateTable{def: def})
}

// Rename renames a table and reverses automatically. PostgreSQL and SQLite
// reject cross-schema renames.
func (s *Schema) Rename(from, to string) {
	s.requireTable("Rename", from)
	s.requireTable("Rename", to)
	s.record(&renameTable{from: from, to: to})
}

// Drop removes a table and is irreversible without WithDown.
func (s *Schema) Drop(table string) {
	s.requireTable("Drop", table)
	s.record(&dropTable{name: table})
}

// DropIfExists removes a table if it exists. Like Drop, it is irreversible.
func (s *Schema) DropIfExists(table string) {
	s.requireTable("DropIfExists", table)
	s.record(&dropTable{name: table, ifExists: true})
}

// CreatePartition creates a child of a partitioned parent; rollback drops
// it. PostgreSQL only.
func (s *Schema) CreatePartition(child, parent string, bound PartitionBound) {
	s.requireTable("CreatePartition", child)
	s.requireTable("CreatePartition", parent)
	if bound.kind == "" {
		s.errf("CreatePartition(%q) needs a bound; use CreateDefaultPartition for the default partition", child)
	}
	s.record(&createPartition{child: child, parent: parent, bound: &bound})
}

// CreateDefaultPartition creates the DEFAULT partition; rollback drops it.
func (s *Schema) CreateDefaultPartition(child, parent string) {
	s.requireTable("CreateDefaultPartition", child)
	s.requireTable("CreateDefaultPartition", parent)
	s.record(&createPartition{child: child, parent: parent})
}

// AttachPartition attaches an existing table as a partition; rollback
// detaches it.
func (s *Schema) AttachPartition(parent, child string, bound PartitionBound) {
	s.requireTable("AttachPartition", parent)
	s.requireTable("AttachPartition", child)
	if bound.kind == "" {
		s.errf("AttachPartition(%q) needs a bound", child)
	}
	s.record(&attachPartition{parent: parent, child: child, bound: &bound})
}

// DetachPartition detaches a partition into a standalone table; the bound
// is discarded, so it is irreversible without WithDown.
func (s *Schema) DetachPartition(parent, child string) {
	s.requireTable("DetachPartition", parent)
	s.requireTable("DetachPartition", child)
	s.record(&detachPartition{parent: parent, child: child})
}

// Exec records raw SQL in plans and checksums. It is irreversible without
// WithDown and uses native dialect placeholders.
func (s *Schema) Exec(query string, args ...any) {
	if strings.TrimSpace(query) == "" {
		s.errf("Exec declares an empty SQL statement")
	}
	s.record(&rawSQL{sql: query, args: args})
}

// Run records an opaque Go data migration. It runs in the migration
// transaction when the dialect provides one, or on the dedicated connection
// otherwise. It is excluded from checksums and is irreversible.
//
// Keep DDL out of fn: rio cannot see inside it, so on MySQL DDL would commit
// implicitly without appearing in failure reports. Use the builders or Exec.
func (s *Schema) Run(fn func(ctx context.Context, db DB) error) {
	if fn == nil {
		s.errf("Run declares a nil function")
		return
	}
	s.record(&goFunc{fn: fn})
}
