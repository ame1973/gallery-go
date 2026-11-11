import { useEffect, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { FormattedMessage } from 'react-intl';
import type { User } from '../auth/api';
import { previewURL } from './api';

export function Preview({ mediaID, user }: { mediaID: string | null; user: User }) {
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
    queryKey: [user.id, user.scope_id, 'preview', mediaID],
    queryFn: ({ signal }) => previewURL(mediaID!, signal),
    enabled: visible && mediaID !== null,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    networkMode: 'always',
  });
  const url = !preview.isError && !preview.isFetching ? preview.data : null;
  return (
    <div className="gallery-preview" ref={element}>
      {url && failedURL !== url ? (
        <img
          alt=""
          src={url}
          loading="lazy"
          decoding="async"
          referrerPolicy="no-referrer"
          onError={() => setFailedURL(url)}
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
