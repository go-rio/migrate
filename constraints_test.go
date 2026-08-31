package migrate

import (
	"strings"
	"testing"
)

func TestUniqueConstraintCreateInline(t *testing.T) {
	create := func(s *Schema) {
		s.Create("inventory", func(t *Table) {
			t.ID()
			t.BigInteger("owner_id")
			t.String("client_ref")
			t.UniqueConstraint("uk_inventory_client_ref", "owner_id", "client_ref")
		})
	}
	pg := compileSchema(t, Postgres, create)
	if want := `CONSTRAINT "uk_inventory_client_ref" UNIQUE ("owner_id", "client_ref")`; !strings.Contains(pg[0], want) {
		t.Fatalf("postgres create = %s, want inline %s", pg[0], want)
	}
	my := compileSchema(t, MySQL, create)
	if want := "CONSTRAINT `uk_inventory_client_ref` UNIQUE (`owner_id`, `client_ref`)"; !strings.Contains(my[0], want) {
		t.Fatalf("mysql create = %s, want inline %s", my[0], want)
	}
	lite := compileSchema(t, SQLite, create)
	if want := `CONSTRAINT "uk_inventory_client_ref" UNIQUE ("owner_id", "client_ref")`; !strings.Contains(lite[0], want) {
		t.Fatalf("sqlite create = %s, want inline %s", lite[0], want)
	}
}

func TestUniqueConstraintAlter(t *testing.T) {
	alter := func(s *Schema) {
		s.Table("inventory", func(t *Table) {
			t.UniqueConstraint("uk_x", "a", "b")
		})
	}
	assertSQL(t, compileSchema(t, Postgres, alter), []string{
		`ALTER TABLE "inventory" ADD CONSTRAINT "uk_x" UNIQUE ("a", "b")`,
	})
	assertSQL(t, compileSchema(t, MySQL, alter), []string{
		"ALTER TABLE `inventory` ADD CONSTRAINT `uk_x` UNIQUE (`a`, `b`)",
	})
	assertErrContains(t, compileErr(SQLite, alter), "Schema.Recreate")

	drop := func(s *Schema) {
		s.Table("inventory", func(t *Table) { t.DropConstraint("uk_x") })
	}
	assertSQL(t, compileSchema(t, Postgres, drop), []string{
		`ALTER TABLE "inventory" DROP CONSTRAINT "uk_x"`,
	})
	assertSQL(t, compileSchema(t, MySQL, drop), []string{
		"ALTER TABLE `inventory` DROP CONSTRAINT `uk_x`",
	})
	assertErrContains(t, compileErr(SQLite, drop), "Schema.Recreate")
}

func TestUniqueConstraintInverse(t *testing.T) {
	alter := &alterTable{table: "t", changes: []change{
		&addUniqueConstraint{uc: &uniqueConstraintDef{name: "uk_x", columns: []string{"a"}}},
	}}
	inv, err := alter.inverse()
	if err != nil {
		t.Fatal(err)
	}
	dc, ok := inv.(*alterTable).changes[0].(*dropConstraint)
	if !ok || dc.name != "uk_x" {
		t.Fatalf("inverse = %#v, want dropConstraint uk_x", inv.(*alterTable).changes[0])
	}
	if _, err := (&alterTable{table: "t", changes: []change{&dropConstraint{name: "uk_x"}}}).inverse(); err == nil {
		t.Fatal("dropping a constraint must be irreversible")
	}
}

func TestUniqueConstraintValidation(t *testing.T) {
	err := compileErr(Postgres, func(s *Schema) {
		s.Create("t", func(t *Table) { t.ID(); t.UniqueConstraint("", "a") })
	})
	assertErrContains(t, err, "explicit name")
	err = compileErr(Postgres, func(s *Schema) {
		s.Create("t", func(t *Table) { t.ID(); t.UniqueConstraint("uk_x") })
	})
	assertErrContains(t, err, "no columns")
	err = compileErr(ClickHouse, func(s *Schema) {
		s.Create("t", func(t *Table) {
			t.UUID("id")
			t.ClickHouseEngine("MergeTree() ORDER BY id")
			t.UniqueConstraint("uk_x", "id")
		})
	})
	assertErrContains(t, err, "no unique constraints")
}

// A PostgreSQL Recreate's temporary table must not register the constraint
// name while the live table still owns its backing index.
func TestUniqueConstraintRecreateAnonymous(t *testing.T) {
	got := compileSchema(t, Postgres, func(s *Schema) {
		s.Recreate("inventory", func(t *Table) {
			t.ID()
			t.BigInteger("owner_id")
			t.UniqueConstraint("uk_inv_owner", "owner_id")
		})
	})
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, `UNIQUE ("owner_id")`) {
		t.Fatalf("temporary table lost the unique constraint:\n%s", joined)
	}
	if strings.Contains(strings.Split(joined, "ALTER TABLE")[0], "uk_inv_owner") &&
		strings.Contains(got[0], `CONSTRAINT "uk_inv_owner"`) {
		t.Fatalf("temporary table must not name the constraint:\n%s", got[0])
	}
}
