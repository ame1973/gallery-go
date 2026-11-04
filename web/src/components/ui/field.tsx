import type { ComponentProps } from 'react';

export function Field({
  label,
  error,
  ...props
}: ComponentProps<'input'> & { label: string; error?: string | undefined }) {
  const errorId = `${props.id}-error`;
  return (
    <div className="form-field">
      <label htmlFor={props.id}>{label}</label>
      <input
        {...props}
        aria-invalid={Boolean(error)}
        aria-describedby={error ? errorId : undefined}
      />
      {error && (
        <p className="error-message" id={errorId} role="alert">
          {error}
        </p>
      )}
    </div>
  );
}
