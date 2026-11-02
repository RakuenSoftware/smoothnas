export type PluginWebUI = {
  scheme: 'http' | 'https';
  port: number;
  path: string;
};

// Use the appliance hostname the operator is already visiting. An absolute URL
// in a third-party manifest must never redirect the Open action elsewhere.
export function pluginWebURL(webUI?: PluginWebUI): string | null {
  if (!webUI || !['http', 'https'].includes(webUI.scheme)
    || !Number.isInteger(webUI.port) || webUI.port < 1 || webUI.port > 65535) return null;
  const path = webUI.path || '/';
  if (!path.startsWith('/') || path.startsWith('//') || /[\\\r\n]/.test(path)) return null;
  const url = new URL(window.location.href);
  url.protocol = `${webUI.scheme}:`;
  url.port = String(webUI.port);
  url.pathname = path;
  url.search = '';
  url.hash = '';
  return url.href;
}
