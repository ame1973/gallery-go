import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { BrowserRouter } from 'react-router';
import { setNonce } from 'get-nonce';
import '@fontsource-variable/manrope';
import '../styles/tokens.css';
import '../styles/app.css';
import '../styles/auth.css';
import { App } from './App';

// Radix's scroll-lock stylesheet uses the same response-specific CSP nonce.
setNonce(
  document.querySelector<HTMLMetaElement>('meta[name="gallerygo-style-nonce"]')?.content ?? '',
);

const root = document.getElementById('root');
if (!root) throw new Error('Application root is missing');

const queryClient = new QueryClient({
  defaultOptions: { queries: { staleTime: 15_000, retry: 1 } },
});

createRoot(root).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
);
