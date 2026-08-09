package integration

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-rio/migrate"

	_ "github.com/ClickHouse/clickhouse-go/v2"
)

func openClickHouse(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MIGRATE_CLICKHOUSE_DSN")
	if dsn == "" {
		t.Skip("MIGRATE_CLICKHOUSE_DSN not set")
	}
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		t.Fatalf("open clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping clickhouse: %v", err)
	}
	return db
}

func dropClickHouseObjects(t *testing.T, db *sql.DB, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := db.Exec("DROP TABLE IF EXISTS " + quoteClickHouseTestIdent(name) + " SYNC"); err != nil {
			t.Fatalf("drop %s: %v", name, err)
		}
	}
}

func quoteClickHouseTestIdent(name string) string {
	name = strings.ReplaceAll(name, `\`, `\\`)
	return "`" + strings.ReplaceAll(name, "`", "\\`") + "`"
}

func clickHouseAppSchema() *migrate.Collection {
	c := migrate.NewCollection()
	c.Add("001_create_events", func(s *migrate.Schema) {
		s.Create("events", func(t *migrate.Table) {
			t.UUID("id")
			t.String("tenant_id")
			t.TimestampTz("occurred_at")
			t.JSON("payload")
			t.Check("events_tenant_not_empty", "tenant_id != ''")
			t.ClickHouseEngine("MergeTree() PARTITION BY toYYYYMM(occurred_at) ORDER BY (tenant_id, occurred_at)")
			t.Comment("event stream")
		})
	})
	c.Add("002_add_source", func(s *migrate.Schema) {
		s.Table("events", func(t *migrate.Table) {
			t.String("source").Default("api").After("tenant_id")
		})
	})
	return c
}

func newClickHouseMigrator(t *testing.T, db *sql.DB, c *migrate.Collection, opts ...migrate.Option) *migrate.Migrator {
	t.Helper()
	opts = append([]migrate.Option{migrate.WithCollection(c), migrate.WithoutLock()}, opts...)
	m, err := migrate.New(db, migrate.ClickHouse, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func TestClickHouseEndToEnd(t *testing.T) {
	ctx := context.Background()
	db := openClickHouse(t)
	dropClickHouseObjects(t, db, "events", "schema_migrations")

	c := clickHouseAppSchema()
	m := newClickHouseMigrator(t, db, c)
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}

	mustExec(t, db, `INSERT INTO events (id, tenant_id, occurred_at, payload)
		VALUES (generateUUIDv4(), 'tenant-a', now64(6), '{"kind":"created"}')`)
	if _, err := db.Exec(`INSERT INTO events (id, tenant_id, occurred_at, payload)
		VALUES (generateUUIDv4(), '', now64(6), '{}')`); err == nil {
		t.Fatal("the CHECK constraint should reject an empty tenant")
	}
	if got := count(t, db, "SELECT count() FROM events WHERE source = 'api'"); got != 1 {
		t.Fatalf("source default should apply, got %d rows", got)
	}

	var engine, sortingKey, partitionKey, comment string
	err := db.QueryRow(`SELECT engine, sorting_key, partition_key, comment
		FROM system.tables WHERE database = currentDatabase() AND name = 'events'`).
		Scan(&engine, &sortingKey, &partitionKey, &comment)
	if err != nil {
		t.Fatalf("inspect system.tables: %v", err)
	}
	if engine != "MergeTree" || !strings.Contains(sortingKey, "tenant_id") ||
		!strings.Contains(sortingKey, "occurred_at") || partitionKey != "toYYYYMM(occurred_at)" ||
		comment != "event stream" {
		t.Fatalf("unexpected table metadata: engine=%q sorting=%q partition=%q comment=%q",
			engine, sortingKey, partitionKey, comment)
	}

	types := map[string]string{}
	rows, err := db.Query(`SELECT name, type FROM system.columns
		WHERE database = currentDatabase() AND table = 'events'`)
	if err != nil {
		t.Fatalf("inspect system.columns: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatalf("scan system.columns: %v", err)
		}
		types[name] = typ
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate system.columns: %v", err)
	}
	if types["id"] != "UUID" || types["occurred_at"] != "DateTime64(6, 'UTC')" ||
		!strings.HasPrefix(types["payload"], "JSON") || types["source"] != "String" {
		t.Fatalf("unexpected column types: %+v", types)
	}

	statuses, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(statuses) != 2 || !statuses[0].Applied || !statuses[1].Applied {
		t.Fatalf("unexpected status: %+v", statuses)
	}
	if plans, err := m.Plan(ctx); err != nil || len(plans) != 0 {
		t.Fatalf("Plan after Up = %+v, %v; want empty", plans, err)
	}

	if err := m.Rollback(ctx, 1); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := count(t, db, `SELECT count() FROM system.columns
		WHERE database = currentDatabase() AND table = 'events' AND name = 'source'`); got != 0 {
		t.Fatalf("source should be absent after rollback, got %d metadata rows", got)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("re-Up: %v", err)
	}
	if err := m.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := count(t, db, `SELECT count() FROM system.tables
		WHERE database = currentDatabase() AND name = 'events'`); got != 0 {
		t.Fatalf("events should be absent after Reset, got %d metadata rows", got)
	}
	if got := count(t, db, `SELECT count() FROM schema_migrations`); got != 0 {
		t.Fatalf("migration records should be empty after Reset, got %d", got)
	}
}

func TestClickHouseRepeatableChecksumRepairBaselineRunAndFresh(t *testing.T) {
	ctx := context.Background()
	db := openClickHouse(t)
	dropClickHouseObjects(t, db, "event_ids", "events", "run_events", "schema_migrations")

	build := func(order string, minTenant string) *migrate.Collection {
		c := migrate.NewCollection()
		c.Add("001_create_events", func(s *migrate.Schema) {
			s.Create("events", func(t *migrate.Table) {
				t.UUID("id")
				t.String("tenant_id")
				t.ClickHouseEngine("MergeTree() ORDER BY " + order)
			})
		})
		c.AddRepeatable("event_ids_view", func(s *migrate.Schema) {
			s.Exec("CREATE OR REPLACE VIEW event_ids AS SELECT id FROM events WHERE tenant_id >= '" + minTenant + "'")
		})
		return c
	}

	c1 := build("id", "a")
	m1 := newClickHouseMigrator(t, db, c1)
	if err := m1.Up(ctx); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	mustExec(t, db, `INSERT INTO events VALUES (generateUUIDv4(), 'a')`)
	if got := count(t, db, `SELECT count() FROM event_ids`); got != 1 {
		t.Fatalf("repeatable view should expose one row, got %d", got)
	}

	c2 := build("id", "z")
	m2 := newClickHouseMigrator(t, db, c2)
	plans, err := m2.Plan(ctx)
	if err != nil {
		t.Fatalf("Plan repeatable: %v", err)
	}
	if len(plans) != 1 || plans[0].Name != "event_ids_view" {
		t.Fatalf("only the changed repeatable should be planned, got %+v", plans)
	}
	if err := m2.Up(ctx); err != nil {
		t.Fatalf("repeatable Up: %v", err)
	}
	if got := count(t, db, `SELECT count() FROM event_ids`); got != 0 {
		t.Fatalf("updated view should expose no rows, got %d", got)
	}

	drifted := build("(tenant_id, id)", "z")
	strict := newClickHouseMigrator(t, db, drifted, migrate.WithStrictChecksum())
	if err := strict.Up(ctx); !errors.Is(err, migrate.ErrChecksumMismatch) {
		t.Fatalf("strict Up = %v, want ErrChecksumMismatch", err)
	}
	statuses, err := strict.Status(ctx)
	if err != nil {
		t.Fatalf("Status drift: %v", err)
	}
	if len(statuses) != 2 || !statuses[0].Drifted {
		t.Fatalf("versioned migration should report drift: %+v", statuses)
	}
	if err := strict.Repair(ctx); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if err := strict.Up(ctx); err != nil {
		t.Fatalf("Up after Repair: %v", err)
	}

	if err := strict.Fresh(ctx); err != nil {
		t.Fatalf("Fresh: %v", err)
	}
	if got := count(t, db, `SELECT count() FROM system.tables
		WHERE database = currentDatabase() AND name IN ('events', 'event_ids')`); got != 2 {
		t.Fatalf("Fresh should recreate the table and repeatable view, got %d objects", got)
	}

	if err := strict.Reset(ctx); err != nil {
		t.Fatalf("Reset before Baseline: %v", err)
	}
	dropClickHouseObjects(t, db, "event_ids", "events", "schema_migrations")
	baseline := newClickHouseMigrator(t, db, build("id", "a"))
	if err := baseline.Baseline(ctx); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if got := count(t, db, `SELECT count() FROM schema_migrations`); got != 2 {
		t.Fatalf("Baseline should record both migrations, got %d", got)
	}
	if got := count(t, db, `SELECT count() FROM system.tables
		WHERE database = currentDatabase() AND name = 'events'`); got != 0 {
		t.Fatalf("Baseline must not create events, got %d metadata rows", got)
	}
}

func TestClickHouseRunUsesDedicatedConnection(t *testing.T) {
	ctx := context.Background()
	db := openClickHouse(t)
	dropClickHouseObjects(t, db, "run_events", "schema_migrations")

	c := migrate.NewCollection()
	c.Add("001_run", func(s *migrate.Schema) {
		s.Create("run_events", func(t *migrate.Table) {
			t.BigInteger("value").Unsigned()
			t.ClickHouseEngine("MergeTree() ORDER BY value")
		})
		s.Run(func(ctx context.Context, db migrate.DB) error {
			if _, err := db.ExecContext(ctx, "INSERT INTO run_events VALUES (?)", 1); err != nil {
				return err
			}
			_, err := db.ExecContext(ctx, "INSERT INTO run_events VALUES (?)", 2)
			return err
		})
	})
	m := newClickHouseMigrator(t, db, c)
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if got := count(t, db, `SELECT sum(value) FROM run_events`); got != 3 {
		t.Fatalf("Run should insert both values, sum = %d", got)
	}
}

func TestClickHouseFailedMigrationLeavesAppliedPrefix(t *testing.T) {
	ctx := context.Background()
	db := openClickHouse(t)
	dropClickHouseObjects(t, db, "partial_first", "schema_migrations")

	c := migrate.NewCollection()
	c.Add("001_partial", func(s *migrate.Schema) {
		s.Exec("CREATE TABLE partial_first (id UUID) ENGINE = MergeTree() ORDER BY id")
		s.Exec("SELECT throwIf(1, 'intentional migration failure')")
	}, migrate.WithDown(func(s *migrate.Schema) {
		s.DropIfExists("partial_first")
	}))
	m := newClickHouseMigrator(t, db, c)
	err := m.Up(ctx)
	if err == nil || !strings.Contains(err.Error(), "statement 2/3") ||
		!strings.Contains(err.Error(), "statement 1 may already be effective") ||
		!strings.Contains(err.Error(), "reconcile the actual schema") {
		t.Fatalf("unexpected partial failure: %v", err)
	}
	if got := count(t, db, `SELECT count() FROM system.tables
		WHERE database = currentDatabase() AND name = 'partial_first'`); got != 1 {
		t.Fatalf("the first DDL should remain applied, got %d metadata rows", got)
	}
	if got := count(t, db, `SELECT count() FROM schema_migrations`); got != 0 {
		t.Fatalf("the failed migration must not be recorded, got %d", got)
	}
}

func TestClickHouseEscapedIdentifiersAndFresh(t *testing.T) {
	ctx := t.Context()
	db := openClickHouse(t)
	tableName := "events\\`archive"
	columnName := "value\\`text"
	recordsTable := "schema\\migrations"
	dropClickHouseObjects(t, db, tableName, recordsTable)
	t.Cleanup(func() { dropClickHouseObjects(t, db, tableName, recordsTable) })

	c := migrate.NewCollection()
	c.Add("001_special_names", func(s *migrate.Schema) {
		s.Create(tableName, func(t *migrate.Table) {
			t.String(columnName)
			t.ClickHouseEngine("MergeTree() ORDER BY tuple()")
		})
	})
	m, err := migrate.New(db, migrate.ClickHouse,
		migrate.WithCollection(c), migrate.WithTable(recordsTable), migrate.WithoutLock())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up with escaped identifiers: %v", err)
	}

	var tables int
	if err := db.QueryRowContext(ctx, `SELECT count() FROM system.tables
		WHERE database = currentDatabase() AND name = ?`, tableName).Scan(&tables); err != nil {
		t.Fatalf("inspect escaped table: %v", err)
	}
	if tables != 1 {
		t.Fatalf("escaped table metadata rows = %d, want 1", tables)
	}
	var columns int
	if err := db.QueryRowContext(ctx, `SELECT count() FROM system.columns
		WHERE database = currentDatabase() AND table = ? AND name = ?`, tableName, columnName).Scan(&columns); err != nil {
		t.Fatalf("inspect escaped column: %v", err)
	}
	if columns != 1 {
		t.Fatalf("escaped column metadata rows = %d, want 1", columns)
	}

	if err := m.Fresh(ctx); err != nil {
		t.Fatalf("Fresh with escaped identifiers: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count() FROM system.tables
		WHERE database = currentDatabase() AND name IN (?, ?)`, tableName, recordsTable).Scan(&tables); err != nil {
		t.Fatalf("inspect escaped tables after Fresh: %v", err)
	}
	if tables != 2 {
		t.Fatalf("Fresh should recreate both escaped tables, metadata rows = %d", tables)
	}
}
