import { useEffect, useRef } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { FormattedMessage, useIntl } from 'react-intl';
import { Link, NavLink } from 'react-router';
import { APIError } from '../auth/api';

export function LibraryNavigation() {
  const intl = useIntl();
  return (
    <nav
      className="library-navigation"
      aria-label={intl.formatMessage({ id: 'library.navigation' })}
    >
      <NavLink to="/albums">
        <FormattedMessage id="library.albums" />
      </NavLink>
      <NavLink to="/settings/security">
        <FormattedMessage id="library.account" />
      </NavLink>
    </nav>
  );
}

export function usePageFocus(cursor: string | null, resource = '') {
  const main = useRef<HTMLElement>(null);
  useEffect(() => {
    main.current?.focus();
  }, [cursor, resource]);
  return main;
}

export function useRevalidateSession(error: unknown) {
  const client = useQueryClient();
  useEffect(() => {
    if (error instanceof APIError && error.code === 'UNAUTHENTICATED') {
      void client.invalidateQueries({ queryKey: ['auth', 'me'] });
    }
  }, [client, error]);
}

export function GalleryLoading({ message }: { message: string }) {
  return (
    <div role="status">
      <p>
        <FormattedMessage id={message} />
      </p>
      <div className="gallery-grid" aria-hidden="true">
        {[0, 1, 2, 3].map((item) => (
          <div className="gallery-preview" key={item} />
        ))}
      </div>
    </div>
  );
}

export function PageError({
  error,
  onRetry,
  first,
}: {
  error: unknown;
  onRetry: () => void;
  first: string;
}) {
  const code = error instanceof APIError ? error.code : '';
  const invalid = code === 'INVALID_CURSOR' || code === 'INVALID_FILTER';
  return (
    <div className="gallery-message" role="alert">
      <p>
        <FormattedMessage
          id={
            invalid
              ? 'library.invalidPage'
              : code === 'NOT_FOUND'
                ? 'library.noAccess'
                : 'library.requestError'
          }
        />
      </p>
      {invalid || code === 'NOT_FOUND' ? (
        <Link className="button button-secondary" to={first}>
          <FormattedMessage id={invalid ? 'library.first' : 'library.backAlbums'} />
        </Link>
      ) : (
        <button className="button button-secondary" onClick={onRetry}>
          <FormattedMessage id="library.refresh" />
        </button>
      )}
    </div>
  );
}

export function PageNavigation({
  first,
  next,
  cursor,
  state,
}: {
  first: string;
  next: string | null;
  cursor: string | null;
  state?: { from: string };
}) {
  const intl = useIntl();
  if (!cursor && !next) return null;
  return (
    <nav
      className="gallery-pagination"
      aria-label={intl.formatMessage({ id: 'library.pagination' })}
    >
      {cursor && (
        <Link className="button button-secondary" to={first} state={state}>
          <FormattedMessage id="library.first" />
        </Link>
      )}
      {next && (
        <Link className="button button-secondary" to={next} state={state}>
          <FormattedMessage id="library.next" />
        </Link>
      )}
    </nav>
  );
}
