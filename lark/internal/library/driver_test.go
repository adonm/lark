package library

import (
	"database/sql"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// The library package opens databases by the server's driver name. Register
// it here so this package's tests stand alone; the sqlite_fts5 build tag adds
// FTS5 support only where a test needs it.
func init() {
	sql.Register("lark-sqlite3", &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			_, err := conn.Exec("PRAGMA temp_store=MEMORY; PRAGMA mmap_size=268435456;", nil)
			return err
		},
	})
}
