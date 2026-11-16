import { useTranslation } from 'react-i18next';
import { Alert, Form, Input, Select, Switch } from 'antd';
import { useOutboundTags } from '@/api/queries/useOutboundTags';

export default function PptpFields() {
  const { t } = useTranslation();
  const form = Form.useFormInstance();
  const routeThroughXray = Form.useWatch(['settings', 'routeThroughXray'], form) as boolean | undefined;
  const { data: outboundTags } = useOutboundTags();
  return (
    <>
      <Alert type="warning" showIcon message={t('pages.inbounds.form.pptpSecurityWarning')} className="mb-12" />
      <Form.Item name={['settings', 'localIP']} label={t('pages.inbounds.form.pptpLocalIP')}><Input placeholder="10.253.0.1" /></Form.Item>
      <Form.Item name={['settings', 'poolCIDR']} label={t('pages.inbounds.form.pptpPoolCIDR')}><Input placeholder="10.253.0.0/24" /></Form.Item>
      <Form.Item name={['settings', 'dns1']} label={t('pages.inbounds.form.pptpDns1')}><Input placeholder="1.1.1.1" /></Form.Item>
      <Form.Item name={['settings', 'dns2']} label={t('pages.inbounds.form.pptpDns2')}><Input placeholder="8.8.8.8" /></Form.Item>
      <Form.Item name={['settings', 'routeThroughXray']} label={t('pages.inbounds.form.daemonRouteThroughXray')} valuePropName="checked"><Switch /></Form.Item>
      {routeThroughXray && <Form.Item name={['settings', 'outboundTag']} label={t('pages.inbounds.form.mtgRouteOutbound')}>
        <Select allowClear showSearch options={(outboundTags ?? []).map((tag) => ({ value: tag, label: tag }))} />
      </Form.Item>}
    </>
  );
}
