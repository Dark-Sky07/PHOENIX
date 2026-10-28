import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Alert, Button, Card, Input, Modal, Select, Space, Tag, Typography } from 'antd';
import { DeleteOutlined, EditOutlined, FilterOutlined, PlusOutlined, SwapOutlined } from '@ant-design/icons';

import { useInboundOptions } from '@/api/queries/useInboundOptions';
import { canEnableSniffing } from '@/lib/xray/protocol-capabilities';
import type { XraySettingsValue, SetTemplate } from '@/hooks/useXraySetting';
import type { RuleObject } from '@/schemas/routing';
import { buildRemarkByTag, formatInboundTag } from './helpers';
import {
  DOMAIN_PRESET_VALUES,
  FILTER_PRESETS,
  FILTER_PRESET_GROUPS,
  IP_PRESET_VALUES,
  type FilterPresetKind,
} from './filter-presets';
import {
  findBlackholeOutboundTag,
  findLegacyGlobalBlockRules,
  groupInboundFilterPolicies,
  INBOUND_FILTER_RULE_TAG_PREFIX,
  isInboundReadyForDomainFilters,
  makeInboundFilterRules,
  normalizeCustomFilterDomains,
  normalizeCustomFilterIPs,
  removeInboundFilterRule,
  removeLegacyGlobalBlockRule,
  upsertInboundFilterRules,
} from './filter-rules';

interface InboundFiltersProps {
  templateSettings: XraySettingsValue | null;
  setTemplateSettings: SetTemplate;
}

interface PolicyDraft {
  ruleTag?: string;
  legacySignature?: string;
  inboundTag: string[];
  domain: string[];
  ip: string[];
}

function newPolicyId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
}

function presetOptions(kind: FilterPresetKind, t: (key: string) => string) {
  return FILTER_PRESET_GROUPS
    .map((group) => ({
      label: t(`pages.xray.inboundFilters.groups.${group}`),
      options: FILTER_PRESETS
        .filter((preset) => preset.kind === kind && preset.group === group)
        .map((preset) => ({
          value: preset.value,
          label: t(`pages.xray.inboundFilters.presets.${preset.key}`),
        })),
    }))
    .filter((group) => group.options.length > 0);
}

function presetLabel(value: string, kind: FilterPresetKind, t: (key: string) => string): string {
  const preset = FILTER_PRESETS.find((item) => item.kind === kind && item.value === value);
  return preset ? t(`pages.xray.inboundFilters.presets.${preset.key}`) : value;
}

function customText(values: string[], known: Set<string>): string {
  return values.filter((value) => !known.has(value)).join('\n');
}

export default function InboundFilters({
  templateSettings,
  setTemplateSettings,
}: InboundFiltersProps) {
  const { t } = useTranslation();
  const { data: inboundOptions, isLoading: loadingInbounds, isError: inboundOptionsError } = useInboundOptions();
  const [draft, setDraft] = useState<PolicyDraft | null>(null);
  const [customDomainsText, setCustomDomainsText] = useState('');
  const [customIPsText, setCustomIPsText] = useState('');
  const [invalidDomains, setInvalidDomains] = useState<string[]>([]);
  const [invalidIPs, setInvalidIPs] = useState<string[]>([]);
  const [modal, modalContextHolder] = Modal.useModal();

  const rules = useMemo(
    () => (templateSettings?.routing?.rules || []) as RuleObject[],
    [templateSettings?.routing?.rules],
  );
  const policies = useMemo(() => groupInboundFilterPolicies(rules), [rules]);
  const remarkByTag = useMemo(() => buildRemarkByTag(inboundOptions || []), [inboundOptions]);
  const inboundsByTag = useMemo(
    () => new Map((inboundOptions || []).map((item) => [item.tag || '', item])),
    [inboundOptions],
  );
  const knownTags = useMemo(
    () => [...new Set((inboundOptions || []).map((item) => item.tag).filter((tag): tag is string => Boolean(tag)))],
    [inboundOptions],
  );
  const blackholeTags = useMemo(
    () => new Set((templateSettings?.outbounds || [])
      .filter((outbound) => outbound.protocol === 'blackhole' && outbound.tag)
      .map((outbound) => outbound.tag as string)),
    [templateSettings?.outbounds],
  );
  const legacyRules = useMemo(
    () => findLegacyGlobalBlockRules(rules, blackholeTags),
    [blackholeTags, rules],
  );

  const selectedDomainPresetValues = useMemo(
    () => (draft?.domain || []).filter((domain) => DOMAIN_PRESET_VALUES.has(domain)),
    [draft?.domain],
  );
  const selectedIPPresetValues = useMemo(
    () => (draft?.ip || []).filter((ip) => IP_PRESET_VALUES.has(ip)),
    [draft?.ip],
  );
  const hasDomainFilters = (draft?.domain.length || 0) > 0;
  const sniffingDisabledTags = useMemo(
    () => hasDomainFilters
      ? (draft?.inboundTag || []).filter((tag) => !isInboundReadyForDomainFilters(inboundsByTag.get(tag)))
      : [],
    [draft?.inboundTag, hasDomainFilters, inboundsByTag],
  );
  const selectedCautions = useMemo(
    () => FILTER_PRESETS.filter((preset) => preset.caution &&
      (preset.kind === 'domain' ? selectedDomainPresetValues : selectedIPPresetValues).includes(preset.value)),
    [selectedDomainPresetValues, selectedIPPresetValues],
  );

  function resetEditorState() {
    setInvalidDomains([]);
    setInvalidIPs([]);
    setCustomDomainsText('');
    setCustomIPsText('');
  }

  function openNewPolicy() {
    resetEditorState();
    setDraft({ inboundTag: [], domain: [], ip: [] });
  }

  function openEditPolicy(policy: {
    ruleTag: string;
    inboundTags: string[];
    domains: string[];
    ips: string[];
  }) {
    resetEditorState();
    setCustomDomainsText(customText(policy.domains, DOMAIN_PRESET_VALUES));
    setCustomIPsText(customText(policy.ips, IP_PRESET_VALUES));
    setDraft({ ruleTag: policy.ruleTag, inboundTag: policy.inboundTags, domain: policy.domains, ip: policy.ips });
  }

  function openLegacyConversion(entry: (typeof legacyRules)[number]) {
    resetEditorState();
    const domains = Array.isArray(entry.rule.domain) ? entry.rule.domain : [];
    const ips = Array.isArray(entry.rule.ip) ? entry.rule.ip : [];
    setCustomDomainsText(customText(domains, DOMAIN_PRESET_VALUES));
    setCustomIPsText(customText(ips, IP_PRESET_VALUES));
    setDraft({ legacySignature: entry.signature, inboundTag: [], domain: domains, ip: ips });
  }

  function closeEditor() {
    setDraft(null);
    resetEditorState();
  }

  function updateDraftPresets(kind: FilterPresetKind, values: string[]) {
    if (kind === 'domain') {
      const custom = normalizeCustomFilterDomains(customDomainsText);
      setDraft((previous) => previous ? { ...previous, domain: [...values, ...custom.domains] } : previous);
      setInvalidDomains(custom.invalid);
    } else {
      const custom = normalizeCustomFilterIPs(customIPsText);
      setDraft((previous) => previous ? { ...previous, ip: [...values, ...custom.ips] } : previous);
      setInvalidIPs(custom.invalid);
    }
  }

  function savePolicy() {
    if (!draft) return;
    const customDomains = normalizeCustomFilterDomains(customDomainsText);
    const customIPs = normalizeCustomFilterIPs(customIPsText);
    setInvalidDomains(customDomains.invalid);
    setInvalidIPs(customIPs.invalid);
    const domains = [...new Set([...selectedDomainPresetValues, ...customDomains.domains])];
    const ips = [...new Set([...selectedIPPresetValues, ...customIPs.ips])];
    if (
      draft.inboundTag.length === 0 ||
      (domains.length === 0 && ips.length === 0) ||
      customDomains.invalid.length > 0 ||
      customIPs.invalid.length > 0 ||
      sniffingDisabledTags.length > 0
    ) return;

    const blackholeTag = findBlackholeOutboundTag(templateSettings?.outbounds || []);
    setTemplateSettings((previous) => {
      if (!previous) return previous;
      const next = JSON.parse(JSON.stringify(previous)) as XraySettingsValue;
      if (!next.routing) next.routing = { rules: [] };
      if (!Array.isArray(next.routing.rules)) next.routing.rules = [];
      if (!Array.isArray(next.outbounds)) next.outbounds = [];

      let outboundTag = blackholeTag;
      if (!outboundTag) {
        const used = new Set(next.outbounds.map((outbound) => outbound.tag).filter((tag): tag is string => Boolean(tag)));
        outboundTag = 'blocked';
        let suffix = 1;
        while (used.has(outboundTag)) {
          outboundTag = `phoenix-filter-blocked-${suffix}`;
          suffix += 1;
        }
        next.outbounds.push({ tag: outboundTag, protocol: 'blackhole', settings: {} });
      }

      if (draft.legacySignature) {
        next.routing.rules = removeLegacyGlobalBlockRule(next.routing.rules as RuleObject[], draft.legacySignature);
      }
      const policyId = draft.ruleTag?.startsWith(INBOUND_FILTER_RULE_TAG_PREFIX)
        ? draft.ruleTag.slice(INBOUND_FILTER_RULE_TAG_PREFIX.length).replace(/:ip$/, '')
        : newPolicyId();
      const nextPolicyRules = makeInboundFilterRules({
        id: policyId,
        inboundTags: draft.inboundTag,
        domains,
        ips,
        outboundTag,
      });
      next.routing.rules = upsertInboundFilterRules(next.routing.rules as RuleObject[], nextPolicyRules);
      return next;
    });
    closeEditor();
  }

  function deletePolicy(ruleTag: string) {
    modal.confirm({
      title: t('pages.xray.inboundFilters.deleteConfirmTitle'),
      content: t('pages.xray.inboundFilters.deleteConfirmDesc'),
      okText: t('delete'),
      okType: 'danger',
      cancelText: t('cancel'),
      onOk: () => setTemplateSettings((previous) => {
        if (!previous?.routing?.rules) return previous;
        const next = JSON.parse(JSON.stringify(previous)) as XraySettingsValue;
        if (next.routing?.rules) {
          next.routing.rules = removeInboundFilterRule(next.routing.rules as RuleObject[], ruleTag);
        }
        return next;
      }),
    });
  }

  function deleteLegacyRule(signature: string) {
    modal.confirm({
      title: t('pages.xray.inboundFilters.deleteGlobalConfirmTitle'),
      content: t('pages.xray.inboundFilters.deleteGlobalConfirmDesc'),
      okText: t('delete'),
      okType: 'danger',
      cancelText: t('cancel'),
      onOk: () => setTemplateSettings((previous) => {
        if (!previous?.routing?.rules) return previous;
        const next = JSON.parse(JSON.stringify(previous)) as XraySettingsValue;
        if (next.routing?.rules) {
          next.routing.rules = removeLegacyGlobalBlockRule(next.routing.rules as RuleObject[], signature);
        }
        return next;
      }),
    });
  }

  const isEditing = Boolean(draft?.ruleTag);
  const isMigratingGlobalRule = Boolean(draft?.legacySignature);

  return (
    <Space orientation="vertical" size="middle" style={{ width: '100%' }}>
      {modalContextHolder}
      <Alert
        type="info"
        showIcon
        title={t('pages.xray.inboundFilters.intro')}
        description={t('pages.xray.inboundFilters.sniffingHint')}
      />
      <Alert
        type="warning"
        showIcon
        title={t('pages.xray.inboundFilters.youtubeWarning')}
        description={t('pages.xray.inboundFilters.scopeWarning')}
      />

      <Button type="primary" icon={<PlusOutlined />} onClick={openNewPolicy}>
        {t('pages.xray.inboundFilters.addPolicy')}
      </Button>

      {policies.length === 0 ? (
        <Typography.Text type="secondary">{t('pages.xray.inboundFilters.empty')}</Typography.Text>
      ) : (
        <Space orientation="vertical" size="small" style={{ width: '100%' }}>
          {policies.map((policy) => (
            <Card
              key={policy.id}
              size="small"
              title={<Space><FilterOutlined />{t('pages.xray.inboundFilters.policyTitle')}</Space>}
              extra={(
                <Space size="small">
                  <Button
                    aria-label={t('edit')}
                    icon={<EditOutlined />}
                    size="small"
                    onClick={() => openEditPolicy(policy)}
                  />
                  <Button
                    aria-label={t('delete')}
                    danger
                    icon={<DeleteOutlined />}
                    size="small"
                    onClick={() => deletePolicy(policy.ruleTag)}
                  />
                </Space>
              )}
            >
              <Space wrap>
                {policy.inboundTags.map((tag) => (
                  <Tag color="blue" key={tag}>{formatInboundTag(tag, remarkByTag)}</Tag>
                ))}
                {policy.domains.map((value) => (
                  <Tag color="volcano" key={`domain:${value}`}>
                    {presetLabel(value, 'domain', t)}
                  </Tag>
                ))}
                {policy.ips.map((value) => (
                  <Tag color="gold" key={`ip:${value}`}>
                    {presetLabel(value, 'ip', t)}
                  </Tag>
                ))}
                <Tag color="red">{policy.outboundTag || '—'}</Tag>
              </Space>
              {policy.domains.length > 0 && policy.inboundTags.some(
                (tag) => !isInboundReadyForDomainFilters(inboundsByTag.get(tag)),
              ) && (
                <Alert
                  className="mt-8"
                  type="warning"
                  showIcon
                  title={t('pages.xray.inboundFilters.enableSniffingFirst', {
                    tags: policy.inboundTags
                      .filter((tag) => !isInboundReadyForDomainFilters(inboundsByTag.get(tag)))
                      .map((tag) => formatInboundTag(tag, remarkByTag)).join(', '),
                  })}
                />
              )}
            </Card>
          ))}
        </Space>
      )}

      {legacyRules.length > 0 && (
        <Card size="small" title={t('pages.xray.inboundFilters.legacyTitle')}>
          <Space orientation="vertical" size="small" style={{ width: '100%' }}>
            <Alert
              type="warning"
              showIcon
              title={t('pages.xray.inboundFilters.legacyGlobalWarning')}
            />
            {legacyRules.map((entry) => {
              const values = [
                ...(entry.rule.domain || []),
                ...(entry.rule.ip || []),
              ];
              return (
                <Card
                  key={`${entry.index}:${entry.signature}`}
                  size="small"
                  title={values.map((value) => <Tag key={value}>{value}</Tag>)}
                  extra={(
                    <Space size="small">
                      <Button
                        icon={<SwapOutlined />}
                        size="small"
                        disabled={!entry.migratable || loadingInbounds || inboundOptionsError}
                        onClick={() => openLegacyConversion(entry)}
                      >
                        {t('pages.xray.inboundFilters.convertLegacy')}
                      </Button>
                      <Button
                        aria-label={t('delete')}
                        danger
                        icon={<DeleteOutlined />}
                        size="small"
                        onClick={() => deleteLegacyRule(entry.signature)}
                      />
                    </Space>
                  )}
                >
                  {!entry.migratable && (
                    <Typography.Text type="secondary">
                      {t('pages.xray.inboundFilters.legacyCannotConvert')}
                    </Typography.Text>
                  )}
                </Card>
              );
            })}
          </Space>
        </Card>
      )}

      <Modal
        open={draft !== null}
        title={isMigratingGlobalRule
          ? t('pages.xray.inboundFilters.convertTitle')
          : isEditing ? t('pages.xray.inboundFilters.editPolicy') : t('pages.xray.inboundFilters.addPolicy')}
        okText={t('pages.xray.save')}
        cancelText={t('close')}
        width={720}
        onOk={savePolicy}
        onCancel={closeEditor}
        okButtonProps={{
          disabled: !draft || draft.inboundTag.length === 0 ||
            (draft.domain.length === 0 && draft.ip.length === 0) ||
            sniffingDisabledTags.length > 0 || invalidDomains.length > 0 || invalidIPs.length > 0,
        }}
      >
        <Space orientation="vertical" size="middle" style={{ width: '100%' }}>
          {inboundOptionsError && (
            <Alert type="error" showIcon title={t('pages.xray.inboundFilters.loadInboundsFailed')} />
          )}
          <div>
            <Typography.Text strong>{t('pages.xray.inboundFilters.selectInbounds')}</Typography.Text>
            <Select
              mode="multiple"
              loading={loadingInbounds}
              value={draft?.inboundTag || []}
              style={{ width: '100%', marginTop: 6 }}
              placeholder={t('pages.xray.inboundFilters.selectInboundsPlaceholder')}
              options={knownTags.map((tag) => {
                const inbound = inboundOptions?.find((item) => item.tag === tag);
                const supported = Boolean(inbound && canEnableSniffing({ protocol: inbound.protocol || '' }));
                const sniffingReady = isInboundReadyForDomainFilters(inbound);
                return {
                  value: tag,
                  label: `${formatInboundTag(tag, remarkByTag)}${!supported
                    ? ` — ${t('pages.xray.inboundFilters.routingUnsupported')}`
                    : draft?.domain.length && !sniffingReady
                      ? ` — ${t('pages.xray.inboundFilters.sniffingRequired')}`
                      : ''}`,
                  disabled: !supported,
                };
              })}
              onChange={(tags: string[]) => setDraft((previous) => previous ? { ...previous, inboundTag: tags } : previous)}
            />
          </div>

          {hasDomainFilters && draft && draft.inboundTag.length > 0 && sniffingDisabledTags.length > 0 && (
            <Alert
              type="warning"
              showIcon
              title={t('pages.xray.inboundFilters.enableSniffingFirst', {
                tags: sniffingDisabledTags.map((tag) => formatInboundTag(tag, remarkByTag)).join(', '),
              })}
            />
          )}

          <div>
            <Typography.Text strong>{t('pages.xray.inboundFilters.domainPackages')}</Typography.Text>
            <Select
              mode="multiple"
              value={selectedDomainPresetValues}
              style={{ width: '100%', marginTop: 6 }}
              placeholder={t('pages.xray.inboundFilters.selectPackagesPlaceholder')}
              options={presetOptions('domain', t)}
              onChange={(values: string[]) => updateDraftPresets('domain', values)}
            />
            <Input.TextArea
              className="mt-8"
              rows={3}
              value={customDomainsText}
              placeholder={t('pages.xray.inboundFilters.customDomainsPlaceholder')}
              onChange={(event) => {
                setCustomDomainsText(event.target.value);
                const normalized = normalizeCustomFilterDomains(event.target.value);
                setInvalidDomains(normalized.invalid);
                const presets = (draft?.domain || []).filter((value) => DOMAIN_PRESET_VALUES.has(value));
                setDraft((previous) => previous ? { ...previous, domain: [...presets, ...normalized.domains] } : previous);
              }}
            />
            {invalidDomains.length > 0 && (
              <Typography.Text type="danger">
                {t('pages.xray.inboundFilters.invalidDomains', { domains: invalidDomains.join(', ') })}
              </Typography.Text>
            )}
          </div>

          <div>
            <Typography.Text strong>{t('pages.xray.inboundFilters.ipPackages')}</Typography.Text>
            <Select
              mode="multiple"
              value={selectedIPPresetValues}
              style={{ width: '100%', marginTop: 6 }}
              placeholder={t('pages.xray.inboundFilters.selectPackagesPlaceholder')}
              options={presetOptions('ip', t)}
              onChange={(values: string[]) => updateDraftPresets('ip', values)}
            />
            <Input.TextArea
              className="mt-8"
              rows={3}
              value={customIPsText}
              placeholder={t('pages.xray.inboundFilters.customIPsPlaceholder')}
              onChange={(event) => {
                setCustomIPsText(event.target.value);
                const normalized = normalizeCustomFilterIPs(event.target.value);
                setInvalidIPs(normalized.invalid);
                const presets = (draft?.ip || []).filter((value) => IP_PRESET_VALUES.has(value));
                setDraft((previous) => previous ? { ...previous, ip: [...presets, ...normalized.ips] } : previous);
              }}
            />
            {invalidIPs.length > 0 && (
              <Typography.Text type="danger">
                {t('pages.xray.inboundFilters.invalidIPs', { ips: invalidIPs.join(', ') })}
              </Typography.Text>
            )}
          </div>

          {selectedCautions.length > 0 && (
            <Alert
              type="warning"
              showIcon
              title={t('pages.xray.inboundFilters.cautionWarning')}
              description={selectedCautions.some((preset) => preset.caution === 'large')
                ? t('pages.xray.inboundFilters.largeListWarning')
                : t('pages.xray.inboundFilters.broadPackageWarning')}
            />
          )}
        </Space>
      </Modal>
    </Space>
  );
}
