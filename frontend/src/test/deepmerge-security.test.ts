// @ts-expect-error The frontend tsconfig excludes Node types; Vitest executes tests in Node.
import { createRequire } from 'node:module';
import { describe, expect, it } from 'vitest';

// swagger-client is the transitive consumer of deepmerge in the current tree.
// Resolve it from the same package context so this test exercises the exact
// overridden dependency that the application installs.
const frontendRequire = createRequire(import.meta.url);
const swaggerClientRequire = createRequire(frontendRequire.resolve('swagger-client'));
const deepmerge = swaggerClientRequire('deepmerge') as (
  target: Record<string, unknown>,
  source: Record<string, unknown>,
) => Record<string, unknown>;

describe('patched deepmerge dependency', () => {
  it('does not copy dangerous own keys from the source into the result', () => {
    const source = JSON.parse(
      '{"__proto__":{"polluted":"yes"},"constructor":{"prototype":{"injected":"yes"}},"prototype":{"alsoInjected":"yes"},"safe":{"value":1}}',
    ) as Record<string, unknown>;

    const result = deepmerge({}, source);

    expect(Object.getPrototypeOf(result)).toBe(Object.prototype);
    expect(Object.hasOwn(result, '__proto__')).toBe(false);
    expect(Object.hasOwn(result, 'constructor')).toBe(false);
    expect(Object.hasOwn(result, 'prototype')).toBe(false);
    expect(Object.prototype.hasOwnProperty.call({}, 'polluted')).toBe(false);
    expect(Object.prototype.hasOwnProperty.call({}, 'injected')).toBe(false);
    expect(result.safe).toEqual({ value: 1 });
  });

  it('does not copy dangerous own keys from the target during recursive merges', () => {
    const target = JSON.parse(
      '{"__proto__":{"polluted":"yes"},"safe":{"before":true}}',
    ) as Record<string, unknown>;

    const result = deepmerge(target, { safe: { after: true } });

    expect(Object.getPrototypeOf(result)).toBe(Object.prototype);
    expect(Object.hasOwn(result, '__proto__')).toBe(false);
    expect(Object.prototype.hasOwnProperty.call({}, 'polluted')).toBe(false);
    expect(result.safe).toEqual({ before: true, after: true });
  });
});
