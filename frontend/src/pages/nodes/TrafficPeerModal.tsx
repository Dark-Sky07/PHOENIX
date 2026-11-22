import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Alert, Button, Checkbox, Form, Input, InputNumber, Modal, Select, Space, Tabs, Typography, message } from 'antd';
import { HttpUtil } from '@/utils';
import type { NodeRecord } from '@/schemas/node';

type SenderState = { sender: string; generation: string; lastSequence: number };
type ReceiverFields = { tokenId: number; sender: string; generation: string; reconciled: boolean };
type SenderFields = { nodeId: number; reconciled: boolean };

interface Props { open: boolean; nodes: NodeRecord[]; onClose: () => void; onChanged: () => void }

// Explicit setup only; no operation resets an existing sequence or silently
// enables legacy fallback. The server remains the authority for permissions.
export default function TrafficPeerModal({ open, nodes, onClose, onChanged }: Props) {
  const { t } = useTranslation();
  const [senderForm] = Form.useForm<SenderFields>();
  const [receiverForm] = Form.useForm<ReceiverFields>();
  const [busy, setBusy] = useState(false);
  const [senderState, setSenderState] = useState<SenderState | null>(null);
  const [error, setError] = useState('');
  const senderAcknowledged = Form.useWatch('reconciled', senderForm);
  const receiverAcknowledged = Form.useWatch('reconciled', receiverForm);
  const selectedNode = Form.useWatch('nodeId', senderForm);
  const options = nodes.filter((node) => !node.transitive).map((node) => ({ value: node.id, label: `${node.name || node.id} (#${node.id})` }));

  const registerSender = async (values: SenderFields) => {
    if (!values.reconciled) return;
    setBusy(true); setError(''); setSenderState(null);
    try {
      const msg = await HttpUtil.post<SenderState>(`/panel/api/nodes/trafficSnapshot/sender/${values.nodeId}`, { reconciled: true }, { silent: true });
      if (!msg.success || !msg.obj?.sender || !msg.obj?.generation) { setError(msg.msg || t('somethingWentWrong')); return; }
      setSenderState(msg.obj); senderForm.setFieldValue('reconciled', false); onChanged(); message.success(t('pages.nodes.trafficPeers.senderSaved'));
    } finally { setBusy(false); }
  };

  const activateSender = async () => {
    if (!selectedNode || !senderAcknowledged) return;
    setBusy(true); setError('');
    try {
      const msg = await HttpUtil.post(`/panel/api/nodes/trafficSnapshot/sender/${selectedNode}/activate`, { reconciled: true }, { silent: true });
      if (!msg.success) { setError(msg.msg || t('somethingWentWrong')); return; }
      senderForm.setFieldValue('reconciled', false); onChanged(); message.success(t('pages.nodes.trafficPeers.senderActivated'));
    } finally { setBusy(false); }
  };

  const readSender = async () => {
    if (!selectedNode) return;
    setBusy(true); setError(''); setSenderState(null);
    try {
      const msg = await HttpUtil.get<SenderState>(`/panel/api/nodes/trafficSnapshot/sender/${selectedNode}`, undefined, { silent: true });
      if (!msg.success || !msg.obj?.sender || !msg.obj?.generation) { setError(msg.msg || t('somethingWentWrong')); return; }
      setSenderState(msg.obj);
    } finally { setBusy(false); }
  };

  const registerReceiver = async (values: ReceiverFields) => {
    if (!values.reconciled) return;
    setBusy(true); setError('');
    try {
      const msg = await HttpUtil.post('/panel/api/nodes/trafficSnapshot/receiver', { ...values, reconciled: true }, { silent: true });
      if (!msg.success) { setError(msg.msg || t('somethingWentWrong')); return; }
      message.success(t('pages.nodes.trafficPeers.receiverSaved'));
      receiverForm.resetFields();
    } finally { setBusy(false); }
  };

  const close = () => {
    if (busy) return;
    senderForm.resetFields(); receiverForm.resetFields(); setSenderState(null); setError(''); onClose();
  };
  const required = [{ required: true, message: t('pages.nodes.trafficPeers.required') }];
  return (
    <Modal open={open} title={t('pages.nodes.trafficPeers.title')} footer={null} onCancel={close} closable={!busy} mask={{ closable: !busy }} keyboard={!busy} destroyOnHidden>
      <Space orientation="vertical" size="middle" style={{ width: '100%' }}>
        <Alert type="warning" showIcon title={t('pages.nodes.trafficPeers.warningTitle')} description={t('pages.nodes.trafficPeers.warning')} />
        {error && <Alert type="error" showIcon title={error} />}
        <Tabs items={[
          { key: 'sender', label: t('pages.nodes.trafficPeers.senderTab'), children: (
            <Form form={senderForm} layout="vertical" onFinish={registerSender} initialValues={{ reconciled: false }}>
              <Form.Item name="nodeId" label={t('pages.nodes.trafficPeers.node')} rules={required}>
                <Select options={options} onChange={() => { setSenderState(null); setError(''); }} />
              </Form.Item>
              <Form.Item name="reconciled" valuePropName="checked"><Checkbox>{t('pages.nodes.trafficPeers.confirm')}</Checkbox></Form.Item>
              <Space wrap>
                <Button htmlType="submit" type="primary" disabled={!senderAcknowledged} loading={busy}>{t('pages.nodes.trafficPeers.registerSender')}</Button>
                <Button onClick={readSender} disabled={!selectedNode || busy}>{t('pages.nodes.trafficPeers.readSender')}</Button>
                <Button onClick={activateSender} disabled={!selectedNode || !senderAcknowledged || busy}>{t('pages.nodes.trafficPeers.activateSender')}</Button>
              </Space>
              {senderState && <Alert style={{ marginTop: 16 }} type="info" showIcon title={t('pages.nodes.trafficPeers.transferTitle')} description={(
                <Space orientation="vertical" size="small">
                  <span>{t('pages.nodes.trafficPeers.transferHint')}</span>
                  <Typography.Text copyable>{senderState.sender}</Typography.Text>
                  <Typography.Text copyable>{senderState.generation}</Typography.Text>
                  <span>{t('pages.nodes.trafficPeers.sequence', { count: senderState.lastSequence })}</span>
                </Space>
              )} />}
            </Form>
          ) },
          { key: 'receiver', label: t('pages.nodes.trafficPeers.receiverTab'), children: (
            <Form form={receiverForm} layout="vertical" onFinish={registerReceiver} initialValues={{ reconciled: false }}>
              <Form.Item name="tokenId" label={t('pages.nodes.trafficPeers.tokenId')} rules={required}><InputNumber min={1} precision={0} style={{ width: '100%' }} /></Form.Item>
              <Form.Item name="sender" label={t('pages.nodes.trafficPeers.senderIdentity')} rules={required}><Input maxLength={256} autoComplete="off" /></Form.Item>
              <Form.Item name="generation" label={t('pages.nodes.trafficPeers.generation')} rules={required}><Input maxLength={256} autoComplete="off" /></Form.Item>
              <Form.Item name="reconciled" valuePropName="checked"><Checkbox>{t('pages.nodes.trafficPeers.confirm')}</Checkbox></Form.Item>
              <Button htmlType="submit" type="primary" disabled={!receiverAcknowledged} loading={busy}>{t('pages.nodes.trafficPeers.registerReceiver')}</Button>
            </Form>
          ) },
        ]} />
      </Space>
    </Modal>
  );
}
