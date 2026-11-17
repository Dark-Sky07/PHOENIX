import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Card, Col, Empty, Row } from 'antd';
import {
  Bar,
  BarChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';

import type { ResellerStat } from '@/api/queries/useSession';
import { SizeFormatter } from '@/utils';

interface ResellerUsageChartsProps {
  stat?: ResellerStat;
}

/** A current-snapshot dashboard using only the reseller-scoped profile payload. */
export default function ResellerUsageCharts({ stat }: ResellerUsageChartsProps) {
  const { t } = useTranslation();
  const traffic = useMemo(() => {
    if (!stat) return [];
    const points = [
      { label: t('resellers.usedTraffic', 'Used'), bytes: Math.max(0, stat.usedTraffic) },
      { label: t('resellers.totalAllocatedTraffic', 'Allocated'), bytes: Math.max(0, stat.allocatedTraffic) },
    ];
    if (stat.reseller.trafficLimit > 0) {
      points.push({ label: t('resellers.trafficLimit', 'Reseller limit'), bytes: stat.reseller.trafficLimit });
    }
    return points;
  }, [stat, t]);
  const clients = useMemo(() => {
    if (!stat) return [];
    const points = [
      { label: t('resellers.table.clients', 'Clients'), count: stat.clientCount },
      { label: t('resellers.online', 'Online'), count: stat.onlineCount },
    ];
    if (stat.reseller.clientLimit > 0) {
      points.push({ label: t('resellers.clientLimit', 'Client limit'), count: stat.reseller.clientLimit });
    }
    return points;
  }, [stat, t]);

  return (
    <Row gutter={[12, 12]}>
      <Col xs={24} lg={14}>
        <Card size="small" title={t('resellers.dashboardTraffic', 'Traffic usage snapshot')}>
          {traffic.length === 0 ? (
            <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t('resellers.noData', 'No usage data')} />
          ) : (
            <div role="img" aria-label={t('resellers.dashboardTraffic', 'Traffic usage snapshot')} style={{ width: '100%', height: 230 }}>
              <ResponsiveContainer width="100%" height="100%">
                <BarChart data={traffic} margin={{ top: 8, right: 16, bottom: 4, left: 8 }}>
                  <CartesianGrid strokeDasharray="3 3" vertical={false} />
                  <XAxis dataKey="label" />
                  <YAxis width={86} tickFormatter={(value: number) => SizeFormatter.sizeFormat(value)} />
                  <Tooltip formatter={(value) => [SizeFormatter.sizeFormat(Number(value ?? 0)), t('resellers.table.traffic', 'Traffic')]} />
                  <Bar dataKey="bytes" name={t('resellers.table.traffic', 'Traffic')} fill="#1677ff" radius={[5, 5, 0, 0]} />
                </BarChart>
              </ResponsiveContainer>
            </div>
          )}
        </Card>
      </Col>
      <Col xs={24} lg={10}>
        <Card size="small" title={t('resellers.dashboardClients', 'Clients and online users')}>
          {clients.length === 0 ? (
            <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t('resellers.noData', 'No usage data')} />
          ) : (
            <div role="img" aria-label={t('resellers.dashboardClients', 'Clients and online users')} style={{ width: '100%', height: 230 }}>
              <ResponsiveContainer width="100%" height="100%">
                <BarChart data={clients} margin={{ top: 8, right: 16, bottom: 4, left: 0 }}>
                  <CartesianGrid strokeDasharray="3 3" vertical={false} />
                  <XAxis dataKey="label" />
                  <YAxis allowDecimals={false} width={42} />
                  <Tooltip formatter={(value) => [Number(value ?? 0), t('resellers.totalClients', 'Users')]} />
                  <Bar dataKey="count" name={t('resellers.totalClients', 'Users')} fill="#52c41a" radius={[5, 5, 0, 0]} />
                </BarChart>
              </ResponsiveContainer>
            </div>
          )}
        </Card>
      </Col>
    </Row>
  );
}
