import { APIError } from '../../features/auth/api';
import type { User } from '../../features/auth/api';

const expectedUser = 'X-GalleryGo-Expected-User';
const expectedScope = 'X-GalleryGo-Expected-Scope';
export type ReadActor = Pick<User, 'id' | 'scope_id'>;

export function identityValue(value: unknown): value is string {
  return typeof value === 'string' && /^[\x21-\x7e]{1,128}$/.test(value);
}

export function validReadActor(actor: ReadActor) {
  return identityValue(actor.id) && identityValue(actor.scope_id);
}

export function identityHeaders(actor: ReadActor) {
  if (!validReadActor(actor)) throw new APIError('READ_CONTEXT_MISMATCH');
  return { [expectedUser]: actor.id, [expectedScope]: actor.scope_id };
}

export function checkReadEcho(response: Response, actor: ReadActor) {
  if (
    response.headers.get(expectedUser) !== actor.id ||
    response.headers.get(expectedScope) !== actor.scope_id
  ) {
    throw new APIError('READ_CONTEXT_MISMATCH');
  }
}
