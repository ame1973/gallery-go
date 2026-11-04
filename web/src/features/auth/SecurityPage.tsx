import { useRef, useState } from 'react';
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import * as AlertDialog from '@radix-ui/react-alert-dialog';
import { FormattedMessage, useIntl } from 'react-intl';
import { useNavigate, useOutletContext } from 'react-router';
import { Field } from '../../components/ui/field';
import { changePassword, errorMessage, listSessions, revokeSession, signOut } from './api';
import type { Session, User } from './api';
import { passwordSchema } from './forms';

export function SecurityPage() {
  const user = useOutletContext<User>();
  const intl = useIntl();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const logout = useMutation({
    mutationFn: signOut,
    onSuccess: async () => {
      await queryClient.cancelQueries();
      queryClient.clear();
      queryClient.setQueryData(['auth', 'me'], null);
      await navigate('/login', { replace: true });
    },
  });
  return (
    <main>
      <div className="page-heading">
        <div>
          <h1>
            <FormattedMessage id="auth.securityTitle" />
          </h1>
          <p className="intro">
            <FormattedMessage id="auth.signedInAs" values={{ username: user.username }} />
          </p>
        </div>
        <button
          className="button button-secondary"
          disabled={logout.isPending}
          onClick={() => logout.mutate()}
        >
          <FormattedMessage id={logout.isPending ? 'auth.signingOut' : 'auth.signOut'} />
        </button>
      </div>
      {logout.isError && (
        <p className="error-message" role="alert">
          {intl.formatMessage({ id: errorMessage(logout.error) })}
        </p>
      )}
      <p className="development-notice">
        <FormattedMessage id="auth.galleryPending" />
      </p>
      <div className="security-layout">
        <PasswordForm />
        <SessionList user={user} />
      </div>
    </main>
  );
}

function PasswordForm() {
  const intl = useIntl();
  const queryClient = useQueryClient();
  const form = useForm({ resolver: zodResolver(passwordSchema) });
  const mutation = useMutation({
    mutationFn: changePassword,
    onSuccess: async (user) => {
      form.reset();
      await queryClient.cancelQueries();
      queryClient.removeQueries({
        predicate: (query) => query.queryKey[0] !== 'auth' || query.queryKey[1] !== 'me',
      });
      queryClient.setQueryData(['auth', 'me'], user);
    },
  });
  const label = (id: string) => intl.formatMessage({ id });
  return (
    <section className="security-panel" aria-labelledby="password-heading">
      <h2 id="password-heading">
        <FormattedMessage id="auth.passwordTitle" />
      </h2>
      <p className="form-note">
        <FormattedMessage id="auth.passwordEffect" />
      </p>
      <form noValidate onSubmit={form.handleSubmit((input) => mutation.mutate(input))}>
        <Field
          id="current-password"
          label={label('auth.currentPassword')}
          type="password"
          autoComplete="current-password"
          {...form.register('old_password')}
          error={
            form.formState.errors.old_password?.message
              ? label(form.formState.errors.old_password.message)
              : undefined
          }
        />
        <Field
          id="new-password"
          label={label('auth.newPassword')}
          type="password"
          autoComplete="new-password"
          {...form.register('new_password')}
          error={
            form.formState.errors.new_password?.message
              ? label(form.formState.errors.new_password.message)
              : undefined
          }
        />
        <p className="form-note">
          <FormattedMessage id="auth.passwordRule" />
        </p>
        <Field
          id="confirm-password"
          label={label('auth.confirmPassword')}
          type="password"
          autoComplete="new-password"
          {...form.register('confirmation')}
          error={
            form.formState.errors.confirmation?.message
              ? label(form.formState.errors.confirmation.message)
              : undefined
          }
        />
        {mutation.isError && (
          <p className="error-message" role="alert">
            {label(errorMessage(mutation.error))}
          </p>
        )}
        {mutation.isSuccess && (
          <p role="status">
            <FormattedMessage id="auth.passwordSaved" />
          </p>
        )}
        <button className="button" disabled={mutation.isPending} type="submit">
          <FormattedMessage id={mutation.isPending ? 'auth.saving' : 'auth.savePassword'} />
        </button>
      </form>
    </section>
  );
}

function SessionList({ user }: { user: User }) {
  const intl = useIntl();
  const queryClient = useQueryClient();
  const [selected, setSelected] = useState<Session | null>(null);
  const trigger = useRef<HTMLButtonElement | null>(null);
  const heading = useRef<HTMLHeadingElement | null>(null);
  const queryKey = [user.scope_id, 'auth', 'sessions'];
  const sessions = useInfiniteQuery({
    queryKey,
    initialPageParam: null as string | null,
    queryFn: ({ pageParam, signal }) => listSessions(pageParam, signal),
    getNextPageParam: (page) => page.next_cursor ?? undefined,
    retry: false,
  });
  const revoke = useMutation({
    mutationFn: revokeSession,
    onSuccess: async () => {
      setSelected(null);
      await queryClient.invalidateQueries({ queryKey });
    },
  });
  const formatTime = (value: string) =>
    intl.formatDate(value, { dateStyle: 'medium', timeStyle: 'short' });
  return (
    <section className="security-panel" aria-labelledby="sessions-heading">
      <h2 id="sessions-heading" ref={heading} tabIndex={-1}>
        <FormattedMessage id="auth.sessionsTitle" />
      </h2>
      <p className="form-note">
        <FormattedMessage id="auth.sessionsDescription" />
      </p>
      {sessions.isPending && (
        <p role="status">
          <FormattedMessage id="auth.sessionsLoading" />
        </p>
      )}
      {sessions.isError && (
        <p role="alert">
          <FormattedMessage id="auth.networkError" />{' '}
          <button className="text-button" onClick={() => void sessions.refetch()}>
            <FormattedMessage id="retry" />
          </button>
        </p>
      )}
      <ul className="session-list">
        {sessions.data?.pages
          .flatMap((page) => page.items)
          .map((session) => (
            <li key={session.id}>
              <p className="session-agent">
                {session.user_agent || intl.formatMessage({ id: 'auth.unknownBrowser' })}
              </p>
              <p className="form-note">{session.ip_address}</p>
              <p className="form-note">
                <FormattedMessage
                  id="auth.lastSeen"
                  values={{ time: formatTime(session.last_seen_at) }}
                />
              </p>
              <p className="form-note">
                <FormattedMessage
                  id="auth.expires"
                  values={{ time: formatTime(session.expires_at) }}
                />
              </p>
              {session.current ? (
                <strong className="current-session">
                  <FormattedMessage id="auth.thisSession" />
                </strong>
              ) : (
                <button
                  className="button button-secondary"
                  onClick={(event) => {
                    trigger.current = event.currentTarget;
                    revoke.reset();
                    setSelected(session);
                  }}
                >
                  <FormattedMessage id="auth.revoke" />
                </button>
              )}
            </li>
          ))}
      </ul>
      {sessions.data?.pages[0]?.items.length === 0 && (
        <p>
          <FormattedMessage id="auth.emptySessions" />
        </p>
      )}
      {revoke.isSuccess && (
        <p role="status">
          <FormattedMessage id="auth.revoked" />
        </p>
      )}
      {sessions.hasNextPage && (
        <button
          className="button button-secondary"
          disabled={sessions.isFetchingNextPage}
          onClick={() => void sessions.fetchNextPage()}
        >
          <FormattedMessage id="auth.loadMore" />
        </button>
      )}
      <AlertDialog.Root
        open={selected !== null}
        onOpenChange={(open) => {
          if (!open && !revoke.isPending) setSelected(null);
        }}
      >
        <AlertDialog.Portal>
          <AlertDialog.Overlay className="dialog-overlay" />
          <AlertDialog.Content
            className="dialog-content"
            onCloseAutoFocus={(event) => {
              event.preventDefault();
              if (trigger.current?.isConnected) trigger.current.focus();
              else heading.current?.focus();
            }}
          >
            <AlertDialog.Title>
              <FormattedMessage id="auth.revokeTitle" />
            </AlertDialog.Title>
            <AlertDialog.Description>
              <FormattedMessage id="auth.revokeDescription" />
            </AlertDialog.Description>
            {revoke.isError && (
              <p role="alert" className="error-message">
                {intl.formatMessage({ id: errorMessage(revoke.error) })}
              </p>
            )}
            <div className="dialog-actions">
              <AlertDialog.Cancel asChild>
                <button className="button button-secondary" disabled={revoke.isPending}>
                  <FormattedMessage id="auth.cancel" />
                </button>
              </AlertDialog.Cancel>
              <button
                className="button"
                disabled={revoke.isPending}
                onClick={() => {
                  if (selected) revoke.mutate(selected.id);
                }}
              >
                <FormattedMessage id={revoke.isPending ? 'auth.revoking' : 'auth.revoke'} />
              </button>
            </div>
          </AlertDialog.Content>
        </AlertDialog.Portal>
      </AlertDialog.Root>
    </section>
  );
}
