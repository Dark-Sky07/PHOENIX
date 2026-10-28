import { describe, expect, it } from 'vitest';

import {
  findBlackholeOutboundTag,
  findLegacyGlobalBlockRules,
  getInboundFilterPolicyId,
  groupInboundFilterPolicies,
  INBOUND_FILTER_RULE_TAG_PREFIX,
  isInboundFilterRule,
  isInboundReadyForDomainFilters,
  makeInboundFilterRules,
  normalizeCustomFilterDomains,
  normalizeCustomFilterIPs,
  removeInboundFilterRule,
  removeLegacyGlobalBlockRule,
  upsertInboundFilterRules,
} from '@/pages/xray/routing/filter-rules';
import type { RuleObject } from '@/schemas/routing';

const policyRules = (id: string, domains: string[], ips: string[] = []) => makeInboundFilterRules({
  id,
  inboundTags: ['in-a', 'in-b'],
  domains,
  ips,
  outboundTag: 'blocked',
});

describe('inbound filter routing rules', () => {
  it('requires sniffing only for domain filters and validates HTTP, TLS and QUIC', () => {
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

  it('creates separate domain and IP rules, scoped to exact inbounds, so either can block', () => {
    const rules = makeInboundFilterRules({
      id: 'test-id',
      inboundTags: ['in-a', 'in-b', 'in-a'],
      domains: ['geosite:category-porn', 'domain:example.com'],
      ips: ['geoip:private', '203.0.113.4'],
      outboundTag: 'blocked',
    });

    expect(rules).toEqual([
      {
        type: 'field',
        ruleTag: `${INBOUND_FILTER_RULE_TAG_PREFIX}test-id`,
        inboundTag: ['in-a', 'in-b'],
        domain: ['geosite:category-porn', 'domain:example.com'],
        outboundTag: 'blocked',
      },
      {
        type: 'field',
        ruleTag: `${INBOUND_FILTER_RULE_TAG_PREFIX}test-id:ip`,
        inboundTag: ['in-a', 'in-b'],
        ip: ['geoip:private', '203.0.113.4'],
        outboundTag: 'blocked',
      },
    ]);
    expect(isInboundFilterRule(rules[0])).toBe(true);
    expect(isInboundFilterRule({ type: 'field', outboundTag: 'blocked' })).toBe(false);
    expect(getInboundFilterPolicyId(rules[1].ruleTag)).toBe('test-id');
  });

  it('groups legacy domain policies and new domain/IP rules into one policy', () => {
    const [domain, ip] = policyRules('same', ['geosite:category-porn'], ['geoip:private']);
    expect(groupInboundFilterPolicies([domain, ip])).toEqual([{
      id: 'same',
      ruleTag: `${INBOUND_FILTER_RULE_TAG_PREFIX}same`,
      inboundTags: ['in-a', 'in-b'],
      domains: ['geosite:category-porn'],
      ips: ['geoip:private'],
      outboundTag: 'blocked',
    }]);
  });

  it('inserts policies before catch-all rules while keeping API routing first', () => {
    const apiRule: RuleObject = { type: 'field', inboundTag: ['api'], outboundTag: 'api' };
    const catchAll: RuleObject = { type: 'field', network: 'tcp,udp', outboundTag: 'proxy' };
    const newRules = policyRules('new', ['geosite:category-ads-all'], ['geoip:private']);

    expect(upsertInboundFilterRules([apiRule, catchAll], newRules)).toEqual([apiRule, ...newRules, catchAll]);
  });

  it('updates managed rules without duplication and restores their priority', () => {
    const original = policyRules('same', ['geosite:category-porn'], ['geoip:private']);
    const apiRule: RuleObject = { type: 'field', inboundTag: ['api'], outboundTag: 'api' };
    const later: RuleObject = { type: 'field', outboundTag: 'proxy', domain: ['geosite:google'] };
    const updated = makeInboundFilterRules({
      id: 'same', inboundTags: ['in-b'], domains: ['geosite:category-ads-all'], ips: [], outboundTag: 'blocked',
    });

    expect(upsertInboundFilterRules([apiRule, later, ...original], updated)).toEqual([apiRule, ...updated, later]);
  });

  it('removes all rules belonging to only the selected managed policy', () => {
    const managed = policyRules('remove-me', ['geosite:category-porn'], ['geoip:private']);
    const otherManaged = policyRules('keep-me', ['geosite:youtube']);
    const userRule: RuleObject = { type: 'field', outboundTag: 'blocked', domain: ['example.net'] };
    expect(removeInboundFilterRule([...managed, ...otherManaged, userRule], managed[1].ruleTag!))
      .toEqual([...otherManaged, userRule]);
  });

  it('normalizes domains and explicit Xray expressions', () => {
    expect(normalizeCustomFilterDomains('example.com\nhttps://ads.example.net/path\nfull:exact.example.org\nregexp:.*\\.bad$\ngeosite:youtube')).toEqual({
      domains: ['domain:example.com', 'domain:ads.example.net', 'full:exact.example.org', 'regexp:.*\\.bad$', 'geosite:youtube'],
      invalid: [],
    });
  });

  it('rejects malformed custom domain entries', () => {
    expect(normalizeCustomFilterDomains('https://\nnot a domain\nfull:\nexample.com/path')).toEqual({
      domains: [], invalid: ['https://', 'not a domain', 'full:', 'example.com/path'],
    });
  });

  it('accepts IPv4/IPv6 addresses, CIDRs and geoip/ext expressions', () => {
    expect(normalizeCustomFilterIPs('203.0.113.8\n10.0.0.0/8\n2001:db8::1\n2001:db8::/32\ngeoip:private\next:geoip_IR.dat:ir')).toEqual({
      ips: ['203.0.113.8', '10.0.0.0/8', '2001:db8::1', '2001:db8::/32', 'geoip:private', 'ext:geoip_IR.dat:ir'],
      invalid: [],
    });
  });

  it('rejects malformed IPs, CIDRs and expressions', () => {
    expect(normalizeCustomFilterIPs('256.1.1.1\n10.0.0.0/33\n2001:db8::/129\ngeoip:\n10.0.0.1/8/2')).toEqual({
      ips: [], invalid: ['256.1.1.1', '10.0.0.0/33', '2001:db8::/129', 'geoip:', '10.0.0.1/8/2'],
    });
  });

  it('finds only unscoped blackhole IP/domain rules and marks safe migrations', () => {
    const migratable: RuleObject = { type: 'field', outboundTag: 'blocked', ip: ['geoip:private'] };
    const combined: RuleObject = { type: 'field', outboundTag: 'blocked', ip: ['geoip:private'], domain: ['geosite:youtube'] };
    const scoped: RuleObject = { type: 'field', inboundTag: ['in-a'], outboundTag: 'blocked', domain: ['domain:example.com'] };
    const nonBlackhole: RuleObject = { type: 'field', outboundTag: 'direct', domain: ['domain:example.net'] };
    const found = findLegacyGlobalBlockRules([migratable, combined, scoped, nonBlackhole], new Set(['blocked']));
    expect(found.map(({ index, migratable: canMigrate }) => [index, canMigrate])).toEqual([[0, true], [1, false]]);
  });

  it('removes exactly one legacy rule matching the migration signature', () => {
    const first: RuleObject = { type: 'field', outboundTag: 'blocked', ip: ['geoip:private'] };
    const second: RuleObject = { type: 'field', outboundTag: 'blocked', ip: ['geoip:cn'] };
    expect(removeLegacyGlobalBlockRule([first, second], JSON.stringify(first))).toEqual([second]);
  });

  it('uses an existing blackhole outbound and ignores non-blackhole tags', () => {
    expect(findBlackholeOutboundTag([
      { tag: 'blocked', protocol: 'freedom' },
      { tag: 'drop', protocol: 'blackhole' },
    ])).toBe('drop');
    expect(findBlackholeOutboundTag([{ tag: 'proxy', protocol: 'freedom' }])).toBeNull();
  });
});
