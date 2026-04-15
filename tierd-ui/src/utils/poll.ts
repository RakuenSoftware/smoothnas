import { pollJob as _pollJob } from '@rakuensoftware/smoothgui';
import { api } from '../api/api';

export function pollJob(
  jobId: string,
  onProgress: ((p: string) => void) | null,
  onComplete: () => void,
  onError: (err: string) => void,
  intervalMs = 2000
): () => void {
  return _pollJob(jobId, api.getJobStatus, onProgress, onComplete, onError, intervalMs);
}

