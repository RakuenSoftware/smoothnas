import { useEffect, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { useToast } from '../../contexts/ToastContext';

type FileEntry = {
  path: string;
  size: number;
  inode: number;
  tier_rank: number;
  pin_state: string;
};

function formatBytes(n: number): string {
  if (!n || n <= 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0; let v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`;
}

// NamespaceFiles renders a small file browser for a single tiering
// namespace, with pin/unpin controls per row. The backend endpoint walks
// the pool's tier backings; it caps at `limit` entries, so this is a
// quick-peek UI rather than a full file manager.
export default function NamespaceFiles({ nsID }: { nsID: string }) {
  const { t } = useI18n();
  const toast = useToast();
  const [prefix, setPrefix] = useState('');
  const [limit, setLimit] = useState(200);
  const [files, setFiles] = useState<FileEntry[]>([]);
  const [loading, setLoading] = useState(false);
  const [pinningRow, setPinningRow] = useState<string | null>(null);

  function load() {
    setLoading(true);
    api.listNamespaceFiles(nsID, prefix, limit)
      .then((rows: FileEntry[]) => {
        setFiles(rows || []);
      })
      .catch(e => toast.error(e?.message || 'Failed to list files'))
      .finally(() => setLoading(false));
  }

  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => { load(); }, [nsID]);

  function togglePin(f: FileEntry) {
    const current = f.pin_state;
    const next = current === 'none' ? 'pinned-hot' : 'none';
    setPinningRow(f.path);
    api.pinObjectByPath(nsID, f.path, next as any)
      .then(() => {
        setFiles(prev => prev.map(r => r.path === f.path ? { ...r, pin_state: next } : r));
      })
      .catch(e => toast.error(e?.message || 'Pin change failed'))
      .finally(() => setPinningRow(null));
  }

  return (
    <div style={{ marginTop: 24, padding: '12px 16px', background: 'var(--bg-alt, #f7f7f7)', borderRadius: 6 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 12 }}>
        <h3 style={{ margin: 0, fontSize: 14, textTransform: 'uppercase', letterSpacing: '0.5px', color: '#555' }}>
          {t('namespaceFiles.title')}
        </h3>
        <input
          value={prefix}
          onChange={e => setPrefix(e.target.value)}
          // i18n-allow: example value, not user copy
          placeholder="prefix (e.g. storage/backup)"
          style={{ flex: 1, fontSize: 13 }}
        />
        <input
          type="number"
          value={limit}
          onChange={e => setLimit(Number(e.target.value) || 200)}
          style={{ width: 80, fontSize: 13 }}
          min={1}
          max={5000}
        />
        <button className="btn" onClick={load} disabled={loading}>
          {loading ? 'Loading…' : 'Refresh'}
        </button>
      </div>

      {files.length === 0 ? (
        <div style={{ color: '#888', fontSize: 13 }}>{loading ? 'Loading…' : 'No files found under this prefix.'}</div>
      ) : (
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 90px 70px 120px', gap: '4px 12px', fontSize: 13, alignItems: 'center' }}>
          <div style={{ fontSize: 11, color: '#999' }}>Path</div>
          <div style={{ fontSize: 11, color: '#999', textAlign: 'right' }}>Size</div>
          <div style={{ fontSize: 11, color: '#999', textAlign: 'center' }}>Tier</div>
          <div style={{ fontSize: 11, color: '#999' }}>Pin</div>
          {files.map(f => (
            <>
              <code key={`p${f.inode}`} style={{ fontSize: 12, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={f.path}>{f.path}</code>
              <span key={`s${f.inode}`} style={{ textAlign: 'right', color: '#666' }}>{formatBytes(f.size)}</span>
              <span key={`t${f.inode}`} style={{ textAlign: 'center', fontSize: 11, background: '#e0e0e0', borderRadius: 8, padding: '1px 5px', color: '#555', minWidth: 20 }}>#{f.tier_rank}</span>
              <button
                key={`b${f.inode}`}
                onClick={() => togglePin(f)}
                disabled={pinningRow === f.path}
                style={{ fontSize: 12, padding: '2px 8px', background: f.pin_state !== 'none' ? '#5cb85c' : undefined, color: f.pin_state !== 'none' ? 'white' : undefined }}
              >
                {pinningRow === f.path ? '…' : f.pin_state !== 'none' ? `📌 ${f.pin_state}` : 'Pin'}
              </button>
            </>
          ))}
        </div>
      )}
    </div>
  );
}
