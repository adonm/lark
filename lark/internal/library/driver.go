// Package library opens databases by the "lark-sqlite3" driver name. Register
// it here so the lark-library provisioner and this package's tests need no
// server package. Build lark-library with `-tags sqlite_fts5` so the driver
// compiles FTS5 support; the gate passes that tag for the tests.
package library

import (
	"database/sql"

	sqlite3 "github.com/mattn/go-sqlite3"
)

func init() {
	sql.Register("lark-sqlite3", &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			_, err := conn.Exec("PRAGMA temp_store=MEMORY; PRAGMA mmap_size=268435456;", nil)
			return err
		},
	})
}
