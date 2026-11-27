import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';

const pageLoad = vi.hoisted(() => {
  let release = () => {};
  const pending = new Promise<void>((resolve) => {
    release = resolve;
  });
  return { pending, release: () => release() };
});

vi.mock('../features/auth/LoginPage', async () => {
  await pageLoad.pending;
  return { LoginPage: () => <h1>Test sign-in page</h1> };
});

function renderApp(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <App />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function selectLocale(locale: string) {
  fireEvent.change(screen.getByRole('combobox'), { target: { value: locale } });
}

afterEach(() => {
  pageLoad.release();
  vi.unstubAllGlobals();
});

describe('application loading states', () => {
  it('announces lazy page loading in the selected language until the page is ready', async () => {
    renderApp('/login');
    expect(screen.getByRole('status')).toHaveTextContent('Loading page…');
    selectLocale('zh-TW');
    expect(screen.getByRole('status')).toHaveTextContent('正在載入頁面…');
    selectLocale('zh-CN');
    expect(screen.getByRole('status')).toHaveTextContent('正在加载页面…');
    await act(async () => pageLoad.release());
    expect(await screen.findByRole('heading', { name: 'Test sign-in page' })).toBeInTheDocument();
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
  });

  it('continues to announce account verification separately', async () => {
    let release!: (response: Response) => void;
    const pending = new Promise<Response>((resolve) => {
      release = resolve;
    });
    const fetch = vi.fn(() => pending);
    vi.stubGlobal('fetch', fetch);
    const view = renderApp('/settings/security');
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
    expect(screen.getByRole('status')).toHaveTextContent('Loading your account…');
    selectLocale('zh-TW');
    expect(screen.getByRole('status')).toHaveTextContent('正在載入帳號…');
    selectLocale('zh-CN');
    expect(screen.getByRole('status')).toHaveTextContent('正在加载账号…');
    view.unmount();
    release(new Response(null, { status: 401 }));
  });
});
