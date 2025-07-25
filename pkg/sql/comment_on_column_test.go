// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sql_test

import (
	"context"
	gosql "database/sql"
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

func TestCommentOnColumn(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	runCommentOnTests(t, func(db *gosql.DB) {
		if _, err := db.Exec(`
		CREATE DATABASE d;
		SET DATABASE = d;
		CREATE TABLE t (c1 INT, c2 INT, c3 INT);
	`); err != nil {
			t.Fatal(err)
		}

		testCases := []struct {
			exec   string
			query  string
			expect gosql.NullString
		}{
			{
				`COMMENT ON COLUMN t.c1 IS 'foo'`,
				`SELECT col_description(attrelid, attnum) FROM pg_attribute WHERE attrelid = 't'::regclass AND attname = 'c1'`,
				gosql.NullString{String: `foo`, Valid: true},
			},
			{
				`TRUNCATE t`,
				`SELECT col_description(attrelid, attnum) FROM pg_attribute WHERE attrelid = 't'::regclass AND attname = 'c1'`,
				gosql.NullString{String: `foo`, Valid: true},
			},
			{
				`ALTER TABLE t RENAME COLUMN c1 TO c1_1`,
				`SELECT col_description(attrelid, attnum) FROM pg_attribute WHERE attrelid = 't'::regclass AND attname = 'c1_1'`,
				gosql.NullString{String: `foo`, Valid: true},
			},
			{
				`COMMENT ON COLUMN t.c1_1 IS NULL`,
				`SELECT col_description(attrelid, attnum) FROM pg_attribute WHERE attrelid = 't'::regclass AND attname = 'c1_1'`,
				gosql.NullString{Valid: false},
			},
			{
				`COMMENT ON COLUMN t.c3 IS 'foo'`,
				`SELECT col_description(attrelid, attnum) FROM pg_attribute WHERE attrelid = 't'::regclass AND attname = 'c3'`,
				gosql.NullString{String: `foo`, Valid: true},
			},
			{
				`ALTER TABLE t DROP COLUMN c2`,
				`SELECT col_description(attrelid, attnum) FROM pg_attribute WHERE attrelid = 't'::regclass AND attname = 'c3'`,
				gosql.NullString{String: `foo`, Valid: true},
			},
		}

		for _, tc := range testCases {
			if _, err := db.Exec(tc.exec); err != nil {
				t.Fatal(err)
			}

			row := db.QueryRow(tc.query)
			var comment gosql.NullString
			if err := row.Scan(&comment); err != nil {
				t.Fatal(err)
			}
			if tc.expect != comment {
				t.Fatalf("expected comment %v, got %v", tc.expect, comment)
			}
		}
	})
}

func TestCommentOnColumnTransaction(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	runCommentOnTests(t, func(db *gosql.DB) {
		if _, err := db.Exec(`
		CREATE DATABASE d;
		SET DATABASE = d;
		CREATE TABLE t (c INT);
		BEGIN;
		ALTER TABLE t ADD COLUMN x INT;
		COMMENT ON COLUMN t.x IS 'foo';
		COMMIT;
	`); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCommentOnColumnWhenDropTable(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	runCommentOnTests(t, func(db *gosql.DB) {
		if _, err := db.Exec(`
		CREATE DATABASE d;
		SET DATABASE = d;
		CREATE TABLE t (c INT);
	`); err != nil {
			t.Fatal(err)
		}

		if _, err := db.Exec(`COMMENT ON COLUMN t.c IS 'foo'`); err != nil {
			t.Fatal(err)
		}

		if _, err := db.Exec(`DROP TABLE t`); err != nil {
			t.Fatal(err)
		}

		row := db.QueryRow(`SELECT comment FROM system.comments LIMIT 1`)
		var comment string
		err := row.Scan(&comment)
		if !errors.Is(err, gosql.ErrNoRows) {
			if err != nil {
				t.Fatal(err)
			}

			t.Fatal("comment remain")
		}
	})
}

func TestCommentOnColumnWhenDropColumn(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	runCommentOnTests(t, func(db *gosql.DB) {
		if _, err := db.Exec(`
		CREATE DATABASE d;
		SET DATABASE = d;
		CREATE TABLE t (c INT);
	`); err != nil {
			t.Fatal(err)
		}

		if _, err := db.Exec(`COMMENT ON COLUMN t.c IS 'foo'`); err != nil {
			t.Fatal(err)
		}

		if _, err := db.Exec(`ALTER TABLE t DROP COLUMN c`); err != nil {
			t.Fatal(err)
		}

		row := db.QueryRow(`SELECT comment FROM system.comments LIMIT 1`)
		var comment string
		err := row.Scan(&comment)
		if !errors.Is(err, gosql.ErrNoRows) {
			if err != nil {
				t.Fatal(err)
			}

			t.Fatal("comment remaining in system.comments despite drop")
		}
	})
}

func TestCommentOnAlteredColumn(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	runCommentOnTestsDeclarativeOnly(t, func(db *gosql.DB) {
		expectedComment := "expected comment"

		if _, err := db.Exec(`
		CREATE DATABASE d;
		SET DATABASE = d;
		CREATE TABLE t (c INT);
	`); err != nil {
			t.Fatal(err)
		}

		if _, err := db.Exec(`COMMENT ON COLUMN t.c IS 'first comment'`); err != nil {
			t.Fatal(err)
		}

		if _, err := db.Exec(`ALTER TABLE t ALTER COLUMN c TYPE character varying;`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(
			fmt.Sprintf(`COMMENT ON COLUMN t.c IS '%s'`, expectedComment)); err != nil {
			t.Fatal(err)
		}
		row := db.QueryRow(`SELECT comment FROM system.comments LIMIT 1`)
		var comment string
		if err := row.Scan(&comment); err != nil {
			t.Fatal(err)
		}

		if expectedComment != comment {
			t.Fatalf("expected comment %v, got %v", expectedComment, comment)
		}
	})
}

func runCommentOnTests(t *testing.T, testFunc func(db *gosql.DB)) {
	for _, setupQuery := range []string{
		`SET use_declarative_schema_changer = 'on'`,
		`SET create_table_with_schema_locked=false;
		 SET use_declarative_schema_changer = 'off';`,
	} {
		runOneCommentOnTest(t, setupQuery, testFunc)
	}
}

func runCommentOnTestsDeclarativeOnly(t *testing.T, testFunc func(db *gosql.DB)) {
	runOneCommentOnTest(t, "SET use_declarative_schema_changer = 'on'", testFunc)
}

func runOneCommentOnTest(t *testing.T, setupQuery string, testFunc func(db *gosql.DB)) {
	params, _ := createTestServerParamsAllowTenants()
	s, db, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.Background())
	_, err := db.Exec(setupQuery)
	require.NoError(t, err)
	testFunc(db)
}
