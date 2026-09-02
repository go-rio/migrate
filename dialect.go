package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Dialect compiles and executes migrations for one database engine. Built-in
// values are Postgres, MySQL, SQLite, and ClickHouse; the methods are
// unexported, so no other implementation exists.
type Dialect interface {
	name() string
	compile(op operation) ([]statement, error)
	ensureTableSQL(table string) string
	quoteIdent(name string) string
	placeholder(n int) string
	transactionMode() transactionMode
	recordInsertSQL(table string) string
	recordUpdateSQL(table string) string
	recordDeleteSQL(table, column string) string
	recordRepairSQL(table string) string
	lock(ctx context.Context, conn *sql.Conn, table string, timeout time.Duration) (lockToken, error)
	unlock(ctx context.Context, conn *sql.Conn, token lockToken) error
	listTablesSQL() string
	freshDropSQL(table string) string
}

type lockToken string

// transactionMode is one migration's transactional guarantee: full, DML-only
// (DDL commits implicitly, as on MySQL), or none (ClickHouse).
type transactionMode uint8

const (
	transactionModeInvalid transactionMode = iota
	transactionModeFull
	transactionModeDML
	transactionModeNone
)

// statement is either SQL or an opaque Go migration function.
type statement struct {
	sql  string
	args []any
	fn   func(context.Context, DB) error
	// desc labels an opaque function in plans and errors.
	desc string
	// ddl marks MySQL's implicitly committing statements.
	ddl bool
}

func sqlStatement(format string, a ...any) statement {
	return statement{sql: fmt.Sprintf(format, a...)}
}

// quoter quotes identifiers for one dialect; table splits schema-qualified
// names into individually quoted segments.
type quoter struct {
	delimiter       byte
	escapeBackslash bool
}

func (q quoter) ident(name string) string {
	c := string(q.delimiter)
	if q.escapeBackslash {
		name = strings.ReplaceAll(name, `\`, `\\`)
		name = strings.ReplaceAll(name, c, `\`+c)
	} else {
		name = strings.ReplaceAll(name, c, c+c)
	}
	return c + name + c
}

func (q quoter) idents(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = q.ident(n)
	}
	return strings.Join(quoted, ", ")
}

func (q quoter) table(name string) string {
	segs := strings.Split(name, ".")
	for i, s := range segs {
		segs[i] = q.ident(s)
	}
	return strings.Join(segs, ".")
}

func standardRecordInsertSQL(d Dialect, table string) string {
	return fmt.Sprintf("INSERT INTO %s (version, batch, checksum, applied_at) VALUES (%s, %s, %s, %s)",
		d.quoteIdent(table), d.placeholder(1), d.placeholder(2), d.placeholder(3), d.placeholder(4))
}

func standardRecordUpdateSQL(d Dialect, table string) string {
	return fmt.Sprintf("UPDATE %s SET checksum = %s, applied_at = %s WHERE version = %s",
		d.quoteIdent(table), d.placeholder(1), d.placeholder(2), d.placeholder(3))
}

func standardRecordDeleteSQL(d Dialect, table, column string) string {
	return fmt.Sprintf("DELETE FROM %s WHERE %s = %s",
		d.quoteIdent(table), column, d.placeholder(1))
}

func standardRecordRepairSQL(d Dialect, table string) string {
	return fmt.Sprintf("UPDATE %s SET checksum = %s WHERE version = %s",
		d.quoteIdent(table), d.placeholder(1), d.placeholder(2))
}

func baseName(table string) string {
	if i := strings.LastIndexByte(table, '.'); i >= 0 {
		return table[i+1:]
	}
	return table
}

func schemaPrefix(table string) string {
	if i := strings.LastIndexByte(table, '.'); i >= 0 {
		return table[:i+1]
	}
	return ""
}

// literal renders DDL defaults, where bind parameters are unavailable.
func literal(v any, backslashEscapes bool) (string, error) {
	if v == nil {
		return "NULL", nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String:
		s := strings.ReplaceAll(rv.String(), "'", "''")
		if backslashEscapes {
			s = strings.ReplaceAll(s, `\`, `\\`)
		}
		return "'" + s + "'", nil
	case reflect.Bool:
		if rv.Bool() {
			return "TRUE", nil
		}
		return "FALSE", nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(rv.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(rv.Uint(), 10), nil
	case reflect.Float32, reflect.Float64:
		value := rv.Float()
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return "", fmt.Errorf("migrate: unsupported non-finite default value %v; use DefaultExpr for a database-specific expression", value)
		}
		return strconv.FormatFloat(value, 'g', -1, rv.Type().Bits()), nil
	default:
		return "", fmt.Errorf("migrate: unsupported default value of type %T; use DefaultExpr for SQL expressions", v)
	}
}

// enumCheckSQL emulates enums where no native type exists.
func enumCheckSQL(q quoter, table string, c *columnDef) string {
	vals := make([]string, len(c.enumVals))
	for i, v := range c.enumVals {
		vals[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	return fmt.Sprintf(" CONSTRAINT %s CHECK (%s IN (%s))",
		q.ident(baseName(table)+"_"+c.name+"_check"), q.ident(c.name), strings.Join(vals, ", "))
}

func generatedClause(c *columnDef) string {
	if c.generatedExpr == "" {
		return ""
	}
	kind := " STORED"
	if c.generatedVirtual {
		kind = " VIRTUAL"
	}
	return " GENERATED ALWAYS AS (" + c.generatedExpr + ")" + kind
}

func checkClause(q quoter, chk *checkDef) string {
	return fmt.Sprintf("CONSTRAINT %s CHECK (%s)", q.ident(chk.name), chk.expr)
}

func uniqueConstraintClause(q quoter, uc *uniqueConstraintDef) string {
	return fmt.Sprintf("CONSTRAINT %s UNIQUE (%s)", q.ident(uc.name), q.idents(uc.columns))
}

func defaultValueSQL(c *columnDef, backslashEscapes bool, currentTS string) (string, error) {
	switch {
	case c.useCurrent:
		return currentTS, nil
	case c.defaultExpr != "":
		return "(" + c.defaultExpr + ")", nil
	case c.hasDefault:
		lit, err := literal(c.defaultVal, backslashEscapes)
		if err != nil {
			return "", fmt.Errorf("column %q: %w", c.name, err)
		}
		return lit, nil
	}
	return "", nil
}

func defaultClause(c *columnDef, backslashEscapes bool, currentTS string) (string, error) {
	value, err := defaultValueSQL(c, backslashEscapes, currentTS)
	if err != nil || value == "" {
		return "", err
	}
	return " DEFAULT " + value, nil
}

func foreignClause(q quoter, table string, fk *foreignDef) string {
	refCols := fk.refColumns
	if len(refCols) == 0 {
		refCols = []string{"id"}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)",
		q.ident(fk.resolvedName(table)), q.idents(fk.columns), q.table(fk.refTable), q.idents(refCols))
	if fk.onDelete != "" {
		b.WriteString(" ON DELETE " + fk.onDelete)
	}
	if fk.onUpdate != "" {
		b.WriteString(" ON UPDATE " + fk.onUpdate)
	}
	return b.String()
}

// validateIndex rejects features a dialect cannot preserve faithfully.
func validateIndex(dialect, table string, idx *indexDef) error {
	name := idx.resolvedName(table)
	if len(idx.desc) > 0 && len(idx.exprs) > 0 {
		return fmt.Errorf("migrate: Desc applies to column indexes; write the direction into the expression of index %q of table %q", name, table)
	}
	for _, c := range idx.desc {
		if !slices.Contains(idx.columns, c) {
			return fmt.Errorf("migrate: index %q of table %q marks %q descending but does not index it", name, table, c)
		}
	}
	switch dialect {
	case "postgres":
		if idx.fulltext {
			return fmt.Errorf("migrate: postgres has no FULLTEXT index (index %q of table %q); index a tsvector expression instead: IndexExpr(name, \"to_tsvector('english', col)\").Using(\"gin\")", name, table)
		}
		if idx.spatial {
			return fmt.Errorf("migrate: postgres has no SPATIAL index (index %q of table %q); use PostGIS with Using(\"gist\")", name, table)
		}
		if idx.nullsNotDistinct && !idx.unique {
			return fmt.Errorf("migrate: NULLS NOT DISTINCT applies to unique indexes only (index %q of table %q)", name, table)
		}
	case "mysql":
		specialized := idx.fulltext || idx.spatial
		if idx.where != "" {
			return fmt.Errorf("migrate: mysql does not support partial indexes (index %q of table %q declares Where); enforce the rule in application code or index a generated column", name, table)
		}
		if len(idx.include) > 0 {
			return fmt.Errorf("migrate: mysql has no INCLUDE columns (index %q of table %q); declare a wider composite index instead", name, table)
		}
		if idx.nullsNotDistinct {
			return fmt.Errorf("migrate: mysql cannot make NULLs distinct in unique indexes (index %q of table %q)", name, table)
		}
		if specialized && idx.using != "" {
			return fmt.Errorf("migrate: fulltext and spatial indexes choose their own structure; drop Using on index %q of table %q", name, table)
		}
		if specialized && len(idx.exprs) > 0 {
			return fmt.Errorf("migrate: fulltext and spatial indexes cover columns, not expressions (index %q of table %q)", name, table)
		}
	case "sqlite":
		if idx.fulltext {
			return fmt.Errorf("migrate: sqlite has no FULLTEXT index (index %q of table %q); create an FTS5 virtual table with Exec", name, table)
		}
		if idx.spatial {
			return fmt.Errorf("migrate: sqlite has no SPATIAL index (index %q of table %q)", name, table)
		}
		if idx.using != "" {
			return fmt.Errorf("migrate: sqlite has a single index type; drop Using on index %q of table %q", name, table)
		}
		if len(idx.include) > 0 {
			return fmt.Errorf("migrate: sqlite has no INCLUDE columns (index %q of table %q); declare a wider composite index instead", name, table)
		}
		if idx.nullsNotDistinct {
			return fmt.Errorf("migrate: sqlite cannot make NULLs distinct in unique indexes (index %q of table %q)", name, table)
		}
	}
	return nil
}

// indexItems renders the key list: quoted columns with DESC where marked, or
// expressions verbatim. Only MySQL wraps each functional key part in
// parentheses (see IndexExpr).
func indexItems(dialect string, q quoter, idx *indexDef) string {
	if len(idx.exprs) == 0 {
		items := make([]string, len(idx.columns))
		for i, c := range idx.columns {
			items[i] = q.ident(c)
			if slices.Contains(idx.desc, c) {
				items[i] += " DESC"
			}
		}
		return strings.Join(items, ", ")
	}
	if dialect != "mysql" {
		return strings.Join(idx.exprs, ", ")
	}
	items := make([]string, len(idx.exprs))
	for i, e := range idx.exprs {
		items[i] = "(" + e + ")"
	}
	return strings.Join(items, ", ")
}

func createIndexSQL(dialect string, q quoter, table string, idx *indexDef, schemaOnIndex bool) (string, error) {
	if err := validateIndex(dialect, table, idx); err != nil {
		return "", err
	}

	kind := ""
	switch {
	case idx.unique:
		kind = "UNIQUE "
	case idx.fulltext:
		kind = "FULLTEXT "
	case idx.spatial:
		kind = "SPATIAL "
	}
	concurrently := ""
	if idx.concurrently && dialect == "postgres" {
		concurrently = "CONCURRENTLY "
	}

	name := idx.resolvedName(table)
	// SQLite qualifies the index name and takes a bare table name.
	indexRef, tableRef := q.ident(name), q.table(table)
	if schemaOnIndex {
		indexRef, tableRef = q.table(schemaPrefix(table)+name), q.ident(baseName(table))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE %sINDEX %s%s ON %s", kind, concurrently, indexRef, tableRef)
	if idx.using != "" && dialect == "postgres" {
		b.WriteString(" USING " + idx.using)
	}
	b.WriteString(" (" + indexItems(dialect, q, idx) + ")")
	if idx.using != "" && dialect == "mysql" {
		b.WriteString(" USING " + strings.ToUpper(idx.using))
	}
	if len(idx.include) > 0 {
		b.WriteString(" INCLUDE (" + q.idents(idx.include) + ")")
	}
	if idx.nullsNotDistinct {
		b.WriteString(" NULLS NOT DISTINCT")
	}
	if idx.where != "" {
		b.WriteString(" WHERE " + idx.where)
	}
	return b.String(), nil
}

func charLength(n int) int {
	if n <= 0 {
		return 255
	}
	return n
}

func inlineIndexes(cols []*columnDef) []*indexDef {
	var idxs []*indexDef
	for _, c := range cols {
		if c.unique {
			idxs = append(idxs, &indexDef{columns: []string{c.name}, unique: true})
		}
		if c.indexed {
			idxs = append(idxs, &indexDef{columns: []string{c.name}})
		}
	}
	return idxs
}

// resolveCompositeIdentity copies def, never mutating it, so that an identity
// column listed in a table-level Primary drops its inline PRIMARY KEY.
func resolveCompositeIdentity(def *tableDef) *tableDef {
	for i, c := range def.columns {
		if c.inlinePrimary() && slices.Contains(def.primary, c.name) {
			out := *def
			out.columns = slices.Clone(def.columns)
			cc := *c
			cc.primary = false
			out.columns[i] = &cc
			return &out
		}
	}
	return def
}

func primaryColumns(def *tableDef) ([]string, error) {
	var inline bool
	var cols []string
	for _, c := range def.columns {
		if c.inlinePrimary() {
			if inline {
				return nil, fmt.Errorf("migrate: table %q declares more than one auto-incrementing column", def.name)
			}
			inline = true
			continue
		}
		if c.primary {
			cols = append(cols, c.name)
		}
	}
	cols = append(cols, def.primary...)
	if inline && len(cols) > 0 {
		return nil, fmt.Errorf("migrate: table %q combines an auto-incrementing primary key with other primary key declarations", def.name)
	}
	return cols, nil
}

func declarationErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("migrate: invalid declaration: %w", errors.Join(errs...))
}

// compileRecreate copies rows into a temporary table and swaps names. Live
// triggers are captured and restored: DROP TABLE would silently discard them.
func compileRecreate(
	d Dialect,
	q quoter,
	schemaOnIndex bool,
	renameSQL func(from, to string) statement,
	listTriggers func(context.Context, DB, string) ([]string, error),
	def *tableDef,
) ([]statement, error) {
	tmp := def.name + "__migrate_new"
	tmpDef := &tableDef{
		name:           tmp,
		constraintBase: def.name,
		primary:        def.primary,
		checks:         def.checks,
		uniques:        def.uniques,
		comment:        def.comment,
	}
	for _, c := range def.columns {
		cc := *c
		cc.unique, cc.indexed = false, false // indexes rebuild after the rename
		tmpDef.columns = append(tmpDef.columns, &cc)
	}
	for _, fk := range def.fks {
		f := *fk
		f.name = fk.resolvedName(def.name)
		tmpDef.fks = append(tmpDef.fks, &f)
	}

	stmts, err := d.compile(&createTable{def: tmpDef})
	if err != nil {
		return nil, err
	}
	var copyCols, copyExprs []string
	for _, c := range def.columns {
		if c.skipCopy || c.generatedExpr != "" { // generated columns fill themselves
			continue
		}
		copyCols = append(copyCols, c.name)
		if c.copyFrom != "" {
			copyExprs = append(copyExprs, c.copyFrom)
		} else {
			copyExprs = append(copyExprs, q.ident(c.name))
		}
	}
	if len(copyCols) > 0 {
		stmts = append(stmts, sqlStatement("INSERT INTO %s (%s) SELECT %s FROM %s",
			q.table(tmp), q.idents(copyCols), strings.Join(copyExprs, ", "), q.table(def.name)))
	}
	var triggers []string
	stmts = append(stmts,
		statement{
			desc: fmt.Sprintf("capture the triggers of %s", q.table(def.name)),
			fn: func(ctx context.Context, db DB) error {
				var err error
				triggers, err = listTriggers(ctx, db, def.name)
				return err
			},
		},
		sqlStatement("DROP TABLE %s", q.table(def.name)),
		renameSQL(tmp, def.name))
	for _, idx := range append(inlineIndexes(def.columns), def.indexes...) {
		sql, err := createIndexSQL(d.name(), q, def.name, idx, schemaOnIndex)
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, statement{sql: sql})
	}
	stmts = append(stmts, statement{
		desc: fmt.Sprintf("recreate the captured triggers of %s", q.table(def.name)),
		fn: func(ctx context.Context, db DB) error {
			for _, ddl := range triggers {
				if _, err := db.ExecContext(ctx, ddl); err != nil {
					return fmt.Errorf("%s: %w", describeStatement(statement{sql: ddl}), err)
				}
			}
			return nil
		},
	})
	return stmts, nil
}

func queryStrings(ctx context.Context, db DB, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
