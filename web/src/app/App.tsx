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
const JobsPage = lazy(() =>
  import('../features/jobs/JobsPage').then((module) => ({ default: module.JobsPage })),
);
const JobPage = lazy(() =>
  import('../features/jobs/JobPage').then((module) => ({ default: module.JobPage })),
);
const SourcesPage = lazy(() =>
  import('../features/sources/SourcesPage').then((module) => ({ default: module.SourcesPage })),
);
const SourcePage = lazy(() =>
  import('../features/sources/SourcesPage').then((module) => ({ default: module.SourcePage })),
);
const UserPage = lazy(() =>
  import('../features/users/UsersPage').then((module) => ({ default: module.UserPage })),
);
const UsersPage = lazy(() =>
  import('../features/users/UsersPage').then((module) => ({ default: module.UsersPage })),
);
const GroupsPage = lazy(() =>
  import('../features/groups/GroupsPage').then((module) => ({ default: module.GroupsPage })),
);
const GroupPage = lazy(() =>
  import('../features/groups/GroupsPage').then((module) => ({ default: module.GroupPage })),
);
const AlbumsPage = lazy(() =>
  import('../features/library/AlbumsPage').then((module) => ({ default: module.AlbumsPage })),
);
const AlbumPage = lazy(() =>
  import('../features/library/AlbumPage').then((module) => ({ default: module.AlbumPage })),
);
const MediaPage = lazy(() =>
  import('../features/library/MediaPage').then((module) => ({ default: module.MediaPage })),
);
const PeoplePage = lazy(() =>
  import('../features/library/PeoplePage').then((module) => ({ default: module.PeoplePage })),
);
const PersonPage = lazy(() =>
  import('../features/library/PersonPage').then((module) => ({ default: module.PersonPage })),
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
              <Route path="/admin/jobs" element={<JobsPage />} />
              <Route path="/admin/jobs/:jobId/details" element={<JobPage />} />
              <Route path="/admin/sources" element={<SourcesPage />} />
              <Route path="/admin/sources/:sourceId/details" element={<SourcePage />} />
              <Route path="/admin/users" element={<UsersPage />} />
              <Route path="/admin/users/:userId/details" element={<UserPage />} />
              <Route path="/admin/groups" element={<GroupsPage />} />
              <Route path="/admin/groups/:groupId/details" element={<GroupPage />} />
              <Route path="/albums" element={<AlbumsPage />} />
              <Route path="/albums/:albumId" element={<AlbumPage />} />
              <Route path="/inspect/:mediaId/details" element={<MediaPage />} />
              <Route path="/people" element={<PeoplePage />} />
              <Route path="/people/:personId" element={<PersonPage />} />
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
