import { api } from '../../lib/api/client';
import type { components } from '../../lib/api/schema';

export type User = components['schemas']['User'];
export type Session = components['schemas']['Session'];
type Problem = components['schemas']['Problem'];

export class APIError extends Error {
  constructor(
    public readonly code: string,
    public readonly requestID?: string,
  ) {
    super(code);
  }
}

function failure(problem?: Problem) {
  return new APIError(problem?.code ?? 'INTERNAL_ERROR', problem?.request_id);
}

export async function currentUser(signal?: AbortSignal) {
  const result = await api.GET('/auth/me', { ...(signal ? { signal } : {}) });
  if (result.response.status === 401) return null;
  if (!result.data) throw failure(result.error);
  return result.data;
}

export async function securityHeaders(signal?: AbortSignal) {
  const { data, error, response } = await api.GET('/auth/csrf', { ...(signal ? { signal } : {}) });
  if (!data) {
    if (response.status === 401) throw new APIError('UNAUTHENTICATED', error?.request_id);
    throw failure(error);
  }
  return { Origin: window.location.origin, 'X-CSRF-Token': data.token };
}

export async function signIn(body: components['schemas']['LoginRequest']) {
  const { data, error } = await api.POST('/auth/login', {
    body,
    params: { header: { Origin: window.location.origin } },
  });
  if (!data) throw failure(error);
  return data;
}

export async function signOut() {
  const result = await api.POST('/auth/logout', { params: { header: await securityHeaders() } });
  if (!result.response.ok) throw failure(result.error);
}

export async function changePassword(input: components['schemas']['PasswordRequest']) {
  const { data, error } = await api.POST('/auth/password', {
    body: { old_password: input.old_password, new_password: input.new_password },
    params: { header: await securityHeaders() },
  });
  if (!data) throw failure(error);
  return data;
}

export async function listSessions(cursor: string | null, signal: AbortSignal) {
  const { data, error } = await api.GET('/auth/sessions', {
    signal,
    params: { query: { limit: 30, ...(cursor ? { cursor } : {}) } },
  });
  if (!data) throw failure(error);
  return data;
}

export async function revokeSession(id: string) {
  const result = await api.DELETE('/auth/sessions/{session_id}', {
    params: { path: { session_id: id }, header: await securityHeaders() },
  });
  if (!result.response.ok) throw failure(result.error);
}

export function errorMessage(error: unknown) {
  if (!(error instanceof APIError)) return 'auth.networkError';
  switch (error.code) {
    case 'INVALID_CREDENTIALS':
      return 'auth.invalidCredentials';
    case 'RATE_LIMITED':
      return 'auth.rateLimited';
    case 'ORIGIN_REJECTED':
      return 'auth.originRejected';
    case 'CSRF_REJECTED':
      return 'auth.csrfRejected';
    case 'UNAUTHENTICATED':
      return 'auth.sessionExpired';
    case 'VALIDATION_FAILED':
      return 'auth.passwordRule';
    case 'VERSION_CONFLICT':
      return 'auth.conflict';
    default:
      return 'auth.requestFailed';
  }
}
