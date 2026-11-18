import { useCallback, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Button, Card, Empty, Segmented, Space, Statistic, Table, Typography, message } from 'antd';
import { DownloadOutlined, ShoppingCartOutlined } from '@ant-design/icons';
import { Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts';
import { keys } from '@/api/queryKeys';
import { HttpUtil, SizeFormatter } from '@/utils';

type SalesPeriod = 'daily' | 'monthly' | 'yearly';

type SalesBucket = {
  period: string;
  startAt: number;
  sales: number;
  trafficBytes: number;
  unknownPrices: number;
  amountsMinor: Record<string, string>;
};

type SalesReport = {
  resellerId: number;
  period: SalesPeriod;
  totalSales: number;
  totalTrafficBytes: number;
  unknownPrices: number;
  amountsMinor: Record<string, string>;
  buckets: SalesBucket[];
};

type Props = {
  resellerId: number;
  self?: boolean;
};

export default function ResellerSalesSection({ resellerId, self = false }: Props) {
  const { t } = useTranslation();
  const [period, setPeriod] = useState<SalesPeriod>('monthly');
  const [messageApi, messageContextHolder] = message.useMessage();
  const [exporting, setExporting] = useState(false);

  const reportQuery = useQuery({
    queryKey: keys.resellers.sales(resellerId, period),
    enabled: resellerId > 0,
    queryFn: async (): Promise<SalesReport> => {
      const endpoint = self
        ? '/panel/api/reseller/sales'
        : `/panel/api/resellers/report/${resellerId}/sales`;
      const msg = await HttpUtil.get<SalesReport>(endpoint, { period });
      if (!msg.success) throw new Error(msg.msg || 'Failed to fetch reseller sales');
      return msg.obj as SalesReport;
    },
  });

  const exportCsv = useCallback(async () => {
    setExporting(true);
    try {
      const endpoint = self
        ? '/panel/api/reseller/sales/export'
        : `/panel/api/resellers/report/${resellerId}/sales/export`;
      const ok = await HttpUtil.downloadFile(`${endpoint}?period=${period}`, `reseller-sales-${period}.csv`);
      if (!ok) messageApi.error(t('somethingWentWrong'));
    } finally {
      setExporting(false);
    }
  }, [messageApi, period, resellerId, self, t]);

  const report = reportQuery.data;
  const buckets = report?.buckets ?? [];
  return (
    <>
      {messageContextHolder}
      <Card
        size="small"
        title={<Space><ShoppingCartOutlined />{t('resellers.salesReport', 'Account sales')}</Space>}
        extra={(
          <Space wrap>
            <Segmented
              size="small"
              value={period}
              onChange={(value) => setPeriod(value as SalesPeriod)}
              options={[
                { value: 'daily', label: t('resellers.daily', 'Daily') },
                { value: 'monthly', label: t('resellers.monthly', 'Monthly') },
                { value: 'yearly', label: t('resellers.yearly', 'Yearly') },
              ]}
            />
            <Button size="small" icon={<DownloadOutlined />} loading={exporting} onClick={exportCsv}>
              {t('resellers.exportCsv', 'Export CSV')}
            </Button>
          </Space>
        )}
      >
        <Typography.Paragraph type="secondary" style={{ marginBottom: 12 }}>
          {t('resellers.salesReportDescription', 'Successful reseller-created accounts and their initial allocated traffic, grouped by UTC period. Prices are self-declared, not verified payments; missing prices remain unknown.')}
        </Typography.Paragraph>
        <Space size="large" wrap style={{ marginBottom: 16 }}>
          <Statistic title={t('resellers.totalSales', 'Accounts sold')} value={report?.totalSales ?? 0} />
          <Statistic title={t('resellers.totalQuotaSold', 'Traffic allocated')} value={SizeFormatter.sizeFormat(report?.totalTrafficBytes ?? 0)} />
          <Statistic title={t('resellers.unknownPrices', 'Unknown prices')} value={report?.unknownPrices ?? 0} />
          {Object.entries(report?.amountsMinor ?? {}).map(([currency, amount]) => (
            <Statistic key={currency} title={`${t('resellers.declaredSales', 'Declared sales')} (${currency}, ${t('resellers.minorUnits', 'minor units')})`} value={amount} />
          ))}
        </Space>
        {reportQuery.isError ? (
          <Typography.Text type="danger">{t('somethingWentWrong')}</Typography.Text>
        ) : (
          <>
            {buckets.length > 0 && (
              <div role="img" aria-label={t('resellers.salesChart', 'Account sales by period')} style={{ width: '100%', height: 220, marginBottom: 12 }}>
                <ResponsiveContainer>
                  <BarChart data={buckets} margin={{ top: 8, right: 12, left: 0, bottom: 4 }}>
                    <CartesianGrid strokeDasharray="3 3" vertical={false} />
                    <XAxis dataKey="period" minTickGap={24} />
                    <YAxis allowDecimals={false} width={36} />
                    <Tooltip />
                    <Bar dataKey="sales" name={t('resellers.totalSales', 'Accounts sold')} fill="#1677ff" radius={[4, 4, 0, 0]} />
                  </BarChart>
                </ResponsiveContainer>
              </div>
            )}
            <Table<SalesBucket>
              size="small"
              rowKey="period"
              loading={reportQuery.isLoading}
              dataSource={buckets}
              pagination={{ pageSize: period === 'daily' ? 31 : 12 }}
              locale={{ emptyText: <Empty description={t('resellers.noSales', 'No recorded account sales')} /> }}
              columns={[
                { title: t('resellers.table.date', 'Period'), dataIndex: 'period', key: 'period' },
                { title: t('resellers.totalSales', 'Accounts sold'), dataIndex: 'sales', key: 'sales' },
                { title: t('resellers.unknownPrices', 'Unknown prices'), dataIndex: 'unknownPrices', key: 'unknownPrices' },
                { title: t('resellers.declaredSalesMinor', 'Declared sales (minor units)'), dataIndex: 'amountsMinor', key: 'amountsMinor', render: (values: Record<string, string> | undefined) => Object.entries(values ?? {}).map(([code, amount]) => `${code}: ${amount}`).join(', ') || '—' },
                {
                  title: t('resellers.totalQuotaSold', 'Traffic allocated'),
                  dataIndex: 'trafficBytes',
                  key: 'trafficBytes',
                  render: (value: number) => SizeFormatter.sizeFormat(value),
                },
              ]}
            />
          </>
        )}
      </Card>
    </>
  );
}
