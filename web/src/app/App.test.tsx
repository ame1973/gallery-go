import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter } from 'react-router';
import { App } from './App';
import { initialLocale, locales, messages } from '../lib/i18n';

function renderApp() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={['/status']}>
        <App />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function healthyFetch(
  readiness: Record<string, string> = { status: 'ok', database: 'ok', worker: 'ok', sources: 'ok' },
) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (request: Request) =>
      Response.json(
        request.url.endsWith('/public/config')
          ? { version: 'test-build', offline_enabled: false }
          : request.url.endsWith('/health/ready')
            ? readiness
            : { status: 'ok' },
      ),
    ),
  );
}

afterEach(() => vi.unstubAllGlobals());

describe('service connection', () => {
  it('renders real contract responses and explicitly unavailable product features', async () => {
    healthyFetch();
    renderApp();
    expect(await screen.findByText('The service is ready.')).toBeInTheDocument();
    expect(screen.getByText('Version test-build')).toBeInTheDocument();
    expect(screen.getByText(/still under development/)).toBeInTheDocument();
    expect(fetch).toHaveBeenCalledTimes(3);
  });

  it('shows an actionable failure and retries all checks', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => Response.json({ code: 'NOT_READY' }, { status: 503 })),
    );
    renderApp();
    expect(await screen.findByRole('alert')).toHaveTextContent('The service is not ready');
    healthyFetch();
    await userEvent.click(screen.getByRole('button', { name: 'Check again' }));
    expect(await screen.findByText('The service is ready.')).toBeInTheDocument();
  });

  it('shows degraded source observations without claiming processing or storage success', async () => {
    healthyFetch({ status: 'degraded', database: 'ok', worker: 'ok', sources: 'degraded' });
    renderApp();
    expect(
      await screen.findByText('The service is available, but media sources need attention.'),
    ).toBeInTheDocument();
    expect(screen.getByText('Needs attention')).toBeInTheDocument();
    expect(screen.queryByText('The service is ready.')).not.toBeInTheDocument();
  });

  it('labels an explicitly optional worker instead of claiming a healthy heartbeat', async () => {
    healthyFetch({ status: 'ok', database: 'ok', worker: 'optional', sources: 'ok' });
    renderApp();
    expect(await screen.findByText('Not required')).toBeInTheDocument();
    expect(screen.getByText('The service is ready.')).toBeInTheDocument();
  });

  it('does not turn an incomplete readiness response into a successful status', async () => {
    healthyFetch({ status: 'ok' });
    renderApp();
    expect(await screen.findByRole('alert')).toHaveTextContent('The service is not ready');
    expect(screen.queryByText('The service is ready.')).not.toBeInTheDocument();
  });

  it('persists locale and translates the complete page', async () => {
    healthyFetch();
    renderApp();
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Language' }), 'zh-TW');
    expect(screen.getByRole('heading', { name: '服務連線', level: 1 })).toBeInTheDocument();
    expect(await screen.findByText('服務已就緒。')).toBeInTheDocument();
    expect(document.documentElement.lang).toBe('zh-TW');
    expect(localStorage.getItem('gallerygo:locale')).toBe('zh-TW');
  });

  it('keeps locale keys aligned and tolerates unavailable storage', () => {
    for (const locale of locales)
      expect(Object.keys(messages[locale]).sort()).toEqual(Object.keys(messages.en).sort());
    localStorage.setItem('gallerygo:locale', 'invalid');
    expect(initialLocale()).toBe('en');
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    expect(initialLocale()).toBe('en');
  });
});
