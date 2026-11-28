import { useCallback, useEffect, useState } from 'react';
import { Button, Empty, Input, InputNumber, List, Modal, Select, Space, Spin, Switch, Tag, message } from 'antd';
import { CloudSyncOutlined, DeleteOutlined, EditOutlined, EyeOutlined, PlusOutlined, SendOutlined } from '@ant-design/icons';

import { HttpUtil } from '@/utils';

interface ApiMsg<T = unknown> { success?: boolean; msg?: string; obj?: T; }
interface RuleSetCatalog {
  id: number;
  name: string;
  url: string;
  kind: 'domain' | 'ip';
  entryCount: number;
  entries?: string[];
  enabled: boolean;
  refreshMinutes: number;
  lastSuccessAt?: number;
  lastError?: string;
  sha256?: string;
}

interface RuleSetCatalogPanelProps {
  onApply: (kind: 'domain' | 'ip', entries: string[], name: string) => void;
}

const MAX_ENTRIES_TO_APPLY = 10_000;

// A catalog remains data-only: it lets an operator review and refresh trusted
// routing lists without letting an untrusted remote source overwrite an Xray
// template. Applying cached entries creates a normal, editable local rule.
export default function RuleSetCatalogPanel({ onApply }: RuleSetCatalogPanelProps) {
  const [rows, setRows] = useState<RuleSetCatalog[]>([]);
  const [loading, setLoading] = useState(false);
  const [name, setName] = useState('');
  const [url, setURL] = useState('');
  const [kind, setKind] = useState<'domain' | 'ip'>('domain');
  const [refreshMinutes, setRefreshMinutes] = useState(360);
  const [enabled, setEnabled] = useState(true);
  const [editingID, setEditingID] = useState<number | null>(null);
  const [inspected, setInspected] = useState<RuleSetCatalog | null>(null);
  const [messageApi, holder] = message.useMessage();
  const [modal, modalHolder] = Modal.useModal();

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const msg = await HttpUtil.get<RuleSetCatalog[]>('/panel/api/xray/rule-sets') as ApiMsg<RuleSetCatalog[]>;
      if (msg?.success) setRows(Array.isArray(msg.obj) ? msg.obj : []);
    } finally { setLoading(false); }
  }, []);

  useEffect(() => { void load(); }, [load]);

  function resetForm() {
    setName('');
    setURL('');
    setKind('domain');
    setRefreshMinutes(360);
    setEnabled(true);
    setEditingID(null);
  }
  function beginEdit(row: RuleSetCatalog) {
    setName(row.name);
    setURL(row.url);
    setKind(row.kind);
    setRefreshMinutes(row.refreshMinutes);
    setEnabled(row.enabled);
    setEditingID(row.id);
  }
  async function saveCatalog() {
    if (!name.trim() || !url.trim()) { messageApi.error('A name and HTTPS source URL are required'); return; }
    const payload = { name: name.trim(), url: url.trim(), kind, enabled, refreshMinutes };
    const endpoint = editingID ? `/panel/api/xray/rule-sets/${editingID}` : '/panel/api/xray/rule-sets';
    const msg = await HttpUtil.post(endpoint, payload) as ApiMsg;
    if (msg?.success) { resetForm(); await load(); }
  }
  async function refresh(row: RuleSetCatalog) {
    const msg = await HttpUtil.post(`/panel/api/xray/rule-sets/${row.id}/refresh`) as ApiMsg;
    if (msg?.success) await load();
  }
  async function remove(row: RuleSetCatalog) {
    modal.confirm({
      title: `Delete ${row.name}?`,
      content: 'The cached entries will be removed. Existing local Xray routing rules are not changed.',
      okText: 'Delete catalog',
      okType: 'danger',
      onOk: async () => {
        const msg = await HttpUtil.post(`/panel/api/xray/rule-sets/${row.id}/del`) as ApiMsg;
        if (msg?.success) await load();
      },
    });
  }
  async function inspect(row: RuleSetCatalog) {
    const msg = await HttpUtil.get<RuleSetCatalog>(`/panel/api/xray/rule-sets/${row.id}?entries=true`) as ApiMsg<RuleSetCatalog>;
    if (msg?.success && msg.obj) setInspected(msg.obj);
  }
  function applyInspected() {
    if (!inspected) return;
    const entries = inspected.entries || [];
    if (!entries.length) { messageApi.warning('Refresh this catalog before adding a routing rule'); return; }
    if (entries.length > MAX_ENTRIES_TO_APPLY) {
      messageApi.error(`This catalog has ${entries.length.toLocaleString()} entries. Review or split it before applying more than ${MAX_ENTRIES_TO_APPLY.toLocaleString()} entries to one Xray rule.`);
      return;
    }
    modal.confirm({
      title: `Create an editable ${inspected.kind} rule from ${inspected.name}?`,
      content: `The ${entries.length.toLocaleString()} reviewed cached entries will be copied into a normal local Xray routing rule. Choose its outbound or balancer in the next step, then save the Xray configuration separately. Future catalog refreshes never alter this rule.`,
      okText: 'Review routing rule',
      onOk: () => {
        onApply(inspected.kind, entries, inspected.name);
        setInspected(null);
      },
    });
  }

  return <div className="api-token-section">
    {holder}{modalHolder}
    <p className="api-token-hint">Cache and review public HTTPS domain/IP lists safely. Remote lists are normalized and refreshed atomically; a failed refresh keeps the last known-good cache. Catalogs cannot execute or replace Xray configuration.</p>
    <Space direction="vertical" style={{ width: '100%' }}>
      <Input value={name} maxLength={64} placeholder="Catalog name" onChange={(event) => setName(event.target.value)} />
      <Input value={url} placeholder="https://example.com/routing-list.txt" onChange={(event) => setURL(event.target.value)} />
      <Select value={kind} onChange={setKind} options={[{ value: 'domain', label: 'Domain rules' }, { value: 'ip', label: 'IP/CIDR rules' }]} />
      <Space wrap><span>Refresh every</span><InputNumber min={5} max={10080} value={refreshMinutes} onChange={(value) => setRefreshMinutes(typeof value === 'number' ? value : 360)} /><span>minutes</span><Switch checked={enabled} onChange={setEnabled} /><span>Enabled</span></Space>
      <Space wrap>
        <Button type="primary" icon={editingID ? <EditOutlined /> : <PlusOutlined />} onClick={saveCatalog}>{editingID ? 'Save catalog' : 'Add rule-set catalog'}</Button>
        {editingID ? <Button onClick={resetForm}>Cancel edit</Button> : null}
      </Space>
    </Space>
    <Spin spinning={loading}>
      {!rows.length && !loading ? <Empty description="No cached rule-set catalogs" /> : null}
      <List
        dataSource={rows}
        renderItem={(row) => <List.Item actions={[
          <Button key="view" size="small" icon={<EyeOutlined />} onClick={() => inspect(row)}>Inspect</Button>,
          <Button key="edit" size="small" icon={<EditOutlined />} onClick={() => beginEdit(row)}>Edit</Button>,
          <Button key="refresh" size="small" icon={<CloudSyncOutlined />} onClick={() => refresh(row)}>Refresh</Button>,
          <Button key="delete" size="small" danger type="text" icon={<DeleteOutlined />} onClick={() => remove(row)}>Delete</Button>,
        ]}>
          <List.Item.Meta
            title={<Space wrap><span>{row.name}</span><Tag color={row.kind === 'domain' ? 'blue' : 'purple'}>{row.kind}</Tag>{row.enabled ? <Tag color="green">enabled</Tag> : <Tag>disabled</Tag>}<Tag>{row.entryCount} entries</Tag></Space>}
            description={<><div>{row.url}</div><div>{row.lastError ? `Last error: ${row.lastError}` : `Refresh: ${row.refreshMinutes} minutes`}</div></>}
          />
        </List.Item>}
      />
    </Spin>
    <Modal open={!!inspected} title={inspected ? `Review ${inspected.name}` : 'Review catalog'} width={820} footer={[
      <Button key="close" onClick={() => setInspected(null)}>Close</Button>,
      <Button key="apply" type="primary" icon={<SendOutlined />} disabled={!inspected?.entries?.length} onClick={applyInspected}>Create editable routing rule</Button>,
    ]} onCancel={() => setInspected(null)}>
      {inspected && <Space direction="vertical" style={{ width: '100%' }}>
        <div><b>Source:</b> {inspected.url}</div>
        <div><b>Digest:</b> <code>{inspected.sha256 || 'not refreshed yet'}</code></div>
        <div><b>Cached entries:</b> {(inspected.entries || []).length.toLocaleString()}</div>
        <Input.TextArea aria-label="Cached normalized catalog entries" readOnly autoSize={{ minRows: 8, maxRows: 20 }} value={(inspected.entries || []).join('\n')} />
        <p className="api-token-hint">Applying copies these currently displayed entries into an editable local rule. It never creates a live link to this catalog.</p>
      </Space>}
    </Modal>
  </div>;
}
