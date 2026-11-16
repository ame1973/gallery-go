import { api } from '../../lib/api/client';
import type { components } from '../../lib/api/schema';
import { APIError } from '../auth/api';
import { safeID } from '../library/api';

export type Source = components['schemas']['Source'];

const states: Source['state'][] = ['unknown', 'healthy', 'unavailable', 'error'];

function integer(value: number, minimum: number, maximum = Number.MAX_SAFE_INTEGER) {
  return Number.isSafeInteger(value) && value >= minimum && value <= maximum;
}

function validSource(source: Source) {
  return (
    source !== null &&
    typeof source === 'object' &&
    safeID(source.id) &&
    safeID(source.library_id) &&
    typeof source.root_name === 'string' &&
    source.root_name.length > 0 &&
    source.root_name.length <= 64 &&
    source.mapping !== null &&
    typeof source.mapping === 'object' &&
    integer(source.mapping.album_depth, 0, 16) &&
    integer(source.mapping.person_depth, 0, source.mapping.album_depth) &&
    typeof source.active === 'boolean' &&
    typeof source.configured === 'boolean' &&
    states.includes(source.state) &&
    integer(source.minimum_files, 0, 1_000_000_000) &&
    integer(source.poll_interval_seconds, 10, 86_400) &&
    integer(source.full_scan_interval_seconds, source.poll_interval_seconds, 604_800) &&
    integer(source.scan_generation, 0) &&
    integer(source.version, 1) &&
    integer(source.config_version, 1) &&
    [source.last_checked_at, source.last_healthy_at, source.last_full_scan_at].every(
      (time) => time === null || typeof time === 'string',
    ) &&
    (source.last_error_code === null || typeof source.last_error_code === 'string')
  );
}

type Failure = { response: Response; error?: components['schemas']['Problem'] };

function failure(result: Failure): never {
  const code =
    result.response.status === 401
      ? 'UNAUTHENTICATED'
      : result.response.status === 403
        ? 'ADMIN_REQUIRED'
        : result.response.status === 404
          ? 'NOT_FOUND'
          : result.error?.code;
  throw new APIError(code ?? 'INTERNAL_ERROR', result.error?.request_id);
}

export async function listSources(cursor: string | null, signal: AbortSignal) {
  const result = await api.GET('/admin/sources', {
    signal,
    params: { query: { limit: 30, ...(cursor && { cursor }) } },
  });
  if (!result.data) return failure(result);
  const page = result.data;
  if (
    !Array.isArray(page.items) ||
    page.items.length > 30 ||
    page.items.some((source) => !validSource(source)) ||
    new Set(page.items.map((source) => source.id)).size !== page.items.length ||
    (page.has_more === false
      ? page.next_cursor !== null
      : page.has_more !== true ||
        typeof page.next_cursor !== 'string' ||
        page.next_cursor.length === 0 ||
        page.next_cursor.length > 512)
  ) {
    throw new APIError('INVALID_SOURCE_RESPONSE');
  }
  return page;
}

export async function getSource(id: string, signal: AbortSignal) {
  const result = await api.GET('/admin/sources/{source_id}', {
    signal,
    params: { path: { source_id: id } },
  });
  if (!result.data) return failure(result);
  if (!validSource(result.data) || result.data.id !== id) {
    throw new APIError('INVALID_SOURCE_RESPONSE');
  }
  return result.data;
}
