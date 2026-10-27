<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { toast } from 'vue-sonner'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ScrollArea } from '@/components/ui/scroll-area'
import { PageHeader, DataTable, DeleteConfirmDialog, type Column } from '@/components/shared'
import { exportsService, type ExportJob, type ExportStatus } from '@/services/api'
import { wsService } from '@/services/websocket'
import { getErrorMessage } from '@/lib/api-utils'
import { formatLogTime } from '@/lib/logs'
import { formatLabel } from '@/lib/utils'
import { Download, FolderDown, Trash2, Loader2 } from 'lucide-vue-next'

const { t } = useI18n()

// While an export is queued or processing, refresh this often in case its
// WebSocket update is missed (e.g. the socket was reconnecting).
const POLL_INTERVAL_MS = 5000

const exports = ref<ExportJob[]>([])
const isLoading = ref(true)
const deleteDialogOpen = ref(false)
const exportToDelete = ref<ExportJob | null>(null)
const isDeleting = ref(false)

const columns = computed<Column<ExportJob>[]>(() => [
  { key: 'file', label: t('exports.columns.file') },
  { key: 'filters', label: t('exports.columns.filters') },
  { key: 'rows', label: t('exports.columns.rows'), align: 'right' },
  { key: 'size', label: t('exports.columns.size'), align: 'right' },
  { key: 'status', label: t('exports.columns.status') },
  { key: 'created_at', label: t('exports.columns.requested') },
  { key: 'expires_at', label: t('exports.columns.expires') },
  { key: 'actions', label: '', align: 'right' },
])

const hasRunning = computed(() => exports.value.some(e => e.status === 'queued' || e.status === 'processing'))

async function fetchExports(showLoading = false) {
  if (showLoading) isLoading.value = true
  try {
    const response = await exportsService.list()
    const data = (response.data as any).data || response.data
    exports.value = data.exports || []
  } catch (e) {
    toast.error(getErrorMessage(e, t('exports.loadFailed')))
  } finally {
    isLoading.value = false
  }
}

function download(job: ExportJob) {
  window.location.href = exportsService.downloadUrl(job.id)
}

function confirmDelete(job: ExportJob) {
  exportToDelete.value = job
  deleteDialogOpen.value = true
}

async function deleteExport() {
  if (!exportToDelete.value) return
  isDeleting.value = true
  try {
    await exportsService.delete(exportToDelete.value.id)
    exports.value = exports.value.filter(e => e.id !== exportToDelete.value?.id)
    deleteDialogOpen.value = false
  } catch (e) {
    toast.error(getErrorMessage(e, t('exports.deleteFailed')))
  } finally {
    isDeleting.value = false
  }
}

function statusClass(status: ExportStatus): string {
  switch (status) {
    case 'completed': return 'bg-green-500/10 text-green-500 border-green-500/20'
    case 'failed': return 'bg-red-500/10 text-red-500 border-red-500/20'
    case 'queued':
    case 'processing': return 'bg-amber-500/10 text-amber-500 border-amber-500/20'
    default: return 'bg-slate-500/10 text-slate-500 border-slate-500/20'
  }
}

function formatBytes(bytes: number): string {
  if (!bytes) return '—'
  const units = ['B', 'KB', 'MB', 'GB']
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit++
  }
  return `${value.toFixed(unit === 0 ? 0 : 1)} ${units[unit]}`
}

function formatDay(iso: string | undefined): string {
  if (!iso) return ''
  return new Date(iso).toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' })
}

// One-line summary of the filters the export was made with, e.g.
// "Oct 1, 2026 – Oct 24, 2026 · Status: failed · Template: otp_v1".
function filtersSummary(job: ExportJob): string {
  const f = job.filters || {}
  const parts = [`${formatDay(f.from)} – ${formatDay(f.to)}`]
  for (const key of ['account', 'status', 'direction', 'source', 'template', 'phone']) {
    if (f[key]) parts.push(`${formatLabel(key)}: ${f[key]}`)
  }
  return parts.join(' · ')
}

let pollTimer: number | null = null
let unsubscribe: (() => void) | null = null

onMounted(async () => {
  await fetchExports(true)
  unsubscribe = wsService.onExportUpdate(() => fetchExports())
  pollTimer = window.setInterval(() => {
    if (hasRunning.value) fetchExports()
  }, POLL_INTERVAL_MS)
})

onUnmounted(() => {
  unsubscribe?.()
  if (pollTimer !== null) window.clearInterval(pollTimer)
})
</script>

<template>
  <div class="flex flex-col h-full bg-[#0a0a0b] light:bg-gray-50">
    <PageHeader
      :title="t('exports.title')"
      :description="t('exports.description')"
      :icon="FolderDown"
      icon-gradient="bg-gradient-to-br from-emerald-500 to-teal-600 shadow-emerald-500/20"
    />

    <ScrollArea class="flex-1">
      <div class="p-6">
        <Card>
          <CardHeader>
            <CardTitle>{{ t('exports.yourExports') }}</CardTitle>
            <CardDescription>{{ t('exports.yourExportsDesc') }}</CardDescription>
          </CardHeader>
          <CardContent>
            <DataTable
              :items="exports"
              :columns="columns"
              :is-loading="isLoading"
              :empty-icon="FolderDown"
              :empty-title="t('exports.empty')"
              :empty-description="t('exports.emptyDesc')"
            >
              <template #cell-file="{ item: job }">
                <div class="min-w-0">
                  <p class="text-sm font-medium truncate max-w-[280px]" :title="job.file_name">
                    {{ job.file_name || t('exports.types.message_logs') }}
                  </p>
                  <p class="text-xs text-muted-foreground">{{ t(`exports.types.${job.type}`) }} · {{ job.timezone }}</p>
                </div>
              </template>

              <template #cell-filters="{ item: job }">
                <span class="text-xs text-muted-foreground block max-w-[320px] truncate" :title="filtersSummary(job)">
                  {{ filtersSummary(job) }}
                </span>
              </template>

              <template #cell-rows="{ item: job }">
                <span class="text-sm">{{ job.status === 'completed' ? job.row_count.toLocaleString() : '—' }}</span>
              </template>

              <template #cell-size="{ item: job }">
                <span class="text-sm text-muted-foreground">{{ formatBytes(job.file_size) }}</span>
              </template>

              <template #cell-status="{ item: job }">
                <div class="max-w-[240px]">
                  <Badge variant="outline" :class="[statusClass(job.status), 'text-xs gap-1']">
                    <Loader2 v-if="job.status === 'queued' || job.status === 'processing'" class="h-3 w-3 animate-spin" />
                    {{ t(`exports.statuses.${job.status}`) }}
                  </Badge>
                  <p v-if="job.status === 'failed' && job.error" class="mt-1 text-xs text-red-500 truncate" :title="job.error">
                    {{ job.error }}
                  </p>
                </div>
              </template>

              <template #cell-created_at="{ item: job }">
                <span class="text-sm whitespace-nowrap">{{ formatLogTime(job.created_at) }}</span>
              </template>

              <template #cell-expires_at="{ item: job }">
                <span class="text-sm text-muted-foreground whitespace-nowrap">
                  {{ job.status === 'completed' ? formatLogTime(job.expires_at) : '—' }}
                </span>
              </template>

              <template #cell-actions="{ item: job }">
                <div class="flex items-center justify-end gap-1">
                  <Button v-if="job.status === 'completed'" size="sm" @click="download(job)">
                    <Download class="h-4 w-4 mr-1" />
                    {{ t('common.download') }}
                  </Button>
                  <Button
                    v-if="job.status !== 'queued' && job.status !== 'processing'"
                    variant="ghost"
                    size="icon"
                    class="h-8 w-8"
                    :aria-label="t('common.delete')"
                    @click="confirmDelete(job)"
                  >
                    <Trash2 class="h-4 w-4" />
                  </Button>
                </div>
              </template>
            </DataTable>
          </CardContent>
        </Card>
      </div>
    </ScrollArea>

    <DeleteConfirmDialog
      v-model:open="deleteDialogOpen"
      :title="t('exports.deleteTitle')"
      :description="t('exports.deleteDescription')"
      :is-submitting="isDeleting"
      @confirm="deleteExport"
    />
  </div>
</template>
