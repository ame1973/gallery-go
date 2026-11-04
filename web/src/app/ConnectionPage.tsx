import { useQuery } from '@tanstack/react-query';
import { FormattedMessage, useIntl } from 'react-intl';
import { readSystem } from '../lib/api/client';

export function ConnectionPage() {
  const intl = useIntl();
  const system = useQuery({
    queryKey: ['system'],
    queryFn: ({ signal }) => readSystem(signal),
    // A health probe must report a failed network, not retain a paused success.
    networkMode: 'always',
    retry: false,
    refetchInterval: 30_000,
  });
  return (
    <main>
      <h1>
        <FormattedMessage id="title" />
      </h1>
      <p className="intro">
        <FormattedMessage id="description" />
      </p>
      <div className="connection-layout">
        <section className="connection-panel" aria-label={intl.formatMessage({ id: 'title' })}>
          <div aria-live="polite" role={system.isError ? 'alert' : 'status'}>
            {system.isFetching ? (
              <p>
                <FormattedMessage id="checking" />
              </p>
            ) : system.isError ? (
              <p className="error-message">
                <FormattedMessage id="failed" />
              </p>
            ) : (
              <>
                <p className="connection-summary">
                  <span className="connection-dot" aria-hidden="true" />
                  <FormattedMessage id="connected" />
                </p>
                <dl className="connection-details">
                  <div>
                    <dt>
                      <FormattedMessage id="api" />
                    </dt>
                    <dd>
                      <FormattedMessage id="healthy" />
                    </dd>
                  </div>
                  <div>
                    <dt>
                      <FormattedMessage id="database" />
                    </dt>
                    <dd>
                      <FormattedMessage id="healthy" />
                    </dd>
                  </div>
                </dl>
              </>
            )}
          </div>
          <button
            className="button"
            type="button"
            disabled={system.isFetching}
            onClick={() => void system.refetch()}
          >
            <FormattedMessage id="retry" />
          </button>
          {system.data && (
            <p className="version">
              <FormattedMessage id="version" values={{ version: system.data.config.version }} />
            </p>
          )}
        </section>
        <aside className="build-notes">
          <h2>
            <FormattedMessage id="development" />
          </h2>
          <p>
            <FormattedMessage id="developmentNote" />
          </p>
          <h2>
            <FormattedMessage id="privacy" />
          </h2>
          <p>
            <FormattedMessage id="privacyNote" />
          </p>
        </aside>
      </div>
    </main>
  );
}
