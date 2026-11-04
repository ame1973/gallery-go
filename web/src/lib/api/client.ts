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
  if (!live.data || !ready.data || !config.data) {
    throw new Error('SYSTEM_UNAVAILABLE');
  }
  return { live: live.data, ready: ready.data, config: config.data };
}
