// Companion-name matching for unaddressed room speech.

function normalize(text: string): string {
  return text.toLowerCase().replace(/[^a-z0-9 ]+/g, " ").replace(/\s+/g, " ").trim();
}

function levenshtein(a: string, b: string): number {
  if (a === b) return 0;
  const rows = new Array<number>(b.length + 1);
  for (let j = 0; j <= b.length; j++) rows[j] = j;
  for (let i = 1; i <= a.length; i++) {
    let previous = rows[0] ?? 0;
    rows[0] = i;
    for (let j = 1; j <= b.length; j++) {
      const current = rows[j] ?? 0;
      rows[j] = Math.min(
        (rows[j] ?? 0) + 1,
        (rows[j - 1] ?? 0) + 1,
        previous + (a[i - 1] === b[j - 1] ? 0 : 1),
      );
      previous = current;
    }
  }
  return rows[b.length] ?? a.length;
}

/** Fuzzy companion-name match in the first five words (edit distance ≤ 2). */
export function nameMatches(transcript: string, name: string): boolean {
  const target = normalize(name);
  if (target === "") return false;
  const words = normalize(transcript).split(" ").slice(0, 5);
  for (const word of words) {
    if (word === "") continue;
    if (word === target) return true;
    for (const part of target.split(" ")) {
      if (part !== "" && levenshtein(word, part) <= 2) return true;
    }
  }
  return false;
}
