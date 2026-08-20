package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ClickHouse is the single-server ClickHouse dialect. It never emits
// ON CLUSTER and does not provide an in-database migration lock.
var ClickHouse Dialect = clickHouseDialect{}

type clickHouseDialect struct{}

var clickHouseQ = quoter{delimiter: '`', escapeBackslash: true}

func (clickHouseDialect) name() string                     { return "clickhouse" }
func (clickHouseDialect) transactionMode() transactionMode { return transactionModeNone }
func (clickHouseDialect) placeholder(int) string           { return "?" }

func (clickHouseDialect) ensureTableSQL(table string) string {
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n"+
		"\tversion String,\n"+
		"\tbatch Int32,\n"+
		"\tchecksum String,\n"+
		"\tapplied_at String\n"+
		") ENGINE = MergeTree() ORDER BY version", clickHouseQ.table(table))
}

func (d clickHouseDialect) recordInsertSQL(table string) string {
	return fmt.Sprintf("INSERT INTO %s (version, batch, checksum, applied_at) SETTINGS async_insert = 0 VALUES (?, ?, ?, ?)",
		d.quoteIdent(table))
}

func (d clickHouseDialect) recordUpdateSQL(table string) string {
	return fmt.Sprintf("ALTER TABLE %s UPDATE checksum = ?, applied_at = ? WHERE version = ? SETTINGS mutations_sync = 1",
		d.quoteIdent(table))
}

func (d clickHouseDialect) recordDeleteSQL(table, column string) string {
	return fmt.Sprintf("ALTER TABLE %s DELETE WHERE %s = ? SETTINGS mutations_sync = 1",
		d.quoteIdent(table), clickHouseQ.ident(column))
}

func (d clickHouseDialect) recordRepairSQL(table string) string {
	return fmt.Sprintf("ALTER TABLE %s UPDATE checksum = ? WHERE version = ? SETTINGS mutations_sync = 1",
		d.quoteIdent(table))
}

func (d clickHouseDialect) compile(op operation) ([]statement, error) {
	switch o := op.(type) {
	case *createTable:
		return d.compileCreate(o.def)
	case *dropTable:
		return []statement{dropTableSQL(clickHouseQ, o)}, nil
	case *renameTable:
		return []statement{sqlStatement("RENAME TABLE %s TO %s",
			clickHouseQ.table(o.from), clickHouseQ.table(o.to))}, nil
	case *alterTable:
		return d.compileAlter(o)
	case *recreateTable:
		return nil, fmt.Errorf("migrate: clickhouse cannot recreate table %q atomically; use native ALTER operations or explicit Exec steps with a reviewed recovery procedure", o.def.name)
	case *rawSQL:
		return []statement{{sql: o.sql, args: o.args}}, nil
	case *goFunc:
		return []statement{{fn: o.fn}}, nil
	default:
		return nil, fmt.Errorf("migrate: clickhouse: unsupported operation %T", op)
	}
}

func (d clickHouseDialect) compileCreate(def *tableDef) ([]statement, error) {
	if !def.clickHouseEngineSet {
		return nil, fmt.Errorf("migrate: clickhouse table %q requires ClickHouseEngine with an explicit engine and sorting key, for example ClickHouseEngine(\"MergeTree() ORDER BY (tenant_id, occurred_at)\")", def.name)
	}
	if len(def.primary) > 0 {
		return nil, clickHousePrimaryError(def.name)
	}
	if len(def.indexes) > 0 {
		return nil, clickHouseIndexError(def.name)
	}
	if len(def.fks) > 0 {
		return nil, clickHouseForeignError(def.name)
	}

	clauses := make([]string, 0, len(def.columns)+len(def.checks))
	for _, c := range def.columns {
		clause, err := d.columnSQL(def.name, c, false)
		if err != nil {
			return nil, err
		}
		clauses = append(clauses, clause)
	}
	for _, chk := range def.checks {
		clauses = append(clauses, checkClause(clickHouseQ, chk))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (\n\t%s\n) ENGINE = %s",
		clickHouseQ.table(def.name), strings.Join(clauses, ",\n\t"), def.clickHouseEngine)
	if def.comment != "" {
		b.WriteString(" COMMENT '")
		b.WriteString(clickHouseEscape(def.comment))
		b.WriteByte('\'')
	}
	return []statement{{sql: b.String()}}, nil
}

func (d clickHouseDialect) compileAlter(op *alterTable) ([]statement, error) {
	table := clickHouseQ.table(op.table)
	stmts := make([]statement, 0, len(op.changes))
	for _, ch := range op.changes {
		switch c := ch.(type) {
		case *addColumn:
			clause, err := d.columnSQL(op.table, c.col, true)
			if err != nil {
				return nil, err
			}
			verb := "ADD"
			if c.col.change {
				verb = "MODIFY"
			}
			stmts = append(stmts, sqlStatement("ALTER TABLE %s %s COLUMN %s", table, verb, clause))
		case *dropColumn:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP COLUMN %s", table, clickHouseQ.ident(c.name)))
		case *renameColumn:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s RENAME COLUMN %s TO %s",
				table, clickHouseQ.ident(c.from), clickHouseQ.ident(c.to)))
		case *addCheck:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD %s", table, checkClause(clickHouseQ, c.chk)))
		case *dropCheck:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP CONSTRAINT %s", table, clickHouseQ.ident(c.name)))
		case *setTableComment:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s MODIFY COMMENT '%s'", table, clickHouseEscape(c.comment)))
		case *addIndex, *dropIndex, *renameIndex:
			return nil, clickHouseIndexError(op.table)
		case *addForeign, *dropForeign:
			return nil, clickHouseForeignError(op.table)
		case *addPrimary, *dropPrimary:
			return nil, clickHousePrimaryError(op.table)
		default:
			return nil, fmt.Errorf("migrate: clickhouse: unsupported change %T", ch)
		}
	}
	return stmts, nil
}

func (d clickHouseDialect) columnSQL(table string, c *columnDef, altering bool) (string, error) {
	switch {
	case c.autoIncr:
		return "", fmt.Errorf("migrate: clickhouse has no traditional auto-increment column for %q of table %q; generate a UUID or integer id in the application", c.name, table)
	case c.primary:
		return "", clickHousePrimaryError(table)
	case c.unique:
		return "", fmt.Errorf("migrate: clickhouse cannot preserve relational uniqueness for column %q of table %q; choose an appropriate MergeTree engine in ClickHouseEngine or enforce uniqueness in the application", c.name, table)
	case c.indexed:
		return "", clickHouseIndexError(table)
	case c.useCurrentOnUpdate:
		return "", fmt.Errorf("migrate: clickhouse does not support UseCurrentOnUpdate on column %q of table %q; update the value explicitly in writes or use a MATERIALIZED/ALIAS expression when appropriate", c.name, table)
	case c.changeUsing != "":
		return "", fmt.Errorf("migrate: Using is a PostgreSQL conversion expression; clickhouse converts modified column %q of table %q according to ALTER TABLE MODIFY COLUMN semantics", c.name, table)
	case c.unsigned && !c.integerKind():
		return "", fmt.Errorf("migrate: clickhouse Unsigned is only valid on integer declarations (column %q of table %q); use an explicit UInt type with Column for database-specific types", c.name, table)
	}

	typ, err := d.typeSQL(c)
	if err != nil {
		return "", err
	}
	if c.nullable {
		typ = "Nullable(" + typ + ")"
	}

	var b strings.Builder
	b.WriteString(clickHouseQ.ident(c.name) + " " + typ)
	switch {
	case c.generatedExpr != "" && c.generatedVirtual:
		b.WriteString(" ALIAS (" + c.generatedExpr + ")")
	case c.generatedExpr != "":
		b.WriteString(" MATERIALIZED (" + c.generatedExpr + ")")
	default:
		def, err := defaultClause(c, true, "now64(6)")
		if err != nil {
			return "", err
		}
		b.WriteString(def)
	}
	if c.comment != "" {
		b.WriteString(" COMMENT '")
		b.WriteString(clickHouseEscape(c.comment))
		b.WriteByte('\'')
	}
	if altering {
		switch {
		case c.first:
			b.WriteString(" FIRST")
		case c.after != "":
			b.WriteString(" AFTER " + clickHouseQ.ident(c.after))
		}
	}
	return b.String(), nil
}

func (clickHouseDialect) typeSQL(c *columnDef) (string, error) {
	switch c.kind {
	case kindRaw:
		return c.rawType, nil
	case kindString, kindText, kindBinary:
		return "String", nil
	case kindChar:
		return fmt.Sprintf("FixedString(%d)", charLength(c.length)), nil
	case kindTinyInt:
		return clickHouseIntegerType("Int8", c.unsigned), nil
	case kindSmallInt:
		return clickHouseIntegerType("Int16", c.unsigned), nil
	case kindInt:
		return clickHouseIntegerType("Int32", c.unsigned), nil
	case kindBigInt:
		return clickHouseIntegerType("Int64", c.unsigned), nil
	case kindBool:
		return "Bool", nil
	case kindDecimal:
		return fmt.Sprintf("Decimal(%d, %d)", c.precision, c.scale), nil
	case kindFloat:
		return "Float32", nil
	case kindDouble:
		return "Float64", nil
	case kindDate:
		return "Date", nil
	case kindTime:
		return "Time", nil
	case kindDateTime, kindTimestamp:
		return "DateTime64(6)", nil
	case kindTimestampTz:
		return "DateTime64(6, 'UTC')", nil
	case kindJSON:
		return "JSON", nil
	case kindUUID:
		return "UUID", nil
	case kindEnum:
		return clickHouseEnumType(c)
	default:
		return "", fmt.Errorf("migrate: clickhouse: unsupported column kind for %q", c.name)
	}
}

func clickHouseIntegerType(signed string, unsigned bool) string {
	if unsigned {
		return "U" + signed
	}
	return signed
}

func clickHouseEnumType(c *columnDef) (string, error) {
	if len(c.enumVals) > 32767 {
		return "", fmt.Errorf("migrate: clickhouse enum column %q has %d values; Enum16 supports at most 32767 positive stable values", c.name, len(c.enumVals))
	}
	kind := "Enum8"
	if len(c.enumVals) > 127 {
		kind = "Enum16"
	}
	seen := make(map[string]struct{}, len(c.enumVals))
	values := make([]string, len(c.enumVals))
	for i, value := range c.enumVals {
		if _, duplicate := seen[value]; duplicate {
			return "", fmt.Errorf("migrate: clickhouse enum column %q declares duplicate value %q", c.name, value)
		}
		seen[value] = struct{}{}
		values[i] = fmt.Sprintf("'%s' = %d", clickHouseEscape(value), i+1)
	}
	return kind + "(" + strings.Join(values, ", ") + ")", nil
}

func clickHousePrimaryError(table string) error {
	return fmt.Errorf("migrate: clickhouse cannot preserve relational Primary semantics on table %q; declare the sorting or sparse primary key in ClickHouseEngine, and use an appropriate MergeTree engine or application logic for deduplication", table)
}

func clickHouseIndexError(table string) error {
	return fmt.Errorf("migrate: clickhouse relational index APIs are unsupported on table %q because a skipping-index type cannot be inferred; use Schema.Exec with ALTER TABLE ... ADD INDEX ... TYPE ... and an explicit WithDown", table)
}

func clickHouseForeignError(table string) error {
	return fmt.Errorf("migrate: clickhouse has no equivalent foreign-key constraint for table %q; keep ForeignID as a regular UInt64 column and enforce the relationship in the application", table)
}

func clickHouseEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", "''")
}

func (clickHouseDialect) lock(context.Context, *sql.Conn, string, time.Duration) (lockToken, error) {
	return "", fmt.Errorf("%w: clickhouse has no built-in session migration lock; serialize migration execution externally, then opt in with WithoutLock", ErrLockUnsupported)
}

func (clickHouseDialect) unlock(context.Context, *sql.Conn, lockToken) error { return nil }

func (clickHouseDialect) quoteIdent(name string) string { return clickHouseQ.table(name) }

func (clickHouseDialect) listTablesSQL() string {
	return "SELECT name FROM system.tables WHERE database = currentDatabase() AND is_temporary = 0"
}

func (clickHouseDialect) freshDropSQL(table string) string {
	return fmt.Sprintf("DROP TABLE IF EXISTS %s SYNC", clickHouseQ.table(table))
}
