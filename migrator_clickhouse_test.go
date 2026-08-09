package migrate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func clickHouseTables() *Collection {
	c := NewCollection()
	c.Add("001_events", func(s *Schema) {
		s.Create("events", func(t *Table) {
			t.UUID("id")
			t.ClickHouseEngine("MergeTree() ORDER BY id")
		})
	})
	c.Add("002_sessions", func(s *Schema) {
		s.Create("sessions", func(t *Table) {
			t.UUID("id")
			t.ClickHouseEngine("MergeTree() ORDER BY id")
		})
	})
	return c
}

func TestClickHouseDefaultLockFailsBeforeDatabaseWrites(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, *Migrator) error
	}{
		{name: "up", run: func(ctx context.Context, m *Migrator) error { return m.Up(ctx) }},
		{name: "rollback", run: func(ctx context.Context, m *Migrator) error { return m.Rollback(ctx, 1) }},
		{name: "rollback batch", run: func(ctx context.Context, m *Migrator) error { return m.RollbackBatch(ctx) }},
		{name: "reset", run: func(ctx context.Context, m *Migrator) error { return m.Reset(ctx) }},
		{name: "baseline", run: func(ctx context.Context, m *Migrator) error { return m.Baseline(ctx) }},
		{name: "repair", run: func(ctx context.Context, m *Migrator) error { return m.Repair(ctx) }},
		{name: "fresh", run: func(ctx context.Context, m *Migrator) error { return m.Fresh(ctx) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeDB()
			m := testMigrator(t, f, ClickHouse, clickHouseTables())
			err := tt.run(context.Background(), m)
			if !errors.Is(err, ErrLockUnsupported) {
				t.Fatalf("error = %v, want ErrLockUnsupported", err)
			}
			if log := f.logged(); len(log) != 0 {
				t.Fatalf("lock failure must precede every database statement, got:\n%s", strings.Join(log, "\n"))
			}
		})
	}
}

func TestClickHouseWithoutLockExecutesWithoutTransaction(t *testing.T) {
	f := newFakeDB()
	c := NewCollection()
	c.Add("001_events", func(s *Schema) {
		s.Create("events", func(t *Table) {
			t.UUID("id")
			t.ClickHouseEngine("MergeTree() ORDER BY id")
		})
		s.Run(func(ctx context.Context, db DB) error {
			_, err := db.ExecContext(ctx, "INSERT INTO events (id) VALUES (?)", "00000000-0000-0000-0000-000000000000")
			return err
		})
	})
	m := testMigrator(t, f, ClickHouse, c, WithoutLock())
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if got := f.loggedContaining("BEGIN"); len(got) != 0 {
		t.Fatalf("ClickHouse must never call BeginTx, got: %v", got)
	}
	assertLogSequence(t, f.logged(), []string{
		"CREATE TABLE IF NOT EXISTS `schema_migrations`",
		"SELECT version, batch, checksum, applied_at",
		"CREATE TABLE `events`",
		"INSERT INTO events (id)",
		"INSERT INTO `schema_migrations` (version, batch, checksum, applied_at) SETTINGS async_insert = 0 VALUES",
	})
	if got := f.loggedContaining("INSERT INTO `schema_migrations`"); len(got) != 1 ||
		!strings.Contains(got[0], "SETTINGS async_insert = 0 VALUES (?, ?, ?, ?)") {
		t.Fatalf("record insert must force synchronous insertion, got: %v", got)
	}
}

func TestClickHouseFailureReportsAppliedPrefixWithoutRecord(t *testing.T) {
	f := newFakeDB()
	c := NewCollection()
	c.Add("001_partial", func(s *Schema) {
		s.Exec("CREATE TABLE first (id UUID) ENGINE = MergeTree() ORDER BY id")
		s.Exec("CREATE BROKEN")
	}, WithDown(func(s *Schema) {
		s.Exec("DROP TABLE first")
	}))
	f.fail("CREATE BROKEN", errors.New("syntax error"))
	err := testMigrator(t, f, ClickHouse, c, WithoutLock()).Up(context.Background())
	if err == nil {
		t.Fatal("Up should fail")
	}
	for _, fragment := range []string{
		"statement 2/3", "CREATE BROKEN", "statement 1 may already be effective",
		"reconcile the actual schema and migration records", "syntax error",
	} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error should mention %q, got: %v", fragment, err)
		}
	}
	if got := f.loggedContaining("INSERT INTO `schema_migrations`"); len(got) != 0 {
		t.Fatalf("a failed migration must not write a new record, got: %v", got)
	}
	if got := f.loggedContaining("ROLLBACK"); len(got) != 0 {
		t.Fatalf("ClickHouse failure must not pretend to roll back, got: %v", got)
	}
}

func TestClickHouseRollbackFailureReportsAppliedPrefix(t *testing.T) {
	f := newFakeDB()
	c := NewCollection()
	c.Add("001_partial_down", func(s *Schema) {
		s.Exec("CREATE TABLE first (id UUID) ENGINE = MergeTree() ORDER BY id")
	}, WithDown(func(s *Schema) {
		s.Exec("DROP TABLE first")
		s.Exec("DROP TABLE missing_dependency")
	}))
	f.setRecords(appliedRecord(t, c, ClickHouse, "001_partial_down", 1))
	f.fail("missing_dependency", errors.New("dependency error"))
	err := testMigrator(t, f, ClickHouse, c, WithoutLock()).Rollback(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "statement 1 may already be effective") ||
		!strings.Contains(err.Error(), "migration records") {
		t.Fatalf("rollback should report its applied prefix, got: %v", err)
	}
	if got := f.loggedContaining("DELETE WHERE `version`"); len(got) != 0 {
		t.Fatalf("failed rollback must retain its record, got: %v", got)
	}
}

func TestClickHouseBookkeepingUsesSynchronousMutations(t *testing.T) {
	t.Run("repeatable update", func(t *testing.T) {
		f := newFakeDB()
		c := NewCollection()
		c.AddRepeatable("r_events", func(s *Schema) {
			s.Exec("CREATE OR REPLACE VIEW event_ids AS SELECT id FROM events")
		})
		f.setRecords(record{
			version: "r_events", batch: repeatableBatch, checksum: "stale",
			appliedAt: "2026-08-09T00:00:00.000000Z",
		})
		m := testMigrator(t, f, ClickHouse, c, WithoutLock())
		if err := m.Up(context.Background()); err != nil {
			t.Fatalf("Up: %v", err)
		}
		got := f.loggedContaining("ALTER TABLE `schema_migrations` UPDATE checksum")
		if len(got) != 1 || !strings.Contains(got[0], "SETTINGS mutations_sync = 1") {
			t.Fatalf("repeatable update must be synchronous, got: %v", got)
		}
	})

	t.Run("rollback delete", func(t *testing.T) {
		f := newFakeDB()
		c := clickHouseTables()
		f.setRecords(appliedRecord(t, c, ClickHouse, "001_events", 1))
		m := testMigrator(t, f, ClickHouse, c, WithoutLock())
		if err := m.Rollback(context.Background(), 1); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		got := f.loggedContaining("ALTER TABLE `schema_migrations` DELETE WHERE `version`")
		if len(got) != 1 || !strings.Contains(got[0], "SETTINGS mutations_sync = 1") {
			t.Fatalf("rollback delete must be synchronous, got: %v", got)
		}
	})

	t.Run("reset repeatables", func(t *testing.T) {
		f := newFakeDB()
		c := clickHouseTables()
		f.setRecords(record{version: "r_view", batch: repeatableBatch, checksum: "x", appliedAt: "2026-08-09T00:00:00.000000Z"})
		m := testMigrator(t, f, ClickHouse, c, WithoutLock())
		if err := m.Reset(context.Background()); err != nil {
			t.Fatalf("Reset: %v", err)
		}
		got := f.loggedContaining("ALTER TABLE `schema_migrations` DELETE WHERE `batch`")
		if len(got) != 1 || !strings.Contains(got[0], "SETTINGS mutations_sync = 1") {
			t.Fatalf("reset repeatable delete must be synchronous, got: %v", got)
		}
	})

	t.Run("failed reset repeatable mutation", func(t *testing.T) {
		f := newFakeDB()
		c := clickHouseTables()
		f.setRecords(
			appliedRecord(t, c, ClickHouse, "001_events", 1),
			record{version: "r_view", batch: repeatableBatch, checksum: "x", appliedAt: "2026-08-09T00:00:00.000000Z"},
		)
		f.fail("DELETE WHERE `batch`", errors.New("mutation failed"))
		err := testMigrator(t, f, ClickHouse, c, WithoutLock()).Reset(context.Background())
		if err == nil {
			t.Fatal("Reset should fail")
		}
		for _, fragment := range []string{"1/1", "001_events", "already rolled back", "not automatically restored", "forget repeatable records"} {
			if !strings.Contains(err.Error(), fragment) {
				t.Errorf("Reset error should contain %q, got: %v", fragment, err)
			}
		}
	})

	t.Run("repair", func(t *testing.T) {
		f := newFakeDB()
		c := clickHouseTables()
		rec := appliedRecord(t, c, ClickHouse, "001_events", 1)
		rec.checksum = "stale"
		f.setRecords(rec)
		m := testMigrator(t, f, ClickHouse, c, WithoutLock())
		if err := m.Repair(context.Background()); err != nil {
			t.Fatalf("Repair: %v", err)
		}
		got := f.loggedContaining("ALTER TABLE `schema_migrations` UPDATE checksum")
		if len(got) != 1 || !strings.Contains(got[0], "SETTINGS mutations_sync = 1") {
			t.Fatalf("repair must be synchronous, got: %v", got)
		}
	})
}

func TestClickHouseRejectsDuplicateMigrationRecords(t *testing.T) {
	f := newFakeDB()
	f.setRecords(
		record{version: "001_events", batch: 1, checksum: "a", appliedAt: "2026-08-09T00:00:00.000000Z"},
		record{version: "001_events", batch: 2, checksum: "b", appliedAt: "2026-08-09T01:00:00.000000Z"},
	)
	m := testMigrator(t, f, ClickHouse, clickHouseTables(), WithoutLock())
	_, err := m.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "duplicate version") ||
		!strings.Contains(err.Error(), "serialization guarantee was violated") {
		t.Fatalf("duplicate records should stop state loading, got: %v", err)
	}
}

func TestClickHouseReadOnlyOperationsDoNotRequireWithoutLock(t *testing.T) {
	f := newFakeDB()
	m := testMigrator(t, f, ClickHouse, clickHouseTables())
	statuses, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("Status returned %d rows, want 2", len(statuses))
	}
	plans, err := m.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("Plan returned %d rows, want 2", len(plans))
	}
	if _, err := clickHouseTables().SQL(ClickHouse); err != nil {
		t.Fatalf("Collection.SQL: %v", err)
	}
	if got := f.loggedContaining("BEGIN"); len(got) != 0 {
		t.Fatalf("read-only operations must not begin transactions, got: %v", got)
	}
}

func TestClickHouseBaselineAndFresh(t *testing.T) {
	t.Run("baseline", func(t *testing.T) {
		f := newFakeDB()
		m := testMigrator(t, f, ClickHouse, clickHouseTables(), WithoutLock())
		if err := m.Baseline(context.Background()); err != nil {
			t.Fatalf("Baseline: %v", err)
		}
		inserts := f.loggedContaining("INSERT INTO `schema_migrations`")
		if len(inserts) != 2 {
			t.Fatalf("Baseline should insert two records, got: %v", inserts)
		}
		for _, query := range inserts {
			if !strings.Contains(query, "SETTINGS async_insert = 0") {
				t.Errorf("baseline insert must disable async insertion: %s", query)
			}
		}
	})

	t.Run("fresh", func(t *testing.T) {
		f := newFakeDB()
		f.tables = []string{"events", "event_ids"}
		m := testMigrator(t, f, ClickHouse, clickHouseTables(), WithoutLock())
		if err := m.Fresh(context.Background()); err != nil {
			t.Fatalf("Fresh: %v", err)
		}
		for _, name := range []string{"events", "event_ids", "schema_migrations"} {
			got := f.loggedContaining("DROP TABLE IF EXISTS `" + name + "` SYNC")
			if len(got) != 1 {
				t.Errorf("Fresh should synchronously drop %s once, got: %v", name, got)
			}
		}
		if got := f.loggedContaining("system.tables"); len(got) != 1 {
			t.Fatalf("Fresh should enumerate ClickHouse tables, got: %v", got)
		}
	})
}
