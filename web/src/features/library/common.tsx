import { useEffect, useRef } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { FormattedMessage, useIntl } from 'react-intl';
import { Link, NavLink } from 'react-router';
import { APIError } from '../auth/api';
import { safeID } from './api';

export function validCursorSearch(search: URLSearchParams) {
  const cursor = search.get('cursor');
  return (
    [...search.keys()].every((key) => key === 'cursor') &&
    search.getAll('cursor').length <= 1 &&
    (cursor === null || (cursor.length > 0 && cursor.length <= 512))
  );
}

export function peopleListBack(from: unknown) {
  if (typeof from !== 'string' || from.includes('#')) return '/people';
  const question = from.indexOf('?');
  const path = question < 0 ? from : from.slice(0, question);
  const search = new URLSearchParams(question < 0 ? '' : from.slice(question + 1));
  return path === '/people' && validCursorSearch(search) ? from : '/people';
}

export function personDetailBack(from: unknown) {
  if (typeof from !== 'string' || from.includes('#')) return null;
  const question = from.indexOf('?');
  const path = question < 0 ? from : from.slice(0, question);
  const segment = /^\/people\/([^/]+)$/.exec(path)?.[1];
  if (!segment) return null;
  try {
    const id = decodeURIComponent(segment);
    if (!safeID(id) || encodeURIComponent(id) !== segment) return null;
  } catch {
    return null;
  }
  const search = new URLSearchParams(question < 0 ? '' : from.slice(question + 1));
  return validCursorSearch(search) ? from : null;
}

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
      <NavLink to="/people">
        <FormattedMessage id="library.people" />
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
  state,
  notFoundMessage = 'library.noAccess',
  notFoundLabel = 'library.backAlbums',
}: {
  error: unknown;
  onRetry: () => void;
  first: string;
  state?: { from: string; peopleFrom?: string };
  notFoundMessage?: string;
  notFoundLabel?: string;
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
                ? notFoundMessage
                : 'library.requestError'
          }
        />
      </p>
      {invalid || code === 'NOT_FOUND' ? (
        <Link className="button button-secondary" to={first} state={state}>
          <FormattedMessage id={invalid ? 'library.first' : notFoundLabel} />
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
  state?: { from: string; peopleFrom?: string };
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
