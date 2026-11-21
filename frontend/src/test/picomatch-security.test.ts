import { spawnSync } from 'node:child_process';
import { describe, expect, it } from 'vitest';
import picomatch from 'picomatch';

describe('patched picomatch ReDoS regression', () => {
  it('preserves matching for repeated-star extglobs', () => {
    for (const pattern of ['+(*)', '+(*a)', '*(*a)', '+(*a*)', '+(*a|b)']) {
      const matcher = picomatch(pattern);
      if (typeof matcher !== 'function') throw new Error(`failed to compile ${pattern}`);
      expect(matcher('a')).toBe(true);
      expect(matcher('ba')).toBe(true);
    }
  });

  it('does not catastrophically backtrack on short hostile non-matches', () => {
    const script = `
      const picomatch = (await import('picomatch')).default;
      const hostile = 'a'.repeat(28) + '/';
      for (const pattern of ['+(*)', '+(*a)', '*(*a)', '+(*a*)', '+(*a|b)', '+(@(*))', '+({a,a})', '+(a|[[:alpha:]])', '@(a|a)+']) {
        picomatch(pattern)(hostile);
      }
    `;
    const result = spawnSync(process.execPath, ['--input-type=module', '-e', script], {
      cwd: process.cwd(),
      encoding: 'utf8',
      timeout: 2500,
    });
    expect((result.error as NodeJS.ErrnoException | undefined)?.code).not.toBe('ETIMEDOUT');
    expect(result.status, result.stderr).toBe(0);
  });
});
