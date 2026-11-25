import { api } from '../../lib/api/client';
import { checkReadEcho, identityHeaders, identityValue } from '../../lib/api/read-identity';
import type { ReadActor } from '../../lib/api/read-identity';
import type { components } from '../../lib/api/schema';
import { APIError } from '../auth/api';
import type { User } from '../auth/api';
import { safeID } from '../library/api';

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

function validUser(value: unknown): value is User {
  if (!value || typeof value !== 'object') return false;
  const user = value as Partial<User>;
  return (
    typeof user.id === 'string' &&
    safeID(user.id) &&
    typeof user.username === 'string' &&
    /^[A-Za-z0-9][A-Za-z0-9._-]{2,63}$/.test(user.username) &&
    (user.role === 'admin' || user.role === 'user') &&
    typeof user.active === 'boolean' &&
    identityValue(user.scope_id) &&
    Number.isSafeInteger(user.version) &&
    (user.version ?? 0) >= 1 &&
    typeof user.download_original === 'boolean'
  );
}

export async function listUsers(actor: ReadActor, cursor: string | null, signal: AbortSignal) {
  const result = await api.GET('/admin/users', {
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
    page.items.some((user) => !validUser(user)) ||
    new Set(page.items.map((user) => user.id)).size !== page.items.length ||
    (page.has_more === false
      ? page.next_cursor !== null
      : page.has_more !== true ||
        typeof page.next_cursor !== 'string' ||
        page.next_cursor.length === 0 ||
        page.next_cursor.length > 512)
  ) {
    throw new APIError('INVALID_USERS_RESPONSE');
  }
  return page;
}

export async function getUser(actor: ReadActor, id: string, signal: AbortSignal) {
  const result = await api.GET('/admin/users/{user_id}', {
    signal,
    headers: identityHeaders(actor),
    params: { path: { user_id: id } },
  });
  if (!result.data) return failure(result);
  checkReadEcho(result.response, actor);
  if (!validUser(result.data) || result.data.id !== id) {
    throw new APIError('INVALID_USERS_RESPONSE');
  }
  return result.data;
}
