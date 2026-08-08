package migrate

import (
	"errors"
	"testing"
)

// MySQL renders Change as a complete MODIFY COLUMN definition.
func TestChangeColumnMySQL(t *testing.T) {
	got := compileSchema(t, MySQL, func(s *Schema) {
		s.Table("users", func(t *Table) {
			t.String("name", 500).Nullable().Default("unknown").Comment("display name").Change()
		})
	})
	assertSQL(t, got, []string{
		"ALTER TABLE `users` MODIFY COLUMN `name` VARCHAR(500) DEFAULT 'unknown' COMMENT 'display name'",
	})
}

// PostgreSQL renders each changed attribute separately.
func TestChangeColumnPostgres(t *testing.T) {
	got := compileSchema(t, Postgres, func(s *Schema) {
		s.Table("users", func(t *Table) {
			t.String("name", 500).Default("unknown").Change()
		})
	})
	assertSQL(t, got, []string{
		`ALTER TABLE "users" ALTER COLUMN "name" TYPE VARCHAR(500)`,
		`ALTER TABLE "users" ALTER COLUMN "name" SET NOT NULL`,
		`ALTER TABLE "users" ALTER COLUMN "name" SET DEFAULT 'unknown'`,
	})

	nullable := compileSchema(t, Postgres, func(s *Schema) {
		s.Table("users", func(t *Table) {
			t.Integer("age").Nullable().Change().Using("age::integer")
		})
	})
	assertSQL(t, nullable, []string{
		`ALTER TABLE "users" ALTER COLUMN "age" TYPE INTEGER USING age::integer`,
		`ALTER TABLE "users" ALTER COLUMN "age" DROP NOT NULL`,
		`ALTER TABLE "users" ALTER COLUMN "age" DROP DEFAULT`,
	})
}

func TestChangeColumnSQLiteRefused(t *testing.T) {
	err := compileErr(SQLite, func(s *Schema) {
		s.Table("users", func(t *Table) { t.String("name", 500).Change() })
	})
	assertErrContains(t, err, "Schema.Recreate")
}

func TestChangeColumnDeclarationRules(t *testing.T) {
	err := compileErr(SQLite, func(s *Schema) {
		s.Create("users", func(t *Table) {
			t.ID()
			t.String("name").Change()
		})
	})
	assertErrContains(t, err, "only valid inside Schema.Table")

	err = compileErr(Postgres, func(s *Schema) {
		s.Table("users", func(t *Table) { t.String("name").Using("name::text") })
	})
	assertErrContains(t, err, "Using without Change")

	err = compileErr(Postgres, func(s *Schema) {
		s.Table("users", func(t *Table) { t.String("name").Unique().Change() })
	})
	assertErrContains(t, err, "Unique/Index")

	err = compileErr(MySQL, func(s *Schema) {
		s.Table("users", func(t *Table) { t.Integer("age").Change().Using("age::integer") })
	})
	assertErrContains(t, err, "implicitly")

	err = compileErr(Postgres, func(s *Schema) {
		s.Table("users", func(t *Table) { t.Enum("role", "a", "b").Change() })
	})
	assertErrContains(t, err, "check constraint")
}

func TestChangeColumnIrreversible(t *testing.T) {
	m := &Migration{name: "m", useTx: true, up: func(s *Schema) {
		s.Table("users", func(t *Table) { t.String("name", 500).Change() })
	}}
	if _, err := m.downOps(); !errors.Is(err, ErrIrreversible) {
		t.Fatalf("downOps error = %v, want ErrIrreversible", err)
	}
}
