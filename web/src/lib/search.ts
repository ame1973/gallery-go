import generated from '../../../contracts/search-normalization-tables.json';

// All normalization, case folding, categories and whitespace use the pinned
// server profile, not the browser's potentially newer or older Unicode tables.
export const searchProfile = 'gallerygo-search-v2-u15';
if (
  generated.profile !== searchProfile ||
  generated.unicode !== '15.0.0' ||
  generated.max_input_bytes !== 16384 ||
  generated.max_normalized_bytes !== 65536 ||
  generated.max_segment_units !== 393216 ||
  generated.max_tokens !== 2048 ||
  generated.max_document_tokens !== 131072
) {
  throw new Error('Search Unicode table version mismatch');
}

export class SearchTextError extends Error {
  constructor(readonly code: 'invalid' | 'limit') {
    super(code === 'invalid' ? 'Invalid Unicode search text' : 'Search text exceeds its budget');
    this.name = 'SearchTextError';
  }
}

export interface SearchTokens {
  normalized: string;
  words: string[];
  cjk_single: string[];
  cjk_bigram: string[];
}

const decomposition: Readonly<Record<string, string>> = generated.nfkd;
const combining: Readonly<Record<string, number>> = generated.ccc;
const composition: Readonly<Record<string, string>> = generated.composition;
const fold: Readonly<Record<string, string>> = generated.fold;

function contains(ranges: number[][], value: number): boolean {
  let low = 0;
  let high = ranges.length;
  while (low < high) {
    const middle = Math.floor((low + high) / 2);
    const range = ranges[middle]!;
    if (value < range[0]!) high = middle;
    else if (value > range[1]!) low = middle + 1;
    else return true;
  }
  return false;
}

function byteLength(text: string): number {
  let bytes = 0;
  for (const scalar of text) {
    const code = scalar.codePointAt(0)!;
    if (code === 0 || (code >= 0xd800 && code <= 0xdfff)) {
      throw new SearchTextError('invalid');
    }
    bytes += code <= 0x7f ? 1 : code <= 0x7ff ? 2 : code <= 0xffff ? 3 : 4;
  }
  return bytes;
}

function lookup<T>(table: Readonly<Record<string, T>>, key: string): T | undefined {
  return Object.hasOwn(table, key) ? table[key] : undefined;
}

// Verify the table-derived expansion ceilings before accepting untrusted text.
for (const [key, value] of Object.entries(decomposition)) {
  const keyBytes = byteLength(key);
  if (
    Array.from(key).length !== 1 ||
    value === '' ||
    byteLength(value) > 11 * keyBytes ||
    Array.from(value).length > 6 * keyBytes
  )
    throw new Error('Incompatible Unicode decomposition table');
}
for (const [key, value] of Object.entries(fold)) {
  const keyBytes = byteLength(key);
  if (Array.from(key).length !== 1 || value === '' || byteLength(value) > 3 * keyBytes) {
    throw new Error('Incompatible Unicode fold table');
  }
}
for (const [key, value] of Object.entries(combining)) {
  if (
    Array.from(key).length !== 1 ||
    byteLength(key) === 0 ||
    !Number.isInteger(value) ||
    value < 1 ||
    value > 255
  ) {
    throw new Error('Incompatible Unicode combining table');
  }
}
for (const [key, value] of Object.entries(composition)) {
  if (
    Array.from(key).length !== 2 ||
    Array.from(value).length !== 1 ||
    byteLength(key) === 0 ||
    byteLength(value) === 0
  ) {
    throw new Error('Incompatible Unicode composition table');
  }
}
for (const ranges of [generated.known, generated.words, generated.han, generated.space]) {
  let previous = -1;
  for (const [start, end] of ranges) {
    if (
      start === undefined ||
      end === undefined ||
      !Number.isInteger(start) ||
      !Number.isInteger(end) ||
      start <= previous ||
      start > end ||
      end > 0x10ffff ||
      (start <= 0xdfff && end >= 0xd800)
    ) {
      throw new Error('Incompatible Unicode category table');
    }
    previous = end;
  }
  if (previous < 0) throw new Error('Empty Unicode category table');
}

// Unicode Hangul syllables use arithmetic rather than a large mechanical table.
function decompose(scalar: string): string {
  const index = scalar.codePointAt(0)! - 0xac00;
  if (index >= 0 && index < 11172) {
    const lead = 0x1100 + Math.floor(index / 588);
    const vowel = 0x1161 + Math.floor((index % 588) / 28);
    const trailing = index % 28;
    return trailing === 0
      ? String.fromCodePoint(lead, vowel)
      : String.fromCodePoint(lead, vowel, 0x11a7 + trailing);
  }
  return lookup(decomposition, scalar) ?? scalar;
}

function compose(first: string, second: string): string | undefined {
  const left = first.codePointAt(0)!;
  const right = second.codePointAt(0)!;
  const lead = left - 0x1100;
  const vowel = right - 0x1161;
  if (lead >= 0 && lead < 19 && vowel >= 0 && vowel < 21) {
    return String.fromCodePoint(0xac00 + (lead * 21 + vowel) * 28);
  }
  const syllable = left - 0xac00;
  const trailing = right - 0x11a7;
  if (syllable >= 0 && syllable < 11172 && syllable % 28 === 0 && trailing > 0 && trailing < 28) {
    return String.fromCodePoint(left + trailing);
  }
  return lookup(composition, first + second);
}

function ccc(scalar: string): number {
  return lookup(combining, scalar) ?? 0;
}

function normalizeStage(text: string): string {
  const inputBytes = byteLength(text);
  if (inputBytes > generated.max_normalized_bytes) throw new SearchTextError('limit');
  const ordered: string[] = [];
  const marks: { scalar: string; class: number }[] = [];
  const flush = () => {
    if (marks.length > 1) marks.sort((a, b) => a.class - b.class);
    for (const mark of marks) ordered.push(mark.scalar);
    marks.length = 0;
  };
  let count = 0;
  for (const original of text) {
    for (const scalar of decompose(original)) {
      count++;
      if (count > 6 * inputBytes || count > generated.max_segment_units) {
        throw new Error('Incompatible Unicode search tables');
      }
      const currentClass = ccc(scalar);
      if (currentClass === 0) {
        flush();
        ordered.push(scalar);
      } else {
        marks.push({ scalar, class: currentClass });
      }
    }
  }
  flush();
  if (ordered.length === 0) return '';
  const result = [ordered[0]!];
  let starterPosition = 0;
  let starter = result[0]!;
  let previousClass = 0;
  for (let index = 1; index < ordered.length; index++) {
    const scalar = ordered[index]!;
    const currentClass = ccc(scalar);
    const combined = compose(starter, scalar);
    if (combined !== undefined && (previousClass === 0 || previousClass < currentClass)) {
      result[starterPosition] = combined;
      starter = combined;
    } else {
      if (currentClass === 0) {
        starterPosition = result.length;
        starter = scalar;
      }
      result.push(scalar);
      previousClass = currentClass;
    }
  }
  const normalized = result.join('');
  if (byteLength(normalized) > generated.max_normalized_bytes) throw new SearchTextError('limit');
  return normalized;
}

export function normalizeSearchText(text: string): string {
  if (typeof text !== 'string') throw new SearchTextError('invalid');
  if (byteLength(text) > generated.max_input_bytes) throw new SearchTextError('limit');
  const first = normalizeStage(text);
  let folded = '';
  for (const scalar of first) folded += lookup(fold, scalar) ?? scalar;
  const scalars = Array.from(normalizeStage(folded));
  let start = 0;
  let end = scalars.length;
  while (start < end && contains(generated.space, scalars[start]!.codePointAt(0)!)) start++;
  while (end > start && contains(generated.space, scalars[end - 1]!.codePointAt(0)!)) end--;
  return scalars.slice(start, end).join('');
}

export function tokenizeSearchText(text: string): SearchTokens {
  return tokenizeSearch(text, generated.max_tokens);
}

// Document tokenization preserves every normalized input inside the byte budget.
export function tokenizeSearchDocument(text: string): SearchTokens {
  return tokenizeSearch(text, generated.max_document_tokens);
}

function tokenizeSearch(text: string, limit: number): SearchTokens {
  const normalized = normalizeSearchText(text);
  const result: SearchTokens = { normalized, words: [], cjk_single: [], cjk_bigram: [] };
  const seen = [new Set<string>(), new Set<string>(), new Set<string>()];
  let count = 0;
  const add = (field: 0 | 1 | 2, value: string) => {
    if (seen[field]!.has(value)) return;
    seen[field]!.add(value);
    count++;
    (field === 0 ? result.words : field === 1 ? result.cjk_single : result.cjk_bigram).push(value);
  };
  let word = '';
  const flush = () => {
    if (word !== '') {
      add(0, word);
      word = '';
    }
  };
  let previous = '';
  for (const scalar of normalized) {
    const code = scalar.codePointAt(0)!;
    if (contains(generated.han, code)) {
      flush();
      add(1, scalar);
      if (previous !== '') add(2, previous + scalar);
      previous = scalar;
    } else {
      previous = '';
      if (contains(generated.words, code)) word += scalar;
      else flush();
    }
    if (count > limit) throw new SearchTextError('limit');
  }
  flush();
  if (count > limit) throw new SearchTextError('limit');
  return result;
}
