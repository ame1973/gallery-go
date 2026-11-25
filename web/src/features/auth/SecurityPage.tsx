import { useRef, useState } from 'react';
import type { RefObject } from 'react';
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import type { UseMutationResult } from '@tanstack/react-query';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import * as AlertDialog from '@radix-ui/react-alert-dialog';
import { FormattedMessage, useIntl } from 'react-intl';
import { useNavigate, useOutletContext } from 'react-router';
import { LibraryNavigation, useRevalidateReadIdentity } from '../library/common';
import { Field } from '../../components/ui/field';
import { validReadActor } from '../../lib/api/read-identity';
import { APIError, changePassword, errorMessage, revokeSession, signOut } from './api';
import type { Session, User } from './api';
import { listSessions } from './session-read';
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
      <LibraryNavigation />
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
        <SessionList key={JSON.stringify([user.id, user.scope_id])} user={user} />
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
  const queryClient = useQueryClient();
  const heading = useRef<HTMLHeadingElement | null>(null);
  const queryKey = [user.id, user.scope_id, 'auth', 'sessions'];
  const canRead = user.active && validReadActor(user);
  const sessions = useInfiniteQuery({
    queryKey,
    initialPageParam: null as string | null,
    queryFn: ({ pageParam, signal }) => listSessions(user, pageParam, signal),
    getNextPageParam: (page) => page.next_cursor ?? undefined,
    enabled: canRead,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    networkMode: 'always',
    refetchOnReconnect: true,
  });
  useRevalidateReadIdentity(sessions.error);
  const revoke = useMutation({
    mutationFn: revokeSession,
    onSettled: async () => {
      await queryClient.invalidateQueries({ queryKey });
    },
  });
  useRevalidateReadIdentity(revoke.error);
  const identityError =
    sessions.error instanceof APIError &&
    ['VERSION_CONFLICT', 'READ_CONTEXT_MISMATCH', 'FORBIDDEN'].includes(sessions.error.code);
  const errorMessageID = identityError
    ? 'auth.readContextChanged'
    : sessions.error instanceof APIError && sessions.error.code === 'UNAUTHENTICATED'
      ? 'auth.sessionExpired'
      : 'auth.networkError';
  return (
    <section className="security-panel" aria-labelledby="sessions-heading">
      <h2 id="sessions-heading" ref={heading} tabIndex={-1}>
        <FormattedMessage id="auth.sessionsTitle" />
      </h2>
      <p className="form-note">
        <FormattedMessage id="auth.sessionsDescription" />
      </p>
      {!canRead ? (
        <p role="alert">
          <FormattedMessage id="auth.readContextChanged" />{' '}
          <button
            className="button button-secondary"
            onClick={() => void queryClient.invalidateQueries({ queryKey: ['auth', 'me'] })}
          >
            <FormattedMessage id="retry" />
          </button>
        </p>
      ) : revoke.isPending ? (
        <p role="status">
          <FormattedMessage id="auth.revoking" />
        </p>
      ) : sessions.isPending || sessions.isFetching ? (
        <p role="status">
          <FormattedMessage id="auth.sessionsLoading" />
        </p>
      ) : sessions.isError ? (
        <p role="alert">
          <FormattedMessage id={errorMessageID} />{' '}
          <button className="button button-secondary" onClick={() => void sessions.refetch()}>
            <FormattedMessage id="retry" />
          </button>
        </p>
      ) : (
        <>
          <SessionRows
            items={sessions.data.pages.flatMap((page) => page.items)}
            heading={heading}
            revoke={revoke}
          />
          {sessions.data.pages[0]?.items.length === 0 && (
            <p>
              <FormattedMessage id="auth.emptySessions" />
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
        </>
      )}
      {revoke.isError && (
        <p role="alert" className="error-message">
          <FormattedMessage id={errorMessage(revoke.error)} />
        </p>
      )}
      {revoke.isSuccess && (
        <p role="status">
          <FormattedMessage id="auth.revoked" />
        </p>
      )}
    </section>
  );
}

function SessionRows({
  items,
  heading,
  revoke,
}: {
  items: Session[];
  heading: RefObject<HTMLHeadingElement | null>;
  revoke: UseMutationResult<void, Error, string, unknown>;
}) {
  const intl = useIntl();
  const [selected, setSelected] = useState<Session | null>(null);
  const trigger = useRef<HTMLButtonElement | null>(null);
  const formatTime = (value: string) =>
    intl.formatDate(value, { dateStyle: 'medium', timeStyle: 'short' });
  return (
    <>
      <ul className="session-list">
        {items.map((session) => (
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
    </>
  );
}
