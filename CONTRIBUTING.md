# Contributing to migrate

## Prerequisites

- Go 1.27 or later.
- Docker, only for the PostgreSQL, MySQL, and ClickHouse integration tests.
  SQLite needs nothing.

## Clone

```bash
git clone https://github.com/go-rio/migrate.git
cd migrate
```

## Tests

The library has no third-party dependencies and its unit tests need no
database:

```bash
gofmt -l .
go vet ./...
go test -race -shuffle=on ./...
```

The integration suite is a separate module under `integration/` (its own
`go.mod` pulls the database drivers). SQLite always runs; PostgreSQL, MySQL,
and ClickHouse run when their DSN variable is set and skip otherwise:

```bash
cd integration
go test -race -shuffle=on -count=1 ./...
```

Start the servers with Docker:

```bash
docker run -d --name migrate-postgres -p 15432:5432 -e POSTGRES_PASSWORD=bench postgres:18-alpine
docker run -d --name migrate-mysql -p 13306:3306 -e MYSQL_ROOT_PASSWORD=bench -e MYSQL_DATABASE=bench mysql:8.4
docker run -d --name migrate-clickhouse -p 19000:9000 -e CLICKHOUSE_USER=default -e CLICKHOUSE_PASSWORD=rio -e CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1 clickhouse/clickhouse-server:26.7-alpine
```

Then run the suite against all four databases:

```bash
cd integration
MIGRATE_POSTGRES_DSN='postgres://postgres:bench@127.0.0.1:15432/postgres?sslmode=disable' \
MIGRATE_MYSQL_DSN='root:bench@tcp(127.0.0.1:13306)/bench?parseTime=true' \
MIGRATE_CLICKHOUSE_DSN='clickhouse://default:rio@127.0.0.1:19000/default' \
go test -race -shuffle=on -count=1 ./...
```

| Variable | Driver | Gates |
|---|---|---|
| `MIGRATE_POSTGRES_DSN` | `github.com/jackc/pgx/v5/stdlib` | `TestPostgres*` |
| `MIGRATE_MYSQL_DSN` | `github.com/go-sql-driver/mysql` | `TestMySQL*` |
| `MIGRATE_CLICKHOUSE_DSN` | `github.com/ClickHouse/clickhouse-go/v2` | `TestClickHouse*` |

The suite drops and recreates its tables, so point it at a scratch
database. Fuzz targets run on demand:

```bash
go test -run='^$' -fuzz=FuzzLiteral -fuzztime=30s .
go test -run='^$' -fuzz=FuzzQuoterIdent -fuzztime=30s .
go test -run='^$' -fuzz=FuzzGuessParentTable -fuzztime=30s .
```

## Pull requests

- Every change ships with tests. Keep one test file per source file
  (`table.go` pairs with `table_test.go`), and add an integration case when
  a dialect's real behavior is involved.
- Commit messages use conventional prefixes: `feat`, `fix`, `docs`, `style`,
  `refactor`, `test`, `chore`, `ci`. Mark API breaks with `feat!` and a
  `BREAKING CHANGE:` footer.
- `gofmt` and `go vet` must be clean; CI also runs `golangci-lint` and
  `govulncheck`.
- The public API is a commitment: do not rename, remove, or reorder exported
  identifiers unless the change is a deliberate, documented break.

## Comment house style

- Every exported identifier carries a doc comment that starts with its name
  and states purpose, when to use it, constraints, and error cases. No
  `Parameters:`/`Returns:` lists, no marketing words, no history narrative.
- Internal comments state contracts: one line normally, two at most. Delete
  paraphrase, signature restatement, and what-was-tried narrative; keep
  non-obvious invariants (SQLite table recreation rules, the lock arbitration
  in `migrator.go`, dialect DDL quirks).
- Within a file, order declarations as imports, constants, types with each
  type's constructor and methods grouped immediately after it, then helpers.

## Releases

- Tags are signed: `git tag -s v0.13.0 -m "v0.13.0"`.
- Before tagging, move the `[Unreleased]` entries in `CHANGELOG.md` under a
  new version heading with the release date and add its compare link.
- Pushing the tag runs the release workflow, which publishes the GitHub
  release with GoReleaser (no binaries; this is a library).
