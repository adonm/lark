package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// Library provisioning picks the largest set of content packs the data volume
// can hold without ever dropping below 20% free. The planner is pure apart from
// stat/statfs, so tests inject fake volume sizes.

const (
	// libraryReservePercent is the share of the volume provisioning must leave
	// free for the database, backups, logs, and the rest of the host.
	libraryReservePercent = 20
	// librarySafetyPercent trims the spendable budget so a pack that grows past
	// its planned size still cannot cross the reserve line.
	librarySafetyPercent = 90

	gib = int64(1) << 30
)

const (
	packStateInstalled = "installed"
	packStateBuilding  = "building"
	packStatePending   = "pending"
	packStateSkipped   = "skipped"
)

// libraryPack is one provisionable content pack. plannedBytes is the peak disk
// needed for a full download plus index build: the temporary dump counts until
// the build reclaims it. buildBytes is the index size used for progress.
//
// Sizes were measured on 2026-11-23 (Gutenberg 1.0 GiB of books, Simple 0.34 GB
// dump + 0.54 GB index, English Wikipedia 25.9 GB dump + 32.4 GiB index).
// Update this table whenever a source or the indexing pipeline changes.
type libraryPack struct {
	name         string
	label        string
	plannedBytes int64
	dumpBytes    int64
	buildBytes   int64
}

var libraryPacks = []libraryPack{
	{name: "gutenberg", label: "Project Gutenberg", plannedBytes: 12 * gib / 10, buildBytes: 2 << 20},
	{name: "simple", label: "Simple English Wikipedia", plannedBytes: gib, dumpBytes: 360149050, buildBytes: 563585024},
	{name: "enwiki", label: "English Wikipedia", plannedBytes: 62 * gib, dumpBytes: 25899045155, buildBytes: 34794643456},
}

type libraryPackPlan struct {
	Name           string  `json:"name"`
	Label          string  `json:"label"`
	State          string  `json:"state"`
	PlannedBytes   int64   `json:"planned_bytes"`
	InstalledBytes int64   `json:"installed_bytes,omitempty"`
	NeededBytes    int64   `json:"needed_bytes,omitempty"`
	Percent        float64 `json:"percent,omitempty"`
	Reason         string  `json:"reason,omitempty"`
}

type libraryPlan struct {
	Dir          string            `json:"dir"`
	TotalBytes   int64             `json:"total_bytes"`
	FreeBytes    int64             `json:"free_bytes"`
	BudgetBytes  int64             `json:"budget_bytes"`
	ReserveBytes int64             `json:"reserve_bytes"`
	UpdatedAt    string            `json:"updated_at"`
	Packs        []libraryPackPlan `json:"packs"`
}

func libraryPackByName(name string) (libraryPack, bool) {
	for _, pack := range libraryPacks {
		if pack.name == name {
			return pack, true
		}
	}
	return libraryPack{}, false
}

// libraryVolume reports the filesystem holding dir, falling back to the nearest
// existing ancestor before the directory is created.
func libraryVolume(dir string) (total, free int64, err error) {
	path := dir
	for {
		var st syscall.Statfs_t
		if statErr := syscall.Statfs(path, &st); statErr == nil {
			return int64(st.Blocks) * int64(st.Bsize), int64(st.Bavail) * int64(st.Bsize), nil
		} else {
			err = statErr
		}
		parent := filepath.Dir(path)
		if parent == path {
			return 0, 0, err
		}
		path = parent
	}
}

func libraryBudget(total, free int64) int64 {
	budget := free - total*libraryReservePercent/100
	if budget <= 0 {
		return 0
	}
	return budget * librarySafetyPercent / 100
}

func libraryPackDB(dir string, pack libraryPack) string {
	return filepath.Join(dir, librarySources[pack.name].db)
}

func libraryPackDump(dir string, pack libraryPack) string {
	if pack.dumpBytes == 0 {
		return ""
	}
	return filepath.Join(dir, "raw", wikiDumpName(pack.name))
}

// libraryLockHeld reports whether another process (or goroutine) holds the
// flock at path. Missing lock files mean nothing is running.
func libraryLockHeld(path string) bool {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return false
}

func libraryPackInstalled(dir string, pack libraryPack) bool {
	_, err := os.Stat(libraryPackDB(dir, pack))
	return err == nil
}

func libraryPackBuilding(dir string, pack libraryPack) bool {
	if libraryLockHeld(libraryPackDB(dir, pack) + ".build.lock") {
		return true
	}
	dump := libraryPackDump(dir, pack)
	return dump != "" && libraryLockHeld(dump+".lock")
}

func libraryPackInstalledBytes(dir string, pack libraryPack) int64 {
	total := fileSize(libraryPackDB(dir, pack))
	if pack.name == "gutenberg" {
		if books, err := os.ReadDir(filepath.Join(dir, "books")); err == nil {
			for _, book := range books {
				if info, err := book.Info(); err == nil {
					total += info.Size()
				}
			}
		}
	}
	return total
}

// libraryPackExistingBytes is the part of a pack already on disk, so a resumed
// download or a build from an existing dump only needs its remainder.
func libraryPackExistingBytes(dir string, pack libraryPack) int64 {
	if pack.name == "gutenberg" {
		total := fileSize(filepath.Join(dir, "raw", "gutenberg.jsonl")) +
			fileSize(filepath.Join(dir, "raw", "pg_catalog.csv.gz"))
		if books, err := os.ReadDir(filepath.Join(dir, "books")); err == nil {
			for _, book := range books {
				if info, err := book.Info(); err == nil {
					total += info.Size()
				}
			}
		}
		return total
	}
	total := fileSize(libraryPackDB(dir, pack) + ".building")
	dump := libraryPackDump(dir, pack)
	if dump == "" {
		return total
	}
	total += fileSize(dump) + fileSize(dump+".part")
	segments, _ := filepath.Glob(dump + ".seg*")
	for _, segment := range segments {
		total += fileSize(segment)
	}
	return total
}

func libraryPackNeededBytes(dir string, pack libraryPack) int64 {
	return max(int64(0), pack.plannedBytes-libraryPackExistingBytes(dir, pack))
}

func libraryPackPercent(dir string, pack libraryPack) float64 {
	if pack.buildBytes > 0 && libraryLockHeld(libraryPackDB(dir, pack)+".build.lock") {
		done := fileSize(libraryPackDB(dir, pack) + ".building")
		return min(99.9, 100*float64(done)/float64(pack.buildBytes))
	}
	dump := libraryPackDump(dir, pack)
	if dump == "" || pack.dumpBytes == 0 {
		return 0
	}
	done := fileSize(dump + ".part")
	segments, _ := filepath.Glob(dump + ".seg*")
	for _, segment := range segments {
		done += fileSize(segment)
	}
	return min(99.9, 100*float64(done)/float64(pack.dumpBytes))
}

// planLibrary inspects the volume and pack files. It never writes anything.
func planLibrary(dir string, statfs func(string) (int64, int64, error)) (libraryPlan, error) {
	if statfs == nil {
		statfs = libraryVolume
	}
	total, free, err := statfs(dir)
	if err != nil {
		return libraryPlan{}, fmt.Errorf("library volume: %w", err)
	}
	plan := libraryPlan{
		Dir:          dir,
		TotalBytes:   total,
		FreeBytes:    free,
		BudgetBytes:  libraryBudget(total, free),
		ReserveBytes: total * libraryReservePercent / 100,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	budget := plan.BudgetBytes
	for _, pack := range libraryPacks {
		entry := libraryPackPlan{Name: pack.name, Label: pack.label, PlannedBytes: pack.plannedBytes}
		switch {
		case libraryPackInstalled(dir, pack):
			entry.State = packStateInstalled
			entry.InstalledBytes = libraryPackInstalledBytes(dir, pack)
		case libraryPackBuilding(dir, pack):
			entry.State = packStateBuilding
			entry.Percent = libraryPackPercent(dir, pack)
		default:
			entry.NeededBytes = libraryPackNeededBytes(dir, pack)
			if entry.NeededBytes <= budget {
				entry.State = packStatePending
				budget -= entry.NeededBytes
			} else {
				entry.State = packStateSkipped
				entry.Reason = fmt.Sprintf("needs %s, budget %s (%d%% reserve, %d%% margin)",
					humanBytes(entry.NeededBytes), humanBytes(budget), libraryReservePercent, 100-librarySafetyPercent)
			}
		}
		plan.Packs = append(plan.Packs, entry)
	}
	return plan, nil
}

// provisionOrder returns pending packs smallest first, so the cheap packs land
// before a long download or index build.
func (p libraryPlan) provisionOrder() []string {
	pending := make([]libraryPackPlan, 0, len(p.Packs))
	for _, pack := range p.Packs {
		if pack.State == packStatePending {
			pending = append(pending, pack)
		}
	}
	slices.SortStableFunc(pending, func(a, b libraryPackPlan) int {
		switch {
		case a.NeededBytes < b.NeededBytes:
			return -1
		case a.NeededBytes > b.NeededBytes:
			return 1
		}
		return 0
	})
	names := make([]string, len(pending))
	for i, pack := range pending {
		names[i] = pack.Name
	}
	return names
}

func (p libraryPlan) pack(name string) (libraryPackPlan, bool) {
	for _, pack := range p.Packs {
		if pack.Name == name {
			return pack, true
		}
	}
	return libraryPackPlan{}, false
}

func libraryPlanReport(dir string) object {
	plan, err := planLibrary(dir, nil)
	if err != nil {
		return object{"dir": dir, "error": err.Error()}
	}
	return object{"dir": plan.Dir, "total_bytes": plan.TotalBytes, "free_bytes": plan.FreeBytes,
		"budget_bytes": plan.BudgetBytes, "reserve_bytes": plan.ReserveBytes,
		"updated_at": plan.UpdatedAt, "packs": plan.Packs}
}
