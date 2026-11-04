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

function healthyFetch() {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (request: Request) =>
      Response.json(
        request.url.endsWith('/public/config')
          ? { version: 'test-build', offline_enabled: false }
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
    expect(await screen.findByRole('alert')).toHaveTextContent('The service could not be reached');
    healthyFetch();
    await userEvent.click(screen.getByRole('button', { name: 'Check again' }));
    expect(await screen.findByText('The service is ready.')).toBeInTheDocument();
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
