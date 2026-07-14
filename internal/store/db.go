// Package store is the plugin's low-level persistence primitive: it opens
// the database (SQLite or Postgres), runs embedded migrations, and hands
// out a *sql.DB plus a dialect helper. It holds no domain logic — the
// agent package builds its CRUD on top of this.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"

	_ "github.com/lib/pq"  // postgres driver
	_ "modernc.org/sqlite" // pure-Go sqlite driver (no cgo)
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Dialect captures the small SQL differences between the supported
// backends. Today that is just placeholder rebinding.
type Dialect struct{ name string }

var (
	sqliteDialect   = Dialect{"sqlite"}
	postgresDialect = Dialect{"postgres"}
)

// Rebind rewrites `?` placeholders to `$1, $2, …` for Postgres; it is a
// no-op for SQLite. Write every query with `?` and pass it through here.
func (d Dialect) Rebind(query string) string {
	if d.name != "postgres" {
		return query
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}

// DB wraps a *sql.DB and its dialect.
type DB struct {
	db      *sql.DB
	Dialect Dialect
}

// SQL returns the underlying *sql.DB for query execution.
func (d *DB) SQL() *sql.DB { return d.db }

// Close closes the underlying connection.
func (d *DB) Close() error { return d.db.Close() }

// Open opens the database for the given driver/dsn and runs any pending
// migrations. driver defaults to "sqlite".
func Open(driver, dsn string) (*DB, error) {
	if driver == "" {
		driver = "sqlite"
	}

	var (
		raw     *sql.DB
		dialect Dialect
		err     error
	)
	switch driver {
	case "sqlite":
		dialect = sqliteDialect
		if dsn == "" {
			dsn = "./agents.db"
		}
		raw, err = sql.Open("sqlite", dsn+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
		if err != nil {
			return nil, fmt.Errorf("agents store: open sqlite: %w", err)
		}
	case "postgres":
		dialect = postgresDialect
		if dsn == "" {
			return nil, fmt.Errorf("agents store: dsn required for postgres")
		}
		raw, err = sql.Open("postgres", dsn)
		if err != nil {
			return nil, fmt.Errorf("agents store: open postgres: %w", err)
		}
		if err = raw.Ping(); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("agents store: ping postgres: %w", err)
		}
	default:
		return nil, fmt.Errorf("agents store: unsupported driver %q (use sqlite or postgres)", driver)
	}

	d := &DB{db: raw, Dialect: dialect}
	if err := d.migrate(); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return d, nil
}

// migrate applies embedded migrations in filename order, tracking the
// applied version in agents_schema_version.
func (d *DB) migrate() error {
	if _, err := d.db.Exec("CREATE TABLE IF NOT EXISTS agents_schema_version (version INTEGER NOT NULL PRIMARY KEY)"); err != nil {
		return fmt.Errorf("agents store: create schema_version: %w", err)
	}

	current, err := d.currentVersion()
	if err != nil {
		return err
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("agents store: read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		n, err := migrationNumber(name)
		if err != nil || n <= current {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("agents store: read migration %s: %w", name, err)
		}
		tx, err := d.db.Begin()
		if err != nil {
			return fmt.Errorf("agents store: begin migration %s: %w", name, err)
		}
		if _, err := tx.Exec(string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("agents store: apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec("DELETE FROM agents_schema_version"); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("agents store: clear version: %w", err)
		}
		if _, err := tx.Exec(d.Dialect.Rebind("INSERT INTO agents_schema_version (version) VALUES (?)"), n); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("agents store: set version: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("agents store: commit migration %s: %w", name, err)
		}
	}
	return nil
}

func (d *DB) currentVersion() (int, error) {
	row := d.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM agents_schema_version")
	var v int
	if err := row.Scan(&v); err != nil {
		return 0, fmt.Errorf("agents store: read schema version: %w", err)
	}
	return v, nil
}

// migrationNumber extracts the leading integer from a migration filename
// like "001_init.sql".
func migrationNumber(name string) (int, error) {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("migration %q has no leading number", name)
	}
	return strconv.Atoi(name[:i])
}
