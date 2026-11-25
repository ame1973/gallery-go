import { api } from '../../lib/api/client';
import { checkReadEcho, identityHeaders } from '../../lib/api/read-identity';
import type { ReadActor } from '../../lib/api/read-identity';
import { APIError } from './api';

export async function listSessions(actor: ReadActor, cursor: string | null, signal: AbortSignal) {
  const result = await api.GET('/auth/sessions', {
    signal,
    headers: identityHeaders(actor),
    params: { query: { limit: 30, ...(cursor ? { cursor } : {}) } },
  });
  if (!result.data) {
    const code =
      result.response.status === 401
        ? 'UNAUTHENTICATED'
        : result.response.status === 403
          ? 'FORBIDDEN'
          : result.response.status === 409
            ? 'VERSION_CONFLICT'
            : (result.error?.code ?? 'INTERNAL_ERROR');
    throw new APIError(code, result.error?.request_id);
  }
  checkReadEcho(result.response, actor);
  return result.data;
}
