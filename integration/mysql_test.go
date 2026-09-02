package integration

import (
	"database/sql"
	"os"
	"testing"

	"github.com/go-rio/migrate"

	_ "github.com/go-sql-driver/mysql"
)

// openMySQL skips unless MIGRATE_MYSQL_DSN is set.
func openMySQL(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MIGRATE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("MIGRATE_MYSQL_DSN not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open mysql: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping mysql: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMySQLEndToEnd(t *testing.T)      { runEndToEnd(t, openMySQL(t), migrate.MySQL) }
func TestMySQLChecksumFlow(t *testing.T)  { runChecksumFlow(t, openMySQL(t), migrate.MySQL) }
func TestMySQLDataMigration(t *testing.T) { runDataMigration(t, openMySQL(t), migrate.MySQL) }
func TestMySQLBaseline(t *testing.T)      { runBaseline(t, openMySQL(t), migrate.MySQL) }

func TestMySQLRepeatable(t *testing.T) { runRepeatable(t, openMySQL(t), migrate.MySQL) }

func TestMySQLDescIndex(t *testing.T) {
	db := openMySQL(t)
	migrateDescIndex(t, db, migrate.MySQL)
	rows, err := db.Query(`SELECT COLUMN_NAME, COLLATION FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'posts' AND INDEX_NAME = 'posts_created_at_id_index'
		ORDER BY SEQ_IN_INDEX`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var column, collation string
		if err := rows.Scan(&column, &collation); err != nil {
			t.Fatal(err)
		}
		got = append(got, column+":"+collation)
	}
	if len(got) != 2 || got[0] != "created_at:D" || got[1] != "id:A" {
		t.Fatalf("index columns = %v, want created_at descending and id ascending", got)
	}
}
