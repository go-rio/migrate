package migrate

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnsafe marks a migration rejected by SafetyStrict.
var ErrUnsafe = errors.New("migrate: unsafe migration")

// SafetyLevel controls handling of potentially disruptive operations.
type SafetyLevel int

const (
	// SafetyWarn logs findings and proceeds.
	SafetyWarn SafetyLevel = iota
	// SafetyStrict rejects all findings before execution.
	SafetyStrict
	// SafetyOff disables the analysis.
	SafetyOff
)

// WithSafety sets the safety level. The default is SafetyWarn.
func WithSafety(level SafetyLevel) Option {
	return func(cfg *config) { cfg.safety = level }
}

// Assured marks a migration as reviewed and skips safety analysis.
func Assured() MigrationOption {
	return func(m *Migration) { m.assured = true }
}

// analyzeSafety checks known disruptive schema operations, not raw SQL or Run.
func analyzeSafety(dialect string, ops []operation) []string {
	var findings []string
	warn := func(format string, a ...any) {
		findings = append(findings, fmt.Sprintf(format, a...))
	}
	for _, op := range ops {
		switch o := op.(type) {
		case *recreateTable:
			warn("recreating table %q copies every row while holding locks and recreates its triggers as captured; plan for the copy time on large tables, check the trigger bodies still match the new shape, and note that a table referenced by foreign keys or views cannot be recreated on Postgres", o.def.name)
		case *dropTable:
			warn("dropping table %q breaks application code still using it; deploy code that stopped using it first", o.name)
		case *renameTable:
			warn("renaming table %q to %q is not backward compatible with running code; prefer creating the new table, migrating data and dropping the old one across deploys", o.from, o.to)
		case *alterTable:
			for _, ch := range o.changes {
				switch c := ch.(type) {
				case *addColumn:
					if c.col.change {
						if dialect == "clickhouse" {
							warn("changing column %q of table %q may rewrite a large amount of ClickHouse data; verify the conversion and plan for mutation cost", c.col.name, o.table)
							continue
						}
						warn("changing column %q of table %q rewrites the table under lock on most engines, and a narrowing type or a new NOT NULL fails on rows that no longer fit; backfill first and plan for the rewrite time", c.col.name, o.table)
						continue
					}
					// Generated columns fill existing rows themselves.
					if !c.col.nullable && !c.col.hasDefault && !c.col.useCurrent && !c.col.autoIncr && c.col.generatedExpr == "" {
						if dialect == "clickhouse" {
							warn("adding non-Nullable column %q to existing ClickHouse table %q makes old rows read the type's zero value; declare an explicit Default when that is not the intended value", c.col.name, o.table)
							continue
						}
						warn("adding NOT NULL column %q to existing table %q fails when rows exist; add a Default, or make it Nullable and backfill", c.col.name, o.table)
					}
				case *dropColumn:
					warn("dropping column %q of table %q breaks application code still reading it; deploy code that stopped using it first", c.name, o.table)
				case *renameColumn:
					warn("renaming column %q of table %q to %q is not backward compatible with running code; prefer adding the new column, dual-writing and dropping the old one across deploys", c.from, o.table, c.to)
				case *addIndex:
					if dialect == "postgres" && !c.idx.concurrently {
						warn("adding index %q blocks writes to %q while it builds; on a large table declare it Concurrently() on a WithoutTransaction migration", c.idx.resolvedName(o.table), o.table)
					}
				case *addForeign:
					if dialect == "postgres" {
						warn("adding foreign key %q validates every row of %q under lock; on a large table add it NOT VALID via Exec, then VALIDATE CONSTRAINT separately", c.fk.resolvedName(o.table), o.table)
					}
				case *addPrimary:
					warn("adding a primary key to existing table %q rewrites the table under lock on most engines", o.table)
				case *addUniqueConstraint:
					if dialect == "postgres" {
						warn("adding unique constraint %q builds its index while blocking writes to %q; on a large table CREATE UNIQUE INDEX CONCURRENTLY via Exec on a WithoutTransaction migration, then ADD CONSTRAINT ... USING INDEX", c.uc.name, o.table)
					}
				case *addCheck:
					switch dialect {
					case "postgres":
						warn("adding check constraint %q validates every row of %q under lock; on a large table add it NOT VALID via Exec, then VALIDATE CONSTRAINT separately", c.chk.name, o.table)
					case "clickhouse":
						warn("adding check constraint %q to ClickHouse table %q only constrains future inserts and does not validate historical rows", c.chk.name, o.table)
					}
				}
			}
		}
	}
	return findings
}

// checkSafety collects every strict finding before execution.
func (m *Migrator) checkSafety(migs []*Migration) error {
	if m.cfg.safety == SafetyOff {
		return nil
	}
	var violations []string
	for _, mig := range migs {
		if mig.assured {
			continue
		}
		ops, err := mig.upOps()
		if err != nil {
			return err
		}
		for _, finding := range analyzeSafety(m.d.name(), ops) {
			if m.cfg.safety == SafetyStrict {
				violations = append(violations, fmt.Sprintf("%s: %s", mig.name, finding))
			} else {
				m.cfg.logger.Warn("migrate: safety", "migration", mig.name, "finding", finding)
			}
		}
	}
	if len(violations) == 0 {
		return nil
	}
	var msg strings.Builder
	for _, v := range violations {
		msg.WriteString("\n  - " + v)
	}
	return fmt.Errorf("%w:%s\n(review each finding, then mark the migration Assured() or lower the level with WithSafety)", ErrUnsafe, msg.String())
}
