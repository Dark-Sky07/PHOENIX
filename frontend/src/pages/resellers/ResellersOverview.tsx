import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Bar,
  BarChart,
  CartesianGrid,
  Legend,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import {
  Card,
  Col,
  Empty,
  Input,
  Progress,
  Row,
  Select,
  Space,
  Table,
  Tag,
  Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { AlertOutlined, ApiOutlined, CloudDownloadOutlined, DatabaseOutlined, ShopOutlined, TeamOutlined, WifiOutlined } from '@ant-design/icons';
import { StatStrip, type StatItem } from '@/components/ui';

import { SizeFormatter } from '@/utils';
import type { ResellerStat } from '@/api/queries/useSession';

type StatusFilter = 'all' | 'active' | 'disabled' | 'expired' | 'overQuota';

export interface ResellerOverviewPoint {
  name: string;
  clients: number;
  online: number;
  usedTraffic: number;
  allocatedTraffic: number;
}

/** Return a bounded top-10 snapshot, sorted by measured usage for chart legibility. */
export function buildResellerOverviewPoints(stats: ResellerStat[]): ResellerOverviewPoint[] {
  return [...stats]
    .sort((a, b) => b.usedTraffic - a.usedTraffic)
    .slice(0, 10)
    .map((stat) => ({
      name: stat.reseller.name || stat.reseller.username,
      clients: stat.clientCount,
      online: stat.onlineCount,
      usedTraffic: Math.max(0, stat.usedTraffic),
      allocatedTraffic: Math.max(0, stat.allocatedTraffic),
    }));
}

function formatDate(ts?: number): string {
  if (!ts) return '—';
  return new Date(ts).toLocaleDateString();
}

/** Coarse health bucket for one reseller, most severe first. */
function statusOf(stat: ResellerStat, t: (key: string) => string): { label: string; color: string } {
  if (!stat.reseller.enable) return { label: t('resellers.statusDisabled'), color: 'red' };
  if (stat.expired) return { label: t('resellers.expired'), color: 'orange' };
  if (stat.overQuota) return { label: t('resellers.overQuota'), color: 'orange' };
  return { label: t('resellers.statusActive'), color: 'green' };
}

interface ResellersOverviewProps {
  stats: ResellerStat[];
  loading?: boolean;
}

/**
 * Live, read-only overview of every reseller (نمایندگی) for the admin:
 * aggregate health numbers on top, a sortable/filterable per-reseller table
 * below. All data comes from the same /panel/api/resellers/list snapshot the
 * management table uses, so it refreshes with it (websocket + periodic
 * refetch) — no extra backend endpoint is needed.
 */
export default function ResellersOverview({ stats, loading }: ResellersOverviewProps) {
  const { t } = useTranslation();
  const [search, setSearch] = useState('');
  const [statusFilter, setStatusFilter] = useState<StatusFilter>('all');

  const summary = useMemo(() => {
    const total = stats.length;
    const active = stats.filter((s) => s.reseller.enable && !s.expired && !s.overQuota).length;
    const disabled = stats.filter((s) => !s.reseller.enable).length;
    const inbounds = stats.reduce((sum, s) => sum + s.inboundCount, 0);
    const clients = stats.reduce((sum, s) => sum + s.clientCount, 0);
    const online = stats.reduce((sum, s) => sum + s.onlineCount, 0);
    const used = stats.reduce((sum, s) => sum + s.usedTraffic, 0);
    const allocated = stats.reduce((sum, s) => sum + s.allocatedTraffic, 0);
    const attention = stats.filter((s) => !s.reseller.enable || s.expired || s.overQuota).length;
    return { total, active, disabled, inbounds, clients, online, used, allocated, attention };
  }, [stats]);

  const chartData = useMemo(() => buildResellerOverviewPoints(stats), [stats]);

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    return stats.filter((stat) => {
      if (statusFilter === 'active' && (!stat.reseller.enable || stat.expired || stat.overQuota)) return false;
      if (statusFilter === 'disabled' && stat.reseller.enable) return false;
      if (statusFilter === 'expired' && !stat.expired) return false;
      if (statusFilter === 'overQuota' && !stat.overQuota) return false;
      if (!q) return true;
      const haystack = `${stat.reseller.name} ${stat.reseller.username} ${stat.reseller.comment ?? ''}`.toLowerCase();
      return haystack.includes(q);
    });
  }, [stats, search, statusFilter]);

  const trafficPercent = (stat: ResellerStat): number =>
    stat.reseller.trafficLimit ? Math.min(100, Math.round((stat.allocatedTraffic / stat.reseller.trafficLimit) * 100)) : 0;

  const columns: ColumnsType<ResellerStat> = [
    {
      title: t('resellers.table.reseller'),
      key: 'reseller',
      render: (_value, stat) => (
        <Space orientation="vertical" size={0}>
          <b>{stat.reseller.name || stat.reseller.username}</b>
          <Typography.Text type="secondary">{stat.reseller.username}</Typography.Text>
        </Space>
      ),
    },
    {
      title: t('resellers.table.status'),
      key: 'status',
      width: 130,
      render: (_value, stat) => {
        const st = statusOf(stat, t);
        return <Tag color={st.color}>{st.label}</Tag>;
      },
    },
    {
      title: t('resellers.table.inbounds'),
      key: 'inbounds',
      width: 100,
      sorter: (a, b) => a.inboundCount - b.inboundCount,
      render: (_value, stat) => stat.inboundCount,
    },
    {
      title: t('resellers.table.clients'),
      key: 'clients',
      width: 120,
      sorter: (a, b) => a.clientCount - b.clientCount,
      render: (_value, stat) =>
        stat.reseller.clientLimit ? `${stat.clientCount} / ${stat.reseller.clientLimit}` : stat.clientCount,
    },
    {
      title: t('resellers.online'),
      key: 'online',
      width: 100,
      sorter: (a, b) => a.onlineCount - b.onlineCount,
      render: (_value, stat) => (
        <Typography.Text type={stat.onlineCount > 0 ? 'success' : undefined}>
          {stat.onlineCount > 0 ? `● ${stat.onlineCount}` : stat.onlineCount}
        </Typography.Text>
      ),
    },
    {
      title: t('resellers.table.used'),
      key: 'used',
      width: 130,
      sorter: (a, b) => a.usedTraffic - b.usedTraffic,
      render: (_value, stat) => SizeFormatter.sizeFormat(stat.usedTraffic),
    },
    {
      title: t('resellers.table.traffic'),
      key: 'traffic',
      width: 230,
      sorter: (a, b) => {
        const ra = a.reseller.trafficLimit ? a.allocatedTraffic / a.reseller.trafficLimit : 0;
        const rb = b.reseller.trafficLimit ? b.allocatedTraffic / b.reseller.trafficLimit : 0;
        return ra - rb;
      },
      render: (_value, stat) => (
        <Space orientation="vertical" size={2} style={{ width: '100%' }}>
          <Typography.Text>
            {SizeFormatter.sizeFormat(stat.allocatedTraffic)}
            {stat.reseller.trafficLimit
              ? ` / ${SizeFormatter.sizeFormat(stat.reseller.trafficLimit)}`
              : ` / ${t('resellers.unlimited')}`}
          </Typography.Text>
          {stat.reseller.trafficLimit > 0 && (
            <Progress percent={trafficPercent(stat)} size="small" showInfo={false} status={stat.overQuota ? 'exception' : 'normal'} />
          )}
        </Space>
      ),
    },
    {
      title: t('resellers.table.remaining'),
      key: 'remaining',
      width: 130,
      sorter: (a, b) => {
        const ra = a.reseller.trafficLimit ? a.remainingTraffic : Number.MAX_SAFE_INTEGER;
        const rb = b.reseller.trafficLimit ? b.remainingTraffic : Number.MAX_SAFE_INTEGER;
        return ra - rb;
      },
      render: (_value, stat) =>
        stat.reseller.trafficLimit ? SizeFormatter.sizeFormat(Math.max(0, stat.remainingTraffic)) : '—',
    },
    {
      title: t('resellers.expiry'),
      key: 'expiry',
      width: 120,
      sorter: (a, b) =>
        (a.reseller.expiryTime || Number.MAX_SAFE_INTEGER) - (b.reseller.expiryTime || Number.MAX_SAFE_INTEGER),
      render: (_value, stat) =>
        stat.reseller.expiryTime ? (
          <Typography.Text type={stat.expired ? 'danger' : undefined}>
            {formatDate(stat.reseller.expiryTime)}
          </Typography.Text>
        ) : (
          <Typography.Text type="secondary">{t('resellers.never')}</Typography.Text>
        ),
    },
    {
      title: t('resellers.table.created'),
      key: 'created',
      width: 120,
      sorter: (a, b) => (a.reseller.createdAt ?? 0) - (b.reseller.createdAt ?? 0),
      render: (_value, stat) => formatDate(stat.reseller.createdAt),
    },
    {
      title: t('resellers.table.updated'),
      key: 'updated',
      width: 120,
      sorter: (a, b) => (a.reseller.updatedAt ?? 0) - (b.reseller.updatedAt ?? 0),
      render: (_value, stat) => formatDate(stat.reseller.updatedAt),
    },
  ];

  const statItems: StatItem[] = [
    {
      key: 'total', icon: <ShopOutlined />, tone: 'primary', label: t('resellers.total'), value: summary.total,
      hint: `${summary.active} ${t('resellers.statusActive')} / ${summary.disabled} ${t('resellers.statusDisabled')}`,
    },
    { key: 'inbounds', icon: <ApiOutlined />, tone: 'default', label: t('resellers.assignedInbounds'), value: summary.inbounds },
    { key: 'clients', icon: <TeamOutlined />, tone: 'accent', label: t('resellers.totalClients'), value: summary.clients },
    { key: 'online', icon: <WifiOutlined />, tone: summary.online > 0 ? 'success' : 'muted', label: t('resellers.onlineNow'), value: summary.online },
    { key: 'used', icon: <CloudDownloadOutlined />, tone: 'default', label: t('resellers.totalUsedTraffic'), value: SizeFormatter.sizeFormat(summary.used) },
    { key: 'allocated', icon: <DatabaseOutlined />, tone: 'muted', label: t('resellers.totalAllocatedTraffic'), value: SizeFormatter.sizeFormat(summary.allocated) },
    {
      key: 'attention', icon: <AlertOutlined />, tone: summary.attention > 0 ? 'warning' : 'muted', label: t('resellers.needsAttention'), value: summary.attention,
      popover: { content: t('resellers.needsAttentionHint') },
    },
  ];

  return (
    <>
      <StatStrip items={statItems} size="compact" className="resellers-stats" />

      <Row gutter={[12, 12]} style={{ marginBottom: 12 }}>
        <Col xs={24} xl={14}>
          <Card size="small" title={t('resellers.dashboardTrafficByReseller', 'Traffic by reseller (top 10)')}>
            {chartData.length === 0 ? (
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t('resellers.noData', 'No usage data')} />
            ) : (
              <div role="img" aria-label={t('resellers.dashboardTrafficByReseller', 'Traffic by reseller (top 10)')} style={{ width: '100%', height: 270 }}>
                <ResponsiveContainer width="100%" height="100%">
                  <BarChart data={chartData} margin={{ top: 8, right: 12, bottom: 20, left: 8 }}>
                    <CartesianGrid strokeDasharray="3 3" vertical={false} />
                    <XAxis dataKey="name" interval={0} angle={-18} textAnchor="end" height={56} />
                    <YAxis width={80} tickFormatter={(value: number) => SizeFormatter.sizeFormat(value)} />
                    <Tooltip formatter={(value, name) => [SizeFormatter.sizeFormat(Number(value ?? 0)), String(name)]} />
                    <Legend />
                    <Bar dataKey="usedTraffic" name={t('resellers.usedTraffic', 'Used')} fill="#1677ff" radius={[3, 3, 0, 0]} />
                    <Bar dataKey="allocatedTraffic" name={t('resellers.totalAllocatedTraffic', 'Allocated')} fill="#13c2c2" radius={[3, 3, 0, 0]} />
                  </BarChart>
                </ResponsiveContainer>
              </div>
            )}
          </Card>
        </Col>
        <Col xs={24} xl={10}>
          <Card size="small" title={t('resellers.dashboardClientsByReseller', 'Clients and online users')}>
            {chartData.length === 0 ? (
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t('resellers.noData', 'No usage data')} />
            ) : (
              <div role="img" aria-label={t('resellers.dashboardClientsByReseller', 'Clients and online users')} style={{ width: '100%', height: 270 }}>
                <ResponsiveContainer width="100%" height="100%">
                  <BarChart data={chartData} margin={{ top: 8, right: 10, bottom: 20, left: 0 }}>
                    <CartesianGrid strokeDasharray="3 3" vertical={false} />
                    <XAxis dataKey="name" interval={0} angle={-18} textAnchor="end" height={56} />
                    <YAxis allowDecimals={false} width={38} />
                    <Tooltip formatter={(value, name) => [Number(value ?? 0), String(name)]} />
                    <Legend />
                    <Bar dataKey="clients" name={t('resellers.totalClients', 'Clients')} fill="#52c41a" radius={[3, 3, 0, 0]} />
                    <Bar dataKey="online" name={t('resellers.online', 'Online')} fill="#fa8c16" radius={[3, 3, 0, 0]} />
                  </BarChart>
                </ResponsiveContainer>
              </div>
            )}
          </Card>
        </Col>
      </Row>

      <Card className="phoenix-table-card">
        <div className="phoenix-toolbar">
          <Input
            allowClear
            placeholder={t('resellers.searchPlaceholder')}
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            style={{ maxWidth: 320 }}
          />
          <span className="phoenix-toolbar-spacer" />
          <Select<StatusFilter>
            value={statusFilter}
            onChange={setStatusFilter}
            style={{ width: 180 }}
            options={[
              { value: 'all', label: t('resellers.allStatuses') },
              { value: 'active', label: t('resellers.statusActive') },
              { value: 'disabled', label: t('resellers.statusDisabled') },
              { value: 'expired', label: t('resellers.expired') },
              { value: 'overQuota', label: t('resellers.overQuota') },
            ]}
          />
        </div>

        <Table<ResellerStat>
          rowKey={(stat) => String(stat.reseller.id)}
          size="middle"
          loading={loading}
          columns={columns}
          dataSource={filtered}
          pagination={filtered.length > 20 ? { pageSize: 20 } : false}
          scroll={{ x: 'max-content' }}
          locale={{ emptyText: <Empty description={t('resellers.noDataForFilter')} /> }}
        />
      </Card>
    </>
  );
}
