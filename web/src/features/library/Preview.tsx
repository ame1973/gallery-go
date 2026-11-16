import { useEffect, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { FormattedMessage } from 'react-intl';
import { APIError } from '../auth/api';
import type { User } from '../auth/api';
import { previewURL } from './api';

export function Preview({
  mediaID,
  user,
  revision,
  alt = '',
  onAccessLost,
  onImageError,
  onImageLoad,
}: {
  mediaID: string | null;
  user: User;
  revision?: string | null;
  alt?: string;
  onAccessLost?: (error: APIError) => void;
  onImageError?: (url: string) => void;
  onImageLoad?: () => void;
}) {
  const element = useRef<HTMLDivElement>(null);
  const [visible, setVisible] = useState(() => typeof IntersectionObserver === 'undefined');
  const [failedURL, setFailedURL] = useState<string | null>(null);
  useEffect(() => {
    const target = element.current;
    if (!target || typeof IntersectionObserver === 'undefined') return;
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((entry) => entry.isIntersecting)) {
          setVisible(true);
          observer.disconnect();
        }
      },
      { rootMargin: '200px' },
    );
    observer.observe(target);
    return () => observer.disconnect();
  }, []);

  const preview = useQuery({
    queryKey: [user.id, user.scope_id, 'preview', mediaID, { revision }],
    queryFn: ({ signal }) => previewURL(mediaID!, signal, revision),
    enabled: visible && mediaID !== null && revision !== null,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    networkMode: 'always',
  });
  useEffect(() => {
    if (
      preview.error instanceof APIError &&
      ['NOT_FOUND', 'UNAUTHENTICATED', 'FORBIDDEN'].includes(preview.error.code)
    ) {
      onAccessLost?.(preview.error);
    }
  }, [onAccessLost, preview.error]);
  const url = revision !== null && !preview.isError && !preview.isFetching ? preview.data : null;
  return (
    <div className="gallery-preview" ref={element}>
      {url && failedURL !== url ? (
        <img
          alt={alt}
          src={url}
          loading="lazy"
          decoding="async"
          referrerPolicy="no-referrer"
          onLoad={onImageLoad}
          onError={() => {
            setFailedURL(url);
            onImageError?.(url);
          }}
        />
      ) : (
        <span>
          <FormattedMessage
            id={preview.isFetching ? 'library.loadingPreview' : 'library.noPreview'}
          />
        </span>
      )}
    </div>
  );
}
