import { useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { FormattedMessage, useIntl } from 'react-intl';
import { Link, useLocation, useOutletContext, useParams, useSearchParams } from 'react-router';
import { APIError } from '../auth/api';
import type { User } from '../auth/api';
import { getMedia, safeID } from './api';
import type { Media } from './api';
import {
  LibraryNavigation,
  PageError,
  albumDetailBack,
  albumListBack,
  peopleListBack,
  personDetailBack,
  usePageFocus,
  useRevalidateSession,
} from './common';
import { Preview } from './Preview';

export function MediaPage() {
  const user = useOutletContext<User>();
  const { mediaId = '' } = useParams();
  const { search } = useLocation();
  return (
    <MediaPageContent
      key={JSON.stringify([user.id, user.scope_id, mediaId, search])}
      user={user}
      mediaId={mediaId}
    />
  );
}

function MediaPageContent({ user, mediaId }: { user: User; mediaId: string }) {
  const location = useLocation();
  const [search] = useSearchParams();
  const origin = location.state as {
    from?: unknown;
    albumFrom?: unknown;
    peopleFrom?: unknown;
  } | null;
  const candidate = albumDetailBack(origin?.from);
  const personBack = personDetailBack(origin?.albumFrom);
  const parent = personBack ?? albumListBack(origin?.albumFrom);
  const peopleBack = peopleListBack(origin?.peopleFrom);
  const validID = safeID(mediaId);
  const validSearch = [...search.keys()].length === 0;
  const identity = JSON.stringify([user.id, user.scope_id, mediaId]);
  const [previewDenial, setPreviewDenial] = useState<APIError | null>(null);
  const failedPreview = useRef<string | null>(null);
  const main = usePageFocus(null, mediaId);
  const query = useQuery({
    queryKey: [user.id, user.scope_id, 'media', mediaId],
    queryFn: ({ signal }) => getMedia(mediaId, signal),
    enabled: validID && validSearch,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    networkMode: 'always',
  });
  useRevalidateSession(query.error);
  useRevalidateSession(previewDenial);

  const retained = candidate && (!query.isSuccess || candidate.id === query.data.album_id);
  const currentAlbum =
    validID && validSearch && query.isSuccess && !query.isFetching && !previewDenial
      ? query.data.album_id
      : null;
  const back = retained
    ? candidate.path
    : currentAlbum
      ? `/albums/${encodeURIComponent(currentAlbum)}`
      : '/albums';
  const backLabel = retained || currentAlbum ? 'media.backAlbum' : 'library.backAlbums';
  const parentState = retained
    ? { from: parent, ...(personBack && { peopleFrom: peopleBack }) }
    : undefined;
  const detailState = candidate
    ? { from: candidate.path, albumFrom: parent, ...(personBack && { peopleFrom: peopleBack }) }
    : undefined;
  const error = !validID
    ? new APIError('NOT_FOUND')
    : !validSearch
      ? new APIError('INVALID_FILTER')
      : previewDenial || (query.isError ? query.error : null);
  const invalidSearch = validID && !validSearch;
  const recoveryState = invalidSearch ? detailState : parentState;
  const intl = useIntl();
  return (
    <main className="gallery-main" ref={main} tabIndex={-1} aria-labelledby="gallery-page-title">
      <LibraryNavigation />
      <Link className="gallery-back" to={back} state={parentState}>
        <FormattedMessage id={backLabel} />
      </Link>
      {error ? (
        <>
          <h1 id="gallery-page-title">
            <FormattedMessage id="media.title" />
          </h1>
          <PageError
            error={error}
            onRetry={() => {
              main.current?.focus();
              setPreviewDenial(null);
              void query.refetch();
            }}
            first={invalidSearch ? `/inspect/${encodeURIComponent(mediaId)}/details` : back}
            {...(recoveryState && { state: recoveryState })}
            notFoundMessage="media.noAccess"
            notFoundLabel={backLabel}
          />
        </>
      ) : query.isPending || query.isFetching || !query.data ? (
        <>
          <h1 id="gallery-page-title">
            <FormattedMessage id="media.title" />
          </h1>
          <div role="status">
            <p>
              <FormattedMessage id="media.loading" />
            </p>
            <div className="media-detail" aria-hidden="true">
              <div className="gallery-preview" />
            </div>
          </div>
        </>
      ) : (
        <>
          <div className="gallery-heading">
            <div>
              <h1 id="gallery-page-title">{query.data.file_name}</h1>
              <p>
                <FormattedMessage
                  id={query.data.kind === 'video' ? 'library.video' : 'library.image'}
                />
              </p>
            </div>
          </div>
          <div className="media-detail">
            <section className="media-detail-preview" aria-labelledby="media-preview-title">
              <h2 id="media-preview-title">
                <FormattedMessage id="media.preview" />
              </h2>
              <Preview
                key={`${identity}:${query.data.revision}`}
                mediaID={mediaId}
                user={user}
                revision={query.data.revision}
                alt={intl.formatMessage({ id: 'media.previewAlt' }, { name: query.data.file_name })}
                onAccessLost={setPreviewDenial}
                onImageLoad={() => {
                  failedPreview.current = null;
                }}
                onImageError={(url) => {
                  // Recheck authorization once per failed URL until a preview loads.
                  if (failedPreview.current !== url) {
                    failedPreview.current = url;
                    void query.refetch();
                  }
                }}
              />
              <p>
                <FormattedMessage id="media.previewNotice" />
              </p>
            </section>
            <MediaDetails media={query.data} />
          </div>
        </>
      )}
    </main>
  );
}

function MediaDetails({ media }: { media: Media }) {
  const intl = useIntl();
  const unknown = <FormattedMessage id="media.unknown" />;
  const instant = media.capture_at === null ? null : new Date(media.capture_at);
  const captured =
    instant && Number.isFinite(instant.getTime()) ? (
      <time dateTime={media.capture_at!}>
        <FormattedMessage
          id="media.utc"
          values={{
            time: intl.formatDate(instant, {
              year: 'numeric',
              month: 'short',
              day: 'numeric',
              hour: '2-digit',
              minute: '2-digit',
              second: '2-digit',
              fractionalSecondDigits: 3,
              timeZone: 'UTC',
            }),
          }}
        />
      </time>
    ) : (
      unknown
    );
  const duration =
    media.duration_ms === null ? (
      unknown
    ) : (
      <FormattedMessage id="media.seconds" values={{ seconds: media.duration_ms / 1000 }} />
    );
  return (
    <section aria-labelledby="media-details-title">
      <h2 id="media-details-title">
        <FormattedMessage id="media.details" />
      </h2>
      <dl className="media-metadata">
        <Detail label="media.dimensions">
          {media.width === null || media.height === null ? (
            unknown
          ) : (
            <FormattedMessage
              id="media.dimensionsValue"
              values={{
                width: intl.formatNumber(media.width),
                height: intl.formatNumber(media.height),
              }}
            />
          )}
        </Detail>
        <Detail label="media.codec">{media.codec ?? unknown}</Detail>
        <Detail label="media.captured">{captured}</Detail>
        <Detail label="media.orientation">
          {media.orientation === null ? unknown : intl.formatNumber(media.orientation)}
        </Detail>
        {media.kind === 'image' && (
          <>
            <Detail label="media.animated">
              {media.animated === null ? (
                unknown
              ) : (
                <FormattedMessage id={media.animated ? 'media.yes' : 'media.no'} />
              )}
            </Detail>
            <Detail label="media.frames">
              {media.frame_count === null ? unknown : intl.formatNumber(media.frame_count)}
            </Detail>
            <Detail label="media.plays">
              {media.loop_count === null ? (
                unknown
              ) : media.loop_count === 0 ? (
                <FormattedMessage id="media.infinite" />
              ) : (
                intl.formatNumber(media.loop_count)
              )}
            </Detail>
          </>
        )}
        <Detail label={media.kind === 'video' ? 'media.duration' : 'media.cycleDuration'}>
          {duration}
        </Detail>
      </dl>
    </section>
  );
}

function Detail({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div>
      <dt>
        <FormattedMessage id={label} />
      </dt>
      <dd>{children}</dd>
    </div>
  );
}
