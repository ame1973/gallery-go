import { useQuery } from '@tanstack/react-query';
import { FormattedMessage } from 'react-intl';
import { Link, useLocation, useOutletContext, useParams, useSearchParams } from 'react-router';
import { APIError } from '../auth/api';
import type { User } from '../auth/api';
import { getAlbum, listMedia } from './api';
import {
  GalleryLoading,
  LibraryNavigation,
  PageError,
  PageNavigation,
  usePageFocus,
  useRevalidateSession,
} from './common';
import { Preview } from './Preview';

export function AlbumPage() {
  const user = useOutletContext<User>();
  const { albumId = '' } = useParams();
  const [search] = useSearchParams();
  const location = useLocation();
  const from: unknown = (location.state as { from?: unknown } | null)?.from;
  const back =
    typeof from === 'string' && (from === '/albums' || from.startsWith('/albums?'))
      ? from
      : '/albums';
  const cursor = search.get('cursor');
  const main = usePageFocus(cursor, albumId);
  const valid =
    [...search.keys()].every((key) => key === 'cursor') &&
    search.getAll('cursor').length <= 1 &&
    (cursor === null || (cursor.length > 0 && cursor.length <= 512));
  const album = useQuery({
    queryKey: [user.id, user.scope_id, 'album', albumId],
    queryFn: ({ signal }) => getAlbum(albumId, signal),
    staleTime: 0,
    gcTime: 0,
    retry: false,
    networkMode: 'always',
  });
  const media = useQuery({
    queryKey: [user.id, user.scope_id, 'album-media', albumId, cursor],
    queryFn: ({ signal }) => listMedia(albumId, cursor, signal),
    enabled: valid,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    networkMode: 'always',
  });
  useRevalidateSession(album.error);
  useRevalidateSession(media.error);
  const accessLost =
    media.error instanceof APIError && ['NOT_FOUND', 'UNAUTHENTICATED'].includes(media.error.code);
  const first = `/albums/${encodeURIComponent(albumId)}`;
  const next =
    media.data?.has_more && media.data.next_cursor
      ? `${first}?${new URLSearchParams({ cursor: media.data.next_cursor })}`
      : null;
  return (
    <main className="gallery-main" ref={main} tabIndex={-1} aria-labelledby="gallery-page-title">
      <LibraryNavigation />
      <Link className="gallery-back" to={back}>
        <FormattedMessage id="library.backAlbums" />
      </Link>
      {album.isPending || album.isFetching ? (
        <>
          <h1 id="gallery-page-title">
            <FormattedMessage id="library.album" />
          </h1>
          <GalleryLoading message="library.loading" />
        </>
      ) : album.isError ? (
        <>
          <h1 id="gallery-page-title">
            <FormattedMessage id="library.album" />
          </h1>
          <PageError error={album.error} onRetry={() => void album.refetch()} first="/albums" />
        </>
      ) : accessLost ? (
        <>
          <h1 id="gallery-page-title">
            <FormattedMessage id="library.album" />
          </h1>
          <PageError error={media.error} onRetry={() => void media.refetch()} first="/albums" />
        </>
      ) : (
        <>
          <div className="gallery-heading">
            <div>
              <h1 id="gallery-page-title">
                {album.data.title || <FormattedMessage id="library.noTitle" />}
              </h1>
              <p>
                <FormattedMessage
                  id="library.mediaCount"
                  values={{ count: album.data.media_count }}
                />
              </p>
            </div>
          </div>
          <p className="gallery-note">
            <FormattedMessage id="library.offlineNotice" />
          </p>
          {!valid ? (
            <PageError error={new APIError('INVALID_CURSOR')} onRetry={() => {}} first={first} />
          ) : media.isPending || media.isFetching ? (
            <GalleryLoading message="library.loadingMedia" />
          ) : media.isError ? (
            <PageError error={media.error} onRetry={() => void media.refetch()} first={first} />
          ) : (
            <>
              {media.data.items.length === 0 ? (
                <div className="gallery-message">
                  <p>
                    <FormattedMessage id="library.noMedia" />
                  </p>
                </div>
              ) : (
                <ul className="gallery-grid">
                  {media.data.items.map((item) => (
                    <li key={item.id}>
                      <figure className="gallery-card gallery-media">
                        <Preview
                          key={`${user.scope_id}:${item.id}`}
                          mediaID={item.id}
                          user={user}
                        />
                        <figcaption>
                          <strong>{item.file_name}</strong>
                          <span>
                            <FormattedMessage
                              id={item.kind === 'video' ? 'library.video' : 'library.image'}
                            />
                          </span>
                        </figcaption>
                      </figure>
                    </li>
                  ))}
                </ul>
              )}
              <PageNavigation first={first} next={next} cursor={cursor} state={{ from: back }} />
            </>
          )}
        </>
      )}
    </main>
  );
}
