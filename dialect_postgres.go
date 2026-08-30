package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Postgres is the PostgreSQL dialect.
var Postgres Dialect = postgresDialect{}

type postgresDialect struct{}

var pgQ = quoter{delimiter: '"'}

func (postgresDialect) name() string                     { return "postgres" }
func (postgresDialect) transactionMode() transactionMode { return transactionModeFull }

func (postgresDialect) placeholder(n int) string { return fmt.Sprintf("$%d", n) }

func (d postgresDialect) recordInsertSQL(table string) string {
	return standardRecordInsertSQL(d, table)
}

func (d postgresDialect) recordUpdateSQL(table string) string {
	return standardRecordUpdateSQL(d, table)
}

func (d postgresDialect) recordDeleteSQL(table, column string) string {
	return standardRecordDeleteSQL(d, table, column)
}

func (d postgresDialect) recordRepairSQL(table string) string {
	return standardRecordRepairSQL(d, table)
}

func (postgresDialect) ensureTableSQL(table string) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	version VARCHAR(191) PRIMARY KEY,
	batch INTEGER NOT NULL,
	checksum CHAR(64) NOT NULL,
	applied_at VARCHAR(32) NOT NULL
)`, pgQ.table(table))
}

func (d postgresDialect) compile(op operation) ([]statement, error) {
	switch o := op.(type) {
	case *createTable:
		return d.compileCreate(o.def)
	case *dropTable:
		return []statement{dropTableSQL(pgQ, o)}, nil
	case *renameTable:
		// RENAME TO takes a bare name within the schema.
		if schemaPrefix(o.from) != schemaPrefix(o.to) {
			return nil, fmt.Errorf("migrate: postgres cannot move table %q to %q with Rename; use Exec with ALTER TABLE ... SET SCHEMA", o.from, o.to)
		}
		return []statement{sqlStatement("ALTER TABLE %s RENAME TO %s", pgQ.table(o.from), pgQ.ident(baseName(o.to)))}, nil
	case *alterTable:
		return d.compileAlter(o)
	case *recreateTable:
		stmts, err := compileRecreate(d, pgQ, false, func(from, to string) statement {
			return sqlStatement("ALTER TABLE %s RENAME TO %s", pgQ.table(from), pgQ.ident(baseName(to)))
		}, d.listTriggers, o.def)
		if err != nil {
			return nil, err
		}
		return append(stmts, d.recreateEpilogue(o.def)...), nil
	case *rawSQL:
		return []statement{{sql: o.sql, args: o.args}}, nil
	case *goFunc:
		return []statement{{fn: o.fn}}, nil
	default:
		return nil, fmt.Errorf("migrate: postgres: unsupported operation %T", op)
	}
}

func (d postgresDialect) compileCreate(def *tableDef) ([]statement, error) {
	pk, err := primaryColumns(def)
	if err != nil {
		return nil, err
	}

	clauses := make([]string, 0, len(def.columns)+len(def.fks)+1)
	var comments []statement
	for _, c := range def.columns {
		clause, err := d.columnSQL(def.constraintTable(), c, def.constraintBase != "")
		if err != nil {
			return nil, err
		}
		clauses = append(clauses, clause)
		if c.comment != "" {
			comments = append(comments, d.commentSQL(def.name, c))
		}
	}
	if len(pk) > 0 {
		if def.constraintBase != "" {
			// A temporary PK must not collide with the live backing index.
			clauses = append(clauses, fmt.Sprintf("PRIMARY KEY (%s)", pgQ.idents(pk)))
		} else {
			clauses = append(clauses, fmt.Sprintf("CONSTRAINT %s PRIMARY KEY (%s)",
				pgQ.ident(primaryName(def.name)), pgQ.idents(pk)))
		}
	}
	for _, chk := range def.checks {
		clauses = append(clauses, checkClause(pgQ, chk))
	}
	for _, fk := range def.fks {
		clauses = append(clauses, foreignClause(pgQ, def.constraintTable(), fk))
	}

	stmts := []statement{sqlStatement("CREATE TABLE %s (\n\t%s\n)",
		pgQ.table(def.name), strings.Join(clauses, ",\n\t"))}
	for _, idx := range append(inlineIndexes(def.columns), def.indexes...) {
		sql, err := createIndexSQL("postgres", pgQ, def.name, idx, false)
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, statement{sql: sql})
	}
	if def.comment != "" {
		stmts = append(stmts, d.tableCommentSQL(def.name, def.comment))
	}
	return append(stmts, comments...), nil
}

func (d postgresDialect) compileAlter(op *alterTable) ([]statement, error) {
	table := pgQ.table(op.table)
	var stmts []statement
	for _, ch := range op.changes {
		switch c := ch.(type) {
		case *addColumn:
			if c.col.change {
				changed, err := d.changeColumnSQL(op.table, c.col)
				if err != nil {
					return nil, err
				}
				stmts = append(stmts, changed...)
				continue
			}
			clause, err := d.columnSQL(op.table, c.col, false)
			if err != nil {
				return nil, err
			}
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD COLUMN %s", table, clause))
			for _, idx := range inlineIndexes([]*columnDef{c.col}) {
				sql, err := createIndexSQL("postgres", pgQ, op.table, idx, false)
				if err != nil {
					return nil, err
				}
				stmts = append(stmts, statement{sql: sql})
			}
			if c.col.comment != "" {
				stmts = append(stmts, d.commentSQL(op.table, c.col))
			}
		case *dropColumn:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP COLUMN %s", table, pgQ.ident(c.name)))
		case *renameColumn:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s RENAME COLUMN %s TO %s", table, pgQ.ident(c.from), pgQ.ident(c.to)))
		case *addIndex:
			sql, err := createIndexSQL("postgres", pgQ, op.table, c.idx, false)
			if err != nil {
				return nil, err
			}
			stmts = append(stmts, statement{sql: sql})
		case *dropIndex:
			// Indexes live in the table's schema.
			concurrently := ""
			if c.concurrently {
				concurrently = "CONCURRENTLY "
			}
			stmts = append(stmts, sqlStatement("DROP INDEX %s%s", concurrently, pgQ.table(schemaPrefix(op.table)+c.name)))
		case *addForeign:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD %s", table, foreignClause(pgQ, op.table, c.fk)))
		case *dropForeign:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP CONSTRAINT %s", table, pgQ.ident(c.name)))
		case *addPrimary:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD CONSTRAINT %s PRIMARY KEY (%s)",
				table, pgQ.ident(primaryName(op.table)), pgQ.idents(c.columns)))
		case *dropPrimary:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP CONSTRAINT %s", table, pgQ.ident(primaryName(op.table))))
		case *renameIndex:
			// The new index name stays bare within the schema.
			stmts = append(stmts, sqlStatement("ALTER INDEX %s RENAME TO %s",
				pgQ.table(schemaPrefix(op.table)+c.from), pgQ.ident(c.to)))
		case *setTableComment:
			stmts = append(stmts, d.tableCommentSQL(op.table, c.comment))
		case *addCheck:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD %s", table, checkClause(pgQ, c.chk)))
		case *dropCheck:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP CONSTRAINT %s", table, pgQ.ident(c.name)))
		default:
			return nil, fmt.Errorf("migrate: postgres: unsupported change %T", ch)
		}
	}
	return stmts, nil
}

// changeColumnSQL emits separate PostgreSQL type, nullability, and default
// alterations. An omitted default is dropped.
func (d postgresDialect) changeColumnSQL(table string, c *columnDef) ([]statement, error) {
	if c.kind == kindEnum {
		return nil, fmt.Errorf("migrate: postgres emulates enums with a check constraint; change column %q of table %q with DropCheck/Check and a plain type instead", c.name, table)
	}
	typ, err := d.typeSQL(c)
	if err != nil {
		return nil, err
	}
	qt, qc := pgQ.table(table), pgQ.ident(c.name)

	using := ""
	if c.changeUsing != "" {
		using = " USING " + c.changeUsing
	}
	stmts := []statement{sqlStatement("ALTER TABLE %s ALTER COLUMN %s TYPE %s%s", qt, qc, typ, using)}

	null := "SET NOT NULL"
	if c.nullable {
		null = "DROP NOT NULL"
	}
	stmts = append(stmts, sqlStatement("ALTER TABLE %s ALTER COLUMN %s %s", qt, qc, null))

	value, err := defaultValueSQL(c, false, "CURRENT_TIMESTAMP")
	if err != nil {
		return nil, err
	}
	if value != "" {
		stmts = append(stmts, sqlStatement("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s", qt, qc, value))
	} else {
		stmts = append(stmts, sqlStatement("ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT", qt, qc))
	}

	if c.comment != "" {
		stmts = append(stmts, d.commentSQL(table, c))
	}
	return stmts, nil
}

func (d postgresDialect) columnSQL(table string, c *columnDef, nameNotNull bool) (string, error) {
	typ, err := d.typeSQL(c)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(pgQ.ident(c.name) + " " + typ)
	if c.autoIncr {
		// Use SQL-standard identity rather than serial.
		b.WriteString(" GENERATED BY DEFAULT AS IDENTITY")
	}
	if c.inlinePrimary() {
		if nameNotNull {
			b.WriteString(" CONSTRAINT " + pgQ.ident(baseName(table)+"_"+c.name+"_not_null") + " NOT NULL")
		}
		// Keep the conventional {table}_pkey constraint name.
		b.WriteString(" PRIMARY KEY")
		return b.String(), nil
	}
	b.WriteString(generatedClause(c))
	if !c.nullable {
		if nameNotNull {
			b.WriteString(" CONSTRAINT " + pgQ.ident(baseName(table)+"_"+c.name+"_not_null") + " NOT NULL")
		} else {
			b.WriteString(" NOT NULL")
		}
	}
	def, err := defaultClause(c, false, "CURRENT_TIMESTAMP")
	if err != nil {
		return "", err
	}
	b.WriteString(def)
	if c.kind == kindEnum {
		b.WriteString(enumCheckSQL(pgQ, table, c))
	}
	return b.String(), nil
}

func (postgresDialect) typeSQL(c *columnDef) (string, error) {
	switch c.kind {
	case kindRaw:
		return c.rawType, nil
	case kindString, kindEnum:
		return fmt.Sprintf("VARCHAR(%d)", charLength(c.length)), nil
	case kindChar:
		return fmt.Sprintf("CHAR(%d)", charLength(c.length)), nil
	case kindText:
		return "TEXT", nil
	case kindTinyInt, kindSmallInt:
		return "SMALLINT", nil
	case kindInt:
		return "INTEGER", nil
	case kindBigInt:
		return "BIGINT", nil
	case kindBool:
		return "BOOLEAN", nil
	case kindDecimal:
		return fmt.Sprintf("NUMERIC(%d, %d)", c.precision, c.scale), nil
	case kindFloat:
		return "REAL", nil
	case kindDouble:
		return "DOUBLE PRECISION", nil
	case kindDate:
		return "DATE", nil
	case kindTime:
		return "TIME", nil
	case kindDateTime, kindTimestamp:
		return "TIMESTAMP", nil
	case kindTimestampTz:
		return "TIMESTAMPTZ", nil
	case kindJSON:
		return "JSONB", nil
	case kindUUID:
		return "UUID", nil
	case kindBinary:
		return "BYTEA", nil
	default:
		return "", fmt.Errorf("migrate: postgres: unsupported column kind for %q", c.name)
	}
}

func (postgresDialect) commentSQL(table string, c *columnDef) statement {
	comment := strings.ReplaceAll(c.comment, "'", "''")
	return sqlStatement("COMMENT ON COLUMN %s.%s IS '%s'", pgQ.table(table), pgQ.ident(c.name), comment)
}

// listTriggers excludes internal constraint triggers, which rebuild themselves.
func (postgresDialect) listTriggers(ctx context.Context, db DB, table string) ([]string, error) {
	return queryStrings(ctx, db,
		"SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgrelid = $1::regclass AND NOT tgisinternal ORDER BY tgname",
		pgQ.table(table))
}

// recreateEpilogue restores the final PK name and advances identity sequences.
func (d postgresDialect) recreateEpilogue(def *tableDef) []statement {
	var stmts []statement
	pk, err := primaryColumns(def)
	hasInline := false
	for _, c := range def.columns {
		if c.inlinePrimary() {
			hasInline = true
		}
	}
	if err == nil && (len(pk) > 0 || hasInline) {
		tmpPkey := baseName(def.name) + "__migrate_new_pkey"
		stmts = append(stmts, sqlStatement("ALTER TABLE %s RENAME CONSTRAINT %s TO %s",
			pgQ.table(def.name), pgQ.ident(tmpPkey), pgQ.ident(primaryName(def.name))))
	}
	for _, c := range def.columns {
		if !c.inlinePrimary() || c.skipCopy {
			continue
		}
		// Copying explicit IDs does not advance the identity sequence.
		table := strings.ReplaceAll(pgQ.table(def.name), "'", "''")
		column := strings.ReplaceAll(c.name, "'", "''")
		stmts = append(stmts, sqlStatement(
			"SELECT setval(pg_get_serial_sequence('%s', '%s'), COALESCE((SELECT MAX(%s) FROM %s), 0) + 1, false)",
			table, column, pgQ.ident(c.name), pgQ.table(def.name)))
	}
	return stmts
}

// The session lock is scoped by database and records table. It dies with the
// connection and does not block CREATE INDEX CONCURRENTLY through a snapshot.
const pgLockKey = "hashtextextended('go-rio/migrate:' || current_database() || ':' || $1, 0)"

func (postgresDialect) lock(
	ctx context.Context,
	conn *sql.Conn,
	table string,
	timeout time.Duration,
) (lockToken, error) {
	deadline := time.Now().Add(timeout)
	for {
		var acquired bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock("+pgLockKey+")", table).Scan(&acquired); err != nil {
			return "", fmt.Errorf("migrate: acquire advisory lock: %w", err)
		}
		if acquired {
			return lockToken(table), nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%w: waited %v for the advisory lock", ErrLockTimeout, timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (postgresDialect) unlock(ctx context.Context, conn *sql.Conn, token lockToken) error {
	var released bool
	row := conn.QueryRowContext(ctx, "SELECT pg_advisory_unlock("+pgLockKey+")", string(token))
	if err := row.Scan(&released); err != nil {
		return fmt.Errorf("migrate: release advisory lock: %w", err)
	}
	if !released {
		return fmt.Errorf("migrate: advisory lock was not held at release time")
	}
	return nil
}

func dropTableSQL(q quoter, o *dropTable) statement {
	ifExists := ""
	if o.ifExists {
		ifExists = "IF EXISTS "
	}
	return sqlStatement("DROP TABLE %s%s", ifExists, q.table(o.name))
}

func (postgresDialect) quoteIdent(name string) string { return pgQ.table(name) }

func (postgresDialect) tableCommentSQL(table, comment string) statement {
	return sqlStatement("COMMENT ON TABLE %s IS '%s'", pgQ.table(table), strings.ReplaceAll(comment, "'", "''"))
}

func (postgresDialect) listTablesSQL() string {
	return "SELECT tablename FROM pg_tables WHERE schemaname = current_schema()"
}

// CASCADE removes dependent objects such as views.
func (postgresDialect) freshDropSQL(table string) string {
	return fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", pgQ.table(table))
}
