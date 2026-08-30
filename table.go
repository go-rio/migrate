package migrate

import (
	"fmt"
	"strings"
)

// Table builds a new table or a set of alterations. Declaration errors are
// collected and returned when the migration compiles.
type Table struct {
	table                 string
	create                *tableDef   // set inside Schema.Create or Schema.Recreate
	alter                 *alterTable // set inside Schema.Table
	allowClickHouseEngine bool
}

func (t *Table) errf(format string, a ...any) {
	err := fmt.Errorf(format, a...)
	if t.create != nil {
		t.create.errs = append(t.create.errs, err)
	} else {
		t.alter.errs = append(t.alter.errs, err)
	}
}

func (t *Table) addColumn(col *columnDef) *Column {
	if col.name == "" {
		t.errf("table %q declares a column with an empty name", t.table)
	}
	if t.create != nil {
		t.create.columns = append(t.create.columns, col)
	} else {
		t.alter.changes = append(t.alter.changes, &addColumn{col: col})
	}
	return &Column{def: col}
}

func (t *Table) addIndexDef(idx *indexDef) {
	if t.create != nil {
		t.create.indexes = append(t.create.indexes, idx)
	} else {
		t.alter.changes = append(t.alter.changes, &addIndex{idx: idx})
	}
}

func (t *Table) addForeignDef(fk *foreignDef) {
	if t.create != nil {
		t.create.fks = append(t.create.fks, fk)
	} else {
		t.alter.changes = append(t.alter.changes, &addForeign{fk: fk})
	}
}

func (t *Table) alterOnly(method string) bool {
	if t.alter == nil {
		t.errf("%s is only valid inside Schema.Table, not Schema.Create (table %q)", method, t.table)
		return false
	}
	return true
}

func optional[T any](v []T, def T) T {
	if len(v) > 0 {
		return v[0]
	}
	return def
}

// ID declares a 64-bit auto-incrementing primary key named "id" by default.
func (t *Table) ID(name ...string) *Column {
	return t.BigInteger(optional(name, "id")).Unsigned().AutoIncrement()
}

// String declares a VARCHAR column. The length defaults to 255.
func (t *Table) String(name string, length ...int) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindString, length: optional(length, 255)})
}

// Char declares a fixed-length CHAR column. The length defaults to 255.
func (t *Table) Char(name string, length ...int) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindChar, length: optional(length, 255)})
}

// Text declares an unbounded text column.
func (t *Table) Text(name string) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindText})
}

// TinyInteger declares an 8-bit integer, or SMALLINT on PostgreSQL.
func (t *Table) TinyInteger(name string) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindTinyInt})
}

// SmallInteger declares a 16-bit integer.
func (t *Table) SmallInteger(name string) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindSmallInt})
}

// Integer declares a 32-bit integer.
func (t *Table) Integer(name string) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindInt})
}

// BigInteger declares a 64-bit integer.
func (t *Table) BigInteger(name string) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindBigInt})
}

// Boolean declares a boolean column.
func (t *Table) Boolean(name string) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindBool})
}

// Decimal declares an exact fixed-point column, e.g. Decimal("amount", 10, 2).
func (t *Table) Decimal(name string, precision, scale int) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindDecimal, precision: precision, scale: scale})
}

// Float declares a single-precision floating point column.
func (t *Table) Float(name string) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindFloat})
}

// Double declares a double-precision floating point column.
func (t *Table) Double(name string) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindDouble})
}

// Date declares a calendar date column.
func (t *Table) Date(name string) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindDate})
}

// Time declares a time-of-day column.
func (t *Table) Time(name string) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindTime})
}

// DateTime declares a timestamp without a time zone.
func (t *Table) DateTime(name string) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindDateTime})
}

// Timestamp is an alias for DateTime, for schemas that prefer the name.
func (t *Table) Timestamp(name string) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindTimestamp})
}

// TimestampTz declares an instant, using the closest type each dialect offers.
func (t *Table) TimestampTz(name string) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindTimestampTz})
}

// JSON declares a JSONB, JSON, or TEXT column according to the dialect.
func (t *Table) JSON(name string) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindJSON})
}

// UUID declares a UUID column (native on Postgres, CHAR(36) elsewhere).
func (t *Table) UUID(name string) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindUUID})
}

// Binary declares a binary blob column.
func (t *Table) Binary(name string) *Column {
	return t.addColumn(&columnDef{name: name, kind: kindBinary})
}

// Enum declares a native MySQL ENUM or an equivalent checked VARCHAR.
// Values may use any defined string type.
func (t *Table) Enum[V ~string](name string, values ...V) *Column {
	if len(values) == 0 {
		t.errf("enum column %q of table %q declares no values", name, t.table)
	}
	enumVals := make([]string, len(values))
	for i, value := range values {
		enumVals[i] = string(value)
	}
	return t.addColumn(&columnDef{name: name, kind: kindEnum, enumVals: enumVals})
}

// Column declares a column with a dialect-specific type written verbatim.
func (t *Table) Column(name, sqlType string) *Column {
	if sqlType == "" {
		t.errf("column %q of table %q declares an empty type", name, t.table)
	}
	return t.addColumn(&columnDef{name: name, kind: kindRaw, rawType: sqlType})
}

// Timestamps declares nullable created_at and updated_at TimestampTz columns.
func (t *Table) Timestamps() {
	t.TimestampTz("created_at").Nullable()
	t.TimestampTz("updated_at").Nullable()
}

// SoftDeletes declares a nullable deleted_at TimestampTz column.
func (t *Table) SoftDeletes(name ...string) *Column {
	return t.TimestampTz(optional(name, "deleted_at")).Nullable()
}

// ForeignID declares an unsigned 64-bit foreign-key column.
func (t *Table) ForeignID(name string) *ForeignColumn {
	col := &columnDef{name: name, kind: kindBigInt, unsigned: true}
	c := t.addColumn(col)
	return &ForeignColumn{Column: c, table: t}
}

// Index declares a conventionally named index over the columns.
func (t *Table) Index(columns ...string) *Index {
	idx := &indexDef{columns: columns}
	if len(columns) == 0 {
		t.errf("index on table %q declares no columns", t.table)
	}
	t.addIndexDef(idx)
	return &Index{def: idx}
}

// Unique declares a conventionally named unique index.
func (t *Table) Unique(columns ...string) *Index {
	idx := &indexDef{columns: columns, unique: true}
	if len(columns) == 0 {
		t.errf("unique index on table %q declares no columns", t.table)
	}
	t.addIndexDef(idx)
	return &Index{def: idx}
}

// FullText declares a MySQL FULLTEXT index. Other dialects reject it.
func (t *Table) FullText(columns ...string) *Index {
	idx := &indexDef{columns: columns, fulltext: true}
	if len(columns) == 0 {
		t.errf("fulltext index on table %q declares no columns", t.table)
	}
	t.addIndexDef(idx)
	return &Index{def: idx}
}

// Spatial declares a MySQL SPATIAL index. Other dialects reject it.
func (t *Table) Spatial(columns ...string) *Index {
	idx := &indexDef{columns: columns, spatial: true}
	if len(columns) == 0 {
		t.errf("spatial index on table %q declares no columns", t.table)
	}
	t.addIndexDef(idx)
	return &Index{def: idx}
}

// IndexExpr declares a named index over SQL expressions written verbatim
// into the index key list. Wrap an expression yourself when the database
// demands it — PostgreSQL needs "(a + b)" for arithmetic — and append
// anything that belongs outside the parentheses, like an operator class:
// "lower(email) text_pattern_ops". MySQL is the one exception: it requires
// every functional key part parenthesized, so each expression gains one
// pair there.
func (t *Table) IndexExpr(name string, exprs ...string) *Index {
	return t.exprIndex(name, exprs, false)
}

// UniqueExpr is the unique form of IndexExpr.
func (t *Table) UniqueExpr(name string, exprs ...string) *Index {
	return t.exprIndex(name, exprs, true)
}

func (t *Table) exprIndex(name string, exprs []string, unique bool) *Index {
	idx := &indexDef{name: name, exprs: exprs, unique: unique}
	if name == "" {
		t.errf("expression index on table %q needs an explicit name: expressions cannot form a conventional one", t.table)
	}
	if len(exprs) == 0 {
		t.errf("expression index %q on table %q declares no expressions", name, t.table)
	}
	for _, e := range exprs {
		if strings.TrimSpace(e) == "" {
			t.errf("expression index %q on table %q declares an empty expression", name, t.table)
		}
	}
	t.addIndexDef(idx)
	return &Index{def: idx}
}

// Primary declares a composite primary key.
func (t *Table) Primary(columns ...string) {
	if len(columns) == 0 {
		t.errf("primary key on table %q declares no columns", t.table)
		return
	}
	if t.create != nil {
		t.create.primary = columns
	} else {
		t.alter.changes = append(t.alter.changes, &addPrimary{columns: columns})
	}
}

// Check declares a named CHECK constraint with a verbatim SQL expression.
// SQLite alterations must use Schema.Recreate instead.
func (t *Table) Check(name, expr string) {
	if name == "" || expr == "" {
		t.errf("check constraint on table %q needs both a name and an expression", t.table)
		return
	}
	chk := &checkDef{name: name, expr: expr}
	if t.create != nil {
		t.create.checks = append(t.create.checks, chk)
	} else {
		t.alter.changes = append(t.alter.changes, &addCheck{chk: chk})
	}
}

// Foreign declares a foreign key on existing columns.
func (t *Table) Foreign(columns ...string) *ForeignKey {
	fk := &foreignDef{columns: columns}
	if len(columns) == 0 {
		t.errf("foreign key on table %q declares no columns", t.table)
	}
	t.addForeignDef(fk)
	return &ForeignKey{def: fk, table: t}
}

// Comment sets a table comment. SQLite ignores comments; altering one is
// irreversible.
func (t *Table) Comment(comment string) {
	if t.create != nil {
		t.create.comment = comment
		return
	}
	t.alter.changes = append(t.alter.changes, &setTableComment{comment: comment})
}

// ClickHouseEngine sets the complete ClickHouse storage fragment rendered
// after ENGINE =. It may include engine parameters, PARTITION BY, ORDER BY,
// PRIMARY KEY, SAMPLE BY, TTL, and storage SETTINGS. It is only valid inside
// Schema.Create; omit ENGINE =, table COMMENT, and a trailing semicolon.
// Other dialects ignore it.
func (t *Table) ClickHouseEngine(clause string) {
	if !t.allowClickHouseEngine {
		t.errf("ClickHouseEngine is only valid inside Schema.Create (table %q)", t.table)
		return
	}
	if t.create.clickHouseEngineSet {
		t.errf("ClickHouseEngine is declared more than once for table %q", t.table)
		return
	}
	t.create.clickHouseEngineSet = true
	trimmed := strings.TrimSpace(clause)
	if trimmed == "" {
		t.errf("ClickHouseEngine for table %q must not be empty", t.table)
		return
	}
	upper := strings.ToUpper(trimmed)
	if strings.HasPrefix(upper, "ENGINE=") || strings.HasPrefix(upper, "ENGINE =") {
		t.errf("ClickHouseEngine for table %q must omit ENGINE =", t.table)
		return
	}
	if strings.HasSuffix(trimmed, ";") {
		t.errf("ClickHouseEngine for table %q must omit the trailing semicolon", t.table)
		return
	}
	t.create.clickHouseEngine = clause
}

// RenameIndex renames an index. It reverses to the opposite rename. SQLite
// cannot rename indexes; dropping and re-declaring is the portable route.
func (t *Table) RenameIndex(from, to string) {
	if t.alterOnly("RenameIndex") {
		t.alter.changes = append(t.alter.changes, &renameIndex{from: from, to: to})
	}
}

// RenameColumn renames a column. It reverses to the opposite rename.
func (t *Table) RenameColumn(from, to string) {
	if t.alterOnly("RenameColumn") {
		t.alter.changes = append(t.alter.changes, &renameColumn{from: from, to: to})
	}
}

// DropColumn removes columns and is irreversible without WithDown.
func (t *Table) DropColumn(names ...string) {
	if !t.alterOnly("DropColumn") {
		return
	}
	for _, name := range names {
		t.alter.changes = append(t.alter.changes, &dropColumn{name: name})
	}
}

// DropIndex removes the conventional index for the columns.
func (t *Table) DropIndex(columns ...string) {
	if t.alterOnly("DropIndex") {
		t.alter.changes = append(t.alter.changes, &dropIndex{name: indexName(t.table, columns, "index")})
	}
}

// DropUnique removes the conventional unique index for the columns.
func (t *Table) DropUnique(columns ...string) {
	if t.alterOnly("DropUnique") {
		t.alter.changes = append(t.alter.changes, &dropIndex{name: indexName(t.table, columns, "unique")})
	}
}

// DropFullText removes the conventional full-text index for the columns.
func (t *Table) DropFullText(columns ...string) {
	if t.alterOnly("DropFullText") {
		t.alter.changes = append(t.alter.changes, &dropIndex{name: indexName(t.table, columns, "fulltext")})
	}
}

// DropSpatial removes the conventional spatial index for the columns.
func (t *Table) DropSpatial(columns ...string) {
	if t.alterOnly("DropSpatial") {
		t.alter.changes = append(t.alter.changes, &dropIndex{name: indexName(t.table, columns, "spatial")})
	}
}

// DropIndexByName removes an index by its exact name.
func (t *Table) DropIndexByName(name string) {
	if t.alterOnly("DropIndexByName") {
		t.alter.changes = append(t.alter.changes, &dropIndex{name: name})
	}
}

// DropForeign removes the conventional foreign key for the columns.
func (t *Table) DropForeign(columns ...string) {
	if t.alterOnly("DropForeign") {
		t.alter.changes = append(t.alter.changes, &dropForeign{name: foreignName(t.table, columns)})
	}
}

// DropForeignByName removes a foreign key by its exact name.
func (t *Table) DropForeignByName(name string) {
	if t.alterOnly("DropForeignByName") {
		t.alter.changes = append(t.alter.changes, &dropForeign{name: name})
	}
}

// DropCheck removes a CHECK constraint and is irreversible.
func (t *Table) DropCheck(name string) {
	if t.alterOnly("DropCheck") {
		t.alter.changes = append(t.alter.changes, &dropCheck{name: name})
	}
}

// DropPrimary removes the table's primary key.
func (t *Table) DropPrimary() {
	if t.alterOnly("DropPrimary") {
		t.alter.changes = append(t.alter.changes, &dropPrimary{})
	}
}

// guessParentTable handles conventional *_id names and regular plurals.
func guessParentTable(column string) string {
	base := strings.TrimSuffix(column, "_id")
	if base == column || base == "" {
		return ""
	}
	switch {
	case len(base) >= 2 && strings.HasSuffix(base, "y") && !strings.ContainsRune("aeiou", rune(base[len(base)-2])):
		return base[:len(base)-1] + "ies"
	case strings.HasSuffix(base, "s") || strings.HasSuffix(base, "x") || strings.HasSuffix(base, "z") ||
		strings.HasSuffix(base, "ch") || strings.HasSuffix(base, "sh"):
		return base + "es"
	default:
		return base + "s"
	}
}
