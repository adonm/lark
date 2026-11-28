// Small shared helpers. Keep this file free of imports so every module can
// depend on it without cycles.

/** Message from an unknown thrown value. */
export function errMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/** Consume an async generator, forwarding each yielded item in order. */
export async function drain<T, R>(gen: AsyncGenerator<T, R>, onItem?: (item: T) => void): Promise<R> {
  for (;;) {
    const step = await gen.next();
    if (step.done) return step.value;
    onItem?.(step.value);
  }
}

export function humanBytes(bytes: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let value = Math.max(0, bytes);
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value >= 10 || unit === 0 ? Math.round(value) : value.toFixed(1)} ${units[unit]}`;
}

const WEEKDAY_NAMES = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"] as const;

function pad2(value: number): string {
  return String(value).padStart(2, "0");
}

export interface ClockParts {
  /** Full weekday name, e.g. "Saturday". */
  weekday: string;
  /** Three-letter weekday, e.g. "Sat". */
  weekdayShort: string;
  /** Local calendar date, e.g. "2026-11-28". */
  date: string;
  /** Local 12-hour time, e.g. "11:35 AM". */
  time: string;
  /** IANA zone when the runtime reports one. */
  timezone: string;
}

/** Local wall-clock parts for a timestamp, independent of locale settings. */
export function clockParts(ms: number): ClockParts {
  const date = new Date(ms);
  const hours = date.getHours();
  const weekday = WEEKDAY_NAMES[date.getDay()] ?? "Sunday";
  return {
    weekday,
    weekdayShort: weekday.slice(0, 3),
    date: `${date.getFullYear()}-${pad2(date.getMonth() + 1)}-${pad2(date.getDate())}`,
    time: `${hours % 12 === 0 ? 12 : hours % 12}:${pad2(date.getMinutes())} ${hours < 12 ? "AM" : "PM"}`,
    timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || "local",
  };
}

export interface SlugOptions {
  max?: number;
  fallback?: string;
}

/** Lowercase ASCII slug; non-alphanumerics collapse to single dashes. */
export function slugify(text: string, options: SlugOptions = {}): string {
  const max = options.max ?? 40;
  let slug = "";
  let previousDash = false;
  for (const character of text.trim().toLowerCase()) {
    if ((character >= "a" && character <= "z") || (character >= "0" && character <= "9")) {
      slug += character;
      previousDash = false;
    } else if (!previousDash && slug.length > 0) {
      slug += "-";
      previousDash = true;
    }
    if (slug.length >= max) break;
  }
  return slug.replace(/^-+|-+$/g, "") || options.fallback || "story";
}
