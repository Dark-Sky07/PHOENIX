import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const api = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
}));

vi.mock('@/utils', () => ({
  HttpUtil: {
    get: api.get,
    post: api.post,
  },
}));
vi.mock('@/utils/messageBus', () => ({ setMessageInstance: vi.fn() }));
vi.mock('@/layouts/AppSidebar', () => ({ default: () => null }));
vi.mock('@/hooks/useMediaQuery', () => ({ useMediaQuery: () => ({ isMobile: false }) }));
vi.mock('@/hooks/useTheme', () => ({
  useTheme: () => ({ isDark: false, isUltra: false, antdThemeConfig: {} }),
}));

import ReconciliationPage from '@/pages/resellers/ReconciliationPage';

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, refetchInterval: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <ReconciliationPage />
    </QueryClientProvider>,
  );
}

describe('ReconciliationPage', () => {
  beforeEach(() => {
    api.get.mockResolvedValue({
      success: true,
      msg: '',
      // Empty Go slices from previously released panels were encoded as null.
      // The review page must still render a safe, actionable empty state.
      obj: {
        available: true,
        generatedAt: 1,
        counterIncident: {
          available: true,
          databaseCount: 0,
          fallbackPresent: false,
          pendingInProcess: false,
          daemonIdentityCount: 0,
        },
        danglingMappings: null,
        dbIncidents: null,
        inFlightPolls: null,
        fallbackFiles: null,
        l2tpMarkerPresent: false,
        l2tpEvidenceFiles: null,
        xrayJournals: null,
        tuicJournals: null,
        xrayApi: { port: 62789, reachable: true },
        warnings: null,
      },
    });
    api.post.mockResolvedValue({ success: true, msg: '', obj: {} });
  });

  afterEach(() => {
    vi.clearAllMocks();
  });

  it('renders a report whose empty collections were encoded as null', async () => {
    const { container } = renderPage();

    await waitFor(() => {
      expect(api.get).toHaveBeenCalledWith('/panel/api/resellers/reconciliation', undefined, { silent: true });
      expect(container.querySelector('.ant-alert-success')).not.toBeNull();
    });

    expect(container.querySelector('.ant-alert-error')).toBeNull();
  });

  it('sends the repair contract as JSON instead of inheriting the legacy form default', async () => {
    renderPage();

    await waitFor(() => expect(api.get).toHaveBeenCalled());
    fireEvent.click(screen.getByRole('button', { name: /dry run/i }));

    await waitFor(() => {
      expect(api.post).toHaveBeenCalledWith(
        '/panel/api/resellers/reconciliation/repair',
        { confirm: '', dryRun: true },
        { silent: true, headers: { 'Content-Type': 'application/json' } },
      );
    });
  });
});
