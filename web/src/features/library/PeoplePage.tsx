import { useQuery } from '@tanstack/react-query';
import { FormattedMessage } from 'react-intl';
import { Link, useLocation, useOutletContext, useSearchParams } from 'react-router';
import { APIError } from '../auth/api';
import type { User } from '../auth/api';
import { listPeople } from './api';
import {
  GalleryLoading,
  LibraryNavigation,
  PageError,
  PageNavigation,
  usePageFocus,
  useRevalidateSession,
  validCursorSearch,
} from './common';
import { Preview } from './Preview';

export function PeoplePage() {
  const user = useOutletContext<User>();
  const location = useLocation();
  const [search] = useSearchParams();
  const cursor = search.get('cursor');
  const valid = validCursorSearch(search);
  const main = usePageFocus(cursor);
  const query = useQuery({
    queryKey: [user.id, user.scope_id, 'people', cursor],
    queryFn: ({ signal }) => listPeople(cursor, signal),
    enabled: valid,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    networkMode: 'always',
  });
  useRevalidateSession(query.error);
  const next =
    query.data?.has_more && query.data.next_cursor
      ? `/people?${new URLSearchParams({ cursor: query.data.next_cursor })}`
      : null;
  return (
    <main className="gallery-main" ref={main} tabIndex={-1} aria-labelledby="gallery-page-title">
      <LibraryNavigation />
      <div className="gallery-heading">
        <div>
          <h1 id="gallery-page-title">
            <FormattedMessage id="library.people" />
          </h1>
          <p>
            <FormattedMessage id="library.peopleDescription" />
          </p>
        </div>
      </div>
      {!valid ? (
        <PageError error={new APIError('INVALID_CURSOR')} onRetry={() => {}} first="/people" />
      ) : query.isPending || query.isFetching ? (
        <GalleryLoading message="library.loadingPeople" />
      ) : query.isError ? (
        <PageError
          error={query.error}
          onRetry={() => void query.refetch()}
          first="/people"
          notFoundMessage="library.noPersonAccess"
          notFoundLabel="library.backPeople"
        />
      ) : (
        <>
          {query.data.items.length === 0 ? (
            <div className="gallery-message">
              <p>
                <FormattedMessage id="library.noPeople" />
              </p>
              {!cursor && (
                <p>
                  <FormattedMessage id="library.noPeopleHelp" />
                </p>
              )}
            </div>
          ) : (
            <ul className="gallery-grid">
              {query.data.items.map((person) => (
                <li key={person.id}>
                  <Link
                    className="gallery-card"
                    to={`/people/${encodeURIComponent(person.id)}`}
                    state={{ from: location.pathname + location.search }}
                  >
                    <Preview
                      key={`${user.id}:${user.scope_id}:${person.cover_media_id}`}
                      mediaID={person.cover_media_id}
                      user={user}
                    />
                    <strong>{person.name || <FormattedMessage id="library.noPersonName" />}</strong>
                    <span>
                      <FormattedMessage
                        id="library.albumCount"
                        values={{ count: person.album_count }}
                      />
                    </span>
                  </Link>
                </li>
              ))}
            </ul>
          )}
          <PageNavigation first="/people" next={next} cursor={cursor} />
        </>
      )}
    </main>
  );
}
