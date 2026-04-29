import { useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import SmbShares from '../SmbShares/SmbShares';
import NfsExports from '../NfsExports/NfsExports';
import IscsiTargets from '../IscsiTargets/IscsiTargets';

type Tab = 'smb' | 'nfs' | 'iscsi';

export default function Sharing() {
  const { t } = useI18n();
  const [activeTab, setActiveTab] = useState<Tab>('smb');
  return (
    <div className="page">
      <div className="page-header">
        <h1>{t('sharing.title')}</h1>
        <p className="subtitle">{t('sharing.subtitle')}</p>
      </div>
      <div className="tabs">
        {(['smb', 'nfs', 'iscsi'] as Tab[]).map(tab => (
          <button key={tab} className={`tab${activeTab === tab ? ' active' : ''}`} onClick={() => setActiveTab(tab)}>
            {tab.toUpperCase()}
          </button>
        ))}
      </div>
      {activeTab === 'smb' && <SmbShares />}
      {activeTab === 'nfs' && <NfsExports />}
      {activeTab === 'iscsi' && <IscsiTargets />}
    </div>
  );
}
