import { useState } from 'react';
import SmbShares from '../SmbShares/SmbShares';
import NfsExports from '../NfsExports/NfsExports';
import IscsiTargets from '../IscsiTargets/IscsiTargets';

type Tab = 'smb' | 'nfs' | 'iscsi';

export default function Sharing() {
  const [activeTab, setActiveTab] = useState<Tab>('smb');
  return (
    <div className="page">
      <div className="page-header">
        <h1>Sharing</h1>
        <p className="subtitle">SMB, NFS, and iSCSI protocol management</p>
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
