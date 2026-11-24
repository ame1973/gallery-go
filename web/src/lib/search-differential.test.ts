import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import {
  searchProfile,
  SearchTextError,
  tokenizeSearchDocument,
  tokenizeSearchText,
  type SearchTokens,
} from './search';

interface Example {
  input: string;
  tokens?: SearchTokens;
  error?: 'invalid' | 'limit';
}

// The corpus is generated independently by the real Go implementation.
const corpusPath = process.env.GALLERYGO_SEARCH_CORPUS;
describe.runIf(corpusPath !== undefined)('Go and browser search differential', () => {
  it('matches generated compositions, Hangul, marks and deterministic mixed text', () => {
    const corpus = JSON.parse(readFileSync(corpusPath!, 'utf8')) as {
      profile: string;
      cases: Example[];
      documents: Example[];
    };
    expect(corpus.profile).toBe(searchProfile);
    expect(corpus.cases.length).toBe(139_903);
    expect(corpus.documents.length).toBe(541);
    for (const example of corpus.cases) {
      try {
        const actual = tokenizeSearchText(example.input);
        if (
          example.error !== undefined ||
          JSON.stringify(actual) !== JSON.stringify(example.tokens)
        ) {
          throw new Error(
            `Search mismatch for ${JSON.stringify(example.input)}: ` +
              `actual ${JSON.stringify(actual)}, expected ${JSON.stringify(example)}`,
          );
        }
      } catch (error) {
        if (!(error instanceof SearchTextError) || error.code !== example.error) throw error;
      }
    }
    for (const example of corpus.documents) {
      try {
        const actual = tokenizeSearchDocument(example.input);
        if (
          example.error !== undefined ||
          JSON.stringify(actual) !== JSON.stringify(example.tokens)
        ) {
          throw new Error(`Document search mismatch for ${JSON.stringify(example.input)}`);
        }
      } catch (error) {
        if (!(error instanceof SearchTextError) || error.code !== example.error) throw error;
      }
    }
  }, 120_000);
});
