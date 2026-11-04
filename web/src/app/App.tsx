import { useEffect, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { FormattedMessage, IntlProvider, useIntl } from 'react-intl';
import { readSystem } from '../lib/api/client';
import { initialLocale, locales, messages } from '../lib/i18n';
import type { Locale } from '../lib/i18n';

export function App() {
  const [locale, setLocale] = useState<Locale>(initialLocale);
  useEffect(() => {
    document.documentElement.lang = locale;
    try {
      localStorage.setItem('gallerygo:locale', locale);
    } catch {
      // A blocked preference store must not prevent access to the application.
    }
  }, [locale]);
  return (
    <IntlProvider locale={locale} messages={messages[locale]} defaultLocale="en">
      <ConnectionPage locale={locale} setLocale={setLocale} />
    </IntlProvider>
  );
}

function ConnectionPage({
  locale,
  setLocale,
}: {
  locale: Locale;
  setLocale: (locale: Locale) => void;
}) {
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
    <div className="app-shell">
      <header className="site-header">
        <a className="wordmark" href="/" aria-label="GalleryGo">
          <img src="/favicon.svg" width="36" height="36" alt="" />
          <span>GalleryGo</span>
        </a>
        <label className="language-picker">
          <span>{intl.formatMessage({ id: 'language' })}</span>
          <select
            value={locale}
            onChange={(event) => {
              const next = locales.find((value) => value === event.target.value);
              if (next) setLocale(next);
            }}
          >
            <option value="en">English</option>
            <option value="zh-TW">繁體中文</option>
            <option value="zh-CN">简体中文</option>
          </select>
        </label>
      </header>
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
      <footer>
        <FormattedMessage id="footer" />
      </footer>
    </div>
  );
}
