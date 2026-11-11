import { useQuery } from '@tanstack/react-query';
import { FormattedMessage } from 'react-intl';
import { Link, useLocation, useOutletContext, useParams, useSearchParams } from 'react-router';
import { APIError } from '../auth/api';
import type { User } from '../auth/api';
import { getPerson, listPersonAlbums, safeID } from './api';
import {
  GalleryLoading,
  LibraryNavigation,
  PageError,
  PageNavigation,
  peopleListBack,
  usePageFocus,
  useRevalidateSession,
  validCursorSearch,
} from './common';
import { Preview } from './Preview';

export function PersonPage() {
  const user = useOutletContext<User>();
  const { personId = '' } = useParams();
  const location = useLocation();
  const [search] = useSearchParams();
  const cursor = search.get('cursor');
  const back = peopleListBack((location.state as { from?: unknown } | null)?.from);
  const first = `/people/${encodeURIComponent(personId)}`;
  const validID = safeID(personId);
  const validSearch = validCursorSearch(search);
  const main = usePageFocus(cursor, personId);
  const person = useQuery({
    queryKey: [user.id, user.scope_id, 'person', personId],
    queryFn: ({ signal }) => getPerson(personId, signal),
    enabled: validID && validSearch,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    networkMode: 'always',
  });
  const albums = useQuery({
    queryKey: [user.id, user.scope_id, 'person-albums', personId, cursor],
    queryFn: ({ signal }) => listPersonAlbums(personId, cursor, signal),
    enabled: validID && validSearch,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    networkMode: 'always',
  });
  useRevalidateSession(person.error);
  useRevalidateSession(albums.error);
  const accessLost = [person.error, albums.error].find(
    (error): error is APIError =>
      error instanceof APIError && ['NOT_FOUND', 'UNAUTHENTICATED'].includes(error.code),
  );
  const loading = person.isPending || person.isFetching || albums.isPending || albums.isFetching;
  const error = !validID
    ? new APIError('NOT_FOUND')
    : !validSearch
      ? new APIError('INVALID_CURSOR')
      : accessLost || (!loading && (person.error || albums.error));
  const missing = error instanceof APIError && error.code === 'NOT_FOUND';
  const next =
    albums.data?.has_more && albums.data.next_cursor
      ? `${first}?${new URLSearchParams({ cursor: albums.data.next_cursor })}`
      : null;
  return (
    <main className="gallery-main" ref={main} tabIndex={-1} aria-labelledby="gallery-page-title">
      <LibraryNavigation />
      <Link className="gallery-back" to={back}>
        <FormattedMessage id="library.backPeople" />
      </Link>
      {error ? (
        <>
          <h1 id="gallery-page-title">
            <FormattedMessage id="library.person" />
          </h1>
          <PageError
            error={error}
            onRetry={() => {
              void person.refetch();
              void albums.refetch();
            }}
            first={missing ? back : first}
            state={{ from: back }}
            notFoundMessage="library.noPersonAccess"
            notFoundLabel="library.backPeople"
          />
        </>
      ) : loading || !person.data || !albums.data ? (
        <>
          <h1 id="gallery-page-title">
            <FormattedMessage id="library.person" />
          </h1>
          <GalleryLoading message="library.loadingPerson" />
        </>
      ) : (
        <>
          <div className="gallery-heading">
            <div>
              <h1 id="gallery-page-title">
                {person.data.name || <FormattedMessage id="library.noPersonName" />}
              </h1>
              <p>
                <FormattedMessage
                  id="library.albumCount"
                  values={{ count: person.data.album_count }}
                />
              </p>
            </div>
          </div>
          {albums.data.items.length === 0 ? (
            <div className="gallery-message">
              <p>
                <FormattedMessage id="library.noAlbums" />
              </p>
            </div>
          ) : (
            <ul className="gallery-grid">
              {albums.data.items.map((album) => (
                <li key={album.id}>
                  <Link
                    className="gallery-card"
                    to={`/albums/${encodeURIComponent(album.id)}`}
                    state={{ from: location.pathname + location.search, peopleFrom: back }}
                  >
                    <Preview
                      key={`${user.id}:${user.scope_id}:${album.cover_media_id}`}
                      mediaID={album.cover_media_id}
                      user={user}
                    />
                    <strong>{album.title || <FormattedMessage id="library.noTitle" />}</strong>
                    <span>
                      <FormattedMessage
                        id="library.mediaCount"
                        values={{ count: album.media_count }}
                      />
                    </span>
                  </Link>
                </li>
              ))}
            </ul>
          )}
          <PageNavigation first={first} next={next} cursor={cursor} state={{ from: back }} />
        </>
      )}
    </main>
  );
}
