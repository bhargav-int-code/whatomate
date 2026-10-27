import type { WebhookDeliveryStatus } from '@/services/api'

// Helpers shared by the log pages (message logs, webhook logs).

// How far back the log APIs serve data.
export const LOG_RETENTION_DAYS = 90

// The log APIs read a bare YYYY-MM-DD as a UTC day, which is up to a day off
// for viewers outside UTC. Send the viewer's local day boundaries instead.
export function localDayBoundary(day: string, endOfDay: boolean): string {
  const [y, m, d] = day.split('-').map(Number)
  const date = endOfDay ? new Date(y, m - 1, d, 23, 59, 59, 999) : new Date(y, m - 1, d)
  return date.toISOString()
}

// Formats a timestamp with seconds, e.g. "Oct 24, 2026, 04:49:51 PM".
export function formatLogTime(iso: string | null | undefined): string {
  if (!iso) return '—'
  return new Date(iso).toLocaleString('en-US', {
    month: 'short', day: 'numeric', year: 'numeric',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
  })
}

// Pretty-prints JSON text, returning it unchanged if it isn't JSON.
export function prettyJSON(text: string): string {
  try {
    return JSON.stringify(JSON.parse(text), null, 2)
  } catch {
    return text
  }
}

// Badge colours for a webhook delivery status.
export function webhookDeliveryStatusClass(status: WebhookDeliveryStatus): string {
  switch (status) {
    case 'success': return 'bg-green-500/10 text-green-500 border-green-500/20'
    case 'failed': return 'bg-red-500/10 text-red-500 border-red-500/20'
    case 'processing': return 'bg-amber-500/10 text-amber-500 border-amber-500/20'
    default: return ''
  }
}
