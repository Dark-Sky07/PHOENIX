import { useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Alert, Button, DatePicker, Form, Input, InputNumber, Modal, Table, message } from 'antd';
import type { Dayjs } from 'dayjs';
import { HttpUtil } from '@/utils';

type HistoricalSale = {
  id: number; reference: string; source: string; email: string;
  trafficBytes: number; amountMinor: number | null; currency: string;
  soldAt: number; recordedAt: number; actorName: string; actorType: string;
};
type Fields = {
  reference: string; source: string; email: string; soldAt: Dayjs;
  trafficBytes: number; amountMinor?: number | null; currency?: string;
};

export default function ResellerHistoryModal({ resellerId, open, onClose }: { resellerId: number; open: boolean; onClose: () => void }) {
  const { t } = useTranslation();
  const [form] = Form.useForm<Fields>();
  const [saving, setSaving] = useState(false);
  const [messageApi, holder] = message.useMessage();
  const queryClient = useQueryClient();
  const base = `/panel/api/resellers/report/${resellerId}/sales/history`;
  const history = useQuery({
    queryKey: ['resellers', 'salesHistory', resellerId], enabled: open,
    queryFn: async () => {
      const msg = await HttpUtil.get<HistoricalSale[]>(base);
      if (!msg.success) throw new Error(msg.msg || 'Failed to fetch history');
      return msg.obj ?? [];
    },
  });
  async function submit(values: Fields) {
    setSaving(true);
    try {
      const msg = await HttpUtil.post(base, {
        reference: values.reference.trim(), source: values.source.trim(), email: values.email.trim(),
        soldAt: values.soldAt.valueOf(), trafficBytes: values.trafficBytes,
        amountMinor: values.amountMinor ?? null,
        currency: values.amountMinor == null ? '' : (values.currency || '').trim().toUpperCase(),
      }, { headers: { 'Content-Type': 'application/json' } });
      if (!msg.success) { messageApi.error(msg.msg || t('somethingWentWrong')); return; }
      form.resetFields();
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['resellers', 'salesHistory', resellerId] }),
        queryClient.invalidateQueries({ queryKey: ['resellers', 'sales', resellerId] }),
      ]);
      messageApi.success(t('resellers.historySaved', 'Historical record saved'));
    } finally { setSaving(false); }
  }
  return <>{holder}<Modal open={open} onCancel={onClose} title={t('resellers.historyTitle', 'Historical sales')} footer={null} width={860} destroyOnHidden>
    <Alert type="warning" showIcon title={t('resellers.historyWarning', 'Manual records are not verified payments. Use a unique reference and check for duplicates against existing sales; entries cannot be edited here.')} />
    <Form form={form} layout="vertical" onFinish={submit} initialValues={{ trafficBytes: 0 }} style={{ marginTop: 16 }}>
      <Form.Item name="reference" label={t('resellers.historyReference', 'Unique source reference')} rules={[{ required: true, whitespace: true, max: 128 }]}><Input /></Form.Item>
      <Form.Item name="source" label={t('resellers.historySource', 'Source / evidence description')} rules={[{ required: true, whitespace: true, max: 512 }]}><Input /></Form.Item>
      <Form.Item name="email" label={t('pages.clients.email')} rules={[{ required: true, whitespace: true, max: 254 }]}><Input /></Form.Item>
      <Form.Item name="soldAt" label={t('resellers.historySoldAt', 'Sale date and time')} rules={[{ required: true }]}><DatePicker showTime /></Form.Item>
      <Form.Item name="trafficBytes" label={t('resellers.historyTrafficBytes', 'Initial traffic (bytes)')} rules={[{ required: true }]}><InputNumber min={0} max={Number.MAX_SAFE_INTEGER} precision={0} /></Form.Item>
      <Form.Item name="amountMinor" label={t('resellers.declaredSaleAmount', 'Declared sale amount (minor units)')}><InputNumber min={0} max={Number.MAX_SAFE_INTEGER} precision={0} /></Form.Item>
      <Form.Item name="currency" label={t('resellers.saleCurrency', 'Currency code')}><Input maxLength={3} placeholder="IRR" /></Form.Item>
      <Button htmlType="submit" type="primary" loading={saving}>{t('resellers.historyAdd', 'Add historical record')}</Button>
    </Form>
    {history.isError && <Alert type="error" title={t('somethingWentWrong')} />}
    <Table<HistoricalSale> style={{ marginTop: 16 }} size="small" rowKey="id" loading={history.isLoading} dataSource={history.data ?? []} scroll={{ x: 1000 }} pagination={{ pageSize: 10 }} columns={[
      { title: t('resellers.historyReference', 'Unique source reference'), dataIndex: 'reference' },
      { title: t('resellers.historySource', 'Source / evidence description'), dataIndex: 'source' },
      { title: t('pages.clients.email'), dataIndex: 'email' },
      { title: t('resellers.historySoldAt', 'Sale date and time'), dataIndex: 'soldAt', render: (v: number) => new Date(v).toISOString() },
      { title: t('resellers.declaredSalesMinor', 'Declared sales (minor units)'), render: (_, row) => row.amountMinor == null ? '—' : `${row.currency}: ${row.amountMinor}` },
      { title: t('resellers.historyActor', 'Recorded by'), render: (_, row) => `${row.actorType}: ${row.actorName}` },
      { title: t('resellers.historyRecordedAt', 'Recorded at'), dataIndex: 'recordedAt', render: (v: number) => new Date(v).toISOString() },
    ]} />
  </Modal></>;
}
