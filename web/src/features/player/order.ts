// This module orders an already acquired snapshot. It does not fetch or authorize media.
export type OrderOptions = Readonly<{
  startIndex: number;
  seed: number;
  shuffle: boolean;
  direction: 'forward' | 'reverse';
  repeat: 'none' | 'all' | 'one';
}>;

export type PlaybackOrder = Readonly<{
  ids: readonly string[];
  options: OrderOptions;
  indices: readonly number[];
  position: number;
  cycle: number;
  ended: boolean;
}>;

function rawOrder(size: number, options: OrderOptions, cycle: number) {
  const indices = Array.from({ length: size }, (_, index) => index);
  if (options.shuffle) {
    let state = (options.seed + Math.imul(cycle, 0x9e3779b9)) >>> 0;
    for (let index = size - 1; index > 0; index--) {
      state = (state + 0x6d2b79f5) >>> 0;
      let value = state;
      value = Math.imul(value ^ (value >>> 15), value | 1);
      value ^= value + Math.imul(value ^ (value >>> 7), value | 61);
      const random = (value ^ (value >>> 14)) >>> 0;
      const selected = Math.floor((random / 0x1_0000_0000) * (index + 1));
      [indices[index], indices[selected]] = [indices[selected]!, indices[index]!];
    }
  }
  if (options.direction === 'reverse') indices.reverse();
  return indices;
}

function firstCycle(size: number, options: OrderOptions) {
  const indices = rawOrder(size, options, 0);
  if (options.shuffle && size > 0) {
    const selected = indices.indexOf(options.startIndex);
    [indices[0], indices[selected]] = [indices[selected]!, indices[0]!];
  }
  return indices;
}

function orderForCycle(size: number, options: OrderOptions, cycle: number): readonly number[] {
  if (cycle === 0 || !options.shuffle || size < 3) {
    return Object.freeze(firstCycle(size, options));
  }
  const indices = rawOrder(size, options, cycle);
  // ponytail: swapping the first two entries avoids a repeated boundary without
  // retaining unbounded shuffle history; two-item rounds keep their initial order.
  const previous = cycle === 1 ? firstCycle(size, options) : rawOrder(size, options, cycle - 1);
  if (indices[0] === previous[size - 1]) {
    [indices[0], indices[1]] = [indices[1]!, indices[0]!];
  }
  return Object.freeze(indices);
}

export function beginOrder(ids: readonly string[], options: OrderOptions): PlaybackOrder {
  if (
    !Array.isArray(ids) ||
    !options ||
    !Number.isFinite(options.startIndex) ||
    !Number.isInteger(options.seed) ||
    options.seed < 0 ||
    options.seed > 0xffff_ffff ||
    typeof options.shuffle !== 'boolean' ||
    !['forward', 'reverse'].includes(options.direction) ||
    !['none', 'all', 'one'].includes(options.repeat)
  ) {
    throw new TypeError('Invalid playback order options');
  }
  const snapshot = Array.from(ids);
  if (
    snapshot.some((id) => typeof id !== 'string' || id.length === 0 || id.length > 128) ||
    new Set(snapshot).size !== snapshot.length
  ) {
    throw new TypeError('Invalid playback snapshot');
  }
  const startIndex = snapshot.length
    ? Math.min(Math.max(Math.trunc(options.startIndex), 0), snapshot.length - 1)
    : 0;
  const settings = Object.freeze({ ...options, startIndex });
  const indices = orderForCycle(snapshot.length, settings, 0);
  const position = settings.shuffle ? 0 : Math.max(0, indices.indexOf(startIndex));
  return Object.freeze({
    ids: Object.freeze(snapshot),
    options: settings,
    indices,
    position,
    cycle: 0,
    ended: snapshot.length === 0,
  });
}

export function current(order: PlaybackOrder): { id: string; index: number } | null {
  const index = order.indices[order.position];
  if (index === undefined) return null;
  return { id: order.ids[index]!, index };
}

export function advance(order: PlaybackOrder, cause: 'manual' | 'automatic'): PlaybackOrder {
  if (cause !== 'manual' && cause !== 'automatic') throw new TypeError('Invalid advance cause');
  if (order.ids.length === 0) return order;
  if (cause === 'automatic' && order.options.repeat === 'one') {
    return order.ended ? Object.freeze({ ...order, ended: false }) : order;
  }
  if (order.position + 1 < order.indices.length) {
    return Object.freeze({ ...order, position: order.position + 1, ended: false });
  }
  if (order.options.repeat !== 'all') {
    return order.ended ? order : Object.freeze({ ...order, ended: true });
  }
  if (order.cycle >= Number.MAX_SAFE_INTEGER) throw new RangeError('Playback cycle exhausted');
  const cycle = order.cycle + 1;
  return Object.freeze({
    ...order,
    indices: orderForCycle(order.ids.length, order.options, cycle),
    position: 0,
    cycle,
    ended: false,
  });
}

export function back(order: PlaybackOrder): PlaybackOrder {
  if (order.position > 0) {
    return Object.freeze({ ...order, position: order.position - 1, ended: false });
  }
  if (order.cycle > 0) {
    const cycle = order.cycle - 1;
    return Object.freeze({
      ...order,
      indices: orderForCycle(order.ids.length, order.options, cycle),
      position: order.ids.length - 1,
      cycle,
      ended: false,
    });
  }
  return order;
}
