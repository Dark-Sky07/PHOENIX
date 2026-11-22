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
