package main

import (
	"bytes"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// createPackDB writes a minimal valid index so health checks see real rows.
func createPackDB(t *testing.T, path, table string) {
	t.Helper()
	db, err := sql.Open("lark-sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE ` + table + ` (id INTEGER PRIMARY KEY, title TEXT NOT NULL, text TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO ` + table + ` (id,title) VALUES (1,'one')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCLIAdminCommands(t *testing.T) {
	t.Parallel()
	_, base := wsHarness(t, nil)
	c := &cliClient{base: base, token: "unit", http: &http.Client{}}

	run := func(cmd string, args ...string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		if err := runCLIWithClient(t.Context(), c, cmd, args, &out, &errOut); err != nil {
			t.Fatalf("%s %v: %v (%s)", cmd, args, err, errOut.String())
		}
		return out.String()
	}

	if got := run("companions", "add", "Cake", "a cream cat"); !strings.Contains(got, "Added companion cake (Cake)") {
		t.Fatal(got)
	}
	if got := run("companions", "add", "Mint", "a mint frog"); !strings.Contains(got, "Added companion mint (Mint)") {
		t.Fatal(got)
	}
	if got := run("companions"); !strings.Contains(got, "cake\tCake") || !strings.Contains(got, "mint\tMint") {
		t.Fatal(got)
	}

	hw := testConnectWS(t, base, "unit", false)
	hw.hello(t, "vocat-33d8", "cake")

	if got := run("terminals"); !strings.Contains(got, "vocat-33d8\tcompanion=cake\tonline=true") {
		t.Fatal(got)
	}
	if got := run("terminals", "attach", "vocat-33d8", "mint"); !strings.Contains(got, "Attached vocat-33d8 to mint") {
		t.Fatal(got)
	}
	if got := run("status"); !strings.Contains(got, "Companions (2):") || !strings.Contains(got, "Terminals (1):") ||
		!strings.Contains(got, "vocat-33d8\tcompanion=mint") {
		t.Fatal(got)
	}
	if got := run("terminals", "detach", "vocat-33d8"); !strings.Contains(got, "Detached vocat-33d8") {
		t.Fatal(got)
	}
	if got := run("terminals", "rm", "vocat-33d8"); !strings.Contains(got, "Removed terminal vocat-33d8") {
		t.Fatal(got)
	}
	if got := run("terminals"); strings.Contains(got, "vocat-33d8") {
		t.Fatal("terminal still listed after rm:", got)
	}
	if got := run("companions", "rm", "cake"); !strings.Contains(got, "Removed companion cake") {
		t.Fatal(got)
	}
	if got := run("companions"); strings.Contains(got, "cake\t") {
		t.Fatal("cake still listed after rm:", got)
	}
}

func TestStatusReportsLibraryPacks(t *testing.T) {
	t.Parallel()
	_, base := wsHarness(t, func(b *larkServer, _ string) {
		if err := os.MkdirAll(b.lib.Dir(), 0o700); err != nil {
			t.Fatal(err)
		}
		createPackDB(t, filepath.Join(b.lib.Dir(), "gutenberg.db"), "books")
		if err := os.MkdirAll(filepath.Join(b.lib.Dir(), "books"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(b.lib.Dir(), "books", "00001.txt"), []byte("text"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	c := &cliClient{base: base, token: "unit", http: &http.Client{}}
	var out, errOut bytes.Buffer
	if err := runCLIWithClient(t.Context(), c, "status", nil, &out, &errOut); err != nil {
		t.Fatalf("status: %v (%s)", err, errOut.String())
	}
	for _, want := range []string{"Library (", "disk:", "gutenberg\tinstalled", "simple\tpending"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status output lacks %q:\n%s", want, out.String())
		}
	}
}
