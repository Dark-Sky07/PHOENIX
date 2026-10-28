import { canEnableSniffing } from '@/lib/xray/protocol-capabilities';
import type { RuleObject } from '@/schemas/routing';

export const INBOUND_FILTER_RULE_TAG_PREFIX = 'phoenix-inbound-filter:v1:';

export function isInboundReadyForDomainFilters(inbound: {
  protocol?: string;
  sniffingEnabled?: boolean;
  sniffingDestOverride?: string[];
} | undefined): boolean {
  if (!inbound || !inbound.sniffingEnabled || !canEnableSniffing({ protocol: inbound.protocol || '' })) return false;
  const targets = new Set(inbound.sniffingDestOverride || []);
  return ['http', 'tls', 'quic'].every((target) => targets.has(target));
}

export interface InboundFilterRuleInput {
  id: string;
  inboundTags: string[];
  domains: string[];
  outboundTag: string;
}

export function isInboundFilterRule(rule: RuleObject): boolean {
  return typeof rule.ruleTag === 'string' && rule.ruleTag.startsWith(INBOUND_FILTER_RULE_TAG_PREFIX);
}

export function makeInboundFilterRule(input: InboundFilterRuleInput): RuleObject {
  return {
    type: 'field',
    ruleTag: `${INBOUND_FILTER_RULE_TAG_PREFIX}${input.id}`,
    inboundTag: [...new Set(input.inboundTags)],
    domain: [...new Set(input.domains)],
    outboundTag: input.outboundTag,
  };
}

/**
 * Put managed block rules ahead of user catch-all rules without disturbing
 * the panel's API route. New and edited policies are inserted immediately
 * after all existing API-specific rules.
 */
export function upsertInboundFilterRule(
  rules: RuleObject[],
  nextRule: RuleObject,
): RuleObject[] {
  const next = [...rules];
  const existingIndex = next.findIndex(
    (rule) => rule.ruleTag === nextRule.ruleTag && isInboundFilterRule(rule),
  );
  if (existingIndex >= 0) next.splice(existingIndex, 1);

  let insertAt = 0;
  for (let index = 0; index < next.length; index += 1) {
    const rule = next[index];
    if (rule.inboundTag?.includes('api') || rule.outboundTag === 'api') {
      insertAt = index + 1;
    }
  }
  next.splice(insertAt, 0, nextRule);
  return next;
}

export function removeInboundFilterRule(rules: RuleObject[], ruleTag: string): RuleObject[] {
  return rules.filter((rule) => !(isInboundFilterRule(rule) && rule.ruleTag === ruleTag));
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

export function findBlackholeOutboundTag(outbounds: Array<{ tag?: string; protocol?: string }>): string | null {
  const taggedBlackhole = outbounds.find((outbound) => outbound.tag === 'blocked' && outbound.protocol === 'blackhole');
  if (taggedBlackhole?.tag) return taggedBlackhole.tag;
  const anyBlackhole = outbounds.find((outbound) => outbound.protocol === 'blackhole' && outbound.tag);
  return anyBlackhole?.tag || null;
}
