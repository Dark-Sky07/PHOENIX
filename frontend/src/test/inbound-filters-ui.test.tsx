import { describe, expect, it, vi } from 'vitest';
import { fireEvent, screen } from '@testing-library/react';

import type { SetTemplate } from '@/hooks/useXraySetting';
import InboundFilters from '@/pages/xray/routing/InboundFilters';
import { FILTER_PRESETS } from '@/pages/xray/routing/filter-presets';
import RoutingBasic from '@/pages/xray/routing/RoutingBasic';
import { renderWithProviders } from './test-utils';

vi.mock('@/api/queries/useInboundOptions', () => ({
  useInboundOptions: () => ({
    data: [{
      id: 1,
      port: 443,
      protocol: 'vless',
      remark: 'VLESS inbound',
      ssMethod: '',
      tag: 'in-vless-443',
      tlsFlowCapable: true,
      sniffingEnabled: true,
      sniffingDestOverride: ['http', 'tls', 'quic'],
    }],
    isLoading: false,
    isError: false,
  }),
}));

describe('inbound filter UI placement and packages', () => {
  it('removes Block IPs and Block Domains from Basic Routing but keeps other routing controls', () => {
    renderWithProviders(<RoutingBasic templateSettings={null} setTemplateSettings={vi.fn() as SetTemplate} />);

    expect(screen.queryByText('Block IPs')).toBeNull();
    expect(screen.queryByText('Block Domains')).toBeNull();
    expect(screen.getByText('Block BitTorrent Protocol')).toBeTruthy();
    expect(screen.getByText('Direct IPs')).toBeTruthy();
    expect(screen.getByText('Direct Domains')).toBeTruthy();
  });

  it('explains YouTube ad-filtering limits and does not offer a whole-site YouTube preset', () => {
    renderWithProviders(<InboundFilters templateSettings={null} setTemplateSettings={vi.fn() as SetTemplate} />);

    expect(screen.getByText(/cannot block only YouTube ads/)).toBeTruthy();
    expect(FILTER_PRESETS.some((preset) => preset.value === 'geosite:youtube')).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: /Add filter policy/ }));

    expect(screen.getByText('Domain packages')).toBeTruthy();
    expect(screen.getByText('IP / CIDR packages')).toBeTruthy();
  });
});
