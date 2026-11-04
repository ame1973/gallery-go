import { useQuery } from '@tanstack/react-query';
import { FormattedMessage } from 'react-intl';
import { Navigate, Outlet } from 'react-router';
import { currentUser } from './api';

export function RequireSession() {
  const user = useQuery({
    queryKey: ['auth', 'me'],
    queryFn: ({ signal }) => currentUser(signal),
    retry: false,
    staleTime: 0,
    networkMode: 'always',
    refetchInterval: 60_000,
  });
  if (user.isPending)
    return (
      <main>
        <p role="status">
          <FormattedMessage id="auth.loading" />
        </p>
      </main>
    );
  if (user.isError)
    return (
      <main>
        <p role="alert">
          <FormattedMessage id="auth.networkError" />
        </p>
        <button className="button" onClick={() => void user.refetch()}>
          <FormattedMessage id="retry" />
        </button>
      </main>
    );
  if (!user.data) return <Navigate to="/login" replace />;
  return <Outlet context={user.data} />;
}
