import { describe, expect, it } from 'vitest';
import { createSlideClock, pause, reset, seek, start, tick } from './slide-clock';

const still = { kind: 'still' } as const;
const settings = { intervalMs: 2000, respectAnimation: true };

describe('monotonic still-image clock', () => {
  it('starts, ticks, and ends exactly once at its deadline', () => {
    let clock = createSlideClock(still, settings);
    expect(clock).toMatchObject({
      phase: 'ready',
      durationMs: 2000,
      elapsedMs: 0,
      deadlineMs: null,
    });
    clock = start(clock, 100);
    expect(clock.deadlineMs).toBe(2100);
    clock = tick(clock, 350);
    expect(clock).toMatchObject({ phase: 'playing', elapsedMs: 250, deadlineMs: 2100 });
    clock = tick(clock, 2100);
    expect(clock).toMatchObject({ phase: 'ended', elapsedMs: 2000, deadlineMs: null });
    expect(tick(clock, 5000)).toBe(clock);
  });

  it('pauses without accumulating hidden time and resumes the remaining duration', () => {
    let clock = start(createSlideClock(still, settings), 100);
    clock = pause(clock, 600);
    expect(clock).toMatchObject({ phase: 'paused', elapsedMs: 500, deadlineMs: null });
    expect(tick(clock, 10_000)).toBe(clock);
    clock = start(clock, 10_000);
    expect(clock.deadlineMs).toBe(11_500);
    expect(tick(clock, 11_499)).toMatchObject({ phase: 'playing', elapsedMs: 1999 });
    expect(tick(clock, 11_500)).toMatchObject({ phase: 'ended', elapsedMs: 2000 });
  });

  it('does not advance multiple slides after a delayed callback or hidden pause', () => {
    let clock = start(createSlideClock(still, settings), 0);
    clock = pause(clock, 60_000);
    expect(clock).toMatchObject({ phase: 'paused', elapsedMs: 2000, deadlineMs: null });
    clock = start(clock, 90_000);
    clock = tick(clock, 90_000);
    expect(clock.phase).toBe('ended');
    expect(tick(clock, 90_000)).toBe(clock);
    const next = start(createSlideClock(still, settings), 90_000);
    expect(tick(next, 90_000)).toMatchObject({ phase: 'playing', elapsedMs: 0 });
  });

  it('uses a known animation cycle only when requested and longer than the interval', () => {
    expect(createSlideClock({ kind: 'animated', cycleMs: 5000 }, settings).durationMs).toBe(5000);
    expect(createSlideClock({ kind: 'animated', cycleMs: 1000 }, settings).durationMs).toBe(2000);
    expect(createSlideClock({ kind: 'animated', cycleMs: null }, settings).durationMs).toBe(2000);
    expect(createSlideClock({ kind: 'animated', cycleMs: 0 }, settings).durationMs).toBe(2000);
    expect(
      createSlideClock(
        { kind: 'animated', cycleMs: 5000 },
        { ...settings, respectAnimation: false },
      ).durationMs,
    ).toBe(2000);
  });

  it('seeks within bounds without restarting playback after the clock ended', () => {
    let clock = start(createSlideClock(still, settings), 100);
    clock = seek(clock, 1500, 500);
    expect(clock).toMatchObject({ phase: 'playing', elapsedMs: 1500, deadlineMs: 1000 });
    clock = tick(clock, 1000);
    expect(clock.phase).toBe('ended');
    clock = seek(clock, -10, 1200);
    expect(clock).toMatchObject({ phase: 'paused', elapsedMs: 0, deadlineMs: null });
    clock = start(clock, 2000);
    expect(clock.deadlineMs).toBe(4000);
    clock = seek(clock, 9999, 2100);
    expect(clock).toMatchObject({ phase: 'playing', elapsedMs: 2000, deadlineMs: 2100 });
    expect(tick(clock, 2100).phase).toBe('ended');
  });

  it('resets a finished clock without carrying a deadline or clock origin', () => {
    const ended = tick(start(createSlideClock(still, settings), 100), 5000);
    const initial = reset(ended);
    expect(initial).toMatchObject({ phase: 'ready', elapsedMs: 0, deadlineMs: null });
    expect(start(initial, 0).deadlineMs).toBe(2000);
    expect(Object.isFrozen(initial)).toBe(true);
  });

  it('validates media duration, settings, and nondecreasing monotonic times', () => {
    for (const intervalMs of [0, 499, 30_001, 1.5, NaN, Infinity]) {
      expect(() => createSlideClock(still, { ...settings, intervalMs })).toThrow();
    }
    for (const cycleMs of [-1, 0.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1]) {
      expect(() => createSlideClock({ kind: 'animated', cycleMs }, settings)).toThrow();
    }
    expect(() =>
      createSlideClock(still, { ...settings, respectAnimation: 1 as unknown as boolean }),
    ).toThrow();
    const clock = start(createSlideClock(still, settings), 100);
    for (const invalid of [-1, 99, NaN, Infinity]) {
      expect(() => tick(clock, invalid)).toThrow();
      expect(() => pause(clock, invalid)).toThrow();
    }
    expect(() => seek(clock, NaN, 100)).toThrow();
    expect(() => start(createSlideClock(still, settings), Number.MAX_SAFE_INTEGER)).toThrow();
  });

  it('keeps ready and ended clocks inert until explicitly started or reset', () => {
    const ready = createSlideClock(still, settings);
    expect(pause(ready, 5)).toBe(ready);
    expect(tick(ready, 5)).toBe(ready);
    const ended = tick(start(ready, 10), 2010);
    expect(start(ended, 3000)).toBe(ended);
    expect(seek(ended, 2000, 3000).phase).toBe('ended');
    expect(start(reset(ended), 0).phase).toBe('playing');
  });

  it('preserves the remaining duration across repeated pause and resume cycles', () => {
    for (const intervalMs of [500, 1000, 5000, 30_000]) {
      let clock = createSlideClock(still, { ...settings, intervalMs });
      let now = 0;
      let active = 0;
      for (let step = 0; step < 10; step++) {
        clock = start(clock, now);
        const increment = intervalMs / 20;
        now += increment;
        active += increment;
        clock = pause(clock, now);
        expect(clock.elapsedMs).toBe(active);
        now += 10_000;
      }
      clock = start(clock, now);
      expect(clock.deadlineMs).toBe(now + intervalMs - active);
      expect(tick(clock, clock.deadlineMs!).phase).toBe('ended');
    }
  });

  it('does not depend on how many intermediate ticks occurred', () => {
    const initial = start(createSlideClock(still, settings), 0.25);
    let frequent = initial;
    for (let index = 1; index <= 1000; index++) {
      frequent = tick(frequent, 0.25 + index * 1.25);
    }
    const single = tick(initial, 1250.25);
    expect(frequent.elapsedMs).toBeCloseTo(single.elapsedMs, 8);
    expect(frequent.deadlineMs).toBe(single.deadlineMs);
    expect(tick(frequent, 2000.25).phase).toBe('ended');
  });

  it('maintains deadline and elapsed invariants across mixed events', () => {
    let clock = createSlideClock({ kind: 'animated', cycleMs: 5000 }, settings);
    let now = 0;
    let random = 12345;
    for (let index = 0; index < 10_000; index++) {
      random = (Math.imul(random, 1664525) + 1013904223) >>> 0;
      now += random % 19;
      switch ((random >>> 8) % 5) {
        case 0:
          clock = start(clock, now);
          break;
        case 1:
          clock = pause(clock, now);
          break;
        case 2:
          clock = tick(clock, now);
          break;
        case 3:
          clock = seek(clock, (random % 7000) - 1000, now);
          break;
        default:
          clock = reset(clock);
      }
      expect(clock.elapsedMs).toBeGreaterThanOrEqual(0);
      expect(clock.elapsedMs).toBeLessThanOrEqual(clock.durationMs);
      if (clock.phase === 'playing') {
        expect(clock.deadlineMs).not.toBeNull();
        expect(clock.observedMs).not.toBeNull();
        expect(clock.deadlineMs!).toBeGreaterThanOrEqual(clock.observedMs!);
      } else {
        expect(clock.deadlineMs).toBeNull();
      }
      if (clock.phase === 'ended') expect(clock.elapsedMs).toBe(clock.durationMs);
    }
  });

  it('accepts exact interval bounds and fractional monotonic clock values', () => {
    const minimum = createSlideClock(still, { intervalMs: 500, respectAnimation: false });
    const maximum = createSlideClock(still, { intervalMs: 30_000, respectAnimation: false });
    expect(minimum.durationMs).toBe(500);
    expect(maximum.durationMs).toBe(30_000);
    const clock = tick(start(minimum, 0.25), 125.75);
    expect(clock).toMatchObject({ elapsedMs: 125.5, deadlineMs: 500.25 });
  });
});
