import { useTranslation } from 'react-i18next';
import { Alert, Form, Input, Select, Switch } from 'antd';
import { useOutboundTags } from '@/api/queries/useOutboundTags';

export default function SstpFields() {
  const { t } = useTranslation();
  const form = Form.useFormInstance();
  const routeThroughXray = Form.useWatch(['settings', 'routeThroughXray'], form) as boolean | undefined;
  const { data: outboundTags } = useOutboundTags();
  return (
    <>
      <Alert type="info" showIcon message={t('pages.inbounds.form.sstpDaemonHint')} className="mb-12" />
      <Form.Item name={['settings', 'certificatePEM']} label={t('pages.inbounds.form.sstpCertificate')} rules={[{ required: true }]}>
        <Input.TextArea rows={6} autoSize={{ minRows: 5, maxRows: 12 }} spellCheck={false} />
      </Form.Item>
      <Form.Item name={['settings', 'privateKeyPEM']} label={t('pages.inbounds.form.sstpPrivateKey')} rules={[{ required: true }]}>
        <Input.TextArea rows={6} autoSize={{ minRows: 5, maxRows: 12 }} spellCheck={false} />
      </Form.Item>
      <Form.Item name={['settings', 'localIP']} label={t('pages.inbounds.form.sstpLocalIP')}><Input placeholder="10.254.0.1" /></Form.Item>
      <Form.Item name={['settings', 'poolCIDR']} label={t('pages.inbounds.form.sstpPoolCIDR')}><Input placeholder="10.254.0.0/24" /></Form.Item>
      <Form.Item name={['settings', 'dns1']} label={t('pages.inbounds.form.sstpDns1')}><Input placeholder="1.1.1.1" /></Form.Item>
      <Form.Item name={['settings', 'dns2']} label={t('pages.inbounds.form.sstpDns2')}><Input placeholder="8.8.8.8" /></Form.Item>
      <Form.Item name={['settings', 'routeThroughXray']} label={t('pages.inbounds.form.daemonRouteThroughXray')} valuePropName="checked"><Switch /></Form.Item>
      {routeThroughXray && <Form.Item name={['settings', 'outboundTag']} label={t('pages.inbounds.form.mtgRouteOutbound')}>
        <Select allowClear showSearch options={(outboundTags ?? []).map((tag) => ({ value: tag, label: tag }))} />
      </Form.Item>}
    </>
  );
}
