package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// MySQL targets MySQL 8.0+ or an equivalent MariaDB release. DDL commits
// implicitly, so failures report the statement and persisted prefix.
var MySQL Dialect = mysqlDialect{}

type mysqlDialect struct{}

var myQ = quoter{delimiter: '`'}

func (mysqlDialect) name() string                     { return "mysql" }
func (mysqlDialect) transactionMode() transactionMode { return transactionModeDML }

func (mysqlDialect) placeholder(int) string { return "?" }

func (d mysqlDialect) recordInsertSQL(table string) string {
	return standardRecordInsertSQL(d, table)
}

func (d mysqlDialect) recordUpdateSQL(table string) string {
	return standardRecordUpdateSQL(d, table)
}

func (d mysqlDialect) recordDeleteSQL(table, column string) string {
	return standardRecordDeleteSQL(d, table, column)
}

func (d mysqlDialect) recordRepairSQL(table string) string {
	return standardRecordRepairSQL(d, table)
}

func (mysqlDialect) ensureTableSQL(table string) string {
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n"+
		"\tversion VARCHAR(191) PRIMARY KEY,\n"+
		"\tbatch INTEGER NOT NULL,\n"+
		"\tchecksum CHAR(64) NOT NULL,\n"+
		"\tapplied_at VARCHAR(32) NOT NULL\n"+
		")", myQ.table(table))
}

func (d mysqlDialect) compile(op operation) ([]statement, error) {
	switch o := op.(type) {
	case *createTable:
		return d.compileCreate(o.def)
	case *dropTable:
		return []statement{dropTableSQL(myQ, o)}, nil
	case *renameTable:
		return []statement{sqlStatement("RENAME TABLE %s TO %s", myQ.table(o.from), myQ.table(o.to))}, nil
	case *alterTable:
		return d.compileAlter(o)
	case *recreateTable:
		return nil, fmt.Errorf("migrate: mysql cannot rebuild table %q atomically (DDL commits implicitly); use Schema.Table with native ALTER operations, or Exec", o.def.name)
	case *rawSQL:
		return []statement{{sql: o.sql, args: o.args}}, nil
	case *goFunc:
		return []statement{{fn: o.fn}}, nil
	default:
		return nil, fmt.Errorf("migrate: mysql: unsupported operation %T", op)
	}
}

func (d mysqlDialect) compileCreate(def *tableDef) ([]statement, error) {
	def = resolveCompositeIdentity(def)
	pk, err := primaryColumns(def)
	if err != nil {
		return nil, err
	}

	clauses := make([]string, 0, len(def.columns)+len(def.fks)+1)
	for _, c := range def.columns {
		clause, err := d.columnSQL(c, false)
		if err != nil {
			return nil, err
		}
		clauses = append(clauses, clause)
	}
	if len(pk) > 0 {
		// MySQL always names the primary key PRIMARY.
		clauses = append(clauses, fmt.Sprintf("PRIMARY KEY (%s)", myQ.idents(pk)))
	}
	for _, chk := range def.checks {
		clauses = append(clauses, checkClause(myQ, chk))
	}
	for _, uc := range def.uniques {
		clauses = append(clauses, uniqueConstraintClause(myQ, uc))
	}
	for _, fk := range def.fks {
		clauses = append(clauses, foreignClause(myQ, def.constraintTable(), fk))
	}

	suffix := ""
	if def.comment != "" {
		suffix = " COMMENT = '" + mysqlEscape(def.comment) + "'"
	}
	stmts := []statement{sqlStatement("CREATE TABLE %s (\n\t%s\n)%s",
		myQ.table(def.name), strings.Join(clauses, ",\n\t"), suffix)}
	for _, idx := range append(inlineIndexes(def.columns), def.indexes...) {
		sql, err := createIndexSQL("mysql", myQ, def.name, idx, false)
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, statement{sql: sql})
	}
	return stmts, nil
}

func (d mysqlDialect) compileAlter(op *alterTable) ([]statement, error) {
	table := myQ.table(op.table)
	var stmts []statement
	for _, ch := range op.changes {
		switch c := ch.(type) {
		case *addColumn:
			if c.col.change && c.col.changeUsing != "" {
				return nil, fmt.Errorf("migrate: Using is a Postgres conversion expression; mysql converts column %q of table %q implicitly", c.col.name, op.table)
			}
			clause, err := d.columnSQL(c.col, true)
			if err != nil {
				return nil, err
			}
			verb := "ADD"
			if c.col.change {
				verb = "MODIFY"
			}
			stmts = append(stmts, sqlStatement("ALTER TABLE %s %s COLUMN %s", table, verb, clause))
			if c.col.change {
				continue
			}
			for _, idx := range inlineIndexes([]*columnDef{c.col}) {
				sql, err := createIndexSQL("mysql", myQ, op.table, idx, false)
				if err != nil {
					return nil, err
				}
				stmts = append(stmts, statement{sql: sql})
			}
		case *dropColumn:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP COLUMN %s", table, myQ.ident(c.name)))
		case *renameColumn:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s RENAME COLUMN %s TO %s", table, myQ.ident(c.from), myQ.ident(c.to)))
		case *addIndex:
			sql, err := createIndexSQL("mysql", myQ, op.table, c.idx, false)
			if err != nil {
				return nil, err
			}
			stmts = append(stmts, statement{sql: sql})
		case *dropIndex:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP INDEX %s", table, myQ.ident(c.name)))
		case *addForeign:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD %s", table, foreignClause(myQ, op.table, c.fk)))
		case *dropForeign:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP FOREIGN KEY %s", table, myQ.ident(c.name)))
		case *addPrimary:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD PRIMARY KEY (%s)", table, myQ.idents(c.columns)))
		case *dropPrimary:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP PRIMARY KEY", table))
		case *renameIndex:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s RENAME INDEX %s TO %s", table, myQ.ident(c.from), myQ.ident(c.to)))
		case *setTableComment:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s COMMENT = '%s'", table, mysqlEscape(c.comment)))
		case *addCheck:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD %s", table, checkClause(myQ, c.chk)))
		case *dropCheck:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP CHECK %s", table, myQ.ident(c.name)))
		case *addUniqueConstraint:
			// A MySQL unique constraint is a unique index under the given name.
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD %s", table, uniqueConstraintClause(myQ, c.uc)))
		case *dropConstraint:
			// DROP CONSTRAINT needs MySQL 8.0.19+.
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP CONSTRAINT %s", table, myQ.ident(c.name)))
		default:
			return nil, fmt.Errorf("migrate: mysql: unsupported change %T", ch)
		}
	}
	return stmts, nil
}

// Position modifiers apply only while altering.
func (d mysqlDialect) columnSQL(c *columnDef, altering bool) (string, error) {
	typ, err := d.typeSQL(c)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(myQ.ident(c.name) + " " + typ)
	b.WriteString(generatedClause(c))
	if !c.nullable {
		b.WriteString(" NOT NULL")
	}
	def, err := defaultClause(c, true, "CURRENT_TIMESTAMP(6)")
	if err != nil {
		return "", err
	}
	b.WriteString(def)
	if c.useCurrentOnUpdate {
		b.WriteString(" ON UPDATE CURRENT_TIMESTAMP(6)")
	}
	if c.autoIncr {
		b.WriteString(" AUTO_INCREMENT")
	}
	if c.inlinePrimary() {
		b.WriteString(" PRIMARY KEY")
	}
	if c.comment != "" {
		b.WriteString(" COMMENT '" + mysqlEscape(c.comment) + "'")
	}
	if altering {
		switch {
		case c.first:
			b.WriteString(" FIRST")
		case c.after != "":
			b.WriteString(" AFTER " + myQ.ident(c.after))
		}
	}
	return b.String(), nil
}

func (mysqlDialect) typeSQL(c *columnDef) (string, error) {
	unsigned := func(t string) string {
		if c.unsigned {
			return t + " UNSIGNED"
		}
		return t
	}
	switch c.kind {
	case kindRaw:
		return c.rawType, nil
	case kindString:
		return fmt.Sprintf("VARCHAR(%d)", charLength(c.length)), nil
	case kindChar:
		return fmt.Sprintf("CHAR(%d)", charLength(c.length)), nil
	case kindText:
		// LONGTEXT preserves the builder's unbounded Text contract.
		return "LONGTEXT", nil
	case kindTinyInt:
		return unsigned("TINYINT"), nil
	case kindSmallInt:
		return unsigned("SMALLINT"), nil
	case kindInt:
		return unsigned("INT"), nil
	case kindBigInt:
		return unsigned("BIGINT"), nil
	case kindBool:
		return "TINYINT(1)", nil
	case kindDecimal:
		return fmt.Sprintf("DECIMAL(%d, %d)", c.precision, c.scale), nil
	case kindFloat:
		return "FLOAT", nil
	case kindDouble:
		return "DOUBLE", nil
	case kindDate:
		return "DATE", nil
	case kindTime:
		return "TIME", nil
	case kindDateTime, kindTimestamp, kindTimestampTz:
		// DATETIME(6) avoids TIMESTAMP's horizon and implicit defaults.
		return "DATETIME(6)", nil
	case kindJSON:
		return "JSON", nil
	case kindUUID:
		return "CHAR(36)", nil
	case kindBinary:
		return "BLOB", nil
	case kindEnum:
		vals := make([]string, len(c.enumVals))
		for i, v := range c.enumVals {
			vals[i] = "'" + mysqlEscape(v) + "'"
		}
		return "ENUM(" + strings.Join(vals, ", ") + ")", nil
	default:
		return "", fmt.Errorf("migrate: mysql: unsupported column kind for %q", c.name)
	}
}

func (mysqlDialect) lock(ctx context.Context, conn *sql.Conn, table string, timeout time.Duration) (lockToken, error) {
	var database sql.NullString
	if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database); err != nil {
		return "", fmt.Errorf("migrate: resolve advisory lock namespace: %w", err)
	}
	token := mysqlLockName(database.String, table)
	// GET_LOCK counts whole seconds, so round up.
	seconds := int64((timeout + time.Second - 1) / time.Second)
	var acquired sql.NullInt64
	err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", string(token), seconds).Scan(&acquired)
	if err != nil {
		return "", fmt.Errorf("migrate: acquire advisory lock: %w", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return "", fmt.Errorf("%w: waited %v for the advisory lock", ErrLockTimeout, timeout)
	}
	return token, nil
}

func (mysqlDialect) unlock(ctx context.Context, conn *sql.Conn, token lockToken) error {
	var released sql.NullInt64
	err := conn.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", string(token)).Scan(&released)
	if err != nil {
		return fmt.Errorf("migrate: release advisory lock: %w", err)
	}
	if !released.Valid || released.Int64 != 1 {
		return fmt.Errorf("migrate: advisory lock was not held at release time")
	}
	return nil
}

func (mysqlDialect) quoteIdent(name string) string { return myQ.table(name) }

func (mysqlDialect) listTablesSQL() string {
	return "SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'"
}

func (mysqlDialect) freshDropSQL(table string) string {
	return fmt.Sprintf("DROP TABLE IF EXISTS %s", myQ.table(table))
}

// mysqlLockName scopes GET_LOCK by database and records table; hashing keeps
// the name within MySQL's 64-character limit.
func mysqlLockName(database, table string) lockToken {
	sum := sha256.Sum256([]byte("go-rio.migrate\x00" + database + "\x00" + table))
	return lockToken(hex.EncodeToString(sum[:]))
}

func mysqlEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", "''")
}
