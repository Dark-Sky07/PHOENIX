import axios from 'axios';
import { describe, expect, it, vi } from 'vitest';

vi.mock('axios', () => ({
  default: {
    get: vi.fn(),
    post: vi.fn(),
  },
}));

import { HttpUtil } from '@/utils';

describe('HttpUtil', () => {
  it('surfaces a panel JSON-envelope error message from a failed request', async () => {
    vi.mocked(axios.post).mockRejectedValueOnce({
      response: { data: { success: false, msg: 'invalid reconciliation request; send a JSON object' } },
      message: 'Request failed with status code 400',
    });

    const result = await HttpUtil.post('/panel/api/resellers/reconciliation/repair', {}, { silent: true });

    expect(result.success).toBe(false);
    expect(result.msg).toBe('invalid reconciliation request; send a JSON object');
  });
});
