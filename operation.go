package migrate

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// operation is one compilable, reversible migration step.
type operation interface {
	// inverse fails with ErrIrreversible after information has been discarded.
	inverse() (operation, error)
}

func irreversible(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrIrreversible, fmt.Sprintf(format, a...))
}

// colKind is the portable column type understood by each dialect.
type colKind int

const (
	kindRaw colKind = iota
	kindString
	kindChar
	kindText
	kindTinyInt
	kindSmallInt
	kindInt
	kindBigInt
	kindBool
	kindDecimal
	kindFloat
	kindDouble
	kindDate
	kindTime
	kindDateTime
	kindTimestamp
	kindTimestampTz
	kindJSON
	kindUUID
	kindBinary
	kindEnum
)

type columnDef struct {
	name      string
	kind      colKind
	rawType   string // kindRaw only
	length    int    // string/char/binary
	precision int    // decimal/float/time precision
	scale     int    // decimal
	enumVals  []string

	unsigned bool
	autoIncr bool
	primary  bool

	nullable           bool
	hasDefault         bool
	defaultVal         any
	defaultExpr        string
	useCurrent         bool
	useCurrentOnUpdate bool

	unique  bool
	indexed bool
	comment string
	after   string
	first   bool

	generatedExpr    string // GENERATED ALWAYS AS (expr)
	generatedVirtual bool   // VIRTUAL instead of STORED

	skipCopy bool   // Recreate: no matching column in the old table
	copyFrom string // Recreate: SELECT expression replacing the by-name copy

	change      bool   // Schema.Table only: restate the column instead of adding it
	changeUsing string // Change on Postgres: USING expression converting existing rows
}

func (c *columnDef) integerKind() bool {
	switch c.kind {
	case kindTinyInt, kindSmallInt, kindInt, kindBigInt:
		return true
	default:
		return false
	}
}

// Auto-incrementing keys render inline because SQLite requires it.
func (c *columnDef) inlinePrimary() bool {
	return c.autoIncr && c.primary
}

type indexDef struct {
	name     string // empty means the conventional name
	columns  []string
	exprs    []string // expression index: SQL rendered verbatim instead of columns
	unique   bool
	fulltext bool // MySQL FULLTEXT
	spatial  bool // MySQL SPATIAL

	where            string   // partial index predicate (Postgres, SQLite)
	using            string   // index method: gin/gist/brin/hash on Postgres, btree/hash on MySQL
	include          []string // covering columns (Postgres INCLUDE)
	nullsNotDistinct bool     // Postgres 15+: NULLS NOT DISTINCT on a unique index
	concurrently     bool     // Postgres: CREATE INDEX CONCURRENTLY (needs WithoutTransaction)
}

// suffix participates in reconstructible conventional index names.
func (i *indexDef) suffix() string {
	switch {
	case i.fulltext:
		return "fulltext"
	case i.spatial:
		return "spatial"
	case i.unique:
		return "unique"
	default:
		return "index"
	}
}

type checkDef struct {
	name string
	expr string
}

// uniqueConstraintDef is a named table-level UNIQUE constraint — the form ON
// CONFLICT ON CONSTRAINT can reference, unlike a unique index.
type uniqueConstraintDef struct {
	name    string
	columns []string
}

type foreignDef struct {
	name       string // empty means the conventional name
	columns    []string
	refTable   string
	refColumns []string
	onDelete   string
	onUpdate   string
}

// partitionBy is a PostgreSQL declarative partitioning clause on a parent.
type partitionBy struct {
	method  string // RANGE, LIST, HASH
	columns []string
}

type tableDef struct {
	name                string
	columns             []*columnDef
	indexes             []*indexDef
	checks              []*checkDef
	uniques             []*uniqueConstraintDef
	fks                 []*foreignDef
	partition           *partitionBy
	primary             []string // composite primary key columns
	comment             string
	clickHouseEngine    string
	clickHouseEngineSet bool
	errs                []error // declaration mistakes, surfaced at compile time

	// Recreate uses the final name while compiling temporary constraints.
	constraintBase string
}

func (d *tableDef) constraintTable() string {
	if d.constraintBase != "" {
		return d.constraintBase
	}
	return d.name
}

// Conventional names use the unqualified table name and can be reconstructed.

func indexName(table string, columns []string, suffix string) string {
	return baseName(table) + "_" + strings.Join(columns, "_") + "_" + suffix
}

func foreignName(table string, columns []string) string {
	return baseName(table) + "_" + strings.Join(columns, "_") + "_foreign"
}

func primaryName(table string) string {
	return baseName(table) + "_pkey"
}

func (i *indexDef) resolvedName(table string) string {
	if i.name != "" {
		return i.name
	}
	return indexName(table, i.columns, i.suffix())
}

func (f *foreignDef) resolvedName(table string) string {
	if f.name != "" {
		return f.name
	}
	return foreignName(table, f.columns)
}

type createTable struct {
	def *tableDef
}

func (o *createTable) inverse() (operation, error) {
	return &dropTable{name: o.def.name}, nil
}

type dropTable struct {
	name     string
	ifExists bool
}

func (o *dropTable) inverse() (operation, error) {
	return nil, irreversible("dropping table %q discards its definition", o.name)
}

type renameTable struct {
	from, to string
}

func (o *renameTable) inverse() (operation, error) {
	return &renameTable{from: o.to, to: o.from}, nil
}

type alterTable struct {
	table   string
	changes []change
	errs    []error
}

func (o *alterTable) inverse() (operation, error) {
	inv := &alterTable{table: o.table, changes: make([]change, 0, len(o.changes))}
	for _, v := range slices.Backward(o.changes) {
		cs, err := v.inverseChange(o.table)
		if err != nil {
			return nil, err
		}
		inv.changes = append(inv.changes, cs...)
	}
	return inv, nil
}

type recreateTable struct {
	def *tableDef
}

func (o *recreateTable) inverse() (operation, error) {
	return nil, irreversible("recreating table %q discards its previous definition", o.def.name)
}

// PartitionBound places one child under a partitioned parent. Construct it
// with ForValuesFromTo, ForValuesIn, or ForValuesWith; the zero value is
// invalid (CreateDefaultPartition covers DEFAULT).
type PartitionBound struct {
	kind      string // "range", "list", "hash"
	from, to  any
	in        []any
	modulus   int
	remainder int
}

// ForValuesFromTo bounds a RANGE partition: FOR VALUES FROM (from) TO (to).
// Values render as SQL literals.
func ForValuesFromTo(from, to any) PartitionBound {
	return PartitionBound{kind: "range", from: from, to: to}
}

// ForValuesIn bounds a LIST partition: FOR VALUES IN (values...).
func ForValuesIn(values ...any) PartitionBound {
	return PartitionBound{kind: "list", in: values}
}

// ForValuesWith bounds a HASH partition: FOR VALUES WITH (MODULUS m, REMAINDER r).
func ForValuesWith(modulus, remainder int) PartitionBound {
	return PartitionBound{kind: "hash", modulus: modulus, remainder: remainder}
}

type createPartition struct {
	child, parent string
	bound         *PartitionBound // nil renders DEFAULT
}

func (o *createPartition) inverse() (operation, error) {
	return &dropTable{name: o.child}, nil
}

type attachPartition struct {
	parent, child string
	bound         *PartitionBound // nil attaches DEFAULT
}

func (o *attachPartition) inverse() (operation, error) {
	return &detachPartition{parent: o.parent, child: o.child}, nil
}

type detachPartition struct {
	parent, child string
}

func (o *detachPartition) inverse() (operation, error) {
	return nil, irreversible("detaching partition %q of %q discards its bound; declare an explicit down with WithDown", o.child, o.parent)
}

type rawSQL struct {
	sql  string
	args []any
}

func (o *rawSQL) inverse() (operation, error) {
	return nil, irreversible("raw SQL cannot be reversed; declare an explicit down with WithDown")
}

type goFunc struct {
	fn func(context.Context, DB) error
}

func (o *goFunc) inverse() (operation, error) {
	return nil, irreversible("a Go function cannot be reversed; declare an explicit down with WithDown")
}

// change is one independently reversible table alteration.
type change interface {
	inverseChange(table string) ([]change, error)
}

type addColumn struct {
	col *columnDef
}

func (c *addColumn) inverseChange(table string) ([]change, error) {
	if c.col.change {
		return nil, irreversible("changing column %q of table %q discards its previous definition", c.col.name, table)
	}
	// SQLite requires dependent indexes to be dropped first.
	var out []change
	for _, idx := range inlineIndexes([]*columnDef{c.col}) {
		out = append(out, &dropIndex{name: idx.resolvedName(table)})
	}
	return append(out, &dropColumn{name: c.col.name}), nil
}

type dropColumn struct {
	name string
}

func (c *dropColumn) inverseChange(table string) ([]change, error) {
	return nil, irreversible("dropping column %q of table %q discards its definition", c.name, table)
}

type renameColumn struct {
	from, to string
}

func (c *renameColumn) inverseChange(string) ([]change, error) {
	return []change{&renameColumn{from: c.to, to: c.from}}, nil
}

type addIndex struct {
	idx *indexDef
}

func (c *addIndex) inverseChange(table string) ([]change, error) {
	// The rollback keeps the original migration's concurrent mode.
	return []change{&dropIndex{name: c.idx.resolvedName(table), concurrently: c.idx.concurrently}}, nil
}

type dropIndex struct {
	name         string
	concurrently bool // Postgres: DROP INDEX CONCURRENTLY
}

func (c *dropIndex) inverseChange(table string) ([]change, error) {
	return nil, irreversible("dropping index %q of table %q discards its definition", c.name, table)
}

type addForeign struct {
	fk *foreignDef
}

func (c *addForeign) inverseChange(table string) ([]change, error) {
	return []change{&dropForeign{name: c.fk.resolvedName(table)}}, nil
}

type dropForeign struct {
	name string
}

func (c *dropForeign) inverseChange(table string) ([]change, error) {
	return nil, irreversible("dropping foreign key %q of table %q discards its definition", c.name, table)
}

type addPrimary struct {
	columns []string
}

func (c *addPrimary) inverseChange(string) ([]change, error) {
	return []change{&dropPrimary{}}, nil
}

type dropPrimary struct{}

func (c *dropPrimary) inverseChange(table string) ([]change, error) {
	return nil, irreversible("dropping the primary key of table %q discards its definition", table)
}

type addUniqueConstraint struct {
	uc *uniqueConstraintDef
}

func (c *addUniqueConstraint) inverseChange(string) ([]change, error) {
	return []change{&dropConstraint{name: c.uc.name}}, nil
}

// dropConstraint drops any named table constraint by name.
type dropConstraint struct {
	name string
}

func (c *dropConstraint) inverseChange(table string) ([]change, error) {
	return nil, irreversible("dropping constraint %q of table %q discards its definition", c.name, table)
}

type addCheck struct {
	chk *checkDef
}

func (c *addCheck) inverseChange(string) ([]change, error) {
	return []change{&dropCheck{name: c.chk.name}}, nil
}

type dropCheck struct {
	name string
}

func (c *dropCheck) inverseChange(table string) ([]change, error) {
	return nil, irreversible("dropping check constraint %q of table %q discards its expression", c.name, table)
}

type renameIndex struct {
	from, to string
}

func (c *renameIndex) inverseChange(string) ([]change, error) {
	return []change{&renameIndex{from: c.to, to: c.from}}, nil
}

type setTableComment struct {
	comment string
}

func (c *setTableComment) inverseChange(table string) ([]change, error) {
	return nil, irreversible("changing the comment of table %q discards the previous comment", table)
}
