// Library disk planner: 20% volume reserve, 90% spend cap, pack health.
import { existsSync, readdirSync, statSync, statfsSync } from "node:fs";
import { dirname, join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { humanBytes } from "./util.ts";
import { SOURCES, type SourceName } from "./library.ts";

// ---- disk planner -------------------------------------------------------

export const LIBRARY_RESERVE_PERCENT = 20;
export const LIBRARY_SAFETY_PERCENT = 90;
const GIB = 1 << 30;

export interface PackSpec {
  name: SourceName;
  label: string;
  plannedBytes: number;
  dumpBytes: number;
  buildBytes: number;
}

/** Measured 2026-11-23; keep in sync with lark/internal/library/plan.go. */
export const LIBRARY_PACKS: readonly PackSpec[] = [
  { name: "simple", label: "Simple English Wikipedia", plannedBytes: GIB, dumpBytes: 360149050, buildBytes: 563585024 },
  { name: "gutenberg", label: "Project Gutenberg", plannedBytes: (12 * GIB) / 10, dumpBytes: 0, buildBytes: 2 << 20 },
  { name: "enwiki", label: "English Wikipedia", plannedBytes: 62 * GIB, dumpBytes: 25899045155, buildBytes: 34794643456 },
];

export type PackState = "installed" | "building" | "pending" | "skipped";

export interface PackPlan {
  name: SourceName;
  label: string;
  state: PackState;
  plannedBytes: number;
  installedBytes: number;
  neededBytes: number;
  percent?: number;
  reason?: string;
}

export interface LibraryPlan {
  dir: string;
  totalBytes: number;
  freeBytes: number;
  budgetBytes: number;
  reserveBytes: number;
  packs: PackPlan[];
}

export interface VolumeInfo {
  total: number;
  free: number;
}

export type StatFs = (path: string) => VolumeInfo;

export function libraryVolume(dir: string): VolumeInfo {
  let path = dir;
  for (;;) {
    try {
      const stat = statfsSync(path);
      return { total: stat.bsize * stat.blocks, free: stat.bsize * stat.bavail };
    } catch {
      const parent = dirname(path);
      if (parent === path) throw new Error(`library volume unavailable for ${dir}`);
      path = parent;
    }
  }
}

export function libraryBudget(total: number, free: number): number {
  const spendable = free - (total * LIBRARY_RESERVE_PERCENT) / 100;
  if (spendable <= 0) return 0;
  return Math.floor((spendable * LIBRARY_SAFETY_PERCENT) / 100);
}

function fileSize(path: string): number {
  const stat = statSync(path, { throwIfNoEntry: false });
  return stat?.isFile() === true ? stat.size : 0;
}

function dirSize(path: string): number {
  let total = 0;
  try {
    for (const entry of readdirSync(path, { withFileTypes: true })) {
      const full = join(path, entry.name);
      total += entry.isDirectory() ? dirSize(full) : fileSize(full);
    }
  } catch {
    return 0;
  }
  return total;
}

function packDB(dir: string, pack: PackSpec): string {
  return join(dir, SOURCES[pack.name].db);
}

function packDump(dir: string, pack: PackSpec): string {
  if (pack.dumpBytes === 0) return "";
  return join(dir, "raw", `${pack.name === "simple" ? "simplewiki" : "enwiki"}-latest-pages-articles.xml.bz2`);
}

export function packHealth(dir: string, pack: PackSpec): { ok: boolean; reason: string } {
  const path = packDB(dir, pack);
  const info = statSync(path, { throwIfNoEntry: false });
  if (info === undefined) return { ok: false, reason: "" };
  if (!info.isFile() || info.size === 0) return { ok: false, reason: "index file is empty" };
  let db: DatabaseSync;
  try {
    db = new DatabaseSync(path, { readOnly: true, timeout: 2000 });
  } catch (error) {
    return { ok: false, reason: `index cannot be opened: ${(error as Error).message}` };
  }
  try {
    const table = pack.name === "gutenberg" ? "books" : "pages";
    const row = db.prepare(`SELECT 1 AS found FROM ${table} LIMIT 1`).get();
    if (row === undefined) return { ok: false, reason: `index is missing rows in ${table}` };
    if (pack.name === "gutenberg") {
      const books = readdirSync(join(dir, "books"), { withFileTypes: true }).filter(
        (entry) => entry.isFile() && entry.name.endsWith(".txt"),
      );
      if (books.length === 0) return { ok: false, reason: "no book files" };
    }
    return { ok: true, reason: "" };
  } catch (error) {
    return { ok: false, reason: `index is unreadable: ${(error as Error).message}` };
  } finally {
    db.close();
  }
}

function installedBytes(dir: string, pack: PackSpec): number {
  let total = fileSize(packDB(dir, pack));
  if (pack.name === "gutenberg") total += dirSize(join(dir, "books"));
  return total;
}

function existingBytes(dir: string, pack: PackSpec): number {
  if (pack.name === "gutenberg") {
    return (
      fileSize(join(dir, "raw", "gutenberg.jsonl")) +
      fileSize(join(dir, "raw", "pg_catalog.csv.gz")) +
      fileSize(`${packDB(dir, pack)}.building`) +
      dirSize(join(dir, "books"))
    );
  }
  const dump = packDump(dir, pack);
  let total = fileSize(`${packDB(dir, pack)}.building`);
  if (dump !== "") total += fileSize(dump) + fileSize(`${dump}.part`);
  return total;
}

function packBuilding(dir: string, pack: PackSpec): boolean {
  return existsSync(`${packDB(dir, pack)}.build.lock`) || existsSync(`${packDB(dir, pack)}.building`);
}

export function planLibrary(dir: string, statfs: StatFs = libraryVolume): LibraryPlan {
  const { total, free } = statfs(dir);
  const plan: LibraryPlan = {
    dir,
    totalBytes: total,
    freeBytes: free,
    budgetBytes: libraryBudget(total, free),
    reserveBytes: Math.floor((total * LIBRARY_RESERVE_PERCENT) / 100),
    packs: [],
  };
  let budget = plan.budgetBytes;
  for (const pack of LIBRARY_PACKS) {
    const health = packHealth(dir, pack);
    const entry: PackPlan = {
      name: pack.name,
      label: pack.label,
      state: "pending",
      plannedBytes: pack.plannedBytes,
      installedBytes: 0,
      neededBytes: 0,
    };
    if (health.ok) {
      entry.state = "installed";
      entry.installedBytes = installedBytes(dir, pack);
    } else if (packBuilding(dir, pack)) {
      entry.state = "building";
      entry.reason = health.reason;
      entry.percent = Math.min(99.9, (100 * fileSize(`${packDB(dir, pack)}.building`)) / pack.buildBytes);
    } else {
      if (health.reason !== "") entry.reason = health.reason;
      const needed = Math.max(0, pack.plannedBytes - existingBytes(dir, pack));
      entry.neededBytes = needed;
      if (needed <= budget) {
        entry.state = "pending";
        budget -= needed;
      } else {
        entry.state = "skipped";
        entry.reason = `needs ${humanBytes(needed)}, budget ${humanBytes(budget)} (${LIBRARY_RESERVE_PERCENT}% reserve, ${100 - LIBRARY_SAFETY_PERCENT}% margin)`;
      }
    }
    plan.packs.push(entry);
  }
  return plan;
}
