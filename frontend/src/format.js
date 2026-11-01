const names = { jdownloader: 'JDownloader', deluge: 'Deluge', sabnzbd: 'SABnzbd', drop: 'drop' };

export function sourceName(s) {
  return names[s] ?? s;
}

export function formatBytes(n) {
  if (!n || n < 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n >= 100 || i === 0 ? Math.round(n) : n.toFixed(1)} ${units[i]}`;
}

export function formatEta(s) {
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
  return `${Math.floor(s / 86400)}d ${Math.floor((s % 86400) / 3600)}h`;
}

const order = ['downloading', 'crawling', 'checking', 'queued', 'paused', 'error', 'seeding', 'done'];

export function stateOrder(s) {
  const i = order.indexOf(s);
  return i < 0 ? order.length : i;
}
