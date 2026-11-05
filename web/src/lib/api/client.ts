import createClient from 'openapi-fetch';
import type { paths } from './schema';

export const api = createClient<paths>({
  baseUrl: new URL('/api/v2', window.location.origin).href,
  credentials: 'same-origin',
  cache: 'no-store',
  fetch: (request) => globalThis.fetch(request),
});

export async function readSystem(signal: AbortSignal) {
  const [live, ready, config] = await Promise.all([
    api.GET('/health/live', { signal }),
    api.GET('/health/ready', { signal }),
    api.GET('/public/config', { signal }),
  ]);
  if (
    !live.data ||
    !ready.data ||
    !config.data ||
    live.data.status !== 'ok' ||
    ready.data.database !== 'ok' ||
    !['ok', 'degraded'].includes(ready.data.status) ||
    !['ok', 'optional'].includes(ready.data.worker) ||
    !['ok', 'degraded'].includes(ready.data.sources) ||
    (ready.data.status === 'degraded') !== (ready.data.sources === 'degraded')
  ) {
    throw new Error('SYSTEM_UNAVAILABLE');
  }
  return { live: live.data, ready: ready.data, config: config.data };
}
