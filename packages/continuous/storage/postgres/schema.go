package postgres

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// LayoutVersion is the database layout this build creates and accepts.
// Every other version is refused, with no automatic migration.
const LayoutVersion = 1

// Schema is the fixed schema holding all application tables.
const Schema = "zot_continuous"

// Advisory lock keys. PostgreSQL keeps the single-bigint and two-integer
// key spaces separate.
const (
	schemaLockKey   int64 = 6506653941096960001
	writerNamespace int32 = 1515140163
)

// layoutStatements create layout version 1. Each is one execution unit.
var layoutStatements = []string{
	`CREATE SCHEMA zot_continuous`,
	`CREATE TABLE zot_continuous.schema_meta (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    version integer NOT NULL CHECK (version > 0)
)`,
	`INSERT INTO zot_continuous.schema_meta VALUES (true, 1)`,
	`CREATE TABLE zot_continuous.stores (
    id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY CHECK (id > 0),
    name text COLLATE "C" NOT NULL UNIQUE
        CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    store_uuid uuid NOT NULL UNIQUE,
    revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0),
    epoch bigint NOT NULL DEFAULT 0 CHECK (epoch >= 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((revision = 0 AND epoch = 0)
        OR (revision >= epoch AND epoch > 0))
)`,
	`CREATE TABLE zot_continuous.commits (
    store_id integer NOT NULL REFERENCES zot_continuous.stores(id),
    revision bigint NOT NULL CHECK (revision > 0),
    epoch bigint NOT NULL CHECK (epoch > 0 AND epoch <= revision),
    commit_schema integer NOT NULL CHECK (commit_schema = 1),
    committed_time text NOT NULL,
    actor bytea NOT NULL,
    operation_count integer NOT NULL CHECK (operation_count >= 0),
    PRIMARY KEY (store_id, revision)
)`,
	`CREATE TABLE zot_continuous.records (
    store_id integer NOT NULL,
    revision bigint NOT NULL,
    ordinal integer NOT NULL CHECK (ordinal >= 0),
    key bytea NOT NULL CHECK (octet_length(key) BETWEEN 1 AND 4096),
    key_digest bytea NOT NULL CHECK (octet_length(key_digest) = 32),
    key_prefix bytea NOT NULL
        CHECK (key_prefix = substring(key FROM 1 FOR 512)),
    deleted boolean NOT NULL,
    value bytea,
    PRIMARY KEY (store_id, revision, ordinal),
    FOREIGN KEY (store_id, revision)
        REFERENCES zot_continuous.commits(store_id, revision),
    CHECK ((deleted AND value IS NULL)
        OR (NOT deleted AND value IS NOT NULL))
)`,
	`CREATE INDEX records_get
    ON zot_continuous.records (store_id, key_digest, revision DESC)`,
	`CREATE INDEX records_prefix
    ON zot_continuous.records (store_id, key_prefix)`,
}

// expectedColumns is the required column shape: name, type, nullability.
var expectedColumns = map[string][]string{
	"schema_meta": {"singleton boolean NO", "version integer NO"},
	"stores": {"created_at timestamp with time zone NO", "epoch bigint NO", "id integer NO",
		"name text NO", "revision bigint NO", "store_uuid uuid NO"},
	"commits": {"actor bytea NO", "commit_schema integer NO", "committed_time text NO", "epoch bigint NO",
		"operation_count integer NO", "revision bigint NO", "store_id integer NO"},
	"records": {"deleted boolean NO", "key bytea NO", "key_digest bytea NO", "key_prefix bytea NO",
		"ordinal integer NO", "revision bigint NO", "store_id integer NO", "value bytea YES"},
}

// expectedIndexes are the required secondary index definitions, compared
// in the normalized form PostgreSQL reports.
var expectedIndexes = map[string]string{
	"records_get":    "CREATE INDEX records_get ON zot_continuous.records USING btree (store_id, key_digest, revision DESC)",
	"records_prefix": "CREATE INDEX records_prefix ON zot_continuous.records USING btree (store_id, key_prefix)",
}

// expectedConstraints is the minimum count of constraints per type and
// table: primary key (p), unique (u), foreign key (f), check (c).
var expectedConstraints = map[string]map[string]int{
	"schema_meta": {"p": 1, "c": 2},
	"stores":      {"p": 1, "u": 2, "c": 5},
	"commits":     {"p": 1, "f": 1, "c": 4},
	"records":     {"p": 1, "f": 1, "c": 5},
}

// ensureLayout validates an existing layout, or creates it when allowed.
// It runs inside the initialization transaction, after the schema lock.
// It reports whether the schema exists afterwards.
func ensureLayout(ctx context.Context, tx pgx.Tx, create bool) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)`, Schema).Scan(&exists); err != nil {
		return false, safeError("inspect schema", err)
	}
	if !exists {
		if !create {
			return false, nil
		}
		for _, stmt := range layoutStatements {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return false, safeError("create schema", err)
			}
		}
		return true, nil
	}
	return true, validateLayout(ctx, tx)
}

func validateLayout(ctx context.Context, tx pgx.Tx) error {
	var tables int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_tables WHERE schemaname = $1 AND tablename = 'schema_meta'`, Schema).Scan(&tables); err != nil {
		return safeError("inspect schema", err)
	}
	if tables != 1 {
		return fmt.Errorf("continuous postgres schema %s exists without layout metadata, refusing to use it", Schema)
	}
	var rows, version int
	if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(max(version), 0) FROM zot_continuous.schema_meta`).Scan(&rows, &version); err != nil {
		return safeError("read layout version", err)
	}
	if rows != 1 {
		return fmt.Errorf("continuous postgres layout metadata is inconsistent")
	}
	if version != LayoutVersion {
		return fmt.Errorf("continuous postgres layout version %d is not supported by this build (supports %d), no automatic migration is performed", version, LayoutVersion)
	}
	columns := map[string][]string{}
	r, err := tx.Query(ctx, `SELECT table_name, column_name, data_type, is_nullable FROM information_schema.columns WHERE table_schema = $1`, Schema)
	if err != nil {
		return safeError("inspect layout", err)
	}
	for r.Next() {
		var table, column, typ, nullable string
		if err := r.Scan(&table, &column, &typ, &nullable); err != nil {
			r.Close()
			return safeError("inspect layout", err)
		}
		columns[table] = append(columns[table], column+" "+typ+" "+nullable)
	}
	r.Close()
	if err := r.Err(); err != nil {
		return safeError("inspect layout", err)
	}
	for table, want := range expectedColumns {
		got := columns[table]
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			return fmt.Errorf("continuous postgres table %s.%s does not match layout version %d", Schema, table, LayoutVersion)
		}
	}
	for name, want := range expectedIndexes {
		var def string
		err := tx.QueryRow(ctx, `SELECT indexdef FROM pg_catalog.pg_indexes WHERE schemaname = $1 AND indexname = $2`, Schema, name).Scan(&def)
		if err == pgx.ErrNoRows || (err == nil && def != want) {
			return fmt.Errorf("continuous postgres index %s does not match layout version %d", name, LayoutVersion)
		}
		if err != nil {
			return safeError("inspect layout", err)
		}
	}
	for table, kinds := range expectedConstraints {
		for kind, min := range kinds {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_constraint c JOIN pg_catalog.pg_class t ON t.oid = c.conrelid JOIN pg_catalog.pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = $1 AND t.relname = $2 AND c.contype = $3`, Schema, table, kind).Scan(&n); err != nil {
				return safeError("inspect layout", err)
			}
			if n < min {
				return fmt.Errorf("continuous postgres table %s.%s is missing required constraints", Schema, table)
			}
		}
	}
	return nil
}
