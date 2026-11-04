import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { FormattedMessage, useIntl } from 'react-intl';
import { Link, useNavigate } from 'react-router';
import { Field } from '../../components/ui/field';
import { errorMessage, signIn } from './api';
import { loginSchema } from './forms';

export function LoginPage() {
  const intl = useIntl();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const form = useForm({ resolver: zodResolver(loginSchema) });
  const login = useMutation({
    mutationFn: signIn,
    onSuccess: async (user) => {
      // Private query data never survives an account change.
      await queryClient.cancelQueries();
      queryClient.clear();
      queryClient.setQueryData(['auth', 'me'], user);
      await navigate('/settings/security', { replace: true });
    },
  });
  const label = (id: string) => intl.formatMessage({ id });

  return (
    <main className="auth-layout">
      <section aria-labelledby="login-heading" className="auth-form-panel">
        <h1 id="login-heading">
          <FormattedMessage id="auth.signIn" />
        </h1>
        <p className="intro">
          <FormattedMessage id="auth.signInDescription" />
        </p>
        <form noValidate onSubmit={form.handleSubmit((input) => login.mutate(input))}>
          <Field
            id="username"
            label={label('auth.username')}
            autoComplete="username"
            autoCapitalize="none"
            spellCheck={false}
            {...form.register('username')}
            error={
              form.formState.errors.username?.message
                ? label(form.formState.errors.username.message)
                : undefined
            }
          />
          <Field
            id="password"
            label={label('auth.password')}
            type="password"
            autoComplete="current-password"
            {...form.register('password')}
            error={
              form.formState.errors.password?.message
                ? label(form.formState.errors.password.message)
                : undefined
            }
          />
          {login.isError && (
            <p className="error-message" role="alert">
              {label(errorMessage(login.error))}
            </p>
          )}
          <button className="button" disabled={login.isPending} type="submit">
            <FormattedMessage id={login.isPending ? 'auth.signingIn' : 'auth.signIn'} />
          </button>
        </form>
        <p className="form-note">
          <FormattedMessage id="auth.httpsNote" />
        </p>
      </section>
      <aside className="build-notes">
        <h2>
          <FormattedMessage id="auth.bootstrapTitle" />
        </h2>
        <p>
          <FormattedMessage id="auth.bootstrapNote" />
        </p>
        <code className="setup-command">
          gallerygo bootstrap-admin --config config.yaml --username owner
        </code>
        <Link to="/status" className="text-link">
          <FormattedMessage id="auth.statusLink" />
        </Link>
      </aside>
    </main>
  );
}
