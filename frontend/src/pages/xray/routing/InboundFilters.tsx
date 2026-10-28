import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Alert, Button, Card, Input, Modal, Select, Space, Tag, Typography } from 'antd';
import { DeleteOutlined, EditOutlined, FilterOutlined, PlusOutlined } from '@ant-design/icons';

import { useInboundOptions } from '@/api/queries/useInboundOptions';
import { canEnableSniffing } from '@/lib/xray/protocol-capabilities';
import type { XraySettingsValue, SetTemplate } from '@/hooks/useXraySetting';
import type { RuleObject } from '@/schemas/routing';
import { buildRemarkByTag, formatInboundTag } from './helpers';
import {
  findBlackholeOutboundTag,
  INBOUND_FILTER_RULE_TAG_PREFIX,
  isInboundReadyForDomainFilters,
  isInboundFilterRule,
  makeInboundFilterRule,
  normalizeCustomFilterDomains,
  removeInboundFilterRule,
  upsertInboundFilterRule,
} from './filter-rules';

const CATEGORY_OPTIONS = [
  { key: 'ads', value: 'geosite:category-ads-all' },
  { key: 'adult', value: 'geosite:category-porn' },
  { key: 'adsIran', value: 'ext:geosite_IR.dat:category-ads-all' },
  { key: 'adsRussia', value: 'ext:geosite_RU.dat:category-ads-all' },
  { key: 'malwareIran', value: 'ext:geosite_IR.dat:malware' },
  { key: 'phishingIran', value: 'ext:geosite_IR.dat:phishing' },
  { key: 'cryptominersIran', value: 'ext:geosite_IR.dat:cryptominers' },
] as const;

interface InboundFiltersProps {
  templateSettings: XraySettingsValue | null;
  setTemplateSettings: SetTemplate;
}

interface PolicyDraft {
  ruleTag?: string;
  inboundTag: string[];
  domain: string[];
}

function newPolicyId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
}

function ruleDomainList(rule: RuleObject): string[] {
  return Array.isArray(rule.domain) ? rule.domain : [];
}

export default function InboundFilters({
  templateSettings,
  setTemplateSettings,
}: InboundFiltersProps) {
  const { t } = useTranslation();
  const { data: inboundOptions, isLoading: loadingInbounds, isError: inboundOptionsError } = useInboundOptions();
  const [draft, setDraft] = useState<PolicyDraft | null>(null);
  const [customDomainsText, setCustomDomainsText] = useState('');
  const [invalidDomains, setInvalidDomains] = useState<string[]>([]);
  const [modal, modalContextHolder] = Modal.useModal();

  const rules = useMemo(
    () => (templateSettings?.routing?.rules || []) as RuleObject[],
    [templateSettings?.routing?.rules],
  );
  const policies = useMemo(() => rules.filter(isInboundFilterRule), [rules]);
  const remarkByTag = useMemo(() => buildRemarkByTag(inboundOptions || []), [inboundOptions]);
  const inboundsByTag = useMemo(
    () => new Map((inboundOptions || []).map((item) => [item.tag || '', item])),
    [inboundOptions],
  );
  const knownTags = useMemo(
    () => [...new Set((inboundOptions || []).map((item) => item.tag).filter((tag): tag is string => Boolean(tag)))],
    [inboundOptions],
  );

  const categoryValues = useMemo(() => new Set(CATEGORY_OPTIONS.map((item) => item.value)), []);
  const selectedCategoryValues = useMemo(
    () => (draft?.domain || []).filter((domain) => categoryValues.has(domain as typeof CATEGORY_OPTIONS[number]['value'])),
    [categoryValues, draft?.domain],
  );
  const sniffingDisabledTags = useMemo(
    () => (draft?.inboundTag || []).filter((tag) => !isInboundReadyForDomainFilters(inboundsByTag.get(tag))),
    [draft?.inboundTag, inboundsByTag],
  );

  function openNewPolicy() {
    setInvalidDomains([]);
    setCustomDomainsText('');
    setDraft({ inboundTag: [], domain: [] });
  }

  function openEditPolicy(rule: RuleObject) {
    setInvalidDomains([]);
    const domain = ruleDomainList(rule);
    setCustomDomainsText(domain
      .filter((entry) => !categoryValues.has(entry as typeof CATEGORY_OPTIONS[number]['value']))
      .join('\n'));
    setDraft({ ruleTag: rule.ruleTag, inboundTag: rule.inboundTag || [], domain });
  }

  function closeEditor() {
    setDraft(null);
    setInvalidDomains([]);
  }

  function updateDraftDomains(nextCategories: string[]) {
    const parsed = normalizeCustomFilterDomains(customDomainsText);
    const combined = [...nextCategories, ...parsed.domains];
    setDraft((previous) => previous ? { ...previous, domain: combined } : previous);
  }

  function savePolicy() {
    if (!draft) return;
    const custom = normalizeCustomFilterDomains(customDomainsText);
    setInvalidDomains(custom.invalid);
    const domains = [...new Set([...selectedCategoryValues, ...custom.domains])];
    if (draft.inboundTag.length === 0 || domains.length === 0 || custom.invalid.length > 0 || sniffingDisabledTags.length > 0) return;

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

      const policyId = draft.ruleTag?.startsWith(INBOUND_FILTER_RULE_TAG_PREFIX)
        ? draft.ruleTag.slice(INBOUND_FILTER_RULE_TAG_PREFIX.length)
        : newPolicyId();
      const rule = makeInboundFilterRule({
        id: policyId,
        inboundTags: draft.inboundTag,
        domains,
        outboundTag,
      });
      next.routing.rules = upsertInboundFilterRule(next.routing.rules as RuleObject[], rule);
      return next;
    });
    closeEditor();
  }

  function deletePolicy(rule: RuleObject) {
    if (!rule.ruleTag) return;
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
          next.routing.rules = removeInboundFilterRule(next.routing.rules as RuleObject[], rule.ruleTag!);
        }
        return next;
      }),
    });
  }

  const isEditing = Boolean(draft?.ruleTag);

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
      />

      <Button type="primary" icon={<PlusOutlined />} onClick={openNewPolicy}>
        {t('pages.xray.inboundFilters.addPolicy')}
      </Button>

      {policies.length === 0 ? (
        <Typography.Text type="secondary">{t('pages.xray.inboundFilters.empty')}</Typography.Text>
      ) : (
        <Space orientation="vertical" size="small" style={{ width: '100%' }}>
          {policies.map((rule) => {
            const categories = ruleDomainList(rule);
            const sniffingDisabledForRule = (rule.inboundTag || []).filter(
              (tag) => !isInboundReadyForDomainFilters(inboundsByTag.get(tag)),
            );
            return (
              <Card
                key={rule.ruleTag}
                size="small"
                title={<Space><FilterOutlined />{t('pages.xray.inboundFilters.policyTitle')}</Space>}
                extra={(
                  <Space size="small">
                    <Button aria-label={t('edit')} icon={<EditOutlined />} size="small" onClick={() => openEditPolicy(rule)} />
                    <Button aria-label={t('delete')} danger icon={<DeleteOutlined />} size="small" onClick={() => deletePolicy(rule)} />
                  </Space>
                )}
              >
                <Space wrap>
                  {(rule.inboundTag || []).map((tag) => (
                    <Tag color="blue" key={tag}>{formatInboundTag(tag, remarkByTag)}</Tag>
                  ))}
                  {categories.map((value) => {
                    const preset = CATEGORY_OPTIONS.find((item) => item.value === value);
                    const label = preset
                      ? t(`pages.xray.inboundFilters.categories.${preset.key}`)
                      : value;
                    return <Tag color="volcano" key={value}>{label}</Tag>;
                  })}
                  <Tag color="red">{rule.outboundTag || '—'}</Tag>
                </Space>
                {sniffingDisabledForRule.length > 0 && (
                  <Alert
                    className="mt-8"
                    type="warning"
                    showIcon
                    title={t('pages.xray.inboundFilters.enableSniffingFirst', {
                      tags: sniffingDisabledForRule.map((tag) => formatInboundTag(tag, remarkByTag)).join(', '),
                    })}
                  />
                )}
              </Card>
            );
          })}
        </Space>
      )}

      <Modal
        open={draft !== null}
        title={isEditing ? t('pages.xray.inboundFilters.editPolicy') : t('pages.xray.inboundFilters.addPolicy')}
        okText={t('pages.xray.save')}
        cancelText={t('close')}
        width={680}
        onOk={savePolicy}
        onCancel={closeEditor}
        okButtonProps={{
          disabled: !draft || draft.inboundTag.length === 0 || draft.domain.length === 0 ||
            sniffingDisabledTags.length > 0 || invalidDomains.length > 0,
        }}
      >
        <Space orientation="vertical" size="middle" style={{ width: '100%' }}>
          {inboundOptionsError && <Alert type="error" showIcon title={t('pages.xray.inboundFilters.loadInboundsFailed')} />}
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
                  label: `${formatInboundTag(tag, remarkByTag)}${sniffingReady ? '' : ` — ${t('pages.xray.inboundFilters.sniffingRequired')}`}`,
                  disabled: !supported,
                };
              })}
              onChange={(tags: string[]) => setDraft((previous) => previous ? { ...previous, inboundTag: tags } : previous)}
            />
          </div>

          {draft && draft.inboundTag.length > 0 && sniffingDisabledTags.length > 0 && (
            <Alert
              type="warning"
              showIcon
              title={t('pages.xray.inboundFilters.enableSniffingFirst', {
                tags: sniffingDisabledTags.map((tag) => formatInboundTag(tag, remarkByTag)).join(', '),
              })}
            />
          )}

          <div>
            <Typography.Text strong>{t('pages.xray.inboundFilters.selectCategories')}</Typography.Text>
            <Select
              mode="multiple"
              value={selectedCategoryValues}
              style={{ width: '100%', marginTop: 6 }}
              placeholder={t('pages.xray.inboundFilters.selectCategoriesPlaceholder')}
              options={CATEGORY_OPTIONS.map((item) => ({
                value: item.value,
                label: t(`pages.xray.inboundFilters.categories.${item.key}`),
              }))}
              onChange={(values: string[]) => updateDraftDomains(values)}
            />
          </div>

          <div>
            <Typography.Text strong>{t('pages.xray.inboundFilters.customDomains')}</Typography.Text>
            <Input.TextArea
              rows={3}
              value={customDomainsText}
              placeholder={t('pages.xray.inboundFilters.customDomainsPlaceholder')}
              onChange={(event) => {
                setCustomDomainsText(event.target.value);
                const normalized = normalizeCustomFilterDomains(event.target.value);
                setInvalidDomains(normalized.invalid);
                const categories = (draft?.domain || []).filter((domain) => categoryValues.has(domain as typeof CATEGORY_OPTIONS[number]['value']));
                setDraft((previous) => previous ? { ...previous, domain: [...categories, ...normalized.domains] } : previous);
              }}
            />
            {invalidDomains.length > 0 && (
              <Typography.Text type="danger">
                {t('pages.xray.inboundFilters.invalidDomains', { domains: invalidDomains.join(', ') })}
              </Typography.Text>
            )}
          </div>
        </Space>
      </Modal>
    </Space>
  );
}
