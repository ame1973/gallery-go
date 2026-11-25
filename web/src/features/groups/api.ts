import { api } from '../../lib/api/client';
import { checkReadEcho, identityHeaders } from '../../lib/api/read-identity';
import type { ReadActor } from '../../lib/api/read-identity';
import type { components } from '../../lib/api/schema';
import { APIError } from '../auth/api';
import { safeID } from '../library/api';

type Group = components['schemas']['Group'];
type Failure = { response: Response; error?: components['schemas']['Problem'] };

function failure(result: Failure): never {
  const code =
    result.response.status === 401
      ? 'UNAUTHENTICATED'
      : result.response.status === 403
        ? 'ADMIN_REQUIRED'
        : result.response.status === 404
          ? 'NOT_FOUND'
          : result.response.status === 409
            ? 'VERSION_CONFLICT'
            : result.error?.code;
  throw new APIError(code ?? 'INTERNAL_ERROR', result.error?.request_id);
}

function validGroup(value: unknown): value is Group {
  if (!value || typeof value !== 'object') return false;
  const group = value as Partial<Group>;
  return (
    typeof group.id === 'string' &&
    safeID(group.id) &&
    typeof group.name === 'string' &&
    group.name.length > 0 &&
    [...group.name].length <= 100 &&
    new TextEncoder().encode(group.name).length <= 400 &&
    !/[\p{Cc}\p{Cs}]/u.test(group.name) &&
    Number.isSafeInteger(group.version) &&
    (group.version ?? 0) >= 1 &&
    typeof group.created_at === 'string' &&
    group.created_at.length <= 64 &&
    Number.isFinite(Date.parse(group.created_at))
  );
}

export async function listGroups(actor: ReadActor, cursor: string | null, signal: AbortSignal) {
  const result = await api.GET('/admin/groups', {
    signal,
    headers: identityHeaders(actor),
    params: { query: { limit: 30, ...(cursor && { cursor }) } },
  });
  if (!result.data) return failure(result);
  checkReadEcho(result.response, actor);
  const page = result.data;
  if (
    !Array.isArray(page.items) ||
    page.items.length > 30 ||
    page.items.some((group) => !validGroup(group)) ||
    new Set(page.items.map((group) => group.id)).size !== page.items.length ||
    (page.has_more === false
      ? page.next_cursor !== null
      : page.has_more !== true ||
        typeof page.next_cursor !== 'string' ||
        page.next_cursor.length === 0 ||
        page.next_cursor.length > 512)
  ) {
    throw new APIError('INVALID_GROUPS_RESPONSE');
  }
  return page;
}

export async function getGroup(actor: ReadActor, id: string, signal: AbortSignal) {
  const result = await api.GET('/admin/groups/{group_id}', {
    signal,
    headers: identityHeaders(actor),
    params: { path: { group_id: id } },
  });
  if (!result.data) return failure(result);
  checkReadEcho(result.response, actor);
  if (!validGroup(result.data) || result.data.id !== id) {
    throw new APIError('INVALID_GROUPS_RESPONSE');
  }
  return result.data;
}
