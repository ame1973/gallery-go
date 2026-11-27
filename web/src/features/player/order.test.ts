import { describe, expect, it } from 'vitest';
import { advance, back, beginOrder, current, type OrderOptions } from './order';

const defaults: OrderOptions = {
  startIndex: 0,
  seed: 17,
  shuffle: false,
  direction: 'forward',
  repeat: 'none',
};

function make(ids: string[], options: Partial<OrderOptions> = {}) {
  return beginOrder(ids, { ...defaults, ...options });
}

describe('fixed playback order', () => {
  it('handles empty snapshots and clamps a finite starting index', () => {
    const empty = make([]);
    expect(current(empty)).toBeNull();
    expect(advance(empty, 'automatic')).toBe(empty);
    expect(back(empty)).toBe(empty);
    expect(current(make(['a', 'b', 'c'], { startIndex: -50 }))?.id).toBe('a');
    expect(current(make(['a', 'b', 'c'], { startIndex: 50 }))?.id).toBe('c');
    expect(current(make(['a', 'b', 'c'], { startIndex: 1.9 }))?.index).toBe(1);
    for (const startIndex of [NaN, Infinity, -Infinity]) {
      expect(() => make(['a'], { startIndex })).toThrow();
    }
  });

  it('moves through the supplied snapshot in either direction', () => {
    let forward = make(['a', 'b', 'c'], { startIndex: 1 });
    expect(current(forward)).toEqual({ id: 'b', index: 1 });
    forward = advance(forward, 'manual');
    expect(current(forward)?.id).toBe('c');
    forward = advance(forward, 'automatic');
    expect(forward.ended).toBe(true);
    expect(current(forward)?.id).toBe('c');
    forward = back(forward);
    expect(forward.ended).toBe(false);
    expect(current(forward)?.id).toBe('b');

    let reverse = make(['a', 'b', 'c'], { startIndex: 2, direction: 'reverse' });
    expect(current(reverse)?.id).toBe('c');
    reverse = advance(reverse, 'manual');
    expect(current(reverse)?.id).toBe('b');
    reverse = advance(reverse, 'automatic');
    expect(current(reverse)?.id).toBe('a');
    reverse = advance(reverse, 'automatic');
    expect(reverse.ended).toBe(true);
  });

  it('repeats one only on automatic advance, without trapping manual navigation', () => {
    let order = make(['a', 'b'], { repeat: 'one' });
    order = advance(order, 'automatic');
    expect(current(order)?.id).toBe('a');
    order = advance(order, 'manual');
    expect(current(order)?.id).toBe('b');
    order = back(order);
    expect(current(order)?.id).toBe('a');
  });

  it('repeats all and follows previous history across a cycle boundary', () => {
    let order = make(['a', 'b', 'c'], { startIndex: 1, repeat: 'all' });
    order = advance(order, 'manual');
    expect(current(order)?.id).toBe('c');
    order = advance(order, 'manual');
    expect(current(order)?.id).toBe('a');
    order = back(order);
    expect(current(order)?.id).toBe('c');
    order = advance(order, 'manual');
    expect(current(order)?.id).toBe('a');
    expect(order.cycle).toBe(1);
  });

  it('preserves the snapshot and rejects duplicate or invalid identifiers and options', () => {
    const source = ['a', 'b'];
    const order = make(source);
    source[0] = 'changed';
    expect(current(order)?.id).toBe('a');
    expect(() => make(['a', 'a'])).toThrow();
    expect(() => make([''])).toThrow();
    expect(() => make(['x'.repeat(129)])).toThrow();
    expect(() => make(Array<string>(1))).toThrow();
    expect(() => make(['a'], { seed: -1 })).toThrow();
    expect(() => make(['a'], { seed: 0x1_0000_0000 })).toThrow();
    expect(() => make(['a'], { seed: 0.5 })).toThrow();
    expect(() => make(['a'], { repeat: 'invalid' as OrderOptions['repeat'] })).toThrow();
    expect(() => make(['a'], { direction: 'invalid' as OrderOptions['direction'] })).toThrow();
    expect(() => make(['a'], { shuffle: 'yes' as unknown as boolean })).toThrow();
    expect(Object.isFrozen(order)).toBe(true);
    expect(Object.isFrozen(order.ids)).toBe(true);
    expect(Object.isFrozen(order.options)).toBe(true);
    expect(Object.isFrozen(order.indices)).toBe(true);
  });

  it('produces a deterministic seeded permutation and never repeats within a cycle', () => {
    for (let size = 1; size <= 30; size++) {
      const ids = Array.from({ length: size }, (_, index) => `media-${index}`);
      for (let seed = 0; seed < 40; seed++) {
        for (const direction of ['forward', 'reverse'] as const) {
          const options = { seed, startIndex: size - 1, shuffle: true, direction };
          let first = make(ids, options);
          let second = make(ids, options);
          const seen = new Set<string>();
          expect(current(first)?.id).toBe(ids[size - 1]);
          for (let index = 0; index < size; index++) {
            const item = current(first);
            expect(item).toEqual(current(second));
            expect(item).not.toBeNull();
            expect(seen.has(item!.id)).toBe(false);
            seen.add(item!.id);
            first = advance(first, 'manual');
            second = advance(second, 'manual');
          }
          expect(seen.size).toBe(size);
          expect(first.ended).toBe(true);
        }
      }
    }
  });

  it('uses the seed to vary permutations without changing the selected first item', () => {
    const ids = ['a', 'b', 'c', 'd', 'e', 'f', 'g'];
    const permutations = new Set<string>();
    for (let seed = 0; seed < 32; seed++) {
      const order = make(ids, { startIndex: 2, seed, shuffle: true });
      expect(current(order)?.id).toBe('c');
      permutations.add(JSON.stringify(order.indices));
    }
    expect(permutations.size).toBeGreaterThan(1);
  });

  it('reconstructs reverse shuffle history from a nonzero starting index', () => {
    const ids = ['a', 'b', 'c', 'd', 'e'];
    for (const direction of ['forward', 'reverse'] as const) {
      let order = make(ids, {
        startIndex: 3,
        seed: 0xffff_ffff,
        shuffle: true,
        direction,
        repeat: 'all',
      });
      const visited: string[] = [];
      for (let index = 0; index < 25; index++) {
        visited.push(current(order)!.id);
        order = advance(order, 'manual');
      }
      for (let index = visited.length - 1; index >= 0; index--) {
        order = back(order);
        expect(current(order)?.id).toBe(visited[index]);
      }
      expect(current(order)).toEqual({ id: 'd', index: 3 });
    }
  });

  it('handles a supplied 100,000-item snapshot without losing or duplicating an item', () => {
    const ids = Array.from({ length: 100_000 }, (_, index) => `media-${index}`);
    let order = make(ids, { startIndex: ids.length - 1, seed: 0, shuffle: true, repeat: 'all' });
    expect(current(order)).toEqual({ id: 'media-99999', index: 99_999 });
    expect(new Set(order.indices).size).toBe(ids.length);
    for (let index = 1; index < ids.length; index++) order = advance(order, 'manual');
    const last = current(order)!.id;
    order = advance(order, 'manual');
    expect(current(order)!.id).not.toBe(last);
    expect(new Set(order.indices).size).toBe(ids.length);
    expect(current(back(order))!.id).toBe(last);
  });

  it('reconstructs shuffle history across rounds and avoids a repeated boundary', () => {
    for (let size = 1; size <= 24; size++) {
      const ids = Array.from({ length: size }, (_, index) => `media-${index}`);
      for (let seed = 0; seed < 16; seed++) {
        let order = make(ids, { seed, shuffle: true, repeat: 'all' });
        const visited: string[] = [];
        for (let cycle = 0; cycle < 4; cycle++) {
          const round = new Set<string>();
          for (let index = 0; index < size; index++) {
            const id = current(order)!.id;
            if (size > 1 && index === 0 && visited.length > 0) {
              expect(id).not.toBe(visited[visited.length - 1]);
            }
            expect(round.has(id)).toBe(false);
            round.add(id);
            visited.push(id);
            order = advance(order, 'manual');
          }
          expect(round.size).toBe(size);
        }
        for (let index = visited.length - 1; index >= 0; index--) {
          order = back(order);
          expect(current(order)?.id).toBe(visited[index]);
        }
        const beforeStart = back(order);
        expect(current(beforeStart)).toEqual(current(order));
        for (let index = 1; index < visited.length; index++) {
          order = advance(order, 'manual');
          expect(current(order)?.id).toBe(visited[index]);
        }
      }
    }
  });
});
