import { z } from 'zod';

// Keep form validation compatible with the application's no-eval CSP.
z.config({ jitless: true });

const passwordBytes = (value: string) => new TextEncoder().encode(value).byteLength;
const existingPassword = z
  .string()
  .min(1, 'auth.required')
  .refine((value) => passwordBytes(value) <= 72, 'auth.passwordRule');

export const loginSchema = z.object({
  username: z.string().trim().min(1, 'auth.required').max(64, 'auth.usernameRule'),
  password: existingPassword,
});

export const passwordSchema = z
  .object({
    old_password: existingPassword,
    new_password: z
      .string()
      .refine(
        (value) => [...value].length >= 15 && passwordBytes(value) <= 72,
        'auth.passwordRule',
      ),
    confirmation: z.string(),
  })
  .refine((value) => value.new_password === value.confirmation, {
    message: 'auth.passwordMismatch',
    path: ['confirmation'],
  });
