import { createHash } from 'node:crypto';
import { describe, expect, it } from 'vitest';
import tables from '../../../contracts/search-normalization-tables.json';
import { normalizeSearchText, SearchTextError } from './search';

// The explicit long check covers every valid scalar against the Go-generated hash.
// It runs separately from ordinary UI unit tests via GALLERYGO_SEARCH_SCALAR_CHECK=1.
describe.runIf(process.env.GALLERYGO_SEARCH_SCALAR_CHECK === '1')(
  'search scalar conformance',
  () => {
    it('matches the pinned server result for every Unicode scalar', () => {
      const digest = createHash('sha256');
      const header = Buffer.allocUnsafe(9);
      for (let codepoint = 0; codepoint <= 0x10ffff; codepoint++) {
        if (codepoint >= 0xd800 && codepoint <= 0xdfff) continue;
        let code = 0;
        let normalized = '';
        try {
          normalized = normalizeSearchText(String.fromCodePoint(codepoint));
        } catch (error) {
          if (!(error instanceof SearchTextError)) throw error;
          code = error.code === 'invalid' ? 1 : 2;
        }
        header.writeUInt32LE(codepoint, 0);
        header.writeUInt8(code, 4);
        header.writeUInt32LE(Buffer.byteLength(normalized, 'utf8'), 5);
        digest.update(header);
        digest.update(normalized, 'utf8');
      }
      expect(digest.digest('hex')).toBe(tables.scalar_sha256);
    }, 180_000);
  },
);
