package integration

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/go-rio/migrate"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// openPostgres skips unless MIGRATE_POSTGRES_DSN is set.
func openPostgres(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MIGRATE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("MIGRATE_POSTGRES_DSN not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestPostgresEndToEnd(t *testing.T)     { runEndToEnd(t, openPostgres(t), migrate.Postgres) }
func TestPostgresChecksumFlow(t *testing.T) { runChecksumFlow(t, openPostgres(t), migrate.Postgres) }
func TestPostgresDataMigration(t *testing.T) {
	runDataMigration(t, openPostgres(t), migrate.Postgres)
}
func TestPostgresBaseline(t *testing.T) { runBaseline(t, openPostgres(t), migrate.Postgres) }

// The advisory lock must serialize concurrent migrators.
func TestPostgresConcurrentMigrators(t *testing.T) {
	db1 := openPostgres(t)
	db2 := openPostgres(t)
	dropAll(t, db1)

	run := func(db *sql.DB) error {
		m, err := migrate.New(db, migrate.Postgres, migrate.WithCollection(appSchema()))
		if err != nil {
			return err
		}
		return m.Up(context.Background())
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, db := range []*sql.DB{db1, db2} {
		wg.Go(func() {
			errs[i] = run(db)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("migrator %d: %v", i, err)
		}
	}
	if got := count(t, db1, "SELECT COUNT(*) FROM schema_migrations"); got != 2 {
		t.Errorf("each migration must apply exactly once, records = %d", got)
	}
}

// PostgreSQL transactional DDL must leave no partial state after failure.
func TestPostgresFailedMigrationLeavesNoTrace(t *testing.T) {
	ctx := context.Background()
	db := openPostgres(t)
	dropAll(t, db)

	c := migrate.NewCollection()
	c.Add("001_bad", func(s *migrate.Schema) {
		s.Create("things", func(t *migrate.Table) { t.ID() })
		s.Exec("SELECT 1/0") // fails after the create
	})
	m, err := migrate.New(db, migrate.Postgres, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err == nil {
		t.Fatal("Up should fail")
	}
	if _, err := db.Exec("SELECT COUNT(*) FROM things"); err == nil {
		t.Error("the transaction should have rolled back the CREATE TABLE")
	}
	if got := count(t, db, "SELECT COUNT(*) FROM schema_migrations"); got != 0 {
		t.Errorf("no record may exist, got %d", got)
	}
}

func TestPostgresRepeatable(t *testing.T) { runRepeatable(t, openPostgres(t), migrate.Postgres) }

// Recreate must avoid live primary-key names and advance identity sequences.
func TestPostgresRecreate(t *testing.T) {
	ctx := context.Background()
	db := openPostgres(t)
	dropAll(t, db)
	// These tables are outside dropAll's shared fixture.
	for _, table := range []string{"orders", "counters"} {
		if _, err := db.Exec("DROP TABLE IF EXISTS " + table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}

	c := migrate.NewCollection()
	c.Add("001_orders", func(s *migrate.Schema) {
		s.Create("orders", func(t *migrate.Table) {
			t.Integer("code").Primary()
			t.String("state")
		})
	})
	c.Add("002_counters", func(s *migrate.Schema) {
		s.Create("counters", func(t *migrate.Table) {
			t.ID()
			t.String("name")
		})
	})
	m, err := migrate.New(db, migrate.Postgres, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, "INSERT INTO orders (code, state) VALUES (1, 'open')")
	mustExec(t, db, "INSERT INTO counters (name) VALUES ('a')")
	mustExec(t, db, "INSERT INTO counters (name) VALUES ('b')")

	c.Add("003_rebuild", func(s *migrate.Schema) {
		s.Recreate("orders", func(t *migrate.Table) {
			t.Integer("code").Primary()
			t.Enum("state", "open", "closed")
		})
		s.Recreate("counters", func(t *migrate.Table) {
			t.ID()
			t.String("name").Unique()
		})
	}, migrate.WithDown(func(s *migrate.Schema) {}))
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up with recreates: %v", err)
	}

	if got := count(t, db, `SELECT COUNT(*) FROM pg_constraint WHERE conname = 'orders_pkey'`); got != 1 {
		t.Errorf("orders_pkey should exist after the rebuild, got %d", got)
	}
	if got := count(t, db, `SELECT COUNT(*) FROM pg_constraint WHERE conname LIKE '%__migrate_new%'`); got != 0 {
		t.Fatal("no constraint may keep the temporary name")
	}
	// The copied identity sequence must advance past existing rows.
	mustExec(t, db, "INSERT INTO counters (name) VALUES ('c')")
	if got := count(t, db, "SELECT MAX(id) FROM counters"); got != 3 {
		t.Errorf("the new row should take id 3, max = %d", got)
	}
	for _, table := range []string{"orders", "counters"} {
		mustExec(t, db, "DROP TABLE IF EXISTS "+table)
	}
}

// Recreate must restore triggers removed with PostgreSQL's old table.
func TestPostgresRecreateKeepsTriggers(t *testing.T) {
	ctx := context.Background()
	db := openPostgres(t)
	dropAll(t, db)
	mustExec(t, db, "DROP TABLE IF EXISTS audit")

	c := migrate.NewCollection()
	c.Add("001_users", func(s *migrate.Schema) {
		s.Create("users", func(t *migrate.Table) {
			t.ID()
			t.String("email")
		})
		s.Create("audit", func(t *migrate.Table) {
			t.ID()
			t.String("email")
		})
		s.Exec(`CREATE OR REPLACE FUNCTION users_audit_fn() RETURNS trigger AS $$
			BEGIN INSERT INTO audit (email) VALUES (NEW.email); RETURN NEW; END $$ LANGUAGE plpgsql`)
		s.Exec(`CREATE TRIGGER users_audit AFTER INSERT ON users FOR EACH ROW EXECUTE FUNCTION users_audit_fn()`)
	}, migrate.WithDown(func(s *migrate.Schema) {}))
	m, err := migrate.New(db, migrate.Postgres, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, "INSERT INTO users (email) VALUES ('a@x.dev')")
	if got := count(t, db, "SELECT COUNT(*) FROM audit"); got != 1 {
		t.Fatalf("the trigger should audit inserts, got %d rows", got)
	}

	c.Add("002_unique_emails", func(s *migrate.Schema) {
		s.Recreate("users", func(t *migrate.Table) {
			t.ID()
			t.String("email").Unique()
		})
	}, migrate.WithDown(func(s *migrate.Schema) {}))
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up with recreate: %v", err)
	}

	if got := count(t, db, `SELECT COUNT(*) FROM pg_trigger WHERE tgrelid = 'users'::regclass AND NOT tgisinternal`); got != 1 {
		t.Fatalf("the trigger must survive the rebuild, got %d", got)
	}
	mustExec(t, db, "INSERT INTO users (email) VALUES ('b@x.dev')")
	if got := count(t, db, "SELECT COUNT(*) FROM audit"); got != 2 {
		t.Errorf("the recreated trigger should keep firing, got %d rows", got)
	}
	if _, err := db.Exec("INSERT INTO users (email) VALUES ('b@x.dev')"); err == nil {
		t.Error("the rebuilt unique index should reject duplicates")
	}
}

// A blocked Recreate must roll back with the original table and data intact.
func TestPostgresRecreateReferencedParentFailsCleanly(t *testing.T) {
	ctx := context.Background()
	db := openPostgres(t)
	dropAll(t, db)

	c := appSchema() // users + posts (posts.user_id → users.id)
	m, err := migrate.New(db, migrate.Postgres, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, `INSERT INTO users (email, name) VALUES ('a@x.dev', 'A')`)

	c.Add("003_rebuild_users", func(s *migrate.Schema) {
		s.Recreate("users", func(t *migrate.Table) {
			t.ID()
			t.String("email").Unique()
			t.String("name", 100)
			t.Enum("role", "admin", "member").Default("member")
			t.Timestamps()
		})
	}, migrate.WithDown(func(s *migrate.Schema) {}))
	if err := m.Up(ctx); err == nil {
		t.Fatal("recreating a referenced parent must fail on Postgres")
	}
	if got := count(t, db, `SELECT COUNT(*) FROM users`); got != 1 {
		t.Errorf("users must survive the failed rebuild, got %d rows", got)
	}
	if got := count(t, db, `SELECT COUNT(*) FROM schema_migrations`); got != 2 {
		t.Errorf("the failed migration must not be recorded, got %d records", got)
	}
}

// Every expression-index shape must round-trip through IndexExpr verbatim.
func TestPostgresExpressionIndexes(t *testing.T) {
	ctx := context.Background()
	db := openPostgres(t)
	dropAll(t, db)
	mustExec(t, db, "DROP TABLE IF EXISTS idx_items")

	c := migrate.NewCollection()
	c.Add("20260831000000_expression_indexes", func(s *migrate.Schema) {
		s.Create("idx_items", func(tb *migrate.Table) {
			tb.ID()
			tb.String("email")
			tb.String("order_number")
			tb.Integer("a")
			tb.Integer("b")
			tb.String("code")
		})
		s.Table("idx_items", func(tb *migrate.Table) {
			tb.IndexExpr("ie_lower", "lower(email)")
			tb.IndexExpr("ie_opclass", "lower(order_number) text_pattern_ops")
			tb.IndexExpr("ie_arith", "(a + b)")
			tb.IndexExpr("ie_multi", "(a + b)", "lower(code)")
			tb.IndexExpr("ie_partial", "lower(code)").Where("b > 0")
			tb.IndexExpr("ie_using", "lower(code)").Using("btree")
			tb.UniqueExpr("ie_unique", "lower(email)")
		})
	})
	m, err := migrate.New(db, migrate.Postgres, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	t.Cleanup(func() { mustExec(t, db, "DROP TABLE IF EXISTS idx_items") })

	rows, err := db.Query(
		"SELECT indexname, indexdef FROM pg_indexes WHERE tablename = 'idx_items' AND indexname LIKE 'ie_%'",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	defs := map[string]string{}
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			t.Fatal(err)
		}
		defs[name] = def
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ie_lower", "ie_opclass", "ie_arith", "ie_multi", "ie_partial", "ie_using", "ie_unique"} {
		if defs[want] == "" {
			t.Fatalf("index %s missing; have %v", want, defs)
		}
	}
	if def := defs["ie_opclass"]; !strings.Contains(def, "text_pattern_ops") {
		t.Fatalf("operator class lost: %s", def)
	}
	if def := defs["ie_partial"]; !strings.Contains(def, "WHERE") {
		t.Fatalf("partial predicate lost: %s", def)
	}

	mustExec(t, db, "INSERT INTO idx_items (email, order_number, a, b, code) VALUES ('X@a.com', 'N1', 1, 2, 'c')")
	if _, err := db.Exec("INSERT INTO idx_items (email, order_number, a, b, code) VALUES ('x@A.COM', 'N2', 3, 4, 'd')"); err == nil {
		t.Fatal("unique expression index did not enforce lower(email) uniqueness")
	}
}
