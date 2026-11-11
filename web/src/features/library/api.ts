import { api } from '../../lib/api/client';
import type { components, operations } from '../../lib/api/schema';
import { APIError, securityHeaders } from '../auth/api';

type AlbumQuery = NonNullable<operations['listAlbums']['parameters']['query']>;
export type AlbumSort = NonNullable<AlbumQuery['sort']>;
export type Album = components['schemas']['Album'];
export type Media = components['schemas']['Media'];
type Variant = components['schemas']['Variant'];

type Failure = { response: Response; error?: components['schemas']['Problem'] };

function failure(result: Failure): never {
  const code =
    result.response.status === 401
      ? 'UNAUTHENTICATED'
      : result.response.status === 404
        ? 'NOT_FOUND'
        : result.error?.code;
  throw new APIError(code ?? 'INTERNAL_ERROR', result.error?.request_id);
}

export async function listAlbums(sort: AlbumSort | '', cursor: string | null, signal: AbortSignal) {
  const result = await api.GET('/albums', {
    signal,
    params: { query: { limit: 30, ...(sort && { sort }), ...(cursor && { cursor }) } },
  });
  if (!result.data) return failure(result);
  return result.data;
}

export async function getAlbum(id: string, signal: AbortSignal) {
  const result = await api.GET('/albums/{album_id}', {
    signal,
    params: { path: { album_id: id } },
  });
  if (!result.data) return failure(result);
  return result.data;
}

export type RatingSnapshot = { value: components['schemas']['AlbumRating']; etag: string };
export type RatingCommand = { albumID: string; rating: number | null; etag: string; key: string };

export async function getRating(albumID: string, signal: AbortSignal): Promise<RatingSnapshot> {
  const result = await api.GET('/albums/{album_id}/rating', {
    signal,
    params: { path: { album_id: albumID } },
  });
  if (!result.data) return failure(result);
  const etag = result.response.headers.get('ETag');
  if (
    !etag?.startsWith('"') ||
    !etag.endsWith('"') ||
    etag.indexOf('"', 1) !== etag.length - 1 ||
    result.data.album_id !== albumID ||
    (result.data.rating !== null &&
      (!Number.isInteger(result.data.rating) || result.data.rating < 1 || result.data.rating > 5))
  ) {
    throw new APIError('INVALID_RATING_RESPONSE');
  }
  return { value: result.data, etag };
}

export async function saveRating(command: RatingCommand, signal: AbortSignal) {
  const header = {
    ...(await securityHeaders(signal)),
    'If-Match': command.etag,
    'Idempotency-Key': command.key,
  };
  const options = { signal, params: { path: { album_id: command.albumID }, header } };
  const result =
    command.rating === null
      ? await api.DELETE('/albums/{album_id}/rating', options)
      : await api.PUT('/albums/{album_id}/rating', {
          ...options,
          body: { rating: command.rating },
        });
  if (!result.data) return failure(result);
}

export async function listMedia(id: string, cursor: string | null, signal: AbortSignal) {
  const result = await api.GET('/albums/{album_id}/media', {
    signal,
    params: { path: { album_id: id }, query: { limit: 30, ...(cursor && { cursor }) } },
  });
  if (!result.data) return failure(result);
  return result.data;
}

const preferred = ['thumb_320', 'poster_720', 'thumb_720', 'display_1920'];

export function choosePreview(items: Variant[]) {
  const candidates = items.filter(
    (item) =>
      safeID(item.id) &&
      ['image/webp', 'image/jpeg', 'image/png', 'image/avif'].includes(item.mime) &&
      item.width !== null &&
      item.height !== null &&
      item.width > 0 &&
      item.height > 0 &&
      item.width <= 1920 &&
      item.height <= 1920 &&
      item.size_bytes > 0 &&
      item.size_bytes <= 8 * 1024 * 1024,
  );
  for (const preset of preferred) {
    const candidate = candidates.find((item) => item.preset_key === preset);
    if (candidate) return candidate;
  }
  return candidates[0];
}

function safeID(id: string) {
  return (
    id.length > 0 && id.length <= 128 && id !== '.' && id !== '..' && !/[\\/\p{Cc}\p{Cf}]/u.test(id)
  );
}

export async function previewURL(mediaID: string, signal: AbortSignal) {
  if (!safeID(mediaID)) return null;
  const result = await api.GET('/media/{media_id}/variants', {
    signal,
    params: { path: { media_id: mediaID }, query: { limit: 100 } },
  });
  if (!result.data) return failure(result);
  const candidate = choosePreview(result.data.items);
  if (!candidate) return null;
  // Construct only the documented same-origin route; never navigate to a server-supplied URL.
  return `/api/v2/media/${encodeURIComponent(mediaID)}/variants/${encodeURIComponent(candidate.id)}/content`;
}
