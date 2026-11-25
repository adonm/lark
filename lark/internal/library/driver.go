// Package library opens databases by the "lark-sqlite3" driver name. Register
// it here so the compiled edge (which owns bulk downloads and index builds) and
// this package's tests need no server package. Build with `-tags sqlite_fts5`
// so the driver compiles FTS5 support; every Lark build command and the gate
// pass that tag.
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
