import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Alert,
  Button,
  Card,
  ConfigProvider,
  Descriptions,
  Empty,
  Input,
  Layout,
  Modal,
  Space,
  Table,
  Tag,
  Typography,
  message,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import {
  CheckCircleOutlined,
  ReloadOutlined,
  SafetyCertificateOutlined,
  WarningOutlined,
} from '@ant-design/icons';

import AppSidebar from '@/layouts/AppSidebar';
import { HttpUtil } from '@/utils';
import { setMessageInstance } from '@/utils/messageBus';
import { useMediaQuery } from '@/hooks/useMediaQuery';
import { useTheme } from '@/hooks/useTheme';
import { PageHeader } from '@/components/ui';
import './ResellersPage.css';

const CONFIRM_TOKEN = 'RECONCILE_COUNTER_INCIDENTS';
const RECONCILIATION_KEY = ['resellers', 'reconciliation'];

interface ReconciliationReport {
  available: boolean;
  generatedAt: number;
  counterIncident: {
    available: boolean;
    databaseCount: number;
    fallbackPresent: boolean;
    pendingInProcess: boolean;
    daemonIdentityCount: number;
  };
  danglingMappings: { resellerId: number; username?: string; email: string; createdAt?: number }[];
  dbIncidents: {
    id: number;
    nodeId: number;
    email?: string;
    direction?: string;
    createdAt?: number;
  }[];
  inFlightPolls: string[];
  fallbackFiles: string[];
  l2tpMarkerPresent: boolean;
  l2tpEvidenceFiles: string[];
  xrayJournals: string[];
  tuicJournals: string[];
  xrayApi: { port: number; reachable: boolean; error?: string };
  warnings: string[];
}

interface ReconcileResult {
  dryRun: boolean;
  backupDir?: string;
  archivedMappings: number;
  clearedIncidents: number;
  clearedPolls: number;
  movedFallbackFiles: number;
  movedL2tpMarker: boolean;
  movedL2tpEvidenceFiles: number;
  warnings: string[];
}

function nameList(values: string[]): ReactNode {
  if (!values || values.length === 0) return <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} />;
  return (
    <Space direction="vertical" size={2} style={{ maxHeight: 180, overflowY: 'auto' }}>
      {values.map((value) => (
        <Typography.Text key={value} code>
          {value}
        </Typography.Text>
      ))}
    </Space>
  );
}

export default function ReconciliationPage() {
  const { t } = useTranslation();
  const { isDark, isUltra, antdThemeConfig } = useTheme();
  const { isMobile } = useMediaQuery();
  const queryClient = useQueryClient();
  const [messageApi, messageContextHolder] = message.useMessage();
  const [repairOpen, setRepairOpen] = useState(false);
  const [confirmation, setConfirmation] = useState('');
  const [busy, setBusy] = useState(false);
  const [lastResult, setLastResult] = useState<ReconcileResult | null>(null);

  useEffect(() => {
    setMessageInstance(messageApi);
  }, [messageApi]);

  const query = useQuery({
    queryKey: RECONCILIATION_KEY,
    queryFn: async (): Promise<ReconciliationReport> => {
      const msg = await HttpUtil.get<ReconciliationReport>('/panel/api/resellers/reconciliation', undefined, {
        silent: true,
      });
      if (!msg.success) throw new Error(msg.msg || 'failed');
      return msg.obj as ReconciliationReport;
    },
    refetchInterval: 15_000,
  });

  const report = query.data;
  const warningCount = useMemo(
    () =>
      (report?.danglingMappings.length || 0) +
      (report?.dbIncidents.length || 0) +
      (report?.inFlightPolls.length || 0) +
      (report?.fallbackFiles.length || 0) +
      (report?.l2tpMarkerPresent ? 1 : 0) +
      (report?.l2tpEvidenceFiles.length || 0) +
      (report?.xrayJournals.length || 0) +
      (report?.tuicJournals.length || 0) +
      (report?.xrayApi.reachable ? 0 : 1),
    [report],
  );

  const runRepair = useCallback(
    async (dryRun: boolean, token?: string) => {
      setBusy(true);
      try {
        const msg = await HttpUtil.post<ReconcileResult>(
          '/panel/api/resellers/reconciliation/repair',
          { confirm: token || '', dryRun },
          { silent: true },
        );
        if (!msg.success) {
          messageApi.error(msg.msg || t('somethingWentWrong'));
          return;
        }
        setLastResult(msg.obj || null);
        await queryClient.invalidateQueries({ queryKey: RECONCILIATION_KEY });
      } finally {
        setBusy(false);
      }
    },
    [messageApi, queryClient, t],
  );

  const mappingColumns: ColumnsType<NonNullable<ReconciliationReport['danglingMappings']>[number]> = [
    { title: t('resellers.reconciliationReseller', 'Reseller'), dataIndex: 'username', key: 'username', render: (value, row) => value || `#${row.resellerId}` },
    { title: t('resellers.reconciliationEmail', 'Client email'), dataIndex: 'email', key: 'email' },
    {
      title: t('resellers.reconciliationCreated', 'Mapped at'),
      dataIndex: 'createdAt',
      key: 'createdAt',
      render: (value: number) => (value ? new Date(value).toLocaleString() : '-'),
    },
  ];

  const incidentColumns: ColumnsType<NonNullable<ReconciliationReport['dbIncidents']>[number]> = [
    { title: 'ID', dataIndex: 'id', key: 'id' },
    { title: t('resellers.reconciliationNode', 'Node'), dataIndex: 'nodeId', key: 'nodeId' },
    { title: t('resellers.reconciliationDirection', 'Direction'), dataIndex: 'direction', key: 'direction', render: (value: string) => value || '-' },
    { title: t('resellers.reconciliationEmail', 'Client email'), dataIndex: 'email', key: 'email', render: (value: string) => value || '-' },
  ];

  return (
    <ConfigProvider theme={antdThemeConfig}>
      <Layout className={isDark ? 'phoenix-layout is-dark' : 'phoenix-layout'} style={{ minHeight: '100vh', background: 'transparent' }}>
        <AppSidebar />
        <Layout.Content>
          <div className={['resellers-page', isDark ? 'is-dark' : '', isUltra ? 'is-ultra' : ''].filter(Boolean).join(' ')}>
            {messageContextHolder}
            <PageHeader
              title={t('resellers.reconciliationTitle', 'Traffic reconciliation')}
              subtitle={t('resellers.reconciliationSubtitle', 'Inspect and archive evidence that blocks reseller operations')}
              actions={
                <Space>
                  <Button icon={<ReloadOutlined />} loading={query.isFetching} onClick={() => query.refetch()}>
                    {t('refresh')}
                  </Button>
                  <Button icon={<SafetyCertificateOutlined />} loading={busy} onClick={() => runRepair(true)}>
                    {t('resellers.reconciliationDryRun', 'Dry run')}
                  </Button>
                  <Button type="primary" danger icon={<WarningOutlined />} onClick={() => setRepairOpen(true)}>
                    {t('resellers.reconciliationRepair', 'Backup and clear markers')}
                  </Button>
                </Space>
              }
            />

            {query.isError && (
              <Alert type="error" showIcon style={{ marginBottom: 12 }} message={t('somethingWentWrong')} />
            )}
            {report && !report.available && (
              <Alert type="error" showIcon style={{ marginBottom: 12 }} message={t('resellers.reconciliationUnavailable', 'Reconciliation status is incomplete')} />
            )}
            {warningCount === 0 && report && (
              <Alert
                type="success"
                showIcon
                icon={<CheckCircleOutlined />}
                style={{ marginBottom: 12 }}
                message={t('resellers.reconciliationClear', 'No reconciliation markers found')}
              />
            )}
            {lastResult && (
              <Alert
                type="info"
                showIcon
                style={{ marginBottom: 12 }}
                message={t('resellers.reconciliationResult', 'Repair result')}
                description={
                  <Space direction="vertical" size={2}>
                    <span>
                      {`Dry run: ${String(lastResult.dryRun)}; mappings archived: ${lastResult.archivedMappings}; incidents: ${lastResult.clearedIncidents}; polls: ${lastResult.clearedPolls}; fallback files moved: ${lastResult.movedFallbackFiles}; L2TP marker moved: ${String(lastResult.movedL2tpMarker)}; L2TP evidence files moved: ${lastResult.movedL2tpEvidenceFiles}`}
                    </span>
                    {lastResult.backupDir && <Typography.Text code>{lastResult.backupDir}</Typography.Text>}
                    {lastResult.warnings.map((warning) => (
                      <span key={warning}>{warning}</span>
                    ))}
                  </Space>
                }
              />
            )}

            <Card className="phoenix-table-card" title={t('resellers.reconciliationStatus', 'Status')} style={{ marginBottom: 12 }}>
              <Descriptions column={isMobile ? 1 : 3} size="small" bordered>
                <Descriptions.Item label={t('resellers.reconciliationXrayApi', 'Xray API')}>
                  {report?.xrayApi.reachable ? (
                    <Tag color="green">{`127.0.0.1:${report.xrayApi.port}`}</Tag>
                  ) : (
                    <Tag color="red">{report?.xrayApi.error || t('resellers.reconciliationUnavailable', 'unavailable')}</Tag>
                  )}
                </Descriptions.Item>
                <Descriptions.Item label={t('resellers.reconciliationDbIncidents', 'Counter incidents')}>
                  {report?.counterIncident.databaseCount ?? '-'}
                </Descriptions.Item>
                <Descriptions.Item label={t('resellers.reconciliationPending', 'Pending in process')}>
                  {report?.counterIncident.pendingInProcess ? t('yes', 'Yes') : t('no', 'No')}
                </Descriptions.Item>
              </Descriptions>
              {report?.warnings && report.warnings.length > 0 && (
                <Alert
                  type="warning"
                  showIcon
                  style={{ marginTop: 12 }}
                  message={t('resellers.reconciliationWarnings', 'Operator review required')}
                  description={<Space direction="vertical" size={2}>{report.warnings.map((warning) => <span key={warning}>{warning}</span>)}</Space>}
                />
              )}
            </Card>

            <Card className="phoenix-table-card" title={t('resellers.reconciliationDangling', 'Dangling reseller mappings')} style={{ marginBottom: 12 }}>
              <Table rowKey={(row) => `${row.resellerId}:${row.email}`} columns={mappingColumns} dataSource={report?.danglingMappings || []} pagination={false} size="small" />
            </Card>
            <Card className="phoenix-table-card" title={t('resellers.reconciliationDbIncidents', 'Counter incidents')} style={{ marginBottom: 12 }}>
              <Table rowKey={(row) => row.id} columns={incidentColumns} dataSource={report?.dbIncidents || []} pagination={false} size="small" />
            </Card>
            <Card className="phoenix-table-card" title={t('resellers.reconciliationFiles', 'Evidence files')}>
              <Space direction="vertical" size={12} style={{ width: '100%' }}>
                <div>
                  <Typography.Text strong>{t('resellers.reconciliationFallback', 'Fallback files')}</Typography.Text>
                  {nameList(report?.fallbackFiles || [])}
                </div>
                <div>
                  <Typography.Text strong>{t('resellers.reconciliationL2tp', 'L2TP accounting evidence')}</Typography.Text>
                  {report?.l2tpMarkerPresent && <Typography.Text code style={{ display: 'block' }}>l2tp-accounting-incident</Typography.Text>}
                  {nameList(report?.l2tpEvidenceFiles || [])}
                </div>
                <div>
                  <Typography.Text strong>{t('resellers.reconciliationInFlight', 'In-flight poll markers')}</Typography.Text>
                  {nameList(report?.inFlightPolls || [])}
                </div>
                <div>
                  <Typography.Text strong>{t('resellers.reconciliationJournals', 'Traffic journals (never cleared here)')}</Typography.Text>
                  {nameList([...(report?.xrayJournals || []), ...(report?.tuicJournals || [])])}
                </div>
              </Space>
            </Card>
          </div>
        </Layout.Content>
      </Layout>

      <Modal
        open={repairOpen}
        onCancel={() => {
          setRepairOpen(false);
          setConfirmation('');
        }}
        onOk={() => {
          void runRepair(false, confirmation).then(() => {
            setRepairOpen(false);
            setConfirmation('');
          });
        }}
        okButtonProps={{ disabled: confirmation !== CONFIRM_TOKEN || busy, danger: true }}
        title={t('resellers.reconciliationRepair', 'Backup and clear markers')}
      >
        <Space direction="vertical" size={10} style={{ width: '100%' }}>
          <Alert
            type="warning"
            showIcon
            message={t('resellers.reconciliationRepairWarning', 'This archives dangling mappings, counter incidents, poll markers, Xray fallback files, and L2TP accounting evidence. Traffic journals are never cleared. No service is stopped or restarted.')}
          />
          <Typography.Text>{t('resellers.reconciliationTypeToken', 'Type the confirmation token to continue:')}</Typography.Text>
          <Typography.Text code>{CONFIRM_TOKEN}</Typography.Text>
          <Input value={confirmation} onChange={(event) => setConfirmation(event.target.value)} autoComplete="off" />
        </Space>
      </Modal>
    </ConfigProvider>
  );
}
