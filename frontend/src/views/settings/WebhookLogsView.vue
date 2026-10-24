<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useDebounceFn } from '@vueuse/core'
import { today, getLocalTimeZone } from '@internationalized/date'
import { toast } from 'vue-sonner'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { PageHeader, DataTable, DateRangePicker, SearchInput, type Column } from '@/components/shared'
import WebhookLogDetailSheet from '@/components/webhooks/WebhookLogDetailSheet.vue'
import WebhookLogsPurgeDialog from '@/components/webhooks/WebhookLogsPurgeDialog.vue'
import {
  webhookLogsService,
  webhooksService,
  type Webhook,
  type WebhookEvent,
  type WebhookLog,
  type WebhookLogListParams,
  type WebhookDeliveryStatus,
} from '@/services/api'
import { useDateRange } from '@/composables/useDateRange'
import { useAuthStore } from '@/stores/auth'
import { getErrorMessage } from '@/lib/api-utils'
import { LOG_RETENTION_DAYS, localDayBoundary, formatLogTime, webhookDeliveryStatusClass } from '@/lib/logs'
import { Activity, ChevronLeft, ChevronRight, Trash2 } from 'lucide-vue-next'

const { t } = useI18n()
const route = useRoute()
const authStore = useAuthStore()

const STATUSES: WebhookDeliveryStatus[] = ['success', 'failed', 'processing']

const logs = ref<WebhookLog[]>([])
const isLoading = ref(true)
const nextCursor = ref('')
const prevCursor = ref('')

// Filters ('all' means unfiltered). A webhook's detail page links here with
// ?webhook_id= to open the logs pre-filtered.
const filterWebhook = ref(typeof route.query.webhook_id === 'string' ? route.query.webhook_id : 'all')
const filterEvent = ref('all')
const filterStatus = ref('all')
const filterSearch = ref('')

const webhooks = ref<Webhook[]>([])
const events = ref<WebhookEvent[]>([])

const {
  selectedRange, customDateRange, isDatePickerOpen,
  dateRange, formatDateRangeDisplay, applyCustomRange: baseApplyCustomRange,
} = useDateRange({ defaultPreset: '7days' })
const minDate = today(getLocalTimeZone()).subtract({ days: LOG_RETENTION_DAYS })

const canPurge = authStore.hasPermission('webhook_logs', 'delete')
const purgeDialogOpen = ref(false)

const hasActiveFilters = computed(() =>
  filterWebhook.value !== 'all' || filterEvent.value !== 'all' || filterStatus.value !== 'all' || filterSearch.value !== '')

const columns = computed<Column<WebhookLog>[]>(() => [
  { key: 'created_at', label: t('webhookLogs.columns.time') },
  { key: 'webhook', label: t('webhookLogs.columns.webhook') },
  { key: 'event', label: t('webhookLogs.columns.event') },
  { key: 'status', label: t('webhookLogs.columns.status') },
  { key: 'attempts', label: t('webhookLogs.columns.attempts') },
  { key: 'reference', label: t('webhookLogs.columns.reference') },
])

function buildParams(): WebhookLogListParams {
  const { from, to } = dateRange.value
  const params: WebhookLogListParams = {
    from: localDayBoundary(from, false),
    to: localDayBoundary(to, true),
  }
  if (filterWebhook.value !== 'all') params.webhook_id = filterWebhook.value
  if (filterEvent.value !== 'all') params.event = filterEvent.value
  if (filterStatus.value !== 'all') params.status = filterStatus.value as WebhookDeliveryStatus
  if (filterSearch.value.trim()) params.search = filterSearch.value.trim()
  return params
}

// Guards against an older, slower response overwriting a newer one when
// filters change quickly.
let requestSeq = 0

async function fetchLogs(cursor = '', dir: 'older' | 'newer' = 'older') {
  const seq = ++requestSeq
  isLoading.value = true
  try {
    const params = buildParams()
    if (cursor) {
      params.cursor = cursor
      params.dir = dir
    }
    const response = await webhookLogsService.list(params)
    if (seq !== requestSeq) return
    const data = (response.data as any).data || response.data
    logs.value = data.logs || []
    nextCursor.value = data.next_cursor || ''
    prevCursor.value = data.prev_cursor || ''
  } catch (e) {
    if (seq !== requestSeq) return
    logs.value = []
    nextCursor.value = ''
    prevCursor.value = ''
    toast.error(getErrorMessage(e, t('webhookLogs.loadFailed')))
  } finally {
    if (seq === requestSeq) isLoading.value = false
  }
}

function applyFilter() {
  fetchLogs()
}

const debouncedSearch = useDebounceFn(applyFilter, 400)
watch(filterSearch, () => debouncedSearch())

watch(selectedRange, (val) => {
  if (val !== 'custom') applyFilter()
})

function applyCustomRange() {
  baseApplyCustomRange()
  applyFilter()
}

function clearFilters() {
  filterWebhook.value = 'all'
  filterEvent.value = 'all'
  filterStatus.value = 'all'
  if (filterSearch.value !== '') {
    // Clearing the search triggers its debounced fetch, which picks up the
    // other cleared filters too.
    filterSearch.value = ''
    return
  }
  applyFilter()
}

async function fetchWebhooks() {
  try {
    const response = await webhooksService.list({ limit: 100 })
    const data = (response.data as any).data || response.data
    webhooks.value = data.webhooks || []
    events.value = data.available_events || []
  } catch {
    // The webhook and event filters are optional
  }
}

// Test sends are logged as event "test", which isn't a subscribable event.
const eventOptions = computed(() => [...events.value.map(e => e.value), 'test'])

// Detail panel
const detailOpen = ref(false)
const selectedLog = ref<WebhookLog | null>(null)

function openDetail(log: WebhookLog) {
  selectedLog.value = log
  detailOpen.value = true
}

onMounted(() => {
  fetchLogs()
  fetchWebhooks()
})
</script>

<template>
  <div class="flex flex-col h-full bg-[#0a0a0b] light:bg-gray-50">
    <PageHeader
      :title="t('webhookLogs.title')"
      :description="t('webhookLogs.description')"
      :icon="Activity"
      icon-gradient="bg-gradient-to-br from-indigo-500 to-purple-600 shadow-indigo-500/20"
    >
      <template v-if="canPurge" #actions>
        <Button variant="outline" size="sm" @click="purgeDialogOpen = true">
          <Trash2 class="h-4 w-4 mr-1" />
          {{ t('webhookLogs.purge.button') }}
        </Button>
      </template>
    </PageHeader>

    <ScrollArea class="flex-1">
      <div class="p-6">
        <Card>
          <CardHeader>
            <div class="flex flex-col gap-4">
              <div>
                <CardTitle>{{ t('webhookLogs.allDeliveries') }}</CardTitle>
                <CardDescription>{{ t('webhookLogs.allDeliveriesDesc', { days: LOG_RETENTION_DAYS }) }}</CardDescription>
              </div>
              <div class="flex items-center gap-2 flex-wrap">
                <DateRangePicker
                  v-model:selected-range="selectedRange"
                  v-model:custom-date-range="customDateRange"
                  v-model:is-date-picker-open="isDatePickerOpen"
                  :format-date-range-display="formatDateRangeDisplay"
                  :min-date="minDate"
                  @apply-custom="applyCustomRange"
                />
                <Select v-model="filterWebhook" @update:model-value="applyFilter">
                  <SelectTrigger class="w-[200px]">
                    <SelectValue :placeholder="t('webhookLogs.allWebhooks')" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">{{ t('webhookLogs.allWebhooks') }}</SelectItem>
                    <SelectItem v-for="webhook in webhooks" :key="webhook.id" :value="webhook.id">
                      {{ webhook.name }}
                    </SelectItem>
                  </SelectContent>
                </Select>
                <Select v-model="filterEvent" @update:model-value="applyFilter">
                  <SelectTrigger class="w-[180px]">
                    <SelectValue :placeholder="t('webhookLogs.allEvents')" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">{{ t('webhookLogs.allEvents') }}</SelectItem>
                    <SelectItem v-for="event in eventOptions" :key="event" :value="event">
                      <span class="font-mono text-xs">{{ event }}</span>
                    </SelectItem>
                  </SelectContent>
                </Select>
                <Select v-model="filterStatus" @update:model-value="applyFilter">
                  <SelectTrigger class="w-[140px]">
                    <SelectValue :placeholder="t('webhookLogs.allStatuses')" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">{{ t('webhookLogs.allStatuses') }}</SelectItem>
                    <SelectItem v-for="status in STATUSES" :key="status" :value="status">
                      {{ t(`webhookLogs.statuses.${status}`) }}
                    </SelectItem>
                  </SelectContent>
                </Select>
                <SearchInput v-model="filterSearch" :placeholder="t('webhookLogs.searchPlaceholder')" class="w-[240px]" />
                <Button v-if="hasActiveFilters" variant="ghost" size="sm" @click="clearFilters">
                  {{ t('common.clearFilters') }}
                </Button>
              </div>
            </div>
          </CardHeader>
          <CardContent>
            <DataTable
              :items="logs"
              :columns="columns"
              :is-loading="isLoading"
              :empty-icon="Activity"
              :empty-title="t('webhookLogs.noLogs')"
              :empty-description="t('webhookLogs.noLogsDesc')"
              clickable-rows
              @row-click="openDetail"
            >
              <template #cell-created_at="{ item: log }">
                <span class="text-sm whitespace-nowrap">{{ formatLogTime(log.created_at) }}</span>
              </template>

              <template #cell-webhook="{ item: log }">
                <span class="text-sm font-medium truncate block max-w-[220px]" :title="log.webhook_name">{{ log.webhook_name }}</span>
              </template>

              <template #cell-event="{ item: log }">
                <span class="text-xs font-mono text-muted-foreground">{{ log.event }}</span>
              </template>

              <template #cell-status="{ item: log }">
                <div class="max-w-[260px]">
                  <Badge variant="outline" :class="[webhookDeliveryStatusClass(log.status), 'text-xs']">
                    {{ t(`webhookLogs.statuses.${log.status}`) }}
                  </Badge>
                  <p v-if="log.status === 'failed' && log.failure_reason" class="mt-1 text-xs text-red-500 truncate" :title="log.failure_reason">
                    {{ log.failure_reason }}
                  </p>
                </div>
              </template>

              <template #cell-attempts="{ item: log }">
                <span class="text-sm text-muted-foreground">
                  {{ log.attempt_count }}<template v-if="log.response_status"> · {{ log.response_status }}</template>
                </span>
              </template>

              <template #cell-reference="{ item: log }">
                <div class="min-w-0">
                  <p class="text-xs font-mono truncate max-w-[220px]" :title="log.reference_id">{{ log.reference_id || '—' }}</p>
                  <p v-if="log.contact_phone" class="text-xs text-muted-foreground">{{ log.contact_phone }}</p>
                </div>
              </template>
            </DataTable>

            <div v-if="!isLoading && (prevCursor || nextCursor)" class="flex items-center justify-between border-t pt-4 mt-2">
              <span class="text-sm text-muted-foreground">{{ t('webhookLogs.perPage', { count: 50 }) }}</span>
              <div class="flex items-center gap-2">
                <Button variant="outline" size="sm" :disabled="!prevCursor" @click="fetchLogs(prevCursor, 'newer')">
                  <ChevronLeft class="h-4 w-4 mr-1" />
                  {{ t('webhookLogs.newer') }}
                </Button>
                <Button variant="outline" size="sm" :disabled="!nextCursor" @click="fetchLogs(nextCursor, 'older')">
                  {{ t('webhookLogs.older') }}
                  <ChevronRight class="h-4 w-4 ml-1" />
                </Button>
              </div>
            </div>
          </CardContent>
        </Card>
      </div>
    </ScrollArea>

    <WebhookLogDetailSheet v-model:open="detailOpen" :log="selectedLog" />
    <WebhookLogsPurgeDialog v-model:open="purgeDialogOpen" :webhooks="webhooks" @purged="applyFilter" />
  </div>
</template>
