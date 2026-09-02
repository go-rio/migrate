package integration

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-rio/migrate"
	_ "modernc.org/sqlite" // pure-Go driver, keeps integration CGO-free
)

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	return mustOpen(t, "file:"+filepath.Join(t.TempDir(), "app.db")+
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
}

func TestSQLiteEndToEnd(t *testing.T)     { runEndToEnd(t, openSQLite(t), migrate.SQLite) }
func TestSQLiteChecksumFlow(t *testing.T) { runChecksumFlow(t, openSQLite(t), migrate.SQLite) }
func TestSQLiteDataMigration(t *testing.T) {
	runDataMigration(t, openSQLite(t), migrate.SQLite)
}
func TestSQLiteBaseline(t *testing.T) { runBaseline(t, openSQLite(t), migrate.SQLite) }

// SQLite races must resolve before either migrator changes the schema.
func TestSQLiteConcurrentMigrators(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "app.db") + "?_pragma=busy_timeout(5000)"

	collection := func() *migrate.Collection {
		c := migrate.NewCollection()
		for _, table := range []string{"users", "posts", "tags"} {
			c.Add("00"+table, func(s *migrate.Schema) {
				s.Create(table, func(t *migrate.Table) { t.ID() })
			})
		}
		return c
	}

	const racers = 8
	dbs := make([]*sql.DB, racers)
	for i := range dbs {
		dbs[i] = mustOpen(t, dsn)
	}

	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Go(func() {
			m, err := migrate.New(dbs[i], migrate.SQLite, migrate.WithCollection(collection()))
			if err != nil {
				errs[i] = err
				return
			}
			errs[i] = m.Up(ctx)
		})
	}
	wg.Wait()

	winners := 0
	for i, err := range errs {
		if err == nil {
			winners++
			continue
		}
		if !strings.Contains(err.Error(), "another migrator") {
			t.Errorf("racer %d leaked a raw race error: %v", i, err)
		}
	}
	if winners == 0 {
		t.Error("at least one racer must win")
	}

	for _, table := range []string{"users", "posts", "tags"} {
		if got := count(t, dbs[0], "SELECT COUNT(*) FROM "+table); got != 0 {
			t.Errorf("%s should exist and be empty, got %d", table, got)
		}
	}
	if got := count(t, dbs[0], "SELECT COUNT(*) FROM schema_migrations"); got != 3 {
		t.Errorf("each migration must be recorded exactly once, got %d", got)
	}
	m, err := migrate.New(dbs[0], migrate.SQLite, migrate.WithCollection(collection()))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("a rerun after the race must be a clean no-op: %v", err)
	}
}

// Record-first arbitration must keep a lost race from running the down twice.
func TestSQLiteConcurrentRollback(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "app.db") + "?_pragma=busy_timeout(5000)"

	collection := func() *migrate.Collection {
		c := migrate.NewCollection()
		c.Add("001_counters", func(s *migrate.Schema) {
			s.Create("counters", func(t *migrate.Table) {
				t.ID()
				t.Integer("v").Default(0)
			})
			s.Exec("INSERT INTO counters (v) VALUES (10)")
		}, migrate.WithDown(func(s *migrate.Schema) {
			// Deliberately non-idempotent: a double-applied down is visible.
			s.Exec("UPDATE counters SET v = v - 10")
		}))
		return c
	}

	setup, err := migrate.New(mustOpen(t, dsn), migrate.SQLite, migrate.WithCollection(collection()))
	if err != nil {
		t.Fatal(err)
	}
	if err := setup.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}

	const racers = 8
	dbs := make([]*sql.DB, racers)
	for i := range dbs {
		dbs[i] = mustOpen(t, dsn)
	}
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Go(func() {
			m, err := migrate.New(dbs[i], migrate.SQLite, migrate.WithCollection(collection()))
			if err != nil {
				errs[i] = err
				return
			}
			errs[i] = m.Rollback(ctx, 1)
		})
	}
	wg.Wait()

	winners := 0
	for i, err := range errs {
		if err == nil {
			winners++
			continue
		}
		if !strings.Contains(err.Error(), "another migrator") {
			t.Errorf("racer %d leaked a raw race error: %v", i, err)
		}
	}
	// Late losers see the record already gone and no-op, so several may succeed.
	if winners == 0 {
		t.Error("at least one racer must win")
	}
	if got := count(t, dbs[0], "SELECT v FROM counters"); got != 0 {
		t.Errorf("the down must run exactly once: v = %d, want 0", got)
	}
	if got := count(t, dbs[0], "SELECT COUNT(*) FROM schema_migrations"); got != 0 {
		t.Errorf("the record must be deleted exactly once, got %d rows", got)
	}
}

func mustOpen(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSQLiteAutoIncrement(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	c := migrate.NewCollection()
	c.Add("001_counters", func(s *migrate.Schema) {
		s.Create("counters", func(t *migrate.Table) {
			t.Integer("id").AutoIncrement()
			t.String("name")
		})
	})
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, "INSERT INTO counters (name) VALUES ('a')")
	mustExec(t, db, "INSERT INTO counters (name) VALUES ('b')")
	if got := count(t, db, "SELECT id FROM counters WHERE name = 'b'"); got != 2 {
		t.Errorf("second row should get id 2, got %d", got)
	}
}

// Transactional DDL must leave no partial state after failure.
func TestSQLiteFailedMigrationLeavesNoTrace(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	bad := migrate.NewCollection()
	bad.Add("001_things", func(s *migrate.Schema) {
		s.Create("things", func(t *migrate.Table) { t.ID() })
		s.Exec("CREATE BROKEN SYNTAX")
	})
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(bad))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err == nil {
		t.Fatal("Up should fail on the broken statement")
	}
	if _, err := db.Exec("SELECT COUNT(*) FROM things"); err == nil {
		t.Error("the transaction should have rolled back the CREATE TABLE")
	}
	if got := count(t, db, "SELECT COUNT(*) FROM schema_migrations"); got != 0 {
		t.Errorf("no record may exist for the failed migration, got %d", got)
	}

	fixed := migrate.NewCollection()
	fixed.Add("001_things", func(s *migrate.Schema) {
		s.Create("things", func(t *migrate.Table) { t.ID() })
	})
	m2, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(fixed))
	if err != nil {
		t.Fatal(err)
	}
	if err := m2.Up(ctx); err != nil {
		t.Fatalf("Up after fixing the migration: %v", err)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM things"); got != 0 {
		t.Errorf("things should exist and be empty, got count %d", got)
	}
}

func TestSQLiteWithoutTransaction(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	c := migrate.NewCollection()
	c.Add("001_plain", func(s *migrate.Schema) {
		s.Create("plain", func(t *migrate.Table) { t.ID() })
	}, migrate.WithoutTransaction())
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM schema_migrations"); got != 1 {
		t.Errorf("the migration should be recorded, got %d rows", got)
	}
}

// SQLite must drop implied indexes before their columns during rollback.
func TestSQLiteAlterRollback(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	c := migrate.NewCollection()
	c.Add("001_users", func(s *migrate.Schema) {
		s.Create("users", func(t *migrate.Table) {
			t.ID()
			t.String("email")
		})
	})
	c.Add("002_add_nickname", func(s *migrate.Schema) {
		s.Table("users", func(t *migrate.Table) {
			t.String("nickname").Nullable().Index()
			t.RenameColumn("email", "mail")
		})
	})
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, "INSERT INTO users (mail, nickname) VALUES ('a@x.dev', 'a')")

	if err := m.Rollback(ctx, 1); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM users WHERE email = 'a@x.dev'"); got != 1 {
		t.Error("the rename should have reversed and data survived")
	}
	if _, err := db.Exec("SELECT nickname FROM users"); err == nil {
		t.Error("nickname should be dropped by the automatic down")
	}
}

func TestSQLiteRepeatable(t *testing.T) { runRepeatable(t, openSQLite(t), migrate.SQLite) }

// Recreate must preserve rows while rebuilding SQLite constraints and indexes.
func TestSQLiteRecreate(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	c := migrate.NewCollection()
	c.Add("001_users", func(s *migrate.Schema) {
		s.Create("users", func(t *migrate.Table) {
			t.ID()
			t.String("email")
		})
	})
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, "INSERT INTO users (email) VALUES ('a@x.dev')")
	mustExec(t, db, "INSERT INTO users (email) VALUES ('b@x.dev')")

	c.Add("002_unique_emails", func(s *migrate.Schema) {
		s.Recreate("users", func(t *migrate.Table) {
			t.ID()
			t.String("email").Unique()
			t.Integer("logins").Default(0).SkipCopy()
		})
	}, migrate.WithDown(func(s *migrate.Schema) {
		s.Recreate("users", func(t *migrate.Table) {
			t.ID()
			t.String("email")
		})
	}))
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up with recreate: %v", err)
	}

	if got := count(t, db, "SELECT COUNT(*) FROM users"); got != 2 {
		t.Errorf("rows must survive the rebuild, got %d", got)
	}
	if got := count(t, db, "SELECT id FROM users WHERE email = 'b@x.dev'"); got != 2 {
		t.Errorf("ids must survive the copy, got %d", got)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM users WHERE logins = 0"); got != 2 {
		t.Errorf("the skipped column should take its default, got %d", got)
	}
	if _, err := db.Exec("INSERT INTO users (email) VALUES ('a@x.dev')"); err == nil {
		t.Error("the rebuilt unique index should reject duplicates")
	}
	if got := count(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'users_email_unique'`); got != 1 {
		t.Error("the rebuilt index should carry the conventional final name")
	}
	if got := count(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE name LIKE '%__migrate_new%'`); got != 0 {
		t.Error("no temporary object may survive the rebuild")
	}

	if err := m.Rollback(ctx, 1); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	mustExec(t, db, "INSERT INTO users (email) VALUES ('a@x.dev')")
	if got := count(t, db, "SELECT COUNT(*) FROM users WHERE email = 'a@x.dev'"); got != 2 {
		t.Errorf("after rollback duplicates are allowed again, got %d", got)
	}
}

// Recreate must restore triggers removed with SQLite's old table.
func TestSQLiteRecreateKeepsTriggers(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

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
		s.Exec(`CREATE TRIGGER users_audit AFTER INSERT ON users
			BEGIN INSERT INTO audit (email) VALUES (new.email); END`)
	}, migrate.WithDown(func(s *migrate.Schema) {}))
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(c))
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

	if got := count(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND tbl_name = 'users'`); got != 1 {
		t.Fatalf("the trigger must survive the rebuild, got %d", got)
	}
	mustExec(t, db, "INSERT INTO users (email) VALUES ('b@x.dev')")
	if got := count(t, db, "SELECT COUNT(*) FROM audit"); got != 2 {
		t.Errorf("the recreated trigger should keep firing, got %d rows", got)
	}
}

// Fresh must unwind foreign-key dependencies before reapplying migrations.
func TestSQLiteFresh(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	c := appSchema()
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, `INSERT INTO users (email, name) VALUES ('a@x.dev', 'A')`)
	mustExec(t, db, `INSERT INTO posts (user_id, title) VALUES (1, 'hello')`)

	if err := m.Fresh(ctx); err != nil {
		t.Fatalf("Fresh: %v", err)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM users"); got != 0 {
		t.Errorf("users should be recreated empty, got %d rows", got)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM posts"); got != 0 {
		t.Errorf("posts should be recreated empty, got %d rows", got)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM schema_migrations"); got != 2 {
		t.Errorf("all migrations should be re-recorded, got %d", got)
	}
}

// Fresh must handle a records table in an attached SQLite schema.
func TestSQLiteFreshQualifiedRecordsTable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := mustOpen(t, "file:"+filepath.Join(dir, "main.db"))
	// ATTACH is connection-local.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("ATTACH DATABASE '" + filepath.Join(dir, "aux.db") + "' AS aux"); err != nil {
		t.Fatalf("attach: %v", err)
	}

	c := migrate.NewCollection()
	c.Add("001_users", func(s *migrate.Schema) {
		s.Create("users", func(t *migrate.Table) { t.ID() })
	})
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(c),
		migrate.WithTable("aux.schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, "INSERT INTO users (id) VALUES (1)")

	if err := m.Fresh(ctx); err != nil {
		t.Fatalf("Fresh: %v", err)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM users"); got != 0 {
		t.Errorf("users should be recreated empty, got %d rows", got)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM aux.schema_migrations"); got != 1 {
		t.Errorf("the records table should hold exactly the fresh run, got %d rows", got)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM users"); got != 0 {
		t.Errorf("users must still exist and be empty, got %d", got)
	}
}

func TestSQLiteQualifiedNames(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := mustOpen(t, "file:"+filepath.Join(dir, "main.db")+"?_pragma=foreign_keys(1)")
	// ATTACH is connection-local.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("ATTACH DATABASE '" + filepath.Join(dir, "aux.db") + "' AS aux"); err != nil {
		t.Fatalf("attach: %v", err)
	}

	c := migrate.NewCollection()
	c.Add("001_aux_items", func(s *migrate.Schema) {
		s.Create("aux.items", func(t *migrate.Table) {
			t.ID()
			t.String("name").Unique()
		})
	})
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, "INSERT INTO aux.items (name) VALUES ('x')")
	if _, err := db.Exec("INSERT INTO aux.items (name) VALUES ('x')"); err == nil {
		t.Error("the unique index should exist in the attached schema")
	}
	if err := m.Rollback(ctx, 1); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM sqlite_master WHERE name = 'items'"); got != 0 {
		t.Error("the main schema must stay untouched")
	}
}

// Recreate must preserve generated columns, checks, and converted data.
func TestSQLiteGeneratedChecksAndCopyFrom(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	c := migrate.NewCollection()
	c.Add("001_people", func(s *migrate.Schema) {
		s.Create("people", func(t *migrate.Table) {
			t.ID()
			t.String("first")
			t.String("last")
			t.String("full").StoredAs("first || ' ' || last")
			t.String("age") // stringly typed on purpose; fixed below
			t.Check("people_first_nonempty", "length(first) > 0")
		})
	})
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, `INSERT INTO people (first, last, age) VALUES ('Ada', 'Lovelace', '36')`)
	if got := count(t, db, `SELECT COUNT(*) FROM people WHERE full = 'Ada Lovelace'`); got != 1 {
		t.Error("the stored generated column should compute")
	}
	if _, err := db.Exec(`INSERT INTO people (first, last, age) VALUES ('', 'X', '1')`); err == nil {
		t.Error("the check constraint should reject empty first names")
	}

	c.Add("002_age_integer", func(s *migrate.Schema) {
		s.Recreate("people", func(t *migrate.Table) {
			t.ID()
			t.String("first")
			t.String("last")
			t.String("full").StoredAs("first || ' ' || last")
			t.Integer("age").CopyFrom("CAST(age AS INTEGER)")
			t.Check("people_first_nonempty", "length(first) > 0")
		})
	}, migrate.WithDown(func(s *migrate.Schema) {}))
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up with recreate: %v", err)
	}
	if got := count(t, db, `SELECT age + 1 FROM people WHERE first = 'Ada'`); got != 37 {
		t.Errorf("age should be a real integer after CopyFrom cast, got %d", got)
	}
	if got := count(t, db, `SELECT COUNT(*) FROM people WHERE full = 'Ada Lovelace'`); got != 1 {
		t.Error("the generated column should recompute after the rebuild")
	}
}

func TestSQLitePartialUniqueIndex(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	c := migrate.NewCollection()
	c.Add("001_create_users", func(s *migrate.Schema) {
		s.Create("users", func(t *migrate.Table) {
			t.ID()
			t.String("name")
			t.TimestampTz("deleted_at").Nullable()
			t.Unique("name").Where("deleted_at IS NULL")
		})
	})
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(c))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}

	mustExec(t, db, `INSERT INTO users (name) VALUES ('alice')`)
	if _, err := db.Exec(`INSERT INTO users (name) VALUES ('alice')`); err == nil {
		t.Fatal("a live duplicate name should violate the partial unique index")
	}
	mustExec(t, db, `UPDATE users SET deleted_at = '2026-01-01 00:00:00' WHERE name = 'alice'`)
	if _, err := db.Exec(`INSERT INTO users (name) VALUES ('alice')`); err != nil {
		t.Fatalf("a soft-deleted row should release the name: %v", err)
	}
}

func TestSQLiteDescIndex(t *testing.T) {
	db := openSQLite(t)
	migrateDescIndex(t, db, migrate.SQLite)
	rows, err := db.Query(`PRAGMA index_xinfo('posts_created_at_id_index')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var seq, cid, desc, key int
		var name, coll sql.NullString
		if err := rows.Scan(&seq, &cid, &name, &desc, &coll, &key); err != nil {
			t.Fatal(err)
		}
		if key == 1 {
			got[name.String] = desc
		}
	}
	if got["created_at"] != 1 || got["id"] != 0 {
		t.Fatalf("index directions = %v, want created_at descending and id ascending", got)
	}
}
