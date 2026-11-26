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

describe('picomatch backslash removal', () => {
  it('preserves the original regexp semantics for short inputs', async () => {
    const { createRequire } = await import('node:module');
    const require = createRequire(import.meta.url);
    const { removeBackslashes } = require('../../vendor/picomatch/lib/utils.js') as {
      removeBackslashes: (input: string) => string;
    };
    const original = (input: string) => input.replace(/(?:\[.*?[^\\]\]|\\(?=.))/g, match => match === '\\' ? '' : match);
    const alphabet = ['[', ']', '\\', 'a', '\n', '\r', '\u2028', '\u2029'];
    const check = (input: string, remaining: number) => {
      expect(removeBackslashes(input), JSON.stringify(input)).toBe(original(input));
      if (remaining > 0) for (const char of alphabet) check(input + char, remaining - 1);
    };
    check('', 5);
  });

  it('handles long unmatched brackets in bounded time', () => {
    const script = `
      const { removeBackslashes } = require('./vendor/picomatch/lib/utils.js');
      const input = '['.repeat(1000000) + '\\\\a';
      if (removeBackslashes(input) !== '['.repeat(1000000) + 'a') process.exit(1);
    `;
    const result = spawnSync(process.execPath, ['-e', script], {
      cwd: process.cwd(), encoding: 'utf8', timeout: 3000,
    });
    expect((result.error as NodeJS.ErrnoException | undefined)?.code).not.toBe('ETIMEDOUT');
    expect(result.status, result.stderr).toBe(0);
  });
});
