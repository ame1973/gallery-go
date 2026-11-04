import { describe, expect, it } from 'vitest';
import { loginSchema, passwordSchema } from './forms';
import { APIError, errorMessage } from './api';

describe('credential validation', () => {
  it('matches the server Unicode character and UTF-8 byte limits', () => {
    for (const password of ['x'.repeat(14), 'x'.repeat(73), '界'.repeat(25), '😀'.repeat(14)]) {
      expect(
        passwordSchema.safeParse({
          old_password: 'old',
          new_password: password,
          confirmation: password,
        }).success,
      ).toBe(false);
    }
    for (const password of ['x'.repeat(15), '界'.repeat(24), '😀'.repeat(15)]) {
      expect(
        passwordSchema.safeParse({
          old_password: 'old',
          new_password: password,
          confirmation: password,
        }).success,
      ).toBe(true);
    }
  });
  it('trims usernames but preserves every password character', () => {
    const value = loginSchema.parse({ username: ' owner ', password: '  private phrase  ' });
    expect(value).toEqual({ username: 'owner', password: '  private phrase  ' });
    expect(loginSchema.safeParse({ username: 'owner', password: '界'.repeat(25) }).success).toBe(
      false,
    );
  });
  it('requires matching new passwords without sending a confirmation to the server', () => {
    const result = passwordSchema.safeParse({
      old_password: 'old',
      new_password: 'a sufficiently long password',
      confirmation: 'different',
    });
    expect(result.success).toBe(false);
    if (!result.success) expect(result.error.issues[0]?.path).toEqual(['confirmation']);
  });
  it('uses stable localized error keys instead of untrusted server details', () => {
    expect(errorMessage(new Error('a private path'))).toBe('auth.networkError');
    expect(errorMessage(new APIError('INVALID_CREDENTIALS'))).toBe('auth.invalidCredentials');
    expect(errorMessage(new APIError('RATE_LIMITED'))).toBe('auth.rateLimited');
    expect(errorMessage(new APIError('UNAUTHENTICATED'))).toBe('auth.sessionExpired');
    expect(errorMessage(new APIError('ORIGIN_REJECTED'))).toBe('auth.originRejected');
    expect(errorMessage(new APIError('unknown'))).toBe('auth.requestFailed');
  });
});
