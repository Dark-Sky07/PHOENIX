import { describe, expect, it } from 'vitest';

import {
  findBlackholeOutboundTag,
  INBOUND_FILTER_RULE_TAG_PREFIX,
  isInboundFilterRule,
  isInboundReadyForDomainFilters,
  makeInboundFilterRule,
  normalizeCustomFilterDomains,
  removeInboundFilterRule,
  upsertInboundFilterRule,
} from '@/pages/xray/routing/filter-rules';
import type { RuleObject } from '@/schemas/routing';

describe('inbound filter routing rules', () => {
  it('requires supported inbound sniffing for HTTP, TLS and QUIC before enabling domain filters', () => {
    expect(isInboundReadyForDomainFilters({
      protocol: 'vless', sniffingEnabled: true, sniffingDestOverride: ['http', 'tls', 'quic'],
    })).toBe(true);
    expect(isInboundReadyForDomainFilters({
      protocol: 'vless', sniffingEnabled: false, sniffingDestOverride: ['http', 'tls', 'quic'],
    })).toBe(false);
    expect(isInboundReadyForDomainFilters({
      protocol: 'vless', sniffingEnabled: true, sniffingDestOverride: ['http', 'tls'],
    })).toBe(false);
    expect(isInboundReadyForDomainFilters({
      protocol: 'openvpn', sniffingEnabled: true, sniffingDestOverride: ['http', 'tls', 'quic'],
    })).toBe(false);
  });

  it('creates a rule scoped to exact inbound tags and domains', () => {
    const rule = makeInboundFilterRule({
      id: 'test-id',
      inboundTags: ['in-a', 'in-b', 'in-a'],
      domains: ['geosite:category-porn', 'domain:example.com'],
      outboundTag: 'blocked',
    });

    expect(rule).toEqual({
      type: 'field',
      ruleTag: `${INBOUND_FILTER_RULE_TAG_PREFIX}test-id`,
      inboundTag: ['in-a', 'in-b'],
      domain: ['geosite:category-porn', 'domain:example.com'],
      outboundTag: 'blocked',
    });
    expect(isInboundFilterRule(rule)).toBe(true);
    expect(isInboundFilterRule({ type: 'field', outboundTag: 'blocked' })).toBe(false);
  });

  it('inserts before catch-all rules while keeping API routing first', () => {
    const apiRule: RuleObject = { type: 'field', inboundTag: ['api'], outboundTag: 'api' };
    const catchAll: RuleObject = { type: 'field', network: 'tcp,udp', outboundTag: 'proxy' };
    const policy = makeInboundFilterRule({
      id: 'new', inboundTags: ['in-1'], domains: ['geosite:category-ads-all'], outboundTag: 'blocked',
    });

    expect(upsertInboundFilterRule([apiRule, catchAll], policy)).toEqual([apiRule, policy, catchAll]);
  });

  it('updates managed rules without duplication and restores their priority', () => {
    const original = makeInboundFilterRule({
      id: 'same', inboundTags: ['in-a'], domains: ['geosite:category-porn'], outboundTag: 'blocked',
    });
    const apiRule: RuleObject = { type: 'field', inboundTag: ['api'], outboundTag: 'api' };
    const later: RuleObject = { type: 'field', outboundTag: 'proxy', domain: ['geosite:google'] };
    const updated = makeInboundFilterRule({
      id: 'same', inboundTags: ['in-b'], domains: ['geosite:category-ads-all'], outboundTag: 'blocked',
    });

    expect(upsertInboundFilterRule([apiRule, later, original], updated)).toEqual([apiRule, updated, later]);
  });

  it('removes only the selected managed filter rule', () => {
    const managed = makeInboundFilterRule({
      id: 'remove-me', inboundTags: ['in-a'], domains: ['geosite:category-porn'], outboundTag: 'blocked',
    });
    const userRule: RuleObject = { type: 'field', outboundTag: 'blocked', domain: ['example.net'] };
    expect(removeInboundFilterRule([managed, userRule], managed.ruleTag!)).toEqual([userRule]);
  });

  it('normalizes bare hosts and URLs while preserving explicit Xray expressions', () => {
    expect(normalizeCustomFilterDomains('example.com\nhttps://ads.example.net/path\nfull:exact.example.org\nregexp:.*\\.bad$')).toEqual({
      domains: ['domain:example.com', 'domain:ads.example.net', 'full:exact.example.org', 'regexp:.*\\.bad$'],
      invalid: [],
    });
  });

  it('rejects empty, malformed or whitespace-containing domain expressions', () => {
    expect(normalizeCustomFilterDomains('https://\nnot a domain\nfull:\nexample.com/path')).toEqual({
      domains: [], invalid: ['https://', 'not a domain', 'full:', 'example.com/path'],
    });
  });

  it('uses an existing blackhole outbound and ignores non-blackhole tags', () => {
    expect(findBlackholeOutboundTag([
      { tag: 'blocked', protocol: 'freedom' },
      { tag: 'drop', protocol: 'blackhole' },
    ])).toBe('drop');
    expect(findBlackholeOutboundTag([{ tag: 'proxy', protocol: 'freedom' }])).toBeNull();
  });
});
