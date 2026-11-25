package library

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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

func fakeVolume(total, free int64) func(string) (int64, int64, error) {
	return func(string) (int64, int64, error) { return total, free, nil }
}

func planPacks(t *testing.T, dir string, total, free int64) map[string]libraryPackPlan {
	t.Helper()
	plan, err := planLibrary(dir, fakeVolume(total, free))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]libraryPackPlan{}
	for _, pack := range plan.Packs {
		out[pack.Name] = pack
	}
	return out
}

func TestLibraryBudgetKeepsTwentyPercentFree(t *testing.T) {
	t.Parallel()
	if got := libraryBudget(1000, 200); got != 0 {
		t.Fatalf("free at the reserve line must budget nothing, got %d", got)
	}
	if got := libraryBudget(1000, 300); got != 90 {
		t.Fatalf("budget must keep the reserve and the 10%% margin, got %d", got)
	}
	if got := libraryBudget(1000, 100); got != 0 {
		t.Fatalf("free below the reserve line must budget nothing, got %d", got)
	}
}

func TestLibraryPlanPicksTiersBySpace(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		total int64
		free  int64
		want  map[string]string
	}{
		{
			name:  "plenty",
			total: 1024 * gib,
			free:  1000 * gib,
			want:  map[string]string{"gutenberg": packStatePending, "simple": packStatePending, "enwiki": packStatePending},
		},
		{
			name:  "tight",
			total: 200 * gib,
			free:  90 * gib,
			want:  map[string]string{"gutenberg": packStatePending, "simple": packStatePending, "enwiki": packStateSkipped},
		},
		{
			name:  "almost none",
			total: 100 * gib,
			free:  20 * gib,
			want:  map[string]string{"gutenberg": packStateSkipped, "simple": packStateSkipped, "enwiki": packStateSkipped},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			plan, err := planLibrary(dir, fakeVolume(tc.total, tc.free))
			if err != nil {
				t.Fatal(err)
			}
			if plan.BudgetBytes != libraryBudget(tc.total, tc.free) {
				t.Fatalf("budget = %d, want %d", plan.BudgetBytes, libraryBudget(tc.total, tc.free))
			}
			if plan.ReserveBytes != tc.total*20/100 {
				t.Fatalf("reserve = %d, want %d", plan.ReserveBytes, tc.total*20/100)
			}
			for _, pack := range plan.Packs {
				if got := tc.want[pack.Name]; got != pack.State {
					t.Fatalf("%s state = %s (%s), want %s", pack.Name, pack.State, pack.Reason, got)
				}
				if pack.State == packStateSkipped && !strings.Contains(pack.Reason, "budget") {
					t.Fatalf("%s skip reason lacks needed-vs-budget: %q", pack.Name, pack.Reason)
				}
			}
		})
	}
}

func TestLibraryPlanSkipsInstalledPacks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	createPackDB(t, filepath.Join(dir, "gutenberg.db"), "books")
	createPackDB(t, filepath.Join(dir, "simple.db"), "pages")
	if err := os.MkdirAll(filepath.Join(dir, "books"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "books", "00001.txt"), []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	packs := planPacks(t, dir, 1024*gib, 1000*gib)
	if packs["gutenberg"].State != packStateInstalled || packs["simple"].State != packStateInstalled {
		t.Fatalf("installed packs not recognized: %+v", packs)
	}
	if packs["enwiki"].State != packStatePending {
		t.Fatalf("enwiki should still be pending: %+v", packs["enwiki"])
	}
}

func TestLibraryPlanRespectsRunningBuild(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "enwiki.db.build.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	packs := planPacks(t, dir, 1024*gib, 1000*gib)
	if packs["enwiki"].State != packStateBuilding {
		t.Fatalf("a held build lock must read as building, got %+v", packs["enwiki"])
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	lock.Close()
	if got := planPacks(t, dir, 1024*gib, 1000*gib)["enwiki"].State; got != packStatePending {
		t.Fatalf("released build lock must read as pending, got %s", got)
	}
}

func TestLibraryPlanCountsExistingDump(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw")
	if err := os.MkdirAll(raw, 0o700); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(raw, wikiDumpName("enwiki"))
	file, err := os.OpenFile(dump, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(24 * gib); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	// 63 GiB of budget leaves 60.8 GiB after the small packs: too little for a
	// fresh 62 GiB enwiki, enough for the 38 GiB still needed from the dump.
	if got := planPacks(t, dir, 400*gib, 150*gib)["enwiki"]; got.State != packStatePending {
		t.Fatalf("existing dump not credited: %+v", got)
	}
	if err := os.Remove(dump); err != nil {
		t.Fatal(err)
	}
	if got := planPacks(t, dir, 400*gib, 150*gib)["enwiki"]; got.State != packStateSkipped {
		t.Fatalf("without the dump enwiki must be skipped: %+v", got)
	}
}

func TestLibraryPlanRejectsBrokenInstalledPacks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "simple.db"), []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A Gutenberg index with rows but no book files is not usable either.
	createPackDB(t, filepath.Join(dir, "gutenberg.db"), "books")
	writePackError(dir, "enwiki", os.ErrDeadlineExceeded)
	packs := planPacks(t, dir, 1024*gib, 1000*gib)
	if packs["simple"].State == packStateInstalled {
		t.Fatalf("unreadable index counted as installed: %+v", packs["simple"])
	}
	if packs["simple"].Reason == "" {
		t.Fatalf("broken index has no reason: %+v", packs["simple"])
	}
	if packs["gutenberg"].State == packStateInstalled {
		t.Fatalf("Gutenberg index without books counted as installed: %+v", packs["gutenberg"])
	}
	if packs["enwiki"].LastError == "" {
		t.Fatalf("last provisioning error was not surfaced: %+v", packs["enwiki"])
	}
}

func TestLibraryPlanCountsPartialDownloads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw")
	if err := os.MkdirAll(raw, 0o700); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(raw, wikiDumpName("enwiki"))
	file, err := os.OpenFile(dump+".part", os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(3 * gib); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	// 62 GiB planned minus 3 GiB staged leaves 59 GiB; with 150 GiB free the
	// budget cannot hold enwiki, but a plan that ignored .part would report a
	// larger need and skip at a different threshold.
	got := planPacks(t, dir, 400*gib, 150*gib)["enwiki"]
	if got.State != packStatePending || got.NeededBytes > 62*gib-3*gib+1 {
		t.Fatalf("staged bytes were not credited: %+v", got)
	}
}

func TestLibraryProvisionOrderIsSmallestFirst(t *testing.T) {
	t.Parallel()
	plan, err := planLibrary(t.TempDir(), fakeVolume(1024*gib, 1000*gib))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"simple", "gutenberg", "enwiki"}
	if got := plan.provisionOrder(); !slices.Equal(got, want) {
		t.Fatalf("provision order = %v, want %v", got, want)
	}
}

func TestPinnedGutenbergIDsResolveFromRepoRoot(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scripts", "gutenberg-ids.txt"), []byte("11\n# a comment\n22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LARK_GUTENBERG_IDS", "")
	t.Setenv("LARK_REPO_DIR", repo)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ids, err := pinnedGutenbergIDs()
	if err != nil || !slices.Equal(ids, []int64{11, 22}) {
		t.Fatalf("pinnedGutenbergIDs = %v, %v; want [11 22] from %s", ids, err, repo)
	}
}

// A permission error opening a lock file must read as locked, not free.
func TestLibraryLockHeldTreatsErrorsAsLocked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "enwiki.db.build.lock")
	if err := os.WriteFile(path, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	if !libraryLockHeld(path) {
		t.Fatal("unreadable lock file reported as free")
	}
	if libraryLockHeld(filepath.Join(dir, "missing.lock")) {
		t.Fatal("missing lock file reported as locked")
	}
}
