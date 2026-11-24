import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';

const corpusPath = process.env.GALLERYGO_SEARCH_CORPUS;
const tables = JSON.parse(
  readFileSync(
    new URL('../../contracts/search-normalization-tables.json', import.meta.url),
    'utf8',
  ),
) as { profile: string; scalar_sha256: string };

test('Chromium matches the independent Go multi-scalar corpus', async ({ page }) => {
  if (!corpusPath) throw new Error('Set GALLERYGO_SEARCH_CORPUS to the Go-generated corpus');
  const corpus = JSON.parse(readFileSync(corpusPath, 'utf8')) as {
    profile: string;
    cases: { input: string; tokens?: unknown; error?: string }[];
    documents: { input: string; tokens?: unknown; error?: string }[];
  };
  expect(corpus.profile).toBe(tables.profile);
  expect(corpus.cases.length).toBe(139_903);
  expect(corpus.documents.length).toBe(541);
  await page.goto('/');
  const checked = await page.evaluate(
    async ({ cases, documents }) => {
      const modulePath = '/src/lib/search.ts';
      const search = (await import(modulePath)) as typeof import('../../web/src/lib/search');
      for (const example of cases) {
        try {
          const actual = search.tokenizeSearchText(example.input);
          if (
            example.error !== undefined ||
            JSON.stringify(actual) !== JSON.stringify(example.tokens)
          ) {
            throw new Error(
              `Mismatch for ${JSON.stringify(example.input)}: ${JSON.stringify(actual)} versus ${JSON.stringify(example)}`,
            );
          }
        } catch (error) {
          if (!(error instanceof search.SearchTextError) || error.code !== example.error)
            throw error;
        }
      }
      for (const example of documents) {
        try {
          const actual = search.tokenizeSearchDocument(example.input);
          if (
            example.error !== undefined ||
            JSON.stringify(actual) !== JSON.stringify(example.tokens)
          ) {
            throw new Error(`Document search mismatch for ${JSON.stringify(example.input)}`);
          }
        } catch (error) {
          if (!(error instanceof search.SearchTextError) || error.code !== example.error)
            throw error;
        }
      }
      return { cases: cases.length, documents: documents.length };
    },
    { cases: corpus.cases, documents: corpus.documents },
  );
  expect(checked.cases).toBe(corpus.cases.length);
  expect(checked.documents).toBe(corpus.documents.length);
});

test('Chromium matches every valid scalar in the pinned Go profile', async ({ page }) => {
  await page.goto('/');
  const result = await page.evaluate(async () => {
    const modulePath = '/src/lib/search.ts';
    const search = (await import(modulePath)) as typeof import('../../web/src/lib/search');
    const bytes = new Uint8Array(32 * 1024 * 1024);
    const view = new DataView(bytes.buffer);
    const encoder = new TextEncoder();
    let offset = 0;
    let checked = 0;
    for (let codepoint = 0; codepoint <= 0x10ffff; codepoint++) {
      if (codepoint >= 0xd800 && codepoint <= 0xdfff) continue;
      let code = 0;
      let normalized = '';
      try {
        normalized = search.normalizeSearchText(String.fromCodePoint(codepoint));
      } catch (error) {
        if (!(error instanceof search.SearchTextError)) throw error;
        code = error.code === 'invalid' ? 1 : 2;
      }
      if (offset + 9 > bytes.length) throw new Error('Scalar digest buffer is too small');
      view.setUint32(offset, codepoint, true);
      view.setUint8(offset + 4, code);
      const encoded = encoder.encodeInto(normalized, bytes.subarray(offset + 9));
      if (encoded.read !== normalized.length) throw new Error('Scalar digest buffer is too small');
      view.setUint32(offset + 5, encoded.written, true);
      offset += 9 + encoded.written;
      checked++;
    }
    const digest = await crypto.subtle.digest('SHA-256', bytes.subarray(0, offset));
    return {
      checked,
      hash: Array.from(new Uint8Array(digest), (value) => value.toString(16).padStart(2, '0')).join(
        '',
      ),
    };
  });
  expect(result.checked).toBe(1_112_064);
  expect(result.hash).toBe(tables.scalar_sha256);
});
