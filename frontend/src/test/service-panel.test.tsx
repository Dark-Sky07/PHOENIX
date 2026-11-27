import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, within } from '@testing-library/react';

import { Status } from '@/models/status';
import ServicePanel from '@/pages/index/ServicePanel';

describe('overview service panel', () => {
  it('shows SSTP / PPTP with the same daemon controls as OpenVPN and L2TP', () => {
    const status = new Status({
      xray: { state: 'running', errorMsg: '', version: '1.8.0' },
      openvpn: { state: 'stop', errorMsg: '', inboundCount: 1, onlineClients: 0, manualStop: false },
      l2tp: { state: 'stop', errorMsg: '', inboundCount: 1, onlineClients: 0, manualStop: false },
      pppvpn: { state: 'running', errorMsg: '', inboundCount: 2, onlineClients: 0, manualStop: false },
    });
    const pppvpn = {
      onStart: vi.fn(),
      onStop: vi.fn(),
      onRestart: vi.fn(),
      onUpdate: vi.fn(),
    };
    const noop = vi.fn();

    render(
      <ServicePanel
        status={status}
        accessLogEnable={false}
        onStopXray={noop}
        onRestartXray={noop}
        onOpenXrayLogs={noop}
        onOpenAmneziaWGLogs={noop}
        onOpenVersionSwitch={noop}
        onOpenLogs={noop}
        openvpn={pppvpn}
        l2tp={pppvpn}
        pppvpn={pppvpn}
      />,
    );

    const row = screen.getByText('SSTP / PPTP').closest('.service-row');
    expect(row).not.toBeNull();
    expect(within(row as HTMLElement).getByText(/running/i)).toBeTruthy();
    fireEvent.click(within(row as HTMLElement).getByRole('button', { name: /stop/i }));
    expect(pppvpn.onStop).toHaveBeenCalledTimes(1);
  });
});
