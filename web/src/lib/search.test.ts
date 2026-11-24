import { describe, expect, it } from 'vitest';
import fixtures from '../../../contracts/search-fixtures.json';
import {
  normalizeSearchText,
  searchProfile,
  SearchTextError,
  tokenizeSearchDocument,
  tokenizeSearchText,
} from './search';

describe('pinned search text profile', () => {
  it('uses the same profile as the shared server fixtures', () => {
    expect(searchProfile).toBe(fixtures.profile);
  });

  for (const fixture of fixtures.cases) {
    it(fixture.name, () => {
      expect(tokenizeSearchText(fixture.input)).toEqual({
        normalized: fixture.normalized,
        words: fixture.words,
        cjk_single: fixture.cjk_single,
        cjk_bigram: fixture.cjk_bigram,
      });
    });
  }

  for (const fixture of fixtures.invalid) {
    it(`rejects ${fixture.name}`, () => {
      expect(() => tokenizeSearchText(fixture.input)).toThrowError(SearchTextError);
    });
  }

  it('rejects lone UTF-16 surrogates and validates before the byte budget', () => {
    for (const text of ['\ud800', '\udfff', `${'x'.repeat(16385)}\0`]) {
      try {
        normalizeSearchText(text);
        throw new Error('Invalid text was accepted');
      } catch (error) {
        expect(error).toBeInstanceOf(SearchTextError);
        expect((error as SearchTextError).code).toBe('invalid');
      }
    }
  });

  it('bounds input bytes and query tokens without rejecting valid combining sequences', () => {
    expect(normalizeSearchText(`a${'́'.repeat(24)}`)).toBe(`á${'́'.repeat(23)}`);
    expect(() => normalizeSearchText(`a${'́'.repeat(8191)}`)).not.toThrow();
    expect(() => normalizeSearchText(`a${'́'.repeat(8192)}`)).toThrowError(SearchTextError);
    expect(() => normalizeSearchText('x'.repeat(16385))).toThrowError(SearchTextError);
    const terms = Array.from({ length: 2049 }, (_, index) => `w${index}`);
    expect(() => tokenizeSearchText(terms.slice(0, 2048).join(' '))).not.toThrow();
    expect(() => tokenizeSearchText(terms.join(' '))).toThrowError(SearchTextError);
  });

  it('tokenizes bounded source titles even when their token set exceeds the query budget', () => {
    for (const count of [1024, 1025, 1365]) {
      const input = Array.from({ length: count }, (_, index) =>
        String.fromCodePoint(0x4e00 + index),
      ).join('');
      const tokens = tokenizeSearchDocument(input);
      expect(tokens.cjk_single).toHaveLength(count);
      expect(tokens.cjk_bigram).toHaveLength(count - 1);
      if (count === 1024) expect(() => tokenizeSearchText(input)).not.toThrow();
      else expect(() => tokenizeSearchText(input)).toThrowError(SearchTextError);
    }
    expect(() => tokenizeSearchDocument('bad\0name')).toThrowError(SearchTextError);
  });

  it('normalizes maximum-expansion scalars within the source-title byte limit', () => {
    const input = 'ﷺ'.repeat(1365);
    const expected = 'صلى الله عليه وسلم'.repeat(1365);
    expect(new TextEncoder().encode(input).length).toBe(4095);
    expect(new TextEncoder().encode(expected).length).toBe(45045);
    expect(tokenizeSearchDocument(input).normalized).toBe(expected);
  });

  it('reorders and composes without crossing blocked or Hangul boundaries', () => {
    expect(normalizeSearchText('À̕')).toBe('à̕');
    expect(normalizeSearchText('A̅̊')).toBe('a̅̊');
    expect(normalizeSearchText('각')).toBe('각');
    expect(normalizeSearchText('각')).toBe('각');
    expect(normalizeSearchText('A﻿̊')).toBe('a﻿̊');
  });
});
