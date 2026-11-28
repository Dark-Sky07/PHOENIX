import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Button,
  Empty,
  Form,
  Input,
  InputNumber,
  Modal,
  Select,
  Space,
  Spin,
  Switch,
  Tabs,
  Tag,
  message,
} from 'antd';
import { ApiOutlined, CloudOutlined, GlobalOutlined, KeyOutlined, LaptopOutlined, SafetyCertificateOutlined, SafetyOutlined, TeamOutlined, UserOutlined } from '@ant-design/icons';
import { ClipboardManager, HttpUtil, RandomUtil } from '@/utils';
import type { AllSetting } from '@/models/setting';
import { SettingListItem } from '@/components/ui';
import { useMediaQuery } from '@/hooks/useMediaQuery';
import { catTabLabel } from './catTabLabel';
import TwoFactorModal from './TwoFactorModal';
import './SecurityTab.css';

interface ApiMsg<T = unknown> {
  success?: boolean;
  msg?: string;
  obj?: T;
}

interface ApiTokenRow {
  id: number;
  name: string;
  prefix?: string;
  scope?: string;
  scopes?: string[];
  enabled: boolean;
  expiresAt?: number;
  lastUsedAt?: number;
  createdAt: number;
}

interface SessionRow {
  id: number;
  ip?: string;
  userAgent?: string;
  createdAt?: number;
  lastSeenAt?: number;
  expiresAt?: number;
}

interface WebhookRow {
  id: number;
  name: string;
  url: string;
  events: string[];
  enabled: boolean;
}

interface DNSCertificateProfile {
  id: number;
  name: string;
  domains: string[];
  provider: 'manual' | 'cloudflare';
  hasProviderToken: boolean;
  email?: string;
  applyToPanel: boolean;
  enabled: boolean;
  lastIssuedAt?: number;
  lastError?: string;
  pendingAt?: number;
}

interface OIDCConfig {
  enabled: boolean;
  issuer: string;
  clientId: string;
  hasClientSecret: boolean;
  allowedEmailDomains: string[];
  autoProvision: boolean;
  defaultRole: 'viewer' | 'support' | 'admin';
  groupClaim: string;
  groupRoleMappings: Record<string, string>;
}

interface AdminRow {
  id: number;
  username: string;
  role: 'superadmin' | 'admin' | 'support' | 'viewer';
  permissions: string;
  disabled: boolean;
  note: string;
  createdAt?: number;
  lastLoginAt?: number;
}

const ADMIN_MODULES = [
  'inbounds',
  'clients',
  'resellers',
  'nodes',
  'xray',
  'settings',
  'server',
] as const;

interface SecurityTabProps {
  allSetting: AllSetting;
  updateSetting: (patch: Partial<AllSetting>) => void;
}

type TfaType = 'set' | 'confirm';

interface TfaState {
  open: boolean;
  title: string;
  description: string;
  token: string;
  type: TfaType;
  onConfirm: (success: boolean, code?: string) => void;
}

const TFA_INITIAL: TfaState = {
  open: false,
  title: '',
  description: '',
  token: '',
  type: 'set',
  onConfirm: () => {},
};

export default function SecurityTab({ allSetting, updateSetting }: SecurityTabProps) {
  const { t } = useTranslation();
  const { isMobile } = useMediaQuery();
  const [modal, modalContextHolder] = Modal.useModal();
  const [messageApi, messageContextHolder] = message.useMessage();

  const [tfa, setTfa] = useState<TfaState>(TFA_INITIAL);
  const [user, setUser] = useState({
    oldUsername: '',
    oldPassword: '',
    newUsername: '',
    newPassword: '',
  });
  const [updating, setUpdating] = useState(false);

  const [apiTokens, setApiTokens] = useState<ApiTokenRow[]>([]);
  const [apiTokensLoading, setApiTokensLoading] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  const [createName, setCreateName] = useState('');
  const [createScope, setCreateScope] = useState<'full' | 'read'>('full');
  const [createExpireDays, setCreateExpireDays] = useState<number>(0);
  const [creating, setCreating] = useState(false);
  const [createdToken, setCreatedToken] = useState<{ name: string; token: string } | null>(null);
  const [createPermissions, setCreatePermissions] = useState<string[]>([]);
  const [sessions, setSessions] = useState<SessionRow[]>([]);
  const [sessionsLoading, setSessionsLoading] = useState(false);
  const [recoveryCodes, setRecoveryCodes] = useState<string[] | null>(null);
  const [webhooks, setWebhooks] = useState<WebhookRow[]>([]);
  const [webhooksLoading, setWebhooksLoading] = useState(false);
  const [webhookName, setWebhookName] = useState('');
  const [webhookURL, setWebhookURL] = useState('');
  const [webhookEvents, setWebhookEvents] = useState<string[]>(['security.login.success', 'security.login.failure', 'audit.mutation']);
  const [createdWebhookSecret, setCreatedWebhookSecret] = useState<{ name: string; secret: string } | null>(null);
  const [reauthOpen, setReauthOpen] = useState(false);
  const [reauthPassword, setReauthPassword] = useState('');
  const [reauthCode, setReauthCode] = useState('');
  const [reauthing, setReauthing] = useState(false);
  const [dnsProfiles, setDnsProfiles] = useState<DNSCertificateProfile[]>([]);
  const [dnsProfilesLoading, setDnsProfilesLoading] = useState(false);
  const [dnsName, setDnsName] = useState('');
  const [dnsDomains, setDnsDomains] = useState('');
  const [dnsProvider, setDnsProvider] = useState<'manual' | 'cloudflare'>('manual');
  const [dnsToken, setDnsToken] = useState('');
  const [dnsEmail, setDnsEmail] = useState('');
  const [dnsChallenges, setDnsChallenges] = useState<{ record: string; value: string }[]>([]);
  const [oidcConfig, setOidcConfig] = useState<OIDCConfig>({ enabled: false, issuer: '', clientId: '', hasClientSecret: false, allowedEmailDomains: [], autoProvision: false, defaultRole: 'viewer', groupClaim: 'groups', groupRoleMappings: {} });
  const [oidcClientSecret, setOidcClientSecret] = useState('');
  const [oidcClearClientSecret, setOidcClearClientSecret] = useState(false);
  const [oidcDomains, setOidcDomains] = useState('');
  const [oidcMappings, setOidcMappings] = useState('');
  const [oidcSaving, setOidcSaving] = useState(false);

  const [admins, setAdmins] = useState<AdminRow[]>([]);
  const [adminsLoading, setAdminsLoading] = useState(false);
  const [adminModalOpen, setAdminModalOpen] = useState(false);
  const [adminSaving, setAdminSaving] = useState(false);
  const [editingAdmin, setEditingAdmin] = useState<{
    id: number;
    username: string;
    password: string;
    role: 'superadmin' | 'admin' | 'support' | 'viewer';
    permissions: string[];
    note: string;
    disabled: boolean;
  }>({
    id: 0,
    username: '',
    password: '',
    role: 'support',
    permissions: ['inbounds', 'clients'],
    note: '',
    disabled: false,
  });

  const openTfa = useCallback((opts: Omit<TfaState, 'open'>) => {
    setTfa({ ...opts, open: true });
  }, []);

  const onTfaConfirm = useCallback((success: boolean, code?: string) => {
    tfa.onConfirm(success, code);
  }, [tfa]);

  async function confirmRecentAuthentication() {
    if (!reauthPassword) {
      messageApi.error('Password is required');
      return;
    }
    setReauthing(true);
    try {
      const msg = await HttpUtil.post('/reauth', {
        password: reauthPassword,
        twoFactorCode: reauthCode,
      }) as ApiMsg;
      if (msg?.success) {
        setReauthPassword('');
        setReauthCode('');
        setReauthOpen(false);
      }
    } finally {
      setReauthing(false);
    }
  }

  function updateUserField<K extends keyof typeof user>(key: K, value: string) {
    setUser((prev) => ({ ...prev, [key]: value }));
  }

  const sendUpdateUser = useCallback(async () => {
    setUpdating(true);
    try {
      const msg = await HttpUtil.post('/panel/api/setting/updateUser', user) as ApiMsg;
      if (msg?.success) {
        await HttpUtil.post('/logout');
        const basePath = window.X_UI_BASE_PATH || '/';
        window.location.replace(basePath);
      }
    } finally {
      setUpdating(false);
    }
  }, [user]);

  function onUpdateUserClick() {
    if (allSetting.twoFactorEnable) {
      openTfa({
        title: t('pages.settings.security.twoFactorModalChangeCredentialsTitle'),
        description: t('pages.settings.security.twoFactorModalChangeCredentialsStep'),
        token: allSetting.twoFactorToken,
        type: 'confirm',
        onConfirm: (ok: boolean) => { if (ok) sendUpdateUser(); },
      });
    } else {
      sendUpdateUser();
    }
  }

  const loadApiTokens = useCallback(async () => {
    setApiTokensLoading(true);
    try {
      const msg = await HttpUtil.get('/panel/api/setting/apiTokens') as ApiMsg<ApiTokenRow[]>;
      if (msg?.success) setApiTokens(Array.isArray(msg.obj) ? msg.obj : []);
    } finally {
      setApiTokensLoading(false);
    }
  }, []);

  useEffect(() => {
     
    loadApiTokens();
  }, [loadApiTokens]);

  const loadSessions = useCallback(async () => {
    setSessionsLoading(true);
    try {
      const msg = await HttpUtil.get('/panel/api/setting/sessions') as ApiMsg<SessionRow[]>;
      if (msg?.success) setSessions(Array.isArray(msg.obj) ? msg.obj : []);
    } finally {
      setSessionsLoading(false);
    }
  }, []);

  const loadWebhooks = useCallback(async () => {
    setWebhooksLoading(true);
    try {
      const msg = await HttpUtil.get('/panel/api/setting/webhooks') as ApiMsg<WebhookRow[]>;
      if (msg?.success) setWebhooks(Array.isArray(msg.obj) ? msg.obj : []);
    } finally {
      setWebhooksLoading(false);
    }
  }, []);

  const loadDNSProfiles = useCallback(async () => {
    setDnsProfilesLoading(true);
    try {
      const msg = await HttpUtil.get('/panel/api/setting/cert/dns-profiles') as ApiMsg<DNSCertificateProfile[]>;
      if (msg?.success) setDnsProfiles(Array.isArray(msg.obj) ? msg.obj : []);
    } finally {
      setDnsProfilesLoading(false);
    }
  }, []);

  const loadOIDCConfig = useCallback(async () => {
    const msg = await HttpUtil.get('/panel/api/setting/oidc') as ApiMsg<OIDCConfig>;
    if (msg?.success && msg.obj) {
      setOidcConfig(msg.obj);
      setOidcClearClientSecret(false);
      setOidcDomains((msg.obj.allowedEmailDomains || []).join(', '));
      setOidcMappings(Object.entries(msg.obj.groupRoleMappings || {}).map(([group, role]) => `${group}=${role}`).join(', '));
    }
  }, []);

  useEffect(() => {
    void loadSessions();
    void loadWebhooks();
    void loadDNSProfiles();
    void loadOIDCConfig();
  }, [loadSessions, loadWebhooks, loadDNSProfiles, loadOIDCConfig]);

  async function createDNSProfile() {
    const domains = dnsDomains.split(',').map((value) => value.trim()).filter(Boolean);
    if (!dnsName.trim() || !domains.length) {
      messageApi.error('Profile name and one or more DNS names are required');
      return;
    }
    const msg = await HttpUtil.post('/panel/api/setting/cert/dns-profiles', {
      name: dnsName.trim(), domains, provider: dnsProvider, providerToken: dnsToken, email: dnsEmail.trim(), applyToPanel: true, enabled: true,
    }) as ApiMsg<DNSCertificateProfile>;
    if (msg?.success) {
      setDnsName(''); setDnsDomains(''); setDnsToken(''); setDnsEmail('');
      await loadDNSProfiles();
    }
  }

  async function issueDNSProfile(profile: DNSCertificateProfile, complete = false) {
    const action = complete ? 'complete' : 'issue';
    const msg = await HttpUtil.post(`/panel/api/setting/cert/dns-profiles/${profile.id}/${action}`) as ApiMsg<{ challenges?: { record: string; value: string }[] }>;
    if (msg?.success) {
      setDnsChallenges(Array.isArray(msg.obj?.challenges) ? msg.obj!.challenges! : []);
      await loadDNSProfiles();
    }
  }

  async function deleteDNSProfile(profile: DNSCertificateProfile) {
    const msg = await HttpUtil.post(`/panel/api/setting/cert/dns-profiles/${profile.id}/delete`) as ApiMsg;
    if (msg?.success) await loadDNSProfiles();
  }

  function parseOIDCGroupMappings(): Record<string, string> | null {
    const mappings: Record<string, string> = {};
    for (const item of oidcMappings.split(',')) {
      const entry = item.trim();
      if (!entry) continue;
      const [group, role, ...extra] = entry.split('=').map((value) => value.trim());
      if (!group || !role || extra.length || !['viewer', 'support', 'admin'].includes(role)) {
        messageApi.error('Role mappings must use group=viewer, group=support, or group=admin');
        return null;
      }
      mappings[group] = role;
    }
    return mappings;
  }

  async function saveOIDCConfig() {
    const groupRoleMappings = parseOIDCGroupMappings();
    if (groupRoleMappings == null) return;
    setOidcSaving(true);
    try {
      const msg = await HttpUtil.post('/panel/api/setting/oidc', {
        ...oidcConfig,
        clientSecret: oidcClientSecret,
        clearClientSecret: oidcClearClientSecret,
        allowedEmailDomains: oidcDomains.split(',').map((value) => value.trim()).filter(Boolean),
        groupRoleMappings,
      }) as ApiMsg<OIDCConfig>;
      if (msg?.success && msg.obj) {
        setOidcConfig(msg.obj);
        setOidcClientSecret('');
        setOidcClearClientSecret(false);
      }
    } finally {
      setOidcSaving(false);
    }
  }

  async function revokeSession(row: SessionRow) {
    const msg = await HttpUtil.post(`/panel/api/setting/sessions/revoke/${row.id}`) as ApiMsg;
    if (msg?.success) await loadSessions();
  }

  async function generateRecoveryCodes() {
    const msg = await HttpUtil.post('/panel/api/setting/recoveryCodes/regenerate') as ApiMsg<{ codes?: string[] }>;
    if (msg?.success && Array.isArray(msg.obj?.codes)) setRecoveryCodes(msg.obj.codes);
  }

  async function createWebhook() {
    if (!webhookName.trim() || !webhookURL.trim()) {
      messageApi.error('Webhook name and HTTPS URL are required');
      return;
    }
    const msg = await HttpUtil.post('/panel/api/setting/webhooks/create', { name: webhookName.trim(), url: webhookURL.trim(), events: webhookEvents }) as ApiMsg<WebhookRow & { secret?: string }>;
    if (msg?.success) {
      setWebhookName('');
      setWebhookURL('');
      await loadWebhooks();
      if (msg.obj?.secret) setCreatedWebhookSecret({ name: msg.obj.name, secret: msg.obj.secret });
    }
  }

  async function setWebhookEnabled(row: WebhookRow, enabled: boolean) {
    const msg = await HttpUtil.post(`/panel/api/setting/webhooks/setEnabled/${row.id}`, { enabled }) as ApiMsg;
    if (msg?.success) setWebhooks((prev) => prev.map((value) => value.id === row.id ? { ...value, enabled } : value));
  }

  async function deleteWebhook(row: WebhookRow) {
    const msg = await HttpUtil.post(`/panel/api/setting/webhooks/delete/${row.id}`) as ApiMsg;
    if (msg?.success) await loadWebhooks();
  }

  const loadAdmins = useCallback(async () => {
    setAdminsLoading(true);
    try {
      const msg = await HttpUtil.get('/panel/api/setting/admins') as ApiMsg<AdminRow[]>;
      if (msg?.success && Array.isArray(msg.obj)) {
        setAdmins(msg.obj);
      }
    } finally {
      setAdminsLoading(false);
    }
  }, []);

  useEffect(() => {
    void loadAdmins();
  }, [loadAdmins]);

  function parseAdminPermissions(raw: string): string[] {
    if (!raw) return [];
    try {
      const parsed = JSON.parse(raw);
      return Array.isArray(parsed) ? parsed.map(String) : [];
    } catch {
      return raw.split(',').map((s) => s.trim()).filter(Boolean);
    }
  }

  function openNewAdminModal() {
    setEditingAdmin({
      id: 0,
      username: '',
      password: '',
      role: 'support',
      permissions: ['inbounds', 'clients'],
      note: '',
      disabled: false,
    });
    setAdminModalOpen(true);
  }

  function openEditAdminModal(row: AdminRow) {
    setEditingAdmin({
      id: row.id,
      username: row.username,
      password: '',
      role: (row.role || 'superadmin') as 'superadmin' | 'admin' | 'support' | 'viewer',
      permissions: parseAdminPermissions(row.permissions),
      note: row.note || '',
      disabled: !!row.disabled,
    });
    setAdminModalOpen(true);
  }

  async function saveAdminModal() {
    if (!editingAdmin.username.trim()) {
      messageApi.error(t('pages.settings.security.adminUsernameRequired') || 'Username is required');
      return;
    }
    if (editingAdmin.id === 0 && editingAdmin.password.length < 4) {
      messageApi.error(t('pages.settings.security.adminPasswordMin') || 'Password must be at least 4 characters');
      return;
    }
    setAdminSaving(true);
    try {
      const url = editingAdmin.id > 0
        ? `/panel/api/setting/admins/update/${editingAdmin.id}`
        : '/panel/api/setting/admins/create';
      const msg = await HttpUtil.post(
        url,
        {
          username: editingAdmin.username.trim(),
          password: editingAdmin.password,
          role: editingAdmin.role,
          permissions: editingAdmin.permissions,
          note: editingAdmin.note.trim(),
          disabled: editingAdmin.disabled,
        },
        { headers: { 'Content-Type': 'application/json' } },
      ) as ApiMsg;
      if (msg?.success) {
        setAdminModalOpen(false);
        await loadAdmins();
      }
    } finally {
      setAdminSaving(false);
    }
  }

  function confirmDeleteAdmin(row: AdminRow) {
    modal.confirm({
      title: `${t('delete')} "${row.username}"?`,
      okText: t('delete'),
      cancelText: t('cancel'),
      okType: 'danger',
      onOk: async () => {
        const msg = await HttpUtil.post(`/panel/api/setting/admins/delete/${row.id}`) as ApiMsg;
        if (msg?.success) await loadAdmins();
      },
    });
  }

  async function copyToken(token: string) {
    if (!token) return;
    const ok = await ClipboardManager.copyText(token);
    if (ok) messageApi.success(t('copySuccess'));
    else messageApi.error(t('copyFail') ?? 'Copy failed');
  }

  function openCreateModal() {
    setCreateName('');
    setCreateScope('full');
    setCreatePermissions([]);
    setCreateExpireDays(0);
    setCreateOpen(true);
  }

  async function confirmCreateToken() {
    const name = createName.trim();
    if (!name) {
      messageApi.error(t('pages.settings.security.apiTokenNameRequired') || 'Name is required');
      return;
    }
    setCreating(true);
    try {
      const msg = await HttpUtil.post('/panel/api/setting/apiTokens/create', {
        name,
        scope: createScope,
        scopes: createPermissions,
        expireDays: createExpireDays || 0,
      }) as ApiMsg<{ token?: string }>;
      if (msg?.success) {
        setCreateOpen(false);
        await loadApiTokens();
        if (msg.obj?.token) {
          setCreatedToken({ name, token: msg.obj.token });
        }
      }
    } finally {
      setCreating(false);
    }
  }

  function confirmDeleteToken(row: ApiTokenRow) {
    modal.confirm({
      title: `${t('delete')} "${row.name}"?`,
      content: t('pages.settings.security.apiTokenDeleteWarning')
        || 'Any caller using this token will stop authenticating immediately.',
      okText: t('delete'),
      cancelText: t('cancel'),
      okType: 'danger',
      onOk: async () => {
        const msg = await HttpUtil.post(`/panel/api/setting/apiTokens/delete/${row.id}`) as ApiMsg;
        if (msg?.success) await loadApiTokens();
      },
    });
  }

  async function toggleTokenEnabled(row: ApiTokenRow) {
    const target = !row.enabled;
    const msg = await HttpUtil.post(`/panel/api/setting/apiTokens/setEnabled/${row.id}`, { enabled: target }) as ApiMsg;
    if (msg?.success) {
      setApiTokens((prev) => prev.map((r) => (r.id === row.id ? { ...r, enabled: target } : r)));
    }
  }

  function formatTokenDate(ts?: number): string {
    if (!ts) return '';
    // Model timestamps are unix milliseconds; handle legacy second-precision values gracefully.
    const ms = ts < 1e12 ? ts * 1000 : ts;
    return new Date(ms).toLocaleString();
  }

  function isTokenExpired(row: ApiTokenRow): boolean {
    return !!row.expiresAt && row.expiresAt > 0 && Date.now() > row.expiresAt;
  }

  function toggleTwoFactor() {
    if (!allSetting.twoFactorEnable) {
      const newToken = RandomUtil.randomBase32String();
      openTfa({
        title: t('pages.settings.security.twoFactorModalSetTitle'),
        description: '',
        token: newToken,
        type: 'set',
        onConfirm: (ok: boolean) => {
          if (ok) {
            messageApi.success(t('pages.settings.security.twoFactorModalSetSuccess'));
            updateSetting({ twoFactorToken: newToken, twoFactorEnable: true });
          } else {
            updateSetting({ twoFactorEnable: false });
          }
        },
      });
    } else {
      openTfa({
        title: t('pages.settings.security.twoFactorModalDeleteTitle'),
        description: t('pages.settings.security.twoFactorModalRemoveStep'),
        token: allSetting.twoFactorToken,
        type: 'confirm',
        onConfirm: (ok: boolean) => {
          if (!ok) return;
          messageApi.success(t('pages.settings.security.twoFactorModalDeleteSuccess'));
          updateSetting({ twoFactorEnable: false, twoFactorToken: '' });
        },
      });
    }
  }

  return (
    <>
      {messageContextHolder}
      {modalContextHolder}
      <Tabs defaultActiveKey="1" items={[
        {
          key: '1',
          label: catTabLabel(<UserOutlined />, t('pages.settings.security.admin'), isMobile),
          children: (
            <>
              <SettingListItem paddings="small" title={t('pages.settings.oldUsername')}>
                <Input value={user.oldUsername} autoComplete="username"
                  onChange={(e) => updateUserField('oldUsername', e.target.value)} />
              </SettingListItem>
              <SettingListItem paddings="small" title={t('pages.settings.currentPassword')}>
                <Input.Password value={user.oldPassword} autoComplete="current-password"
                  onChange={(e) => updateUserField('oldPassword', e.target.value)} />
              </SettingListItem>
              <SettingListItem paddings="small" title={t('pages.settings.newUsername')}>
                <Input value={user.newUsername}
                  onChange={(e) => updateUserField('newUsername', e.target.value)} />
              </SettingListItem>
              <SettingListItem paddings="small" title={t('pages.settings.newPassword')}>
                <Input.Password value={user.newPassword} autoComplete="new-password"
                  onChange={(e) => updateUserField('newPassword', e.target.value)} />
              </SettingListItem>
              <div className="security-actions">
                <Space style={{ padding: '0 20px' }}>
                  <Button type="primary" loading={updating} onClick={onUpdateUserClick}>
                    {t('confirm')}
                  </Button>
                </Space>
              </div>
            </>
          ),
        },
        {
          key: '2',
          label: catTabLabel(<SafetyOutlined />, t('pages.settings.security.twoFactor'), isMobile),
          children: (
            <SettingListItem
              paddings="small"
              title={t('pages.settings.security.twoFactorEnable')}
              description={t('pages.settings.security.twoFactorEnableDesc')}
            >
              <Switch checked={allSetting.twoFactorEnable} onClick={toggleTwoFactor} />
            </SettingListItem>
          ),
        },
        {
          key: '3',
          label: catTabLabel(<ApiOutlined />, t('pages.nodes.apiToken'), isMobile),
          children: (
            <div className="api-token-section">
              <div className="api-token-header">
                <p className="api-token-hint">{t('pages.nodes.apiTokenHint')}</p>
                <Button type="primary" size="small" onClick={openCreateModal}>
                  + {t('pages.settings.security.apiTokenNew') || 'New token'}
                </Button>
              </div>
              <Spin spinning={apiTokensLoading}>
                {!apiTokens.length && !apiTokensLoading && (
                  <Empty description={t('pages.settings.security.apiTokenEmpty') || 'No tokens yet'} />
                )}
                {apiTokens.map((row) => {
                  const expired = isTokenExpired(row);
                  const readOnly = row.scope === 'read';
                  return (
                    <div key={row.id} className={`api-token-row${row.enabled && !expired ? '' : ' disabled'}`}>
                      <div className="api-token-row-head">
                        <div className="api-token-name-wrap">
                          <Space size={6} wrap>
                            <span className="api-token-name">{row.name}</span>
                            {row.prefix ? <code>{row.prefix}…</code> : null}
                            <Tag color={readOnly ? 'blue' : 'gold'}>
                              {readOnly
                                ? (t('pages.settings.security.apiTokenScopeRead') || 'Read-only')
                                : (t('pages.settings.security.apiTokenScopeFull') || 'Full access')}
                            </Tag>
                            {(row.scopes || []).map((permission) => <Tag key={permission} color="purple">{permission}</Tag>)}
                            {expired ? (
                              <Tag color="red">{t('pages.settings.security.apiTokenExpired') || 'Expired'}</Tag>
                            ) : null}
                          </Space>
                          <span className="api-token-created">
                            {formatTokenDate(row.createdAt)}
                            {row.expiresAt ? ` · ${t('pages.settings.security.apiTokenExpires') || 'Expires'}: ${formatTokenDate(row.expiresAt)}` : ''}
                            {row.lastUsedAt ? ` · ${t('pages.settings.security.apiTokenLastUsed') || 'Last used'}: ${formatTokenDate(row.lastUsedAt)}` : ''}
                          </span>
                        </div>
                        <div className="api-token-actions">
                          <Switch size="small" checked={row.enabled} onChange={() => toggleTokenEnabled(row)} />
                          <Button size="small" danger type="text" onClick={() => confirmDeleteToken(row)}>
                            {t('delete')}
                          </Button>
                        </div>
                      </div>
                    </div>
                  );
                })}
              </Spin>
            </div>
          ),
        },
        {
          key: '4',
          label: catTabLabel(<TeamOutlined />, t('pages.settings.security.multiAdminTitle') || 'Admins & RBAC', isMobile),
          children: (
            <div className="api-token-section">
              <div className="api-token-header">
                <p className="api-token-hint">
                  {t('pages.settings.security.multiAdminDesc')
                    || 'Create secondary admin accounts with granular RBAC roles (Superadmin, Custom Admin, Support, or Read-only Viewer).'}
                </p>
                <Button type="primary" size="small" onClick={openNewAdminModal}>
                  + {t('pages.settings.security.addAdmin') || 'Add Admin'}
                </Button>
              </div>
              <Spin spinning={adminsLoading}>
                {admins.map((row) => {
                  const roleColor =
                    row.role === 'superadmin'
                      ? 'gold'
                      : row.role === 'admin'
                        ? 'purple'
                        : row.role === 'support'
                          ? 'cyan'
                          : 'blue';
                  const perms = parseAdminPermissions(row.permissions);
                  return (
                    <div key={row.id} className={`api-token-row${row.disabled ? ' disabled' : ''}`}>
                      <div className="api-token-row-head">
                        <div className="api-token-name-wrap">
                          <Space size={6} wrap>
                            <span className="api-token-name">{row.username}</span>
                            {row.id === 1 && <Tag color="gold">Primary</Tag>}
                            <Tag color={roleColor}>
                              {t(`pages.settings.security.role_${row.role || 'superadmin'}`) || row.role}
                            </Tag>
                            {row.role === 'admin' && perms.map((p) => (
                              <Tag key={p}>{p}</Tag>
                            ))}
                            {row.disabled && <Tag color="red">{t('disabled') || 'Disabled'}</Tag>}
                          </Space>
                          <span className="api-token-created">
                            {row.note ? `${row.note} · ` : ''}
                            {row.lastLoginAt
                              ? `${t('pages.settings.security.lastLogin') || 'Last login'}: ${formatTokenDate(row.lastLoginAt)}`
                              : ''}
                          </span>
                        </div>
                        <div className="api-token-actions">
                          <Button size="small" type="text" onClick={() => openEditAdminModal(row)}>
                            {t('edit') || 'Edit'}
                          </Button>
                          {row.id !== 1 && (
                            <Button size="small" danger type="text" onClick={() => confirmDeleteAdmin(row)}>
                              {t('delete')}
                            </Button>
                          )}
                        </div>
                      </div>
                    </div>
                  );
                })}
              </Spin>
            </div>
          ),
        },
        {
          key: '5',
          label: catTabLabel(<LaptopOutlined />, 'Sessions & recovery', isMobile),
          children: (
            <div className="api-token-section">
              <div className="api-token-header">
                <p className="api-token-hint">Review browser sessions created after this security upgrade and revoke a device immediately. Recovery codes are displayed once, stored only as hashes, and each code can be used once for 2FA recovery.</p>
                <Space size="small" wrap>
                  <Button size="small" onClick={() => setReauthOpen(true)}><SafetyOutlined /> Confirm sensitive actions</Button>
                  <Button size="small" onClick={generateRecoveryCodes}><KeyOutlined /> Generate recovery codes</Button>
                </Space>
              </div>
              <Spin spinning={sessionsLoading}>
                {!sessions.length && !sessionsLoading ? <Empty description="No tracked sessions yet" /> : null}
                {sessions.map((row) => (
                  <div key={row.id} className="api-token-row">
                    <div className="api-token-row-head">
                      <div className="api-token-name-wrap">
                        <span className="api-token-name">{row.ip || 'Unknown IP'}</span>
                        <span className="api-token-created">{row.userAgent || 'Unknown browser'} · Last seen: {formatTokenDate(row.lastSeenAt)}</span>
                      </div>
                      <Button size="small" danger type="text" onClick={() => revokeSession(row)}>Revoke</Button>
                    </div>
                  </div>
                ))}
              </Spin>
            </div>
          ),
        },
        {
          key: '6',
          label: catTabLabel(<GlobalOutlined />, 'Webhooks', isMobile),
          children: (
            <div className="api-token-section">
              <p className="api-token-hint">HTTPS-only event destinations. Requests are HMAC-SHA256 signed, queued durably, retried with backoff, and internal/private destinations are rejected.</p>
              <Space direction="vertical" style={{ width: '100%' }}>
                <Input value={webhookName} maxLength={64} placeholder="Webhook name" onChange={(event) => setWebhookName(event.target.value)} />
                <Input value={webhookURL} placeholder="https://events.example.com/phoenix" onChange={(event) => setWebhookURL(event.target.value)} />
                <Select mode="multiple" value={webhookEvents} onChange={setWebhookEvents} options={['security.login.success', 'security.login.failure', 'audit.mutation'].map((value) => ({ value, label: value }))} />
                <Button type="primary" onClick={createWebhook}>Create signed webhook</Button>
              </Space>
              <Spin spinning={webhooksLoading}>
                {webhooks.map((row) => (
                  <div key={row.id} className="api-token-row">
                    <div className="api-token-row-head">
                      <div className="api-token-name-wrap"><span className="api-token-name">{row.name}</span><span className="api-token-created">{row.url} · {row.events.join(', ')}</span></div>
                      <div className="api-token-actions"><Switch size="small" checked={row.enabled} onChange={(value) => setWebhookEnabled(row, value)} /><Button size="small" danger type="text" onClick={() => deleteWebhook(row)}>Delete</Button></div>
                    </div>
                  </div>
                ))}
              </Spin>
            </div>
          ),
        },
        {
          key: '7',
          label: catTabLabel(<SafetyCertificateOutlined />, 'DNS-01 certificates', isMobile),
          children: (
            <div className="api-token-section">
              <p className="api-token-hint">Issue wildcard certificates without opening port 80. Manual TXT works with every DNS provider; Cloudflare API tokens enable automatic issue and renewal. Confirm sensitive actions first.</p>
              <Space direction="vertical" style={{ width: '100%' }}>
                <Input value={dnsName} placeholder="Profile name" onChange={(event) => setDnsName(event.target.value)} />
                <Input value={dnsDomains} placeholder="example.com, *.example.com" onChange={(event) => setDnsDomains(event.target.value)} />
                <Select value={dnsProvider} onChange={setDnsProvider} options={[{ value: 'manual', label: 'Manual DNS TXT (any provider)' }, { value: 'cloudflare', label: 'Cloudflare API token' }]} />
                {dnsProvider === 'cloudflare' ? <Input.Password value={dnsToken} placeholder="Cloudflare API token (DNS Edit, Zone Read)" onChange={(event) => setDnsToken(event.target.value)} /> : null}
                <Input value={dnsEmail} placeholder="ACME account email (optional)" onChange={(event) => setDnsEmail(event.target.value)} />
                <Button type="primary" onClick={createDNSProfile}>Save DNS certificate profile</Button>
              </Space>
              <Spin spinning={dnsProfilesLoading}>
                {dnsProfiles.map((profile) => (
                  <div key={profile.id} className="api-token-row">
                    <div className="api-token-row-head">
                      <div className="api-token-name-wrap"><Space wrap><span className="api-token-name">{profile.name}</span><Tag color={profile.provider === 'cloudflare' ? 'orange' : 'blue'}>{profile.provider}</Tag>{profile.applyToPanel ? <Tag color="green">Panel TLS</Tag> : null}</Space><span className="api-token-created">{profile.domains.join(', ')}{profile.lastError ? ` · ${profile.lastError}` : ''}</span></div>
                      <div className="api-token-actions"><Button size="small" onClick={() => issueDNSProfile(profile)}>Issue / renew</Button>{profile.pendingAt ? <Button size="small" type="primary" onClick={() => issueDNSProfile(profile, true)}>Verify TXT</Button> : null}<Button size="small" danger type="text" onClick={() => deleteDNSProfile(profile)}>Delete</Button></div>
                    </div>
                  </div>
                ))}
              </Spin>
              {dnsChallenges.length ? <div className="api-token-created-notice">Publish every TXT record below, wait for DNS propagation, then press <b>Verify TXT</b> on the profile.<br />{dnsChallenges.map((item) => <code key={`${item.record}-${item.value}`} className="api-token-value">{item.record} TXT {item.value}</code>)}</div> : null}
            </div>
          ),
        },
        {
          key: '8',
          label: catTabLabel(<CloudOutlined />, 'Single sign-on (OIDC)', isMobile),
          children: (
            <div className="api-token-section">
              <p className="api-token-hint">Generic OpenID Connect Discovery with Authorization Code + PKCE. Works with standards-compliant providers such as Keycloak, Authentik, Google Workspace, Azure AD, and GitLab. Existing admin accounts remain available as a recovery path.</p>
              <Space direction="vertical" style={{ width: '100%' }}>
                <SettingListItem paddings="small" title="Enable OIDC"><Switch checked={oidcConfig.enabled} onChange={(enabled) => setOidcConfig((prev) => ({ ...prev, enabled }))} /></SettingListItem>
                <Input value={oidcConfig.issuer} placeholder="https://id.example.com/realms/phoenix" onChange={(event) => setOidcConfig((prev) => ({ ...prev, issuer: event.target.value }))} />
                <Input value={oidcConfig.clientId} placeholder="OIDC client ID" onChange={(event) => setOidcConfig((prev) => ({ ...prev, clientId: event.target.value }))} />
                <Input.Password value={oidcClientSecret} placeholder={oidcConfig.hasClientSecret ? 'Leave blank to keep stored client secret' : 'OIDC client secret (optional for public PKCE clients)'} onChange={(event) => setOidcClientSecret(event.target.value)} />
                {oidcConfig.hasClientSecret ? <SettingListItem paddings="small" title="Clear stored OIDC client secret"><Switch checked={oidcClearClientSecret} onChange={setOidcClearClientSecret} /></SettingListItem> : null}
                <Input value={oidcDomains} placeholder="Allowed email domains, comma-separated (optional)" onChange={(event) => setOidcDomains(event.target.value)} />
                <SettingListItem paddings="small" title="Auto-provision approved identities"><Switch checked={oidcConfig.autoProvision} onChange={(autoProvision) => setOidcConfig((prev) => ({ ...prev, autoProvision }))} /></SettingListItem>
                <Select value={oidcConfig.defaultRole} onChange={(defaultRole) => setOidcConfig((prev) => ({ ...prev, defaultRole }))} options={['viewer', 'support', 'admin'].map((value) => ({ value, label: `Default role: ${value}` }))} />
                <Input value={oidcConfig.groupClaim} placeholder="Group claim (for example: groups)" onChange={(event) => setOidcConfig((prev) => ({ ...prev, groupClaim: event.target.value }))} />
                <Input value={oidcMappings} placeholder="Group role mappings: engineering=admin, helpdesk=support" onChange={(event) => setOidcMappings(event.target.value)} />
                <Button type="primary" loading={oidcSaving} onClick={saveOIDCConfig}>Save OIDC configuration</Button>
              </Space>
            </div>
          ),
        },
      ]} />

      <Modal
        open={adminModalOpen}
        title={
          editingAdmin.id > 0
            ? (t('pages.settings.security.editAdmin') || 'Edit Admin Account')
            : (t('pages.settings.security.addAdmin') || 'Add Admin Account')
        }
        confirmLoading={adminSaving}
        okText={t('save')}
        cancelText={t('cancel')}
        onOk={saveAdminModal}
        onCancel={() => setAdminModalOpen(false)}
      >
        <Form layout="vertical">
          <Form.Item label={t('pages.settings.newUsername') || 'Username'} required>
            <Input
              value={editingAdmin.username}
              onChange={(e) => setEditingAdmin((prev) => ({ ...prev, username: e.target.value }))}
            />
          </Form.Item>
          <Form.Item
            label={t('pages.settings.newPassword') || 'Password'}
            required={editingAdmin.id === 0}
            extra={
              editingAdmin.id > 0
                ? (t('pages.settings.security.adminPasswordLeaveBlank') || 'Leave blank to keep existing password')
                : undefined
            }
          >
            <Input.Password
              value={editingAdmin.password}
              autoComplete="new-password"
              onChange={(e) => setEditingAdmin((prev) => ({ ...prev, password: e.target.value }))}
            />
          </Form.Item>
          <Form.Item label={t('pages.settings.security.adminRole') || 'Role'}>
            <Select
              disabled={editingAdmin.id === 1}
              value={editingAdmin.role}
              onChange={(v) => setEditingAdmin((prev) => ({ ...prev, role: v }))}
              options={[
                { value: 'superadmin', label: t('pages.settings.security.role_superadmin') || 'Superadmin (Full Access)' },
                { value: 'admin', label: t('pages.settings.security.role_admin') || 'Custom Admin (Selected Modules)' },
                { value: 'support', label: t('pages.settings.security.role_support') || 'Support (Clients Read/Write + Inbounds Read)' },
                { value: 'viewer', label: t('pages.settings.security.role_viewer') || 'Viewer (Read-only Auditor)' },
              ]}
            />
          </Form.Item>
          {editingAdmin.role === 'admin' && (
            <Form.Item label={t('pages.settings.security.adminModules') || 'Allowed Modules'}>
              <Select
                mode="multiple"
                value={editingAdmin.permissions}
                onChange={(v) => setEditingAdmin((prev) => ({ ...prev, permissions: v }))}
                options={ADMIN_MODULES.map((m) => ({ value: m, label: m }))}
              />
            </Form.Item>
          )}
          <Form.Item label={t('pages.clients.comment') || 'Note'}>
            <Input
              value={editingAdmin.note}
              onChange={(e) => setEditingAdmin((prev) => ({ ...prev, note: e.target.value }))}
            />
          </Form.Item>
          {editingAdmin.id > 1 && (
            <Form.Item label={t('enable') || 'Enabled'}>
              <Switch
                checked={!editingAdmin.disabled}
                onChange={(v) => setEditingAdmin((prev) => ({ ...prev, disabled: !v }))}
              />
            </Form.Item>
          )}
        </Form>
      </Modal>

      <Modal
        open={createOpen}
        title={t('pages.settings.security.apiTokenNew') || 'New API token'}
        confirmLoading={creating}
        okText={t('confirm')}
        cancelText={t('cancel')}
        onOk={confirmCreateToken}
        onCancel={() => setCreateOpen(false)}
      >
        <Form layout="vertical">
          <Form.Item label={t('pages.settings.security.apiTokenName') || 'Name'} required>
            <Input
              value={createName}
              maxLength={64}
              placeholder={t('pages.settings.security.apiTokenNamePlaceholder') || 'e.g. central-panel-a'}
              onChange={(e) => setCreateName(e.target.value)}
              onPressEnter={confirmCreateToken}
            />
          </Form.Item>
          <Form.Item label={t('pages.settings.security.apiTokenScope') || 'Permissions'}>
            <Select
              value={createScope}
              onChange={(v) => setCreateScope(v)}
              options={[
                {
                  value: 'full',
                  label: t('pages.settings.security.apiTokenScopeFull') || 'Full access (read & write)',
                },
                {
                  value: 'read',
                  label: t('pages.settings.security.apiTokenScopeRead') || 'Read-only (GET only)',
                },
              ]}
            />
          </Form.Item>
          <Form.Item
            label="Granular permissions (optional)"
            extra="When selected, only these API capabilities are granted; the legacy full/read choice remains available when this is empty."
          >
            <Select
              mode="multiple"
              value={createPermissions}
              onChange={(value) => setCreatePermissions(value)}
              options={[
                'clients:read', 'clients:write', 'inbounds:read', 'inbounds:write',
                'nodes:read', 'nodes:write', 'resellers:read', 'resellers:write',
                'server:read', 'server:write', 'xray:read', 'xray:write',
                'settings:read', 'settings:write', 'metrics:read', 'audit:read',
              ].map((value) => ({ value, label: value }))}
            />
          </Form.Item>
          <Form.Item
            label={t('pages.settings.security.apiTokenExpireDays') || 'Expires in (days)'}
            extra={t('pages.settings.security.apiTokenExpireDaysDesc') || '0 means never expires.'}
          >
            <InputNumber
              min={0}
              max={3650}
              style={{ width: '100%' }}
              value={createExpireDays}
              onChange={(v) => setCreateExpireDays(typeof v === 'number' ? v : 0)}
            />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        open={!!createdToken}
        title={t('pages.settings.security.apiTokenCreatedTitle') || 'Token created'}
        okText={t('done')}
        onOk={() => setCreatedToken(null)}
        onCancel={() => setCreatedToken(null)}
        cancelButtonProps={{ style: { display: 'none' } }}
      >
        <p className="api-token-created-notice">
          {t('pages.settings.security.apiTokenCreatedNotice')
            || 'Copy this token now. For security it is not stored in readable form and will not be shown again.'}
        </p>
        <div className="api-token-value-wrap">
          <code className="api-token-value">{createdToken?.token}</code>
          <Button size="small" type="primary" onClick={() => createdToken && copyToken(createdToken.token)}>
            {t('copy')}
          </Button>
        </div>
      </Modal>

      <Modal open={!!recoveryCodes} title="Save recovery codes" okText="I saved them" onOk={() => setRecoveryCodes(null)} onCancel={() => setRecoveryCodes(null)} cancelButtonProps={{ style: { display: 'none' } }}>
        <p>Each code is shown only once. Store them offline; entering one in the 2FA field consumes it.</p>
        <code className="api-token-value">{recoveryCodes?.join('\n')}</code>
      </Modal>
      <Modal open={!!createdWebhookSecret} title="Save webhook signing secret" okText="I saved it" onOk={() => setCreatedWebhookSecret(null)} onCancel={() => setCreatedWebhookSecret(null)} cancelButtonProps={{ style: { display: 'none' } }}>
        <p>This is the only time the secret for {createdWebhookSecret?.name} is shown. Verify each delivery with HMAC-SHA256.</p>
        <div className="api-token-value-wrap"><code className="api-token-value">{createdWebhookSecret?.secret}</code><Button size="small" type="primary" onClick={() => createdWebhookSecret && copyToken(createdWebhookSecret.secret)}>Copy</Button></div>
      </Modal>

      <Modal
        open={reauthOpen}
        title="Confirm sensitive actions"
        okText="Confirm"
        confirmLoading={reauthing}
        onOk={confirmRecentAuthentication}
        onCancel={() => {
          setReauthOpen(false);
          setReauthPassword('');
          setReauthCode('');
        }}
      >
        <p>Confirm your password before changing administrators, API tokens, recovery codes, sessions, or webhook settings. This confirmation expires after 10 minutes.</p>
        <Form layout="vertical">
          <Form.Item label="Password" required>
            <Input.Password value={reauthPassword} autoComplete="current-password" onChange={(event) => setReauthPassword(event.target.value)} onPressEnter={confirmRecentAuthentication} />
          </Form.Item>
          {allSetting.twoFactorEnable ? (
            <Form.Item label="Two-factor or recovery code" required>
              <Input value={reauthCode} inputMode="numeric" autoComplete="one-time-code" onChange={(event) => setReauthCode(event.target.value)} onPressEnter={confirmRecentAuthentication} />
            </Form.Item>
          ) : null}
        </Form>
      </Modal>

      <TwoFactorModal
        open={tfa.open}
        title={tfa.title}
        description={tfa.description}
        token={tfa.token}
        type={tfa.type}
        onConfirm={onTfaConfirm}
        onOpenChange={(open) => setTfa((prev) => ({ ...prev, open }))}
      />
    </>
  );
}
