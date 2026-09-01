package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
)

var (
	// ErrIrreversible marks a migration with no automatic or explicit rollback.
	ErrIrreversible = errors.New("migrate: migration cannot be automatically reversed")
	// ErrLockTimeout marks a timed-out advisory lock acquisition.
	ErrLockTimeout = errors.New("migrate: timed out waiting for the migration lock")
	// ErrLockUnsupported indicates that the dialect has no built-in migration
	// lock. Callers may use WithoutLock only after serializing deployments
	// externally.
	ErrLockUnsupported = errors.New("migrate: migration lock unsupported")
	// ErrChecksumMismatch marks an edited applied migration.
	ErrChecksumMismatch = errors.New("migrate: checksum mismatch")
)

// Migration is one registered declaration; Collection.Add creates it and Name
// identifies it in the records table.
type Migration struct {
	name       string
	up         func(*Schema)
	down       func(*Schema) // nil means derive by reversing up
	useTx      bool
	repeatable bool
	assured    bool // reviewed: skip the safety analysis
}

// Name returns the migration's registered name.
func (m *Migration) Name() string { return m.name }

func (m *Migration) upOps() ([]operation, error) {
	s := &Schema{}
	m.up(s)
	return s.ops, validateSchema(m.name, s)
}

func (m *Migration) downOps() ([]operation, error) {
	if m.down != nil {
		s := &Schema{}
		m.down(s)
		return s.ops, validateSchema(m.name, s)
	}
	ups, err := m.upOps()
	if err != nil {
		return nil, err
	}
	downs := make([]operation, 0, len(ups))
	for _, up := range slices.Backward(ups) {
		inv, err := up.inverse()
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", m.name, err)
		}
		downs = append(downs, inv)
	}
	return downs, nil
}

// checksum covers compiled SQL and arguments, but not opaque Run functions.
// The encoding is injective (type-tagged, length-prefixed) and pointer-free.
func (m *Migration) checksum(d Dialect) (string, error) {
	stmts, err := m.compile(d, true)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, s := range stmts {
		if s.fn != nil {
			_, _ = io.WriteString(h, "g")
			continue
		}
		_, _ = fmt.Fprintf(h, "q%d:%s", len(s.sql), s.sql)
		for _, a := range s.args {
			if err := checksumArg(h, s, a); err != nil {
				return "", err
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (m *Migration) compile(d Dialect, up bool) ([]statement, error) {
	ops, err := m.upOps()
	if !up {
		ops, err = m.downOps()
	}
	if err != nil {
		return nil, err
	}
	if !m.useTx && d.transactionMode() != transactionModeNone {
		for _, op := range ops {
			if _, ok := op.(*recreateTable); ok {
				// A failure between DROP and rename would lose the live table.
				return nil, fmt.Errorf("migration %q: Recreate requires the migration's transaction; keep WithoutTransaction statements in a separate migration", m.name)
			}
		}
	}
	// PostgreSQL refuses concurrent index operations inside transactions.
	if m.useTx && d.name() == "postgres" {
		if idx := firstConcurrentIndex(ops); idx != "" {
			return nil, fmt.Errorf("migration %q: index %q builds Concurrently, which cannot run inside a transaction; declare the migration WithoutTransaction()", m.name, idx)
		}
	}
	var stmts []statement
	for _, op := range ops {
		s, err := d.compile(op)
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", m.name, err)
		}
		if operationCommitsImplicitly(op) {
			for i := range s {
				s[i].ddl = true
			}
		}
		stmts = append(stmts, s...)
	}
	return stmts, nil
}

// MigrationOption configures a single migration at registration time.
type MigrationOption func(*Migration)

// WithDown defines rollback for otherwise irreversible operations.
func WithDown(down func(*Schema)) MigrationOption {
	return func(m *Migration) { m.down = down }
}

// WithoutTransaction permits statements such as PostgreSQL CREATE INDEX
// CONCURRENTLY. Earlier statements may remain applied after a failure.
func WithoutTransaction() MigrationOption {
	return func(m *Migration) { m.useTx = false }
}

// Collection is a named migration set. Package-level Add uses a default one.
type Collection struct {
	mu     sync.Mutex
	byName map[string]*Migration
}

// NewCollection returns an empty collection.
func NewCollection() *Collection {
	return &Collection{byName: map[string]*Migration{}}
}

// Add registers a lexically ordered migration. It panics on an empty,
// whitespace-padded, or over-191-character name, on a duplicate name, or on a
// nil declaration.
func (c *Collection) Add(name string, up func(*Schema), opts ...MigrationOption) {
	c.add(name, up, false, opts)
}

// AddRepeatable registers an idempotent declaration that reruns when its SQL
// checksum changes. Repeatables run after versioned migrations and have no
// rollback; Reset forgets their records.
func (c *Collection) AddRepeatable(name string, run func(*Schema), opts ...MigrationOption) {
	c.add(name, run, true, opts)
}

func (c *Collection) add(name string, up func(*Schema), repeatable bool, opts []MigrationOption) {
	if name == "" {
		panic("migrate: migration name must not be empty")
	}
	if len(name) > 191 {
		panic(fmt.Sprintf("migrate: migration name %q exceeds 191 characters", name))
	}
	if strings.TrimSpace(name) != name {
		panic(fmt.Sprintf("migrate: migration name %q has leading or trailing whitespace", name))
	}
	if up == nil {
		panic(fmt.Sprintf("migrate: migration %q has a nil up function", name))
	}
	m := &Migration{name: name, up: up, useTx: true, repeatable: repeatable}
	for _, opt := range opts {
		opt(m)
	}
	if m.repeatable && m.down != nil {
		panic(fmt.Sprintf("migrate: repeatable migration %q cannot declare WithDown", name))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.byName[name]; dup {
		panic(fmt.Sprintf("migrate: migration %q registered twice", name))
	}
	c.byName[name] = m
}

func (c *Collection) get(name string) *Migration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byName[name]
}

func (c *Collection) sorted() []*Migration {
	return c.list(false)
}

func (c *Collection) repeatables() []*Migration {
	return c.list(true)
}

func (c *Collection) list(repeatable bool) []*Migration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ms []*Migration
	for _, m := range c.byName {
		if m.repeatable == repeatable {
			ms = append(ms, m)
		}
	}
	slices.SortFunc(ms, func(a, b *Migration) int { return strings.Compare(a.name, b.name) })
	return ms
}

var defaultCollection = NewCollection()

// Add registers a migration in the default collection.
func Add(name string, up func(*Schema), opts ...MigrationOption) {
	defaultCollection.Add(name, up, opts...)
}

// AddRepeatable registers a repeatable migration in the default collection.
func AddRepeatable(name string, run func(*Schema), opts ...MigrationOption) {
	defaultCollection.AddRepeatable(name, run, opts...)
}

func validateSchema(name string, s *Schema) error {
	errs := append([]error(nil), s.errs...)
	check := func(table string, cols []*columnDef, altering bool) {
		for _, c := range cols {
			if c.autoIncr {
				switch {
				case !c.integerKind() && c.kind != kindRaw:
					errs = append(errs, fmt.Errorf("auto-increment column %q of table %q must be an integer", c.name, table))
				case c.hasDefault:
					errs = append(errs, fmt.Errorf("auto-increment column %q of table %q cannot have a default value", c.name, table))
				case c.nullable:
					errs = append(errs, fmt.Errorf("auto-increment column %q of table %q cannot be nullable", c.name, table))
				}
			}
			suppliesValue := c.hasDefault || c.useCurrent || c.autoIncr
			if c.generatedExpr != "" && suppliesValue {
				errs = append(errs, fmt.Errorf("generated column %q of table %q cannot combine with defaults or auto-increment", c.name, table))
			}
			if c.copyFrom != "" && c.skipCopy {
				errs = append(errs, fmt.Errorf("column %q of table %q declares both CopyFrom and SkipCopy", c.name, table))
			}
			if c.primary && c.nullable {
				// SQLite otherwise accepts the contradictory NULL primary key.
				errs = append(errs, fmt.Errorf("primary key column %q of table %q cannot be nullable", c.name, table))
			}
			if c.change && !altering {
				errs = append(errs, fmt.Errorf("column %q of table %q declares Change, which is only valid inside Schema.Table", c.name, table))
			}
			if c.changeUsing != "" && !c.change {
				errs = append(errs, fmt.Errorf("column %q of table %q declares Using without Change", c.name, table))
			}
			if c.change {
				if c.unique || c.indexed {
					errs = append(errs, fmt.Errorf("changed column %q of table %q cannot restate Unique/Index modifiers; the existing indexes stay — declare index changes separately", c.name, table))
				}
				restatesIdentity := c.autoIncr || c.primary || c.generatedExpr != ""
				if restatesIdentity {
					errs = append(errs, fmt.Errorf("changed column %q of table %q cannot restate auto-increment, primary key or generated expressions; use Exec for those", c.name, table))
				}
			}
		}
	}
	checkTablePK := func(def *tableDef) {
		nullable := make(map[string]bool, len(def.columns))
		for _, c := range def.columns {
			if _, dup := nullable[c.name]; dup {
				errs = append(errs, fmt.Errorf("table %q declares column %q twice", def.name, c.name))
			}
			nullable[c.name] = c.nullable
		}
		for _, p := range def.primary {
			if nullable[p] {
				errs = append(errs, fmt.Errorf("primary key column %q of table %q cannot be nullable", p, def.name))
			}
		}
	}
	for _, op := range s.ops {
		switch o := op.(type) {
		case *createTable:
			errs = append(errs, o.def.errs...)
			check(o.def.name, o.def.columns, false)
			checkTablePK(o.def)
		case *recreateTable:
			errs = append(errs, o.def.errs...)
			check(o.def.name, o.def.columns, false)
			checkTablePK(o.def)
		case *alterTable:
			errs = append(errs, o.errs...)
			for _, ch := range o.changes {
				if add, ok := ch.(*addColumn); ok {
					check(o.table, []*columnDef{add.col}, true)
				}
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("migration %q: %w", name, declarationErrors(errs))
}

// checksumArg writes one argument's canonical form; unsupported types fail
// rather than hash lossily.
func checksumArg(h io.Writer, s statement, a any) error {
	if a == nil {
		_, _ = io.WriteString(h, "n")
		return nil
	}
	rv := reflect.ValueOf(a)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			_, _ = io.WriteString(h, "n")
			return nil
		}
		rv = rv.Elem()
	}
	if t, ok := rv.Interface().(time.Time); ok {
		// RFC 3339 in UTC rather than UnixNano: no epoch-range surprises.
		stamp := t.UTC().Format(time.RFC3339Nano)
		_, _ = fmt.Fprintf(h, "t%d:%s", len(stamp), stamp)
		return nil
	}
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		_, _ = fmt.Fprintf(h, "i%d", rv.Int())
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		_, _ = fmt.Fprintf(h, "u%d", rv.Uint())
		return nil
	case reflect.Float32, reflect.Float64:
		// Bit-exact: %v would fold 1.0 into 1 and drift on formatting rules.
		_, _ = fmt.Fprintf(h, "f%x", math.Float64bits(rv.Float()))
		return nil
	case reflect.Bool:
		_, _ = fmt.Fprintf(h, "b%t", rv.Bool())
		return nil
	case reflect.String:
		str := rv.String()
		_, _ = fmt.Fprintf(h, "s%d:%s", len(str), str)
		return nil
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			b := rv.Bytes()
			_, _ = fmt.Fprintf(h, "x%d:", len(b))
			_, _ = h.Write(b)
			return nil
		}
	}
	return fmt.Errorf(
		"migrate: cannot checksum a %T argument (statement %q); pass a plain scalar, []byte, or time.Time",
		a, describeStatement(s),
	)
}

// firstConcurrentIndex names the first index declared Concurrently, or "".
func firstConcurrentIndex(ops []operation) string {
	fromDef := func(def *tableDef) string {
		for _, idx := range def.indexes {
			if idx.concurrently {
				return idx.resolvedName(def.name)
			}
		}
		return ""
	}
	for _, op := range ops {
		switch o := op.(type) {
		case *createTable:
			if n := fromDef(o.def); n != "" {
				return n
			}
		case *recreateTable:
			if n := fromDef(o.def); n != "" {
				return n
			}
		case *alterTable:
			for _, ch := range o.changes {
				switch c := ch.(type) {
				case *addIndex:
					if c.idx.concurrently {
						return c.idx.resolvedName(o.table)
					}
				case *dropIndex:
					if c.concurrently {
						return c.name
					}
				}
			}
		}
	}
	return ""
}

// operationCommitsImplicitly classifies MySQL's transaction-ending DDL.
func operationCommitsImplicitly(op operation) bool {
	switch o := op.(type) {
	case *rawSQL:
		return !plainDMLSQL(o.sql)
	case *goFunc:
		return false
	default:
		return true
	}
}

// plainDMLSQL stays conservative so failure reports never understate commits.
func plainDMLSQL(sql string) bool {
	fields := strings.Fields(sql)
	if len(fields) == 0 {
		return false
	}
	switch strings.ToUpper(fields[0]) {
	case "INSERT", "UPDATE", "DELETE", "REPLACE", "SELECT", "WITH", "VALUES", "DO":
		return true
	}
	return false
}
