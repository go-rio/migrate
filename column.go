package migrate

// DefaultLiteral contains the scalar types Default renders portably.
// SQL expressions belong in DefaultExpr.
type DefaultLiteral interface {
	~bool | ~string |
		~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64
}

// Column configures a declared column.
type Column struct {
	def *columnDef
}

// Nullable allows NULL values; columns are NOT NULL by default.
func (c *Column) Nullable() *Column {
	c.def.nullable = true
	return c
}

// Default sets a portable scalar default. Use DefaultExpr for SQL.
func (c *Column) Default[V DefaultLiteral](value V) *Column {
	c.def.hasDefault = true
	c.def.defaultVal = value
	return c
}

// DefaultExpr sets a verbatim SQL default expression.
func (c *Column) DefaultExpr(expr string) *Column {
	c.def.hasDefault = true
	c.def.defaultExpr = expr
	return c
}

// UseCurrent defaults the column to the current timestamp.
func (c *Column) UseCurrent() *Column {
	c.def.useCurrent = true
	return c
}

// UseCurrentOnUpdate enables MySQL's automatic timestamp refresh.
// Other dialects ignore it.
func (c *Column) UseCurrentOnUpdate() *Column {
	c.def.useCurrentOnUpdate = true
	return c
}

// Unsigned uses MySQL's unsigned integer type. Other dialects ignore it.
func (c *Column) Unsigned() *Column {
	c.def.unsigned = true
	return c
}

// AutoIncrement makes an integer column a database-generated primary key.
// It cannot be nullable or have a default.
func (c *Column) AutoIncrement() *Column {
	c.def.autoIncr = true
	c.def.primary = true
	return c
}

// Primary makes this column the table's primary key.
func (c *Column) Primary() *Column {
	c.def.primary = true
	return c
}

// Unique adds a conventionally named unique index.
func (c *Column) Unique() *Column {
	c.def.unique = true
	return c
}

// Index adds a conventionally named index.
func (c *Column) Index() *Column {
	c.def.indexed = true
	return c
}

// Comment sets a column comment; SQLite ignores it.
func (c *Column) Comment(comment string) *Column {
	c.def.comment = comment
	return c
}

// After positions an altered MySQL column after another column.
func (c *Column) After(column string) *Column {
	c.def.after = column
	return c
}

// First positions an altered MySQL column first.
func (c *Column) First() *Column {
	c.def.first = true
	return c
}

// StoredAs makes this a stored generated column using a verbatim expression.
func (c *Column) StoredAs(expr string) *Column {
	c.def.generatedExpr = expr
	c.def.generatedVirtual = false
	return c
}

// VirtualAs makes this a virtual generated column using a verbatim expression.
func (c *Column) VirtualAs(expr string) *Column {
	c.def.generatedExpr = expr
	c.def.generatedVirtual = true
	return c
}

// Change restates a column's complete target definition inside Schema.Table.
// SQLite requires Schema.Recreate. The change is irreversible without
// WithDown; indexes and constraints must be changed separately.
func (c *Column) Change() *Column {
	c.def.change = true
	return c
}

// Using sets the PostgreSQL conversion expression for Change.
// Other dialects reject it.
func (c *Column) Using(expr string) *Column {
	c.def.changeUsing = expr
	return c
}

// CopyFrom sets the SELECT expression used to fill this column during
// Schema.Recreate. It has no effect elsewhere.
func (c *Column) CopyFrom(expr string) *Column {
	c.def.copyFrom = expr
	return c
}

// SkipCopy omits this column while Schema.Recreate copies old rows.
func (c *Column) SkipCopy() *Column {
	c.def.skipCopy = true
	return c
}

// Index configures a declared index.
type Index struct {
	def *indexDef
}

// Name overrides the conventional index name.
func (i *Index) Name(name string) *Index {
	i.def.name = name
	return i
}

// Where makes this a partial index with a verbatim predicate.
// MySQL rejects partial indexes.
func (i *Index) Where(predicate string) *Index {
	i.def.where = predicate
	return i
}

// Using sets the PostgreSQL or MySQL index method. SQLite rejects it.
func (i *Index) Using(method string) *Index {
	i.def.using = method
	return i
}

// Include adds PostgreSQL covering-index columns. Other dialects reject it.
func (i *Index) Include(columns ...string) *Index {
	i.def.include = append(i.def.include, columns...)
	return i
}

// NullsNotDistinct enables PostgreSQL 15+'s NULLS NOT DISTINCT.
func (i *Index) NullsNotDistinct() *Index {
	i.def.nullsNotDistinct = true
	return i
}

// Concurrently builds and drops a PostgreSQL index without blocking writes.
// The migration must use WithoutTransaction. Other dialects ignore it.
func (i *Index) Concurrently() *Index {
	i.def.concurrently = true
	return i
}

// ForeignKey configures a declared foreign key.
type ForeignKey struct {
	def   *foreignDef
	table *Table
}

// References sets the parent table and columns; columns default to "id".
func (f *ForeignKey) References(table string, columns ...string) *ForeignKey {
	f.def.refTable = table
	f.def.refColumns = columns
	if len(columns) == 0 {
		f.def.refColumns = []string{"id"}
	}
	return f
}

// Name overrides the conventional foreign-key name.
func (f *ForeignKey) Name(name string) *ForeignKey {
	f.def.name = name
	return f
}

// CascadeOnDelete deletes child rows when the parent row is deleted.
func (f *ForeignKey) CascadeOnDelete() *ForeignKey { f.def.onDelete = "CASCADE"; return f }

// RestrictOnDelete rejects deleting a parent row that still has children.
func (f *ForeignKey) RestrictOnDelete() *ForeignKey { f.def.onDelete = "RESTRICT"; return f }

// NullOnDelete sets the child columns to NULL when the parent row is deleted.
func (f *ForeignKey) NullOnDelete() *ForeignKey { f.def.onDelete = "SET NULL"; return f }

// NoActionOnDelete defers enforcement where the dialect supports it.
func (f *ForeignKey) NoActionOnDelete() *ForeignKey { f.def.onDelete = "NO ACTION"; return f }

// CascadeOnUpdate propagates key updates to child rows.
func (f *ForeignKey) CascadeOnUpdate() *ForeignKey { f.def.onUpdate = "CASCADE"; return f }

// RestrictOnUpdate rejects updating a key that still has children.
func (f *ForeignKey) RestrictOnUpdate() *ForeignKey { f.def.onUpdate = "RESTRICT"; return f }

// NullOnUpdate sets the child columns to NULL when the parent key changes.
func (f *ForeignKey) NullOnUpdate() *ForeignKey { f.def.onUpdate = "SET NULL"; return f }

// ForeignColumn configures a ForeignID column and its constraint.
type ForeignColumn struct {
	*Column
	table *Table
	fk    *ForeignKey // non-nil once Constrained or References was called
}

// Constrained references id on the given table, or infers it from a *_id name.
func (fc *ForeignColumn) Constrained(table ...string) *ForeignColumn {
	parent := optional(table, "")
	if parent == "" {
		parent = guessParentTable(fc.def.name)
		if parent == "" {
			fc.table.errf("cannot infer the parent table of column %q; pass it to Constrained explicitly", fc.def.name)
		}
	}
	return fc.References(parent, "id")
}

// References adds a foreign key to the table; columns default to "id".
func (fc *ForeignColumn) References(table string, columns ...string) *ForeignColumn {
	if fc.fk == nil {
		fc.fk = fc.table.Foreign(fc.def.name)
	}
	fc.fk.References(table, columns...)
	return fc
}

func (fc *ForeignColumn) constrainedOnly(method string) {
	if fc.fk == nil {
		fc.table.errf("%s on column %q requires Constrained or References first", method, fc.def.name)
	}
}

// CascadeOnDelete deletes child rows when the parent row is deleted.
func (fc *ForeignColumn) CascadeOnDelete() *ForeignColumn {
	fc.constrainedOnly("CascadeOnDelete")
	if fc.fk != nil {
		fc.fk.CascadeOnDelete()
	}
	return fc
}

// RestrictOnDelete rejects deleting a parent row that still has children.
func (fc *ForeignColumn) RestrictOnDelete() *ForeignColumn {
	fc.constrainedOnly("RestrictOnDelete")
	if fc.fk != nil {
		fc.fk.RestrictOnDelete()
	}
	return fc
}

// NullOnDelete sets the column to NULL when the parent row is deleted.
func (fc *ForeignColumn) NullOnDelete() *ForeignColumn {
	fc.constrainedOnly("NullOnDelete")
	if fc.fk != nil {
		fc.fk.NullOnDelete()
	}
	return fc
}

// Nullable allows NULL while preserving the ForeignColumn chain.
func (fc *ForeignColumn) Nullable() *ForeignColumn {
	fc.Column.Nullable()
	return fc
}

// Unique adds a unique index while preserving the ForeignColumn chain.
func (fc *ForeignColumn) Unique() *ForeignColumn {
	fc.Column.Unique()
	return fc
}

// Index adds an index while preserving the ForeignColumn chain.
func (fc *ForeignColumn) Index() *ForeignColumn {
	fc.Column.Index()
	return fc
}
