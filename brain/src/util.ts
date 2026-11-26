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
