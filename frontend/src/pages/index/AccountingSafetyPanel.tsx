import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Button, Card, Tag } from 'antd';
import {
  CheckCircleFilled,
  ExclamationCircleFilled,
  SafetyCertificateOutlined,
  WarningFilled,
} from '@ant-design/icons';

import type { CounterIncidentStatus, NodeSnapshotSafetyStatus } from '@/models/status';
import './AccountingSafetyPanel.css';

type SafetyTone = 'healthy' | 'review' | 'unknown';

interface AccountingSafetyPanelProps {
  status?: CounterIncidentStatus;
  nodeSafety?: NodeSnapshotSafetyStatus;
  onOpenReconciliation: () => void;
}

function safetyTone(status?: CounterIncidentStatus, nodeSafety?: NodeSnapshotSafetyStatus): SafetyTone {
  if (!status?.available || status.daemonIdentityCount === undefined || !nodeSafety?.available) return 'unknown';
  if (status.databaseCount > 0 || status.fallbackPresent || status.pendingInProcess || status.daemonIdentityCount > 0 || nodeSafety.nodeCount > 0) return 'review';
  return 'healthy';
}

/**
 * Explains the fail-closed accounting guard directly on the overview. This is
 * intentionally not a repair control: operators inspect the dedicated
 * reconciliation page before any evidence can be archived or cleared.
 */
export default function AccountingSafetyPanel({ status, nodeSafety, onOpenReconciliation }: AccountingSafetyPanelProps) {
  const { t } = useTranslation();
  const tone = safetyTone(status, nodeSafety);
  const copy = useMemo(() => {
    switch (tone) {
      case 'healthy':
        return {
          icon: <CheckCircleFilled />,
          title: t('pages.index.accountingSafetyHealthy', 'Accounting protection active'),
          description: t('pages.index.accountingSafetyHealthyDescription', 'No unresolved Xray, L2TP, database, or local accounting evidence is currently blocking protected operations.'),
        };
      case 'review':
        return {
          icon: <ExclamationCircleFilled />,
          title: t('pages.index.accountingSafetyReview', 'Accounting review required'),
          description: t('pages.index.accountingSafetyReviewDescription', 'Traffic totals may be incomplete. Protected changes stay fail-closed until the captured evidence is reviewed and reconciled.'),
        };
      default:
        return {
          icon: <WarningFilled />,
          title: t('pages.index.accountingSafetyUnavailable', 'Accounting status unavailable'),
          description: t('pages.index.accountingSafetyUnavailableDescription', 'The panel cannot verify its accounting guards. Treat traffic-sensitive changes as blocked and inspect reconciliation status.'),
        };
    }
  }, [t, tone]);

  const databaseValue = status?.available ? String(status.databaseCount) : '—';
  const evidenceValue = status?.available
    ? (status.fallbackPresent ? t('pages.index.accountingSafetyDetected', 'Detected') : t('pages.index.accountingSafetyClear', 'Clear'))
    : '—';
  const captureValue = status?.available
    ? (status.pendingInProcess ? t('pages.index.accountingSafetyPending', 'Pending') : t('pages.index.accountingSafetyClear', 'Clear'))
    : '—';
  const nodeValue = nodeSafety?.available ? String(nodeSafety.nodeCount) : '—';

  return (
    <Card className={`accounting-safety-panel is-${tone}`}>
      <div className="accounting-safety-main">
        <span className="accounting-safety-icon" aria-hidden="true">{tone === 'healthy' ? <SafetyCertificateOutlined /> : copy.icon}</span>
        <div className="accounting-safety-copy">
          <span className="phoenix-eyebrow">{t('pages.index.accountingSafetyTitle', 'Accounting safety')}</span>
          <strong>{copy.title}</strong>
          <p>{copy.description}</p>
        </div>
      </div>

      <div className="accounting-safety-metrics" aria-label={t('pages.index.accountingSafetyTitle', 'Accounting safety')}>
        <div className="accounting-safety-metric">
          <span>{t('pages.index.accountingSafetyDatabase', 'Database barriers')}</span>
          <b>{databaseValue}</b>
        </div>
        <div className="accounting-safety-metric">
          <span>{t('pages.index.accountingSafetyEvidence', 'Local evidence')}</span>
          <Tag color={tone === 'unknown' ? 'default' : status?.fallbackPresent ? 'red' : 'green'}>{evidenceValue}</Tag>
        </div>
        <div className="accounting-safety-metric">
          <span>{t('pages.index.accountingSafetyCapture', 'Capture')}</span>
          <Tag color={tone === 'unknown' ? 'default' : status?.pendingInProcess ? 'orange' : 'green'}>{captureValue}</Tag>
        </div>
        <div className="accounting-safety-metric">
          <span>{t('pages.index.accountingSafetyNodes', 'Remote nodes to review')}</span>
          <b>{nodeValue}</b>
        </div>
      </div>

      <Button type={tone === 'healthy' ? 'default' : 'primary'} onClick={onOpenReconciliation} className="accounting-safety-action">
        {t('pages.index.accountingSafetyOpen', 'Open reconciliation')}
      </Button>
    </Card>
  );
}
