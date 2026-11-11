import { useQuery } from '@tanstack/react-query';
import { FormattedMessage, useIntl } from 'react-intl';
import { Link, useLocation, useOutletContext, useSearchParams } from 'react-router';
import { APIError } from '../auth/api';
import type { User } from '../auth/api';
import { listAlbums } from './api';
import type { AlbumSort } from './api';
import {
  GalleryLoading,
  LibraryNavigation,
  PageError,
  PageNavigation,
  usePageFocus,
  useRevalidateSession,
} from './common';
import { Preview } from './Preview';

const sorts: { value: AlbumSort | ''; label: string }[] = [
  { value: '', label: 'library.sortId' },
  { value: 'latest', label: 'library.sortLatest' },
  { value: 'oldest', label: 'library.sortOldest' },
  { value: 'name_asc', label: 'library.sortNameAsc' },
  { value: 'name_desc', label: 'library.sortNameDesc' },
  { value: 'popular', label: 'library.sortPopular' },
  { value: 'top_rated', label: 'library.sortRating' },
];

export function AlbumsPage() {
  const user = useOutletContext<User>();
  const intl = useIntl();
  const location = useLocation();
  const [search, setSearch] = useSearchParams();
  const sort = search.get('sort') ?? '';
  const cursor = search.get('cursor');
  const main = usePageFocus(cursor);
  const valid =
    [...search.keys()].every((key) => key === 'sort' || key === 'cursor') &&
    sorts.some((option) => option.value === sort) &&
    search.getAll('sort').length <= 1 &&
    (!search.has('sort') || sort !== '') &&
    search.getAll('cursor').length <= 1 &&
    (cursor === null || (cursor.length > 0 && cursor.length <= 8192));
  const selected = sorts.find((option) => option.value === sort)?.value ?? '';
  const query = useQuery({
    queryKey: [user.id, user.scope_id, 'albums', selected, cursor],
    queryFn: ({ signal }) => listAlbums(selected, cursor, signal),
    enabled: valid,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    networkMode: 'always',
  });
  useRevalidateSession(query.error);
  const firstParams = new URLSearchParams();
  if (selected) firstParams.set('sort', selected);
  const first = `/albums${firstParams.size ? `?${firstParams}` : ''}`;
  const nextParams = new URLSearchParams(firstParams);
  if (query.data?.next_cursor) nextParams.set('cursor', query.data.next_cursor);
  const next = query.data?.has_more && query.data.next_cursor ? `/albums?${nextParams}` : null;
  return (
    <main className="gallery-main" ref={main} tabIndex={-1} aria-labelledby="gallery-page-title">
      <LibraryNavigation />
      <div className="gallery-heading">
        <div>
          <h1 id="gallery-page-title">
            <FormattedMessage id="library.albums" />
          </h1>
          <p>
            <FormattedMessage id="library.description" />
          </p>
        </div>
        <label className="gallery-sort">
          <span>
            <FormattedMessage id="library.sort" />
          </span>
          <select
            value={selected}
            onChange={(event) => {
              const value =
                sorts.find((option) => option.value === event.target.value)?.value ?? '';
              setSearch(value ? { sort: value } : {});
            }}
          >
            {sorts.map((option) => (
              <option key={option.value} value={option.value}>
                {intl.formatMessage({ id: option.label })}
              </option>
            ))}
          </select>
        </label>
      </div>
      {!valid ? (
        <PageError error={new APIError('INVALID_CURSOR')} onRetry={() => {}} first="/albums" />
      ) : query.isPending || query.isFetching ? (
        <GalleryLoading message="library.loading" />
      ) : query.isError ? (
        <PageError error={query.error} onRetry={() => void query.refetch()} first={first} />
      ) : (
        <>
          {query.data.items.length === 0 ? (
            <div className="gallery-message">
              <p>
                <FormattedMessage id="library.noAlbums" />
              </p>
              {!cursor && (
                <p>
                  <FormattedMessage id="library.noAlbumsHelp" />
                </p>
              )}
            </div>
          ) : (
            <ul className="gallery-grid">
              {query.data.items.map((album) => (
                <li key={album.id}>
                  <Link
                    className="gallery-card"
                    to={`/albums/${encodeURIComponent(album.id)}`}
                    state={{ from: location.pathname + location.search }}
                  >
                    <Preview
                      key={`${user.scope_id}:${album.cover_media_id}`}
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
          <PageNavigation first={first} next={next} cursor={cursor} />
        </>
      )}
    </main>
  );
}
