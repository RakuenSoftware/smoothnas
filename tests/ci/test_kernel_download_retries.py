"""Exercise CI's pinned-kernel downloader against real HTTP failures."""
import http.server
import hashlib
import os
import subprocess
import tempfile
import threading
import unittest
from pathlib import Path

WRAPPER = Path(__file__).resolve().parents[2] / 'scripts/ci/fetch-guest-kernel.sh'


class DownloadRetries(unittest.TestCase):
    def check_download(self, transient):
        requests = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                requests.append(self.path)
                code = (503 if len(requests) == 1 else 200) if transient else 404
                self.send_response(code)
                self.end_headers()
                if code == 200:
                    self.wfile.write(b'kernel-index')

            def log_message(self, *args):
                pass

        with http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                with tempfile.TemporaryDirectory() as directory:
                    fetcher = Path(directory) / 'fetch.sh'
                    fetcher.write_text('set -e\ncurl -fsSL http://127.0.0.1:' + str(server.server_port) + '/kernel\n')
                    result = subprocess.run(['bash', str(WRAPPER), str(fetcher)], capture_output=True, text=True, timeout=15)
            finally:
                server.shutdown()
                thread.join()
        return result, requests

    def test_retries_service_unavailable(self):
        result, requests = self.check_download(True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, 'kernel-index')
        self.assertEqual(len(requests), 2)

    def test_missing_kernel_still_fails_without_retry(self):
        result, requests = self.check_download(False)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(requests), 1)


class PinnedKernelChecksums(unittest.TestCase):
    def check_release(self, corrupt):
        payload = b'verified-kernel-package'
        names = ['linux-image-7.2.10-smoothkernel_7.2.10-1_amd64.deb',
                 'linux-headers-7.2.10-smoothkernel_7.2.10-1_amd64.deb']
        sums = ''.join(hashlib.sha256(payload).hexdigest() + '  ' + name + '\n' for name in names).encode()

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.end_headers()
                body = sums if self.path == '/SHA256SUMS' else payload
                if corrupt and self.path.endswith(names[0]):
                    body = b'corrupted-package'
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        with http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                with tempfile.TemporaryDirectory() as directory:
                    env = dict(os.environ,
                               SMOOTHKERNEL_GUEST_DOWNLOAD_ONLY='1',
                               SMOOTHKERNEL_GUEST_DOWNLOAD_DIR=directory,
                               SMOOTHKERNEL_GUEST_BASE_URL='http://127.0.0.1:' + str(server.server_port))
                    result = subprocess.run(['bash', str(WRAPPER.with_name('fetch-smoothkernel-guest.sh'))],
                                            env=env, capture_output=True, text=True, timeout=15)
            finally:
                server.shutdown()
                thread.join()
        return result

    def test_accepts_verified_packages(self):
        result = self.check_release(False)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_rejects_corrupted_package_before_install(self):
        result = self.check_release(True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('FAILED', result.stderr)


if __name__ == '__main__':
    unittest.main()
