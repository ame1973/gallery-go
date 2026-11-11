import { useEffect, useId, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import type { UseQueryResult } from '@tanstack/react-query';
import { FormattedMessage, useIntl } from 'react-intl';
import { APIError } from '../auth/api';
import type { User } from '../auth/api';
import { saveRating } from './api';
import type { Album, RatingCommand, RatingSnapshot } from './api';

type Pending = RatingCommand & { startedAt: number; startedTick: number };
type Status =
  | 'ready'
  | 'saving'
  | 'refreshing'
  | 'saved'
  | 'uncertain'
  | 'conflict'
  | 'expired'
  | 'cannot-retry'
  | 'refresh-error';

// Keep an uncertain command only in memory and retry well inside the 24-hour receipt window.
const retryAge = 15 * 60 * 1000;
const requestTimeout = 15_000;

function expired(command: Pending) {
  const wall = Date.now() - command.startedAt;
  const elapsed = performance.now() - command.startedTick;
  return wall < 0 || elapsed < 0 || wall >= retryAge || elapsed >= retryAge;
}

export function RatingEditor({
  album,
  user,
  rating,
  average,
  onAccessLost,
}: {
  album: Album;
  user: User;
  rating: UseQueryResult<RatingSnapshot, Error>;
  average: UseQueryResult<Album, Error>;
  onAccessLost: (error: APIError) => void;
}) {
  const intl = useIntl();
  const client = useQueryClient();
  const [status, setStatus] = useState<Status>('ready');
  const [pending, setPending] = useState<Pending | null>(null);
  const inFlight = useRef(false);
  const active = useRef(true);
  const controller = useRef<AbortController | null>(null);
  useEffect(() => {
    active.current = true;
    return () => {
      active.current = false;
      controller.current?.abort();
    };
  }, []);

  function currentAccount() {
    const current = client.getQueryData<User | null>(['auth', 'me']);
    return active.current && current?.id === user.id && current.scope_id === user.scope_id;
  }

  async function refresh() {
    try {
      const [personal, shared] = await Promise.all([rating.refetch(), average.refetch()]);
      return personal.isSuccess && shared.isSuccess;
    } catch {
      return false;
    }
  }

  async function reload() {
    if (inFlight.current) return;
    inFlight.current = true;
    const previous = status;
    setStatus('refreshing');
    try {
      const ok = await refresh();
      if (!currentAccount()) return;
      if (ok) {
        setPending(null);
        setStatus('ready');
      } else {
        setStatus(pending ? previous : 'refresh-error');
      }
    } finally {
      inFlight.current = false;
    }
  }

  async function execute(command: Pending) {
    if (inFlight.current) return;
    if (expired(command)) {
      setStatus('expired');
      return;
    }
    inFlight.current = true;
    setPending(command);
    setStatus('saving');
    const request = new AbortController();
    controller.current = request;
    const timer = window.setTimeout(() => request.abort(), requestTimeout);
    try {
      try {
        await saveRating(command, request.signal);
      } catch (error) {
        if (!currentAccount()) return;
        if (error instanceof APIError && ['NOT_FOUND', 'UNAUTHENTICATED'].includes(error.code)) {
          setPending(null);
          onAccessLost(error);
        } else if (error instanceof APIError && error.code === 'VERSION_CONFLICT') {
          setPending(null);
          setStatus('conflict');
        } else if (error instanceof APIError && error.code === 'IDEMPOTENCY_CONFLICT') {
          setPending(null);
          setStatus('cannot-retry');
        } else {
          setStatus('uncertain');
        }
        return;
      } finally {
        window.clearTimeout(timer);
        controller.current = null;
      }
      if (!currentAccount()) return;
      setPending(null);
      setStatus('refreshing');
      const ok = await refresh();
      if (currentAccount()) setStatus(ok ? 'saved' : 'refresh-error');
    } finally {
      inFlight.current = false;
    }
  }

  function save(value: number | null) {
    if (!rating.data || inFlight.current) return;
    try {
      const command: Pending = {
        albumID: album.id,
        rating: value,
        etag: rating.data.etag,
        key: crypto.randomUUID(),
        startedAt: Date.now(),
        startedTick: performance.now(),
      };
      void execute(command);
    } catch {
      setStatus('cannot-retry');
    }
  }

  const displayed = average.data && average.data.version >= album.version ? average.data : album;
  const busy = status === 'saving' || status === 'refreshing';
  const blocked = ['uncertain', 'conflict', 'expired', 'cannot-retry', 'refresh-error'].includes(
    status,
  );
  const message = {
    uncertain: 'rating.uncertain',
    conflict: 'rating.conflict',
    expired: 'rating.expired',
    'cannot-retry': 'rating.cannotRetry',
    'refresh-error': 'rating.refreshError',
  } as const;

  return (
    <section className="rating-panel" aria-labelledby="rating-heading">
      <h2 id="rating-heading">
        <FormattedMessage id="rating.sectionTitle" />
      </h2>
      <p>
        <FormattedMessage id="rating.description" />
      </p>
      {average.isFetching ? (
        <p role="status">
          <FormattedMessage id="rating.averageLoading" />
        </p>
      ) : average.isError ? (
        <p role="alert">
          <FormattedMessage id="rating.averageError" />
        </p>
      ) : (
        <p>
          {displayed.rating_avg === null ? (
            <FormattedMessage id="rating.noAverage" />
          ) : (
            <FormattedMessage
              id="rating.average"
              values={{
                average: intl.formatNumber(displayed.rating_avg, { maximumFractionDigits: 2 }),
              }}
            />
          )}
        </p>
      )}
      {busy && (
        <p role="status">
          <FormattedMessage id={status === 'saving' ? 'rating.saving' : 'rating.refreshing'} />
        </p>
      )}
      {status === 'saved' && (
        <p role="status">
          <FormattedMessage id="rating.saved" />
        </p>
      )}
      {blocked ? (
        <div className="rating-recovery" role="alert">
          <p>
            <FormattedMessage id={message[status as keyof typeof message]} />
          </p>
          {status === 'uncertain' && pending && (
            <button className="button" type="button" onClick={() => void execute(pending)}>
              <FormattedMessage id="rating.retry" />
            </button>
          )}
          <button className="button button-secondary" type="button" onClick={() => void reload()}>
            <FormattedMessage id="rating.reload" />
          </button>
        </div>
      ) : rating.isPending || rating.isFetching ? (
        <p role="status">
          <FormattedMessage id="rating.loading" />
        </p>
      ) : rating.isError || !rating.data ? (
        <div className="rating-recovery" role="alert">
          <p>
            <FormattedMessage id="rating.loadError" />
          </p>
          <button className="button button-secondary" type="button" onClick={() => void reload()}>
            <FormattedMessage id="rating.reload" />
          </button>
        </div>
      ) : (
        <RatingForm
          key={rating.data.etag}
          snapshot={rating.data}
          disabled={busy}
          onEdit={() => setStatus('ready')}
          onSave={save}
        />
      )}
    </section>
  );
}

function RatingForm({
  snapshot,
  disabled,
  onEdit,
  onSave,
}: {
  snapshot: RatingSnapshot;
  disabled: boolean;
  onEdit: () => void;
  onSave: (value: number | null) => void;
}) {
  const name = useId();
  const [value, setValue] = useState<number | null>(snapshot.value.rating);
  return (
    <form
      onSubmit={(event) => {
        event.preventDefault();
        onSave(value);
      }}
    >
      <fieldset className="rating-fieldset" disabled={disabled}>
        <legend>
          <FormattedMessage id="rating.title" />
        </legend>
        <div className="rating-choices">
          {[null, 1, 2, 3, 4, 5].map((choice) => (
            <label className="rating-choice" key={choice ?? 'none'}>
              <input
                type="radio"
                name={name}
                checked={value === choice}
                onChange={() => {
                  setValue(choice);
                  onEdit();
                }}
              />
              {choice === null ? (
                <FormattedMessage id="rating.none" />
              ) : (
                <FormattedMessage id="rating.stars" values={{ count: choice }} />
              )}
            </label>
          ))}
        </div>
        <button className="button" type="submit" disabled={value === snapshot.value.rating}>
          <FormattedMessage id="rating.save" />
        </button>
      </fieldset>
    </form>
  );
}
