import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import TrafficPeerModal from '@/pages/nodes/TrafficPeerModal';
import { HttpUtil, Msg } from '@/utils';

describe('verified traffic peer setup', () => {
  it('requires explicit reconciliation before registering a receiver', async () => {
    const post = vi.spyOn(HttpUtil, 'post').mockResolvedValue(new Msg(true, ''));
    render(<TrafficPeerModal open nodes={[]} onClose={() => {}} onChanged={() => {}} />);
    fireEvent.click(screen.getByRole('tab', { name: 'Receiver' }));
    const submit = screen.getByRole('button', { name: 'Register receiver' }) as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    expect(post).not.toHaveBeenCalled();
    fireEvent.change(screen.getByLabelText('Receiver panel API token ID'), { target: { value: '12' } });
    fireEvent.change(screen.getByLabelText('Sender panel GUID'), { target: { value: 'master-guid' } });
    fireEvent.change(screen.getByLabelText('Sender generation'), { target: { value: 'generation' } });
    fireEvent.click(screen.getByRole('checkbox'));
    await waitFor(() => expect(submit.disabled).toBe(false));
    fireEvent.click(submit);
    await waitFor(() => expect(post).toHaveBeenCalledWith('/panel/api/nodes/trafficSnapshot/receiver', {
      tokenId: 12, sender: 'master-guid', generation: 'generation', reconciled: true,
    }, { silent: true }));
    post.mockRestore();
  });
});

describe('verified node snapshot setup', () => {
  it('requires reconciliation before registering a node producer', async () => {
    const post = vi.spyOn(HttpUtil, 'post').mockResolvedValue(new Msg(true, '', { sender: 'node-guid', generation: 'epoch', lastSequence: 0 }));
    render(<TrafficPeerModal open nodes={[]} onClose={() => {}} onChanged={() => {}} />);
    fireEvent.click(screen.getByRole('tab', { name: 'Node snapshot producer' }));
    const submit = screen.getByRole('button', { name: 'Register node producer' }) as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText('Node panel API token ID'), { target: { value: '13' } });
    fireEvent.click(screen.getByRole('checkbox'));
    await waitFor(() => expect(submit.disabled).toBe(false));
    fireEvent.click(submit);
    await waitFor(() => expect(post).toHaveBeenCalledWith('/panel/api/nodes/nodeSnapshot/producer', { tokenId: 13, reconciled: true }, { silent: true }));
    expect(await screen.findByText('node-guid')).toBeTruthy();
    expect(screen.getByText('epoch')).toBeTruthy();
    post.mockRestore();
  });
  it('registers the reviewed node receiver with the entered stream identity', async () => {
    const post = vi.spyOn(HttpUtil, 'post').mockResolvedValue(new Msg(true, ''));
    render(<TrafficPeerModal open nodes={[{ id: 7, name: 'east' }]} onClose={() => {}} onChanged={() => {}} />);
    fireEvent.click(screen.getByRole('tab', { name: 'Node snapshot receiver' }));
    const select = screen.getByRole('combobox', { name: 'Remote node' });
    fireEvent.mouseDown(select);
    await waitFor(() => expect(document.querySelector('.ant-select-item-option[title="east (#7)"]')).not.toBeNull());
    fireEvent.click(document.querySelector('.ant-select-item-option[title="east (#7)"]')!);
    fireEvent.change(screen.getByLabelText('Node producer GUID'), { target: { value: 'node-guid' } });
    fireEvent.change(screen.getByLabelText('Sender generation'), { target: { value: 'epoch' } });
    fireEvent.click(screen.getByRole('checkbox'));
    const submit = screen.getByRole('button', { name: 'Register receiver and require verified sync' }) as HTMLButtonElement;
    await waitFor(() => expect(submit.disabled).toBe(false));
    fireEvent.click(submit);
    await waitFor(() => expect(post).toHaveBeenCalledWith('/panel/api/nodes/nodeSnapshot/receiver/7', { sender: 'node-guid', generation: 'epoch', reconciled: true }, { silent: true }));
    post.mockRestore();
  });
  it('warns that receiver registration disables legacy sync and requires confirmation', () => {
    const post = vi.spyOn(HttpUtil, 'post').mockResolvedValue(new Msg(true, ''));
    render(<TrafficPeerModal open nodes={[]} onClose={() => {}} onChanged={() => {}} />);
    fireEvent.click(screen.getByRole('tab', { name: 'Node snapshot receiver' }));
    expect(screen.getByText('Registration immediately disables legacy node sync')).toBeTruthy();
    expect((screen.getByRole('button', { name: 'Register receiver and require verified sync' }) as HTMLButtonElement).disabled).toBe(true);
    expect(post).not.toHaveBeenCalled();
    post.mockRestore();
  });
});
