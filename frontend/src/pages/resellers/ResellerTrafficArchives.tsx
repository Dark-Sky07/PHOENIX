import { Card, Table, Typography } from 'antd';
import { useTranslation } from 'react-i18next';
import type { ResellerTrafficArchive } from '@/api/queries/useSession';
import { SizeFormatter } from '@/utils';

export default function ResellerTrafficArchives({ rows }: { rows: ResellerTrafficArchive[] }) {
  const { t } = useTranslation();
  return (
    <Card size="small" title={t('resellers.archiveTitle', 'Traffic snapshots')}>
      <Typography.Paragraph type="secondary">
        {t('resellers.archiveDescription', 'Latest 500 persisted traffic records from deletion, unassignment, automatic renewal or supported resets. Prior windows and current snapshots are shown separately. Runtime traffic not yet persisted is not included. Manual counter corrections preserve the recorded usage floor.')}
      </Typography.Paragraph>
      <Table<ResellerTrafficArchive> size="small" rowKey="id" dataSource={rows} pagination={{ pageSize: 10 }} columns={[
        { title: t('pages.clients.email'), dataIndex: 'email' },
        { title: t('resellers.archiveClientId', 'Client record ID'), dataIndex: 'clientRecordId' },
        { title: t('resellers.archivePriorUsage', 'Prior recorded usage'), dataIndex: 'priorConsumedBytes', render: (value: number) => SizeFormatter.sizeFormat(value || 0) },
        { title: t('resellers.archiveUsage', 'Current window snapshot'), dataIndex: 'consumedBytes', render: (value: number) => SizeFormatter.sizeFormat(value) },
        { title: t('resellers.archiveUpdated', 'Last snapshot (UTC)'), dataIndex: 'updatedAt', render: (value: number) => value ? new Date(value).toISOString() : '—' },
      ]} />
    </Card>
  );
}
