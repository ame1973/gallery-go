import { lazy, Suspense, useEffect, useState } from 'react';
import { FormattedMessage, IntlProvider, useIntl } from 'react-intl';
import { Link, Navigate, Route, Routes } from 'react-router';
import { initialLocale, locales, messages } from '../lib/i18n';
import type { Locale } from '../lib/i18n';
import { RequireSession } from '../features/auth/RequireSession';
import { ConnectionPage } from './ConnectionPage';

const LoginPage = lazy(() =>
  import('../features/auth/LoginPage').then((module) => ({ default: module.LoginPage })),
);
const SecurityPage = lazy(() =>
  import('../features/auth/SecurityPage').then((module) => ({ default: module.SecurityPage })),
);

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
      <div className="app-shell">
        <Header locale={locale} setLocale={setLocale} />
        <Suspense
          fallback={
            <main>
              <p role="status">
                <FormattedMessage id="auth.loading" />
              </p>
            </main>
          }
        >
          <Routes>
            <Route path="/status" element={<ConnectionPage />} />
            <Route path="/login" element={<LoginPage />} />
            <Route path="/" element={<Navigate to="/settings/security" replace />} />
            <Route element={<RequireSession />}>
              <Route path="/settings/security" element={<SecurityPage />} />
            </Route>
            <Route
              path="*"
              element={
                <main>
                  <h1>
                    <FormattedMessage id="auth.notFound" />
                  </h1>
                  <Link className="text-link" to="/">
                    <FormattedMessage id="auth.backToAccount" />
                  </Link>
                </main>
              }
            />
          </Routes>
        </Suspense>
        <footer>
          <FormattedMessage id="footer" />
        </footer>
      </div>
    </IntlProvider>
  );
}

function Header({ locale, setLocale }: { locale: Locale; setLocale: (locale: Locale) => void }) {
  const intl = useIntl();
  return (
    <header className="site-header">
      <Link className="wordmark" to="/" aria-label="GalleryGo">
        <img src="/favicon.svg" width="36" height="36" alt="" />
        <span>GalleryGo</span>
      </Link>
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
  );
}
