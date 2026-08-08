package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Text avoids driver-specific time decoding in the records table.
const appliedAtFormat = "2006-01-02T15:04:05.000000Z"

// Repeatables belong to no rollback batch.
const repeatableBatch = -1

// Migrator applies one Collection to a database. Advisory locks serialize
// concurrent processes unless WithoutLock is set.
type Migrator struct {
	db  *sql.DB
	d   Dialect
	cfg config
}

// New creates a Migrator without taking ownership of db. The dialect must
// match the database driver.
func New(db *sql.DB, dialect Dialect, opts ...Option) (*Migrator, error) {
	if db == nil {
		return nil, errors.New("migrate: db must not be nil")
	}
	if dialect == nil {
		return nil, errors.New("migrate: dialect must not be nil")
	}
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Migrator{db: db, d: dialect, cfg: cfg}, nil
}

type record struct {
	version   string
	batch     int
	checksum  string
	appliedAt string
}

// Up applies pending versioned migrations as one batch, then runs changed
// repeatables. Each migration uses its own transaction unless opted out.
func (m *Migrator) Up(ctx context.Context) error {
	return m.locked(ctx, func(conn *sql.Conn) error {
		recs, err := m.loadState(ctx, conn)
		if err != nil {
			return err
		}
		if err := m.verifyChecksums(recs); err != nil {
			return err
		}

		applied := make(map[string]bool, len(recs))
		batch := 0
		for _, r := range recs {
			applied[r.version] = true
			batch = max(batch, r.batch)
		}
		batch++

		var pending []*Migration
		for _, mig := range m.cfg.collection.sorted() {
			if !applied[mig.name] {
				pending = append(pending, mig)
			}
		}
		due, dueExists, err := m.dueRepeatables(recs)
		if err != nil {
			return err
		}
		// Strict safety must reject the whole run before execution starts.
		if err := m.checkSafety(append(append([]*Migration(nil), pending...), due...)); err != nil {
			return err
		}

		for _, mig := range pending {
			bookkeep, err := m.insertRecord(mig, batch)
			if err != nil {
				return err
			}
			if err := m.runOne(ctx, conn, mig, true, bookkeep); err != nil {
				return err
			}
		}
		for i, mig := range due {
			var bookkeep statement
			var err error
			if dueExists[i] {
				bookkeep, err = m.updateRecord(mig)
			} else {
				bookkeep, err = m.insertRecord(mig, repeatableBatch)
			}
			if err != nil {
				return err
			}
			if err := m.runOne(ctx, conn, mig, true, bookkeep); err != nil {
				return err
			}
		}
		if len(pending)+len(due) == 0 {
			m.cfg.logger.Info("migrate: nothing to apply")
		}
		return nil
	})
}

// dueRepeatables returns new and checksum-changed repeatables.
func (m *Migrator) dueRepeatables(recs []record) (due []*Migration, dueExists []bool, err error) {
	recorded := make(map[string]string)
	for _, r := range recs {
		if r.batch == repeatableBatch {
			recorded[r.version] = strings.TrimSpace(r.checksum)
		}
	}
	for _, mig := range m.cfg.collection.repeatables() {
		sum, err := mig.checksum(m.d)
		if err != nil {
			return nil, nil, err
		}
		prev, exists := recorded[mig.name]
		if exists && prev == sum {
			continue
		}
		due = append(due, mig)
		dueExists = append(dueExists, exists)
	}
	return due, dueExists, nil
}

type rollbackSpec struct {
	steps int  // > 0: that many most recent migrations
	batch bool // the whole latest batch
	reset bool // everything, baselined rows included
}

// Rollback reverses the latest steps versioned migrations. Irreversible
// operations fail with ErrIrreversible, and baselined rows are not touched.
func (m *Migrator) Rollback(ctx context.Context, steps int) error {
	if steps < 1 {
		return fmt.Errorf("migrate: Rollback requires a positive step count, got %d", steps)
	}
	return m.rollback(ctx, rollbackSpec{steps: steps})
}

// RollbackBatch reverses the latest batch without touching baselined rows.
func (m *Migrator) RollbackBatch(ctx context.Context) error {
	return m.rollback(ctx, rollbackSpec{batch: true})
}

// Reset reverses all versioned migrations, including baselined rows.
func (m *Migrator) Reset(ctx context.Context) error {
	return m.rollback(ctx, rollbackSpec{reset: true})
}

func (m *Migrator) rollback(ctx context.Context, spec rollbackSpec) error {
	return m.locked(ctx, func(conn *sql.Conn) error {
		targets, err := m.rollbackTargets(ctx, conn, spec)
		if err != nil {
			return err
		}
		if len(targets) == 0 {
			m.cfg.logger.Info("migrate: nothing to roll back")
			return nil
		}
		for _, mig := range targets {
			if err := m.runOne(ctx, conn, mig, false, m.deleteRecord(mig)); err != nil {
				return err
			}
		}
		if spec.reset {
			// Repeatables have no down; forgetting them makes the next Up rerun all.
			query := fmt.Sprintf("DELETE FROM %s WHERE batch = %s",
				m.d.quoteIdent(m.cfg.table), m.d.placeholder(1))
			if _, err := conn.ExecContext(ctx, query, repeatableBatch); err != nil {
				return fmt.Errorf("migrate: forget repeatable records: %w", err)
			}
		}
		return nil
	})
}

func (m *Migrator) rollbackTargets(ctx context.Context, conn *sql.Conn, spec rollbackSpec) ([]*Migration, error) {
	recs, err := m.loadState(ctx, conn)
	if err != nil {
		return nil, err
	}
	recs = slices.DeleteFunc(recs, func(r record) bool { return r.batch == repeatableBatch })
	if !spec.reset {
		// Only Reset may reverse schema this tool merely baselined.
		recs = slices.DeleteFunc(recs, func(r record) bool { return r.batch == 0 })
	}
	if len(recs) == 0 {
		return nil, nil
	}
	// Reverse application order within and across batches.
	slices.SortFunc(recs, func(a, b record) int {
		if a.batch != b.batch {
			return b.batch - a.batch
		}
		return strings.Compare(b.version, a.version)
	})
	switch {
	case spec.reset:
	case spec.steps > 0:
		recs = recs[:min(spec.steps, len(recs))]
	default:
		latest := recs[0].batch
		for i, r := range recs {
			if r.batch != latest {
				recs = recs[:i]
				break
			}
		}
	}
	targets := make([]*Migration, len(recs))
	for i, r := range recs {
		mig := m.cfg.collection.get(r.version)
		if mig == nil {
			return nil, fmt.Errorf("migrate: cannot roll back %q: not registered in this build (was the migration file deleted?)", r.version)
		}
		targets[i] = mig
	}
	return targets, nil
}

func (m *Migrator) runOne(ctx context.Context, conn *sql.Conn, mig *Migration, up bool, bookkeep statement) error {
	verb, verbed := "apply", "applied"
	if !up {
		verb, verbed = "roll back", "rolled back"
	}
	stmts, err := mig.compile(m.d, up)
	if err != nil {
		return err
	}
	arbiter := -1
	if up && mig.useTx && m.d.name() == "sqlite" {
		// SQLite has no advisory lock. Writing the record first makes a racing
		// migrator lose before it can change the schema.
		stmts = append([]statement{bookkeep}, stmts...)
		arbiter = 0
	} else {
		stmts = append(stmts, bookkeep)
	}

	start := time.Now()
	if mig.useTx {
		err = m.runInTx(ctx, conn, stmts, arbiter)
	} else {
		_, err = runStatements(ctx, conn, stmts)
	}
	if err != nil {
		return fmt.Errorf("migrate: %s %q: %w", verb, mig.name, err)
	}
	m.cfg.logger.Info("migrate: "+verbed, "migration", mig.name, "repeatable", mig.repeatable,
		"duration", time.Since(start).Round(time.Millisecond))
	return nil
}

// arbiter identifies SQLite's record-first concurrency check.
func (m *Migrator) runInTx(ctx context.Context, conn *sql.Conn, stmts []statement, arbiter int) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if failed, err := runStatements(ctx, tx, stmts); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			m.cfg.logger.Warn("migrate: rollback after failure", "error", rbErr)
		}
		switch {
		case failed == arbiter && strings.Contains(err.Error(), "UNIQUE constraint failed"):
			err = fmt.Errorf("%w (another migrator applied this migration concurrently; this transaction rolled back before touching the schema — rerun to verify nothing is pending)", err)
		case !m.d.transactionalDDL():
			err = fmt.Errorf("%w (%s)", err, implicitCommitNote(m.d.name(), stmts, failed))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// implicitCommitNote describes the prefix persisted by MySQL's implicit DDL
// commits. failed is the zero-based failing statement.
func implicitCommitNote(dialect string, stmts []statement, failed int) string {
	dirty := false
	for _, s := range stmts[:min(failed+1, len(stmts))] {
		if s.ddl {
			dirty = true
			break
		}
	}
	if !dirty || failed == 0 {
		return "the transaction rolled back: the database is unchanged by this migration"
	}
	span, verb := fmt.Sprintf("statements 1-%d", failed), "are"
	if failed == 1 {
		span, verb = "statement 1", "is"
	}
	return fmt.Sprintf("%s DDL commits implicitly: %s of this migration %s already committed — DML before a DDL commits with it, statements after one commit individually under autocommit — and the rollback undid none of it; reconcile the schema before retrying",
		dialect, span, verb)
}

func runStatements(ctx context.Context, db DB, stmts []statement) (int, error) {
	for i, s := range stmts {
		var err error
		if s.fn != nil {
			err = s.fn(ctx, db)
		} else if _, execErr := db.ExecContext(ctx, s.sql, s.args...); execErr != nil {
			err = execErr
		}
		if err != nil {
			return i, fmt.Errorf("statement %d/%d (%s): %w", i+1, len(stmts), describeStatement(s), err)
		}
	}
	return len(stmts), nil
}

func describeStatement(s statement) string {
	if s.fn != nil {
		if s.desc != "" {
			return s.desc
		}
		return "Go function"
	}
	sql := strings.Join(strings.Fields(s.sql), " ")
	if len(sql) > 200 {
		sql = sql[:200] + "…"
	}
	return sql
}

func (m *Migrator) insertRecord(mig *Migration, batch int) (statement, error) {
	sum, err := mig.checksum(m.d)
	if err != nil {
		return statement{}, err
	}
	return statement{
		sql: fmt.Sprintf("INSERT INTO %s (version, batch, checksum, applied_at) VALUES (%s, %s, %s, %s)",
			m.d.quoteIdent(m.cfg.table), m.d.placeholder(1), m.d.placeholder(2), m.d.placeholder(3), m.d.placeholder(4)),
		args: []any{mig.name, batch, sum, m.now()},
	}, nil
}

func (m *Migrator) updateRecord(mig *Migration) (statement, error) {
	sum, err := mig.checksum(m.d)
	if err != nil {
		return statement{}, err
	}
	return statement{
		sql: fmt.Sprintf("UPDATE %s SET checksum = %s, applied_at = %s WHERE version = %s",
			m.d.quoteIdent(m.cfg.table), m.d.placeholder(1), m.d.placeholder(2), m.d.placeholder(3)),
		args: []any{sum, m.now(), mig.name},
	}, nil
}

func (m *Migrator) deleteRecord(mig *Migration) statement {
	return statement{
		sql:  fmt.Sprintf("DELETE FROM %s WHERE version = %s", m.d.quoteIdent(m.cfg.table), m.d.placeholder(1)),
		args: []any{mig.name},
	}
}

func (m *Migrator) now() string {
	return m.cfg.clock.Now().UTC().Format(appliedAtFormat)
}

func (m *Migrator) loadState(ctx context.Context, db DB) ([]record, error) {
	if _, err := db.ExecContext(ctx, m.d.ensureTableSQL(m.cfg.table)); err != nil {
		return nil, fmt.Errorf("migrate: create records table: %w", err)
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(
		"SELECT version, batch, checksum, applied_at FROM %s ORDER BY version", m.d.quoteIdent(m.cfg.table)))
	if err != nil {
		return nil, fmt.Errorf("migrate: read records table: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var recs []record
	for rows.Next() {
		var r record
		if err := rows.Scan(&r.version, &r.batch, &r.checksum, &r.appliedAt); err != nil {
			return nil, fmt.Errorf("migrate: read records table: %w", err)
		}
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: read records table: %w", err)
	}
	return recs, nil
}

// verifyChecksums warns on changed versioned migrations or fails in strict
// mode. Repeatables and legacy records without checksums are skipped.
func (m *Migrator) verifyChecksums(recs []record) error {
	for _, r := range recs {
		if r.batch == repeatableBatch || strings.TrimSpace(r.checksum) == "" {
			continue
		}
		mig := m.cfg.collection.get(r.version)
		if mig == nil {
			continue
		}
		sum, err := mig.checksum(m.d)
		if err != nil {
			return err
		}
		if sum != strings.TrimSpace(r.checksum) {
			if m.cfg.strictChecksum {
				return fmt.Errorf("%w: %q no longer compiles to the SQL it was applied with (run Repair to accept the current form)", ErrChecksumMismatch, r.version)
			}
			m.cfg.logger.Warn("migrate: checksum mismatch — the migration changed after it was applied",
				"migration", r.version)
		}
	}
	return nil
}

// locked holds the session advisory lock on one dedicated connection.
func (m *Migrator) locked(ctx context.Context, fn func(*sql.Conn) error) error {
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if m.cfg.lock {
		if err := m.d.lock(ctx, conn, m.cfg.table, m.cfg.lockTimeout); err != nil {
			return err
		}
		defer func() {
			// Unlock after cancellation, but rely on connection close after 10s.
			unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if err := m.d.unlock(unlockCtx, conn, m.cfg.table); err != nil {
				m.cfg.logger.Warn("migrate: release lock", "error", err)
			}
		}()
	}
	return fn(conn)
}
