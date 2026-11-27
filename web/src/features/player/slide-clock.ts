// This clock covers still images and animation cycles, not video playback or authorization.
export type Slide = Readonly<{ kind: 'still' } | { kind: 'animated'; cycleMs: number | null }>;
export type SlideSettings = Readonly<{ intervalMs: number; respectAnimation: boolean }>;
export type SlideClock = Readonly<{
  phase: 'ready' | 'playing' | 'paused' | 'ended';
  durationMs: number;
  elapsedMs: number;
  deadlineMs: number | null;
  observedMs: number | null;
}>;

export function createSlideClock(slide: Slide, settings: SlideSettings): SlideClock {
  if (
    !slide ||
    !settings ||
    (slide.kind !== 'still' && slide.kind !== 'animated') ||
    !Number.isSafeInteger(settings.intervalMs) ||
    settings.intervalMs < 500 ||
    settings.intervalMs > 30_000 ||
    typeof settings.respectAnimation !== 'boolean'
  ) {
    throw new TypeError('Invalid slide clock settings');
  }
  if (
    slide.kind === 'animated' &&
    slide.cycleMs !== null &&
    (!Number.isSafeInteger(slide.cycleMs) || slide.cycleMs < 0)
  ) {
    throw new TypeError('Invalid animation cycle');
  }
  const durationMs =
    slide.kind === 'animated' && settings.respectAnimation && slide.cycleMs !== null
      ? Math.max(settings.intervalMs, slide.cycleMs)
      : settings.intervalMs;
  return Object.freeze({
    phase: 'ready',
    durationMs,
    elapsedMs: 0,
    deadlineMs: null,
    observedMs: null,
  });
}

function checkTime(clock: SlideClock, now: number) {
  if (
    !Number.isFinite(now) ||
    now < 0 ||
    now > Number.MAX_SAFE_INTEGER ||
    (clock.observedMs !== null && now < clock.observedMs)
  ) {
    throw new RangeError('Invalid monotonic time');
  }
}

function deadline(now: number, remaining: number) {
  const result = now + remaining;
  if (
    !Number.isFinite(result) ||
    result > Number.MAX_SAFE_INTEGER ||
    (remaining > 0 && result <= now)
  ) {
    throw new RangeError('Slide deadline is not representable');
  }
  return result;
}

function elapsed(clock: SlideClock, now: number) {
  return Math.min(clock.durationMs, clock.elapsedMs + (now - clock.observedMs!));
}

export function start(clock: SlideClock, now: number): SlideClock {
  checkTime(clock, now);
  if (clock.phase === 'playing' || clock.phase === 'ended') return clock;
  return Object.freeze({
    ...clock,
    phase: 'playing',
    observedMs: now,
    deadlineMs: deadline(now, clock.durationMs - clock.elapsedMs),
  });
}

export function tick(clock: SlideClock, now: number): SlideClock {
  checkTime(clock, now);
  if (clock.phase !== 'playing') return clock;
  const elapsedMs = elapsed(clock, now);
  if (now >= clock.deadlineMs! || elapsedMs >= clock.durationMs) {
    return Object.freeze({
      ...clock,
      phase: 'ended',
      elapsedMs: clock.durationMs,
      deadlineMs: null,
      observedMs: now,
    });
  }
  return Object.freeze({ ...clock, elapsedMs, observedMs: now });
}

export function pause(clock: SlideClock, now: number): SlideClock {
  checkTime(clock, now);
  if (clock.phase !== 'playing') return clock;
  return Object.freeze({
    ...clock,
    phase: 'paused',
    elapsedMs: elapsed(clock, now),
    deadlineMs: null,
    observedMs: now,
  });
}

export function seek(clock: SlideClock, targetMs: number, now: number): SlideClock {
  checkTime(clock, now);
  if (!Number.isFinite(targetMs)) throw new TypeError('Invalid slide seek');
  const elapsedMs = Math.max(0, Math.min(targetMs, clock.durationMs));
  const phase = clock.phase === 'ended' && elapsedMs < clock.durationMs ? 'paused' : clock.phase;
  return Object.freeze({
    ...clock,
    phase,
    elapsedMs,
    observedMs: now,
    deadlineMs: phase === 'playing' ? deadline(now, clock.durationMs - elapsedMs) : null,
  });
}

export function reset(clock: SlideClock): SlideClock {
  return Object.freeze({
    phase: 'ready',
    durationMs: clock.durationMs,
    elapsedMs: 0,
    deadlineMs: null,
    observedMs: null,
  });
}
