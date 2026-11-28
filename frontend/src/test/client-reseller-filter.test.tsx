import { beforeEach, describe, expect, it, vi } from 'vitest';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, waitFor } from '@testing-library/react';

import { HttpUtil } from '@/utils';
import { ThemeProvider } from '@/hooks/useTheme';
import { activeFilterCount, emptyFilters } from '@/pages/clients/filters';
import ClientsPage from '@/pages/clients/ClientsPage';

const sessionState = vi.hoisted(() => ({ role: 'admin' as 'admin' | 'reseller' }));

vi.mock('@/api/queries/useSession', () => ({
  useSession: () => ({
    session: { role: sessionState.role, username: 'admin', name: 'Admin' },
    role: sessionState.role,
    isAdmin: sessionState.role === 'admin',
    isReseller: sessionState.role === 'reseller',
    loading: false,
    error: null,
  }),
}));

vi.mock('@/layouts/AppSidebar', () => ({ default: () => <div /> }));

function ok<T>(obj: T) {
  return { success: true, msg: '', obj } as never;
}

function mockApi() {
  vi.spyOn(HttpUtil, 'get').mockImplementation(((url: string) => {
    if (url.startsWith('/panel/api/clients/list/paged')) {
      return Promise.resolve(ok({
        items: [], total: 0, filtered: 0, page: 1, pageSize: 25,
        summary: { total: 0, active: 0, online: [], depleted: [], expiring: [], deactive: [] },
        groups: [],
      }));
    }
    if (url === '/panel/api/inbounds/options') return Promise.resolve(ok([]));
    if (url === '/panel/api/resellers/assignments') {
      return Promise.resolve(ok([{ resellerId: 9, name: 'Ali', username: 'ali', inboundIds: [], emails: [] }]));
    }
    return Promise.resolve(ok(null));
  }) as never);
  vi.spyOn(HttpUtil, 'post').mockImplementation(((url: string) => {
    if (url === '/panel/api/setting/defaultSettings' || url === '/panel/api/setting/all') return Promise.resolve(ok({}));
    if (url === '/panel/api/clients/onlines') return Promise.resolve(ok([]));
    return Promise.resolve(ok(null));
  }) as never);
}

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, refetchInterval: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider><ClientsPage /></ThemeProvider>
    </QueryClientProvider>,
  );
}

describe('client reseller filter', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.clear();
    sessionState.role = 'admin';
    mockApi();
  });

  it('persists as one active filter and sends selected explicit reseller IDs to paged API', async () => {
    const filters = emptyFilters();
    filters.resellerIds = [9, 11];
    expect(activeFilterCount(filters)).toBe(1);
    localStorage.setItem('clientsFilterState', JSON.stringify({ searchKey: '', filters, sort: '' }));

    renderPage();

    await waitFor(() => {
      const urls = (HttpUtil.get as ReturnType<typeof vi.fn>).mock.calls.map(([url]) => String(url));
      const pageURL = urls.find((url) => new URLSearchParams(url.split('?')[1]).get('reseller') === '9,11');
      expect(pageURL).toBeTruthy();
    });
  });

  it('does not send the admin-only selector or fetch ownership data in a reseller session', async () => {
    sessionState.role = 'reseller';
    const filters = emptyFilters();
    filters.resellerIds = [9];
    localStorage.setItem('clientsFilterState', JSON.stringify({ searchKey: '', filters, sort: '' }));

    renderPage();

    await waitFor(() => {
      const urls = (HttpUtil.get as ReturnType<typeof vi.fn>).mock.calls.map(([url]) => String(url));
      const pageURL = urls.find((url) => url.startsWith('/panel/api/clients/list/paged'));
      expect(pageURL).toBeTruthy();
      expect(new URLSearchParams(pageURL?.split('?')[1]).has('reseller')).toBe(false);
    });
    expect(HttpUtil.get).not.toHaveBeenCalledWith('/panel/api/resellers/assignments', undefined, { silent: true });
  });
});
