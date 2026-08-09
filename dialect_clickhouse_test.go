package migrate

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestClickHouseCreateTable(t *testing.T) {
	got := compileSchema(t, ClickHouse, func(s *Schema) {
		s.Create("analytics.events", func(t *Table) {
			t.UUID("id")
			t.UUID("trace_id").DefaultExpr("generateUUIDv4()")
			t.String("tenant_id", 12).Default("a'b\\c").Comment("tenant's id")
			t.TimestampTz("occurred_at").UseCurrent()
			t.JSON("payload").Nullable()
			t.Integer("kind").Unsigned()
			t.Integer("computed").StoredAs("kind + 1")
			t.Integer("alias").VirtualAs("kind + 2")
			t.Check("events_tenant_not_empty", "tenant_id != ''")
			t.ClickHouseEngine("MergeTree() PARTITION BY toYYYYMM(occurred_at) ORDER BY (tenant_id, occurred_at)")
			t.Comment("event's stream")
		})
	})
	want := []string{
		"CREATE TABLE `analytics`.`events` (\n" +
			"\t`id` UUID,\n" +
			"\t`trace_id` UUID DEFAULT (generateUUIDv4()),\n" +
			"\t`tenant_id` String DEFAULT 'a''b\\\\c' COMMENT 'tenant''s id',\n" +
			"\t`occurred_at` DateTime64(6, 'UTC') DEFAULT now64(6),\n" +
			"\t`payload` Nullable(JSON),\n" +
			"\t`kind` UInt32,\n" +
			"\t`computed` Int32 MATERIALIZED (kind + 1),\n" +
			"\t`alias` Int32 ALIAS (kind + 2),\n" +
			"\tCONSTRAINT `events_tenant_not_empty` CHECK (tenant_id != '')\n" +
			") ENGINE = MergeTree() PARTITION BY toYYYYMM(occurred_at) ORDER BY (tenant_id, occurred_at) COMMENT 'event''s stream'",
	}
	assertSQL(t, got, want)
}

func TestClickHouseColumnKinds(t *testing.T) {
	got := compileSchema(t, ClickHouse, func(s *Schema) {
		s.Create("kitchen", func(t *Table) {
			t.String("str", 10)
			t.Char("fixed", 4)
			t.Text("txt")
			t.Binary("bin")
			t.TinyInteger("i8")
			t.TinyInteger("u8").Unsigned()
			t.SmallInteger("i16")
			t.SmallInteger("u16").Unsigned()
			t.Integer("i32")
			t.Integer("u32").Unsigned()
			t.BigInteger("i64")
			t.BigInteger("u64").Unsigned()
			t.Boolean("flag")
			t.Decimal("dec", 18, 4)
			t.Float("f32")
			t.Double("f64")
			t.Date("date")
			t.Time("time")
			t.DateTime("datetime")
			t.Timestamp("timestamp")
			t.TimestampTz("timestamp_tz")
			t.JSON("json")
			t.UUID("uuid")
			t.Enum("mood", "up", "down")
			t.Column("raw", "LowCardinality(String)")
			t.ForeignID("owner_id")
			t.ClickHouseEngine("MergeTree() ORDER BY tuple()")
		})
	})[0]

	fragments := []string{
		"`str` String", "`fixed` FixedString(4)", "`txt` String", "`bin` String",
		"`i8` Int8", "`u8` UInt8", "`i16` Int16", "`u16` UInt16",
		"`i32` Int32", "`u32` UInt32", "`i64` Int64", "`u64` UInt64",
		"`flag` Bool", "`dec` Decimal(18, 4)", "`f32` Float32", "`f64` Float64",
		"`date` Date", "`time` Time", "`datetime` DateTime64(6)",
		"`timestamp` DateTime64(6)", "`timestamp_tz` DateTime64(6, 'UTC')",
		"`json` JSON", "`uuid` UUID", "`mood` Enum8('up' = 1, 'down' = 2)",
		"`raw` LowCardinality(String)", "`owner_id` UInt64",
	}
	for _, fragment := range fragments {
		if !strings.Contains(got, fragment) {
			t.Errorf("ClickHouse create should contain %q, got:\n%s", fragment, got)
		}
	}
}

func TestClickHouseEnumCapacity(t *testing.T) {
	values := make([]string, 128)
	for i := range values {
		values[i] = fmt.Sprintf("value_%03d", i)
	}
	got := compileSchema(t, ClickHouse, func(s *Schema) {
		s.Create("enum16_table", func(t *Table) {
			t.Enum("value", values...)
			t.ClickHouseEngine("MergeTree() ORDER BY tuple()")
		})
	})[0]
	if !strings.Contains(got, "`value` Enum16('value_000' = 1") || !strings.Contains(got, "'value_127' = 128)") {
		t.Fatalf("128 values should use stable Enum16 numbering, got:\n%s", got)
	}

	duplicateErr := compileErr(ClickHouse, func(s *Schema) {
		s.Create("duplicate_enum", func(t *Table) {
			t.Enum("value", "same", "same")
			t.ClickHouseEngine("MergeTree() ORDER BY tuple()")
		})
	})
	assertErrContains(t, duplicateErr, "duplicate value")

	tooMany := make([]string, 32768)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("v%d", i)
	}
	overflowErr := compileErr(ClickHouse, func(s *Schema) {
		s.Create("overflow_enum", func(t *Table) {
			t.Enum("value", tooMany...)
			t.ClickHouseEngine("MergeTree() ORDER BY tuple()")
		})
	})
	assertErrContains(t, overflowErr, "at most 32767")
}

func TestClickHouseEngineDeclaration(t *testing.T) {
	tests := []struct {
		name string
		fn   func(*Schema)
		want string
	}{
		{
			name: "missing",
			fn: func(s *Schema) {
				s.Create("events", func(t *Table) { t.UUID("id") })
			},
			want: "requires ClickHouseEngine",
		},
		{
			name: "empty",
			fn: func(s *Schema) {
				s.Create("events", func(t *Table) {
					t.UUID("id")
					t.ClickHouseEngine("  ")
				})
			},
			want: "must not be empty",
		},
		{
			name: "duplicate",
			fn: func(s *Schema) {
				s.Create("events", func(t *Table) {
					t.UUID("id")
					t.ClickHouseEngine("MergeTree() ORDER BY id")
					t.ClickHouseEngine("Log")
				})
			},
			want: "more than once",
		},
		{
			name: "alter",
			fn: func(s *Schema) {
				s.Table("events", func(t *Table) { t.ClickHouseEngine("MergeTree() ORDER BY tuple()") })
			},
			want: "only valid inside Schema.Create",
		},
		{
			name: "engine prefix",
			fn: func(s *Schema) {
				s.Create("events", func(t *Table) {
					t.UUID("id")
					t.ClickHouseEngine("ENGINE = MergeTree() ORDER BY id")
				})
			},
			want: "must omit ENGINE =",
		},
		{
			name: "semicolon",
			fn: func(s *Schema) {
				s.Create("events", func(t *Table) {
					t.UUID("id")
					t.ClickHouseEngine("MergeTree() ORDER BY id;")
				})
			},
			want: "trailing semicolon",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertErrContains(t, compileErr(ClickHouse, tt.fn), tt.want)
		})
	}
}

func TestClickHouseEngineIgnoredByOtherDialects(t *testing.T) {
	without := func(s *Schema) {
		s.Create("events", func(t *Table) { t.UUID("id") })
	}
	with := func(s *Schema) {
		s.Create("events", func(t *Table) {
			t.UUID("id")
			t.ClickHouseEngine("MergeTree() ORDER BY id")
		})
	}
	for _, dialect := range []Dialect{Postgres, MySQL, SQLite} {
		t.Run(dialect.name(), func(t *testing.T) {
			if a, b := compileSchema(t, dialect, without), compileSchema(t, dialect, with); !reflect.DeepEqual(a, b) {
				t.Fatalf("ClickHouseEngine changed %s output:\nwithout: %v\nwith: %v", dialect.name(), a, b)
			}
		})
	}

	a := &Migration{name: "a", up: func(s *Schema) {
		s.Create("events", func(t *Table) {
			t.UUID("id")
			t.ClickHouseEngine("MergeTree() ORDER BY id")
		})
	}, useTx: true}
	b := &Migration{name: "b", up: func(s *Schema) {
		s.Create("events", func(t *Table) {
			t.UUID("id")
			t.ClickHouseEngine("MergeTree() ORDER BY tuple()")
		})
	}, useTx: true}
	sumA, err := a.checksum(ClickHouse)
	if err != nil {
		t.Fatalf("checksum A: %v", err)
	}
	sumB, err := b.checksum(ClickHouse)
	if err != nil {
		t.Fatalf("checksum B: %v", err)
	}
	if sumA == sumB {
		t.Fatal("ClickHouseEngine must participate in the ClickHouse checksum")
	}
}

func TestClickHouseAlterTable(t *testing.T) {
	got := compileSchema(t, ClickHouse, func(s *Schema) {
		s.Table("analytics.events", func(t *Table) {
			t.String("source").Default("api").After("id")
			t.String("tenant").Nullable().First().Change()
			t.RenameColumn("source", "origin")
			t.DropColumn("legacy")
			t.Check("tenant_not_empty", "tenant != ''")
			t.DropCheck("old_check")
			t.Comment("new comment")
		})
		s.Rename("analytics.events", "analytics.events_v2")
		s.DropIfExists("analytics.old_events")
	})
	want := []string{
		"ALTER TABLE `analytics`.`events` ADD COLUMN `source` String DEFAULT 'api' AFTER `id`",
		"ALTER TABLE `analytics`.`events` MODIFY COLUMN `tenant` Nullable(String) FIRST",
		"ALTER TABLE `analytics`.`events` RENAME COLUMN `source` TO `origin`",
		"ALTER TABLE `analytics`.`events` DROP COLUMN `legacy`",
		"ALTER TABLE `analytics`.`events` ADD CONSTRAINT `tenant_not_empty` CHECK (tenant != '')",
		"ALTER TABLE `analytics`.`events` DROP CONSTRAINT `old_check`",
		"ALTER TABLE `analytics`.`events` MODIFY COMMENT 'new comment'",
		"RENAME TABLE `analytics`.`events` TO `analytics`.`events_v2`",
		"DROP TABLE IF EXISTS `analytics`.`old_events`",
	}
	assertSQL(t, got, want)
}

func TestClickHouseRejectsRelationalFeatures(t *testing.T) {
	create := func(body func(*Table)) func(*Schema) {
		return func(s *Schema) {
			s.Create("events", func(t *Table) {
				body(t)
				t.ClickHouseEngine("MergeTree() ORDER BY tuple()")
			})
		}
	}
	alter := func(body func(*Table)) func(*Schema) {
		return func(s *Schema) { s.Table("events", body) }
	}
	tests := []struct {
		name string
		fn   func(*Schema)
		want string
	}{
		{name: "id", fn: create(func(t *Table) { t.ID() }), want: "generate a UUID or integer id"},
		{name: "auto increment", fn: create(func(t *Table) { t.Integer("id").AutoIncrement() }), want: "no traditional auto-increment"},
		{name: "column primary", fn: create(func(t *Table) { t.UUID("id").Primary() }), want: "ClickHouseEngine"},
		{name: "table primary", fn: create(func(t *Table) { t.UUID("id"); t.Primary("id") }), want: "ClickHouseEngine"},
		{name: "column unique", fn: create(func(t *Table) { t.UUID("id").Unique() }), want: "application"},
		{name: "table unique", fn: create(func(t *Table) { t.UUID("id"); t.Unique("id") }), want: "ADD INDEX"},
		{name: "column index", fn: create(func(t *Table) { t.UUID("id").Index() }), want: "skipping-index"},
		{name: "table index", fn: create(func(t *Table) { t.UUID("id"); t.Index("id") }), want: "Schema.Exec"},
		{name: "expression index", fn: create(func(t *Table) { t.UUID("id"); t.IndexExpr("idx", "lower(id)") }), want: "ADD INDEX"},
		{name: "fulltext index", fn: create(func(t *Table) { t.String("body"); t.FullText("body") }), want: "skipping-index"},
		{name: "spatial index", fn: create(func(t *Table) { t.String("point"); t.Spatial("point") }), want: "Schema.Exec"},
		{name: "foreign key", fn: create(func(t *Table) { t.ForeignID("owner_id").Constrained("owners") }), want: "no equivalent foreign-key"},
		{name: "on update", fn: create(func(t *Table) { t.Timestamp("updated_at").UseCurrentOnUpdate() }), want: "update the value explicitly"},
		{name: "unsigned raw", fn: create(func(t *Table) { t.Column("value", "Int128").Unsigned() }), want: "explicit UInt type"},
		{name: "using", fn: alter(func(t *Table) { t.Integer("value").Change().Using("toInt32(value)") }), want: "PostgreSQL conversion"},
		{name: "add index", fn: alter(func(t *Table) { t.Index("id") }), want: "skipping-index"},
		{name: "drop index", fn: alter(func(t *Table) { t.DropIndex("id") }), want: "Schema.Exec"},
		{name: "rename index", fn: alter(func(t *Table) { t.RenameIndex("a", "b") }), want: "ADD INDEX"},
		{name: "add foreign", fn: alter(func(t *Table) { t.Foreign("owner_id").References("owners") }), want: "application"},
		{name: "drop foreign", fn: alter(func(t *Table) { t.DropForeign("owner_id") }), want: "UInt64"},
		{name: "add primary", fn: alter(func(t *Table) { t.Primary("id") }), want: "sparse primary key"},
		{name: "drop primary", fn: alter(func(t *Table) { t.DropPrimary() }), want: "ClickHouseEngine"},
		{
			name: "recreate",
			fn: func(s *Schema) {
				s.Recreate("events", func(t *Table) { t.UUID("id") })
			},
			want: "cannot recreate",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertErrContains(t, compileErr(ClickHouse, tt.fn), tt.want)
		})
	}
}
