// Spoken choice matching: tokenise, drop filler words, and accept ordinals.
import type { Choice } from "./stories.ts";

export function normalizeSpeechTokens(value: string): string[] {
  return value
    .toLowerCase()
    .replace(/[^\p{L}\p{N}\s]/gu, " ")
    .split(/\s+/)
    .filter(Boolean);
}

const STOP_TOKENS = new Set(["the", "a", "an", "to", "and", "or", "of", "in", "on", "at", "for", "with"]);

export function filterStopTokens(tokens: readonly string[]): string[] {
  return tokens.filter((token) => !STOP_TOKENS.has(token));
}

/** Match spoken text to one of at most three labels without an LLM. */
export function matchChoice(spoken: string, choices: readonly Choice[]): { index: number; matched: boolean } {
  if (choices.length === 0) return { index: 0, matched: false };
  const tokens = normalizeSpeechTokens(spoken);
  if (tokens.length === 0) return { index: 0, matched: false };

  let bestIdx = -1;
  let bestScore = 0;
  let secondScore = 0;
  const spokenJoined = tokens.join(" ");
  for (let i = 0; i < choices.length; i++) {
    const choice = choices[i];
    if (choice === undefined) continue;
    let choiceTokens = filterStopTokens(normalizeSpeechTokens(choice.label));
    if (choiceTokens.length === 0) choiceTokens = normalizeSpeechTokens(choice.label);
    if (choiceTokens.length === 0) continue;
    let hits = 0;
    for (const token of choiceTokens) if (tokens.includes(token)) hits++;
    if (hits === 0) continue;
    let score = hits / choiceTokens.length;
    if (spokenJoined.includes(choiceTokens.join(" "))) score += 0.5;
    if (score > bestScore) {
      secondScore = bestScore;
      bestScore = score;
      bestIdx = i;
    } else if (score > secondScore) {
      secondScore = score;
    }
  }
  if (bestIdx >= 0 && bestScore >= 0.5 && bestScore > secondScore) {
    return { index: bestIdx, matched: true };
  }

  let ordinal = -1;
  for (const token of tokens) {
    switch (token) {
      case "first":
      case "1st":
      case "one":
      case "1":
        ordinal = 0;
        break;
      case "second":
      case "2nd":
      case "two":
      case "2":
        ordinal = 1;
        break;
      case "third":
      case "3rd":
      case "three":
      case "3":
        ordinal = 2;
        break;
      case "last":
        ordinal = choices.length - 1;
        break;
    }
    if (ordinal >= 0) break;
  }
  if (ordinal >= 0 && ordinal < choices.length) return { index: ordinal, matched: true };
  return { index: 0, matched: false };
}
