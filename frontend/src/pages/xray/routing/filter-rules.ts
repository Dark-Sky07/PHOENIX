import { canEnableSniffing } from '@/lib/xray/protocol-capabilities';
import type { RuleObject } from '@/schemas/routing';

export const INBOUND_FILTER_RULE_TAG_PREFIX = 'phoenix-inbound-filter:v1:';
const IP_RULE_TAG_SUFFIX = ':ip';

export interface InboundFilterPolicy {
  id: string;
  ruleTag: string;
  inboundTags: string[];
  domains: string[];
  ips: string[];
  outboundTag: string;
}

export interface InboundFilterRuleInput {
  id: string;
  inboundTags: string[];
  domains: string[];
  ips?: string[];
  outboundTag: string;
}

export interface LegacyGlobalBlockRule {
  index: number;
  signature: string;
  rule: RuleObject;
  migratable: boolean;
}

export function isInboundReadyForDomainFilters(inbound: {
  protocol?: string;
  sniffingEnabled?: boolean;
  sniffingDestOverride?: string[];
} | undefined): boolean {
  if (!inbound || !inbound.sniffingEnabled || !canEnableSniffing({ protocol: inbound.protocol || '' })) return false;
  const targets = new Set(inbound.sniffingDestOverride || []);
  return ['http', 'tls', 'quic'].every((target) => targets.has(target));
}

export function isInboundFilterRule(rule: RuleObject): boolean {
  return typeof rule.ruleTag === 'string' && rule.ruleTag.startsWith(INBOUND_FILTER_RULE_TAG_PREFIX);
}

export function getInboundFilterPolicyId(ruleTag: string | undefined): string | null {
  if (!ruleTag?.startsWith(INBOUND_FILTER_RULE_TAG_PREFIX)) return null;
  const id = ruleTag.slice(INBOUND_FILTER_RULE_TAG_PREFIX.length);
  return id.endsWith(IP_RULE_TAG_SUFFIX) ? id.slice(0, -IP_RULE_TAG_SUFFIX.length) : id;
}

function makeManagedRule(
  input: InboundFilterRuleInput,
  kind: 'domain' | 'ip',
): RuleObject {
  return {
    type: 'field',
    ruleTag: `${INBOUND_FILTER_RULE_TAG_PREFIX}${input.id}${kind === 'ip' ? IP_RULE_TAG_SUFFIX : ''}`,
    inboundTag: [...new Set(input.inboundTags)],
    [kind]: [...new Set(kind === 'domain' ? input.domains : input.ips || [])],
    outboundTag: input.outboundTag,
  } as RuleObject;
}

/** Domain and IP matches are separate Xray rules so either match blocks. */
export function makeInboundFilterRules(input: InboundFilterRuleInput): RuleObject[] {
  const rules: RuleObject[] = [];
  const domains = [...new Set(input.domains)];
  const ips = [...new Set(input.ips || [])];
  if (domains.length > 0) rules.push(makeManagedRule({ ...input, domains, ips }, 'domain'));
  if (ips.length > 0) rules.push(makeManagedRule({ ...input, domains, ips }, 'ip'));
  return rules;
}

export function groupInboundFilterPolicies(rules: RuleObject[]): InboundFilterPolicy[] {
  const policies = new Map<string, InboundFilterPolicy>();
  for (const rule of rules) {
    const id = getInboundFilterPolicyId(rule.ruleTag);
    if (!id) continue;
    const policy = policies.get(id) || {
      id,
      ruleTag: `${INBOUND_FILTER_RULE_TAG_PREFIX}${id}`,
      inboundTags: [],
      domains: [],
      ips: [],
      outboundTag: rule.outboundTag || '',
    };
    policy.inboundTags = [...new Set([...policy.inboundTags, ...(rule.inboundTag || [])])];
    policy.domains = [...new Set([...policy.domains, ...(Array.isArray(rule.domain) ? rule.domain : [])])];
    policy.ips = [...new Set([...policy.ips, ...(Array.isArray(rule.ip) ? rule.ip : [])])];
    if (!policy.outboundTag && rule.outboundTag) policy.outboundTag = rule.outboundTag;
    policies.set(id, policy);
  }
  return [...policies.values()];
}

/**
 * Put managed block rules ahead of user catch-all rules without disturbing
 * the panel's API route. All components of one policy are inserted together.
 */
export function upsertInboundFilterRules(
  rules: RuleObject[],
  nextRules: RuleObject[],
): RuleObject[] {
  if (nextRules.length === 0) return [...rules];
  const id = getInboundFilterPolicyId(nextRules[0].ruleTag);
  if (!id) return [...rules];
  const next = rules.filter((rule) => getInboundFilterPolicyId(rule.ruleTag) !== id);

  let insertAt = 0;
  for (let index = 0; index < next.length; index += 1) {
    const rule = next[index];
    if (rule.inboundTag?.includes('api') || rule.outboundTag === 'api') {
      insertAt = index + 1;
    }
  }
  next.splice(insertAt, 0, ...nextRules);
  return next;
}

export function upsertInboundFilterRule(
  rules: RuleObject[],
  nextRule: RuleObject,
): RuleObject[] {
  return upsertInboundFilterRules(rules, [nextRule]);
}

export function removeInboundFilterRule(rules: RuleObject[], ruleTag: string): RuleObject[] {
  const id = getInboundFilterPolicyId(ruleTag);
  if (!id) return [...rules];
  return rules.filter((rule) => getInboundFilterPolicyId(rule.ruleTag) !== id);
}

export function normalizeCustomFilterDomains(input: string): { domains: string[]; invalid: string[] } {
  const entries = input
    .split(/[\n,]/)
    .map((entry) => entry.trim())
    .filter(Boolean);
  const domains: string[] = [];
  const invalid: string[] = [];

  for (const entry of entries) {
    const value = entry.startsWith('http://') || entry.startsWith('https://')
      ? (() => {
          try {
            return new URL(entry).hostname;
          } catch {
            return '';
          }
        })()
      : entry;
    if (!value || /\s/.test(value)) {
      invalid.push(entry);
      continue;
    }
    const expression = value.match(/^(domain|full|keyword|regexp|geosite|ext):(.*)$/i);
    if (expression) {
      const [, prefix, body] = expression;
      if (!body || (prefix.toLowerCase() !== 'regexp' && /[/?#]/.test(body))) {
        invalid.push(entry);
        continue;
      }
      domains.push(value);
      continue;
    }
    if (value.includes(':') || /[/?#]/.test(value)) {
      invalid.push(entry);
      continue;
    }
    domains.push(`domain:${value}`);
  }

  return { domains: [...new Set(domains)], invalid };
}

function isIPv4(address: string): boolean {
  const octets = address.split('.');
  return octets.length === 4 && octets.every((octet) => {
    if (!/^\d{1,3}$/.test(octet)) return false;
    const value = Number(octet);
    return value >= 0 && value <= 255;
  });
}

function isIPv6(address: string): boolean {
  if (!address.includes(':') || !/^[0-9a-f:.]+$/i.test(address)) return false;
  try {
    return new URL(`http://[${address}]/`).hostname.startsWith('[');
  } catch {
    return false;
  }
}

function isIpOrCidr(value: string): boolean {
  const parts = value.split('/');
  if (parts.length > 2) return false;
  const address = parts[0];
  const ipv4 = isIPv4(address);
  const ipv6 = !ipv4 && isIPv6(address);
  if (!ipv4 && !ipv6) return false;
  if (parts.length === 1) return true;
  if (!/^\d{1,3}$/.test(parts[1])) return false;
  const prefix = Number(parts[1]);
  return prefix >= 0 && prefix <= (ipv4 ? 32 : 128);
}

export function normalizeCustomFilterIPs(input: string): { ips: string[]; invalid: string[] } {
  const entries = input
    .split(/[\n,]/)
    .map((entry) => entry.trim())
    .filter(Boolean);
  const ips: string[] = [];
  const invalid: string[] = [];

  for (const entry of entries) {
    if (/\s/.test(entry)) {
      invalid.push(entry);
      continue;
    }
    const expression = entry.match(/^!?((geoip|ext):)(.+)$/i);
    if (expression) {
      const body = expression[3];
      if (!body || /[/?#]/.test(body)) invalid.push(entry);
      else ips.push(entry);
      continue;
    }
    if (!isIpOrCidr(entry)) {
      invalid.push(entry);
      continue;
    }
    ips.push(entry);
  }

  return { ips: [...new Set(ips)], invalid };
}

export function findLegacyGlobalBlockRules(
  rules: RuleObject[],
  blackholeTags: Set<string>,
): LegacyGlobalBlockRule[] {
  return rules.flatMap((rule, index) => {
    if (
      isInboundFilterRule(rule) ||
      !rule.outboundTag ||
      !blackholeTags.has(rule.outboundTag) ||
      (rule.inboundTag && rule.inboundTag.length > 0)
    ) return [];

    const hasIp = Array.isArray(rule.ip) && rule.ip.length > 0;
    const hasDomain = Array.isArray(rule.domain) && rule.domain.length > 0;
    if (!hasIp && !hasDomain) return [];
    const extraKeys = Object.keys(rule).filter(
      (key) => !['type', 'outboundTag', 'ip', 'domain', 'ruleTag'].includes(key),
    );
    const migratable = extraKeys.length === 0 && hasIp !== hasDomain;
    return [{ index, signature: JSON.stringify(rule), rule, migratable }];
  });
}

export function removeLegacyGlobalBlockRule(rules: RuleObject[], signature: string): RuleObject[] {
  const index = rules.findIndex((rule) => JSON.stringify(rule) === signature);
  if (index < 0) return [...rules];
  return rules.filter((_, ruleIndex) => ruleIndex !== index);
}

export function findBlackholeOutboundTag(outbounds: Array<{ tag?: string; protocol?: string }>): string | null {
  const taggedBlackhole = outbounds.find((outbound) => outbound.tag === 'blocked' && outbound.protocol === 'blackhole');
  if (taggedBlackhole?.tag) return taggedBlackhole.tag;
  const anyBlackhole = outbounds.find((outbound) => outbound.protocol === 'blackhole' && outbound.tag);
  return anyBlackhole?.tag || null;
}
