package library

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

// provisionLibrary runs once per server launch in the background: it plans
// against this volume, then downloads and indexes missing packs one at a time,
// smallest first. Installed packs and builds already running elsewhere are
// left alone.
func (l *Library) Provision(ctx context.Context) {
	dir := l.Dir()
	if dir == "" {
		return
	}
	for _, sub := range []string{"", "raw", "books"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			log.Printf("library: provisioning disabled: %v", err)
			return
		}
	}
	releaseProvision, err := provisionLock(dir)
	if err != nil {
		log.Printf("library: provisioning disabled: %v", err)
		return
	}
	if releaseProvision == nil {
		log.Printf("library: another provisioning run holds %s", filepath.Join(dir, "provision.lock"))
		return
	}
	defer releaseProvision()
	logFile, err := os.OpenFile(filepath.Join(dir, "setup.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("library: provisioning disabled: %v", err)
		return
	}
	defer logFile.Close()
	out := io.Writer(logFile)
	plan, err := planLibrary(dir, nil)
	if err != nil {
		libraryLog(out, "plan: %v", err)
		return
	}
	libraryLog(out, "plan: %s free of %s, budget %s (%d%% reserve, %d%% margin)",
		humanBytes(plan.FreeBytes), humanBytes(plan.TotalBytes), humanBytes(plan.BudgetBytes),
		libraryReservePercent, 100-librarySafetyPercent)
	for _, pack := range plan.Packs {
		switch pack.State {
		case packStateInstalled:
			libraryLog(out, "%s: installed (%s)", pack.Name, humanBytes(pack.InstalledBytes))
		case packStateBuilding:
			libraryLog(out, "%s: already building (%.0f%%)", pack.Name, pack.Percent)
		case packStateSkipped:
			libraryLog(out, "%s: skipped (%s)", pack.Name, pack.Reason)
		}
	}
	for _, name := range plan.provisionOrder() {
		if ctx.Err() != nil {
			return
		}
		// Re-plan before every pack so a manual run, an installed pack, or a
		// shrunken volume changes the decision instead of being duplicated.
		plan, err = planLibrary(dir, nil)
		if err != nil {
			libraryLog(out, "%s: plan: %v", name, err)
			return
		}
		entry, ok := plan.pack(name)
		if !ok || entry.State != packStatePending {
			continue
		}
		started := time.Now()
		libraryLog(out, "%s: starting (%s needed of %s planned)", name,
			humanBytes(entry.NeededBytes), humanBytes(entry.PlannedBytes))
		switch name {
		case "gutenberg":
			err = downloadGutenberg(ctx, dir, 3000, 8, out)
			if err == nil {
				err = buildGutenberg(ctx, dir, false, out)
			}
		case "simple", "enwiki":
			err = downloadWikipedia(ctx, dir, name, out)
			if err == nil {
				err = buildWikipedia(ctx, dir, name, false, out)
			}
		default:
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				libraryLog(out, "%s: stopped: %v", name, err)
				return
			}
			libraryLog(out, "%s: failed: %v", name, err)
			writePackError(dir, name, err)
			continue
		}
		writePackError(dir, name, nil)
		if fallback, ok := libraryPackByName(name); ok && !libraryPackInstalled(dir, fallback) {
			libraryLog(out, "%s: build is still running elsewhere; leaving it alone", name)
			continue
		}
		pack, _ := libraryPackByName(name)
		libraryLog(out, "%s: landed in %s (planned %s, actual %s)", name,
			time.Since(started).Round(time.Second), humanBytes(entry.PlannedBytes),
			humanBytes(libraryPackInstalledBytes(dir, pack)))
	}
	reclaimLibraryDumps(dir, out)
}

// reclaimLibraryDumps deletes downloaded dumps once their index is installed.
// The dump is only input to indexing, so this frees the largest temporary file
// (about 24 GB for English Wikipedia) without affecting search.
func reclaimLibraryDumps(dir string, out io.Writer) {
	for _, name := range []string{"simple", "enwiki"} {
		pack, ok := libraryPackByName(name)
		if !ok || !libraryPackInstalled(dir, pack) || libraryPackBuilding(dir, pack) {
			continue
		}
		dump := libraryPackDump(dir, pack)
		if dump == "" || fileSize(dump) == 0 {
			continue
		}
		reclaimed := reclaimWikiDump(dump)
		if reclaimed > 0 {
			libraryLog(out, "%s: removed the downloaded dump after indexing (%s reclaimed)", name, humanBytes(reclaimed))
		}
	}
}

// reclaimWikiDump removes the dump and any resumable pieces. Callers must hold
// or verify the build lock first.
func reclaimWikiDump(dump string) int64 {
	var reclaimed int64
	segments, _ := filepath.Glob(dump + ".seg*")
	paths := append([]string{dump, dump + ".part", dump + ".lock"}, segments...)
	for _, path := range paths {
		size := fileSize(path)
		if os.Remove(path) == nil {
			reclaimed += size
		}
	}
	return reclaimed
}
