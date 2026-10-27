<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useDebounceFn } from '@vueuse/core'
import { today, getLocalTimeZone } from '@internationalized/date'
import { toast } from 'vue-sonner'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { Command, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList } from '@/components/ui/command'
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet'
import { PageHeader, DataTable, DateRangePicker, SearchInput, type Column } from '@/components/shared'
import {
  messageLogsService,
  accountsService,
  templatesService,
  type MessageLog,
  type MessageLogListParams,
  type MessageLogSource,
  type MessageLogStatus,
} from '@/services/api'
import { useDateRange } from '@/composables/useDateRange'
import { useAuthStore } from '@/stores/auth'
import { getErrorMessage } from '@/lib/api-utils'
import { formatLabel } from '@/lib/utils'
import { LOG_RETENTION_DAYS, localDayBoundary, formatLogTime } from '@/lib/logs'
import {
  ListChecks,
  ArrowDownLeft,
  ArrowUpRight,
  Check,
  ChevronsUpDown,
  ChevronLeft,
  ChevronRight,
  Copy,
  MessageSquare,
  Download,
} from 'lucide-vue-next'

const { t } = useI18n()
const authStore = useAuthStore()

const STATUSES: MessageLogStatus[] = ['pending', 'sent', 'delivered', 'read', 'failed', 'received']
const SOURCES: MessageLogSource[] = ['api', 'mcp', 'agent', 'chatbot', 'campaign', 'automation', 'phone_app', 'incoming', 'call']
// Template names are listed with the templates API's maximum page size, up to
// this many pages.
const TEMPLATE_PAGE_SIZE = 100
const TEMPLATE_MAX_PAGES = 10

const logs = ref<MessageLog[]>([])
const isLoading = ref(true)
const nextCursor = ref('')
const prevCursor = ref('')

// Filters ('all' means unfiltered)
const filterAccount = ref('all')
const filterStatus = ref('all')
const filterDirection = ref('all')
const filterSource = ref('all')
const filterTemplate = ref('')
const filterPhone = ref('')
const templateComboboxOpen = ref(false)

const accounts = ref<{ id: string; name: string }[]>([])
const templateNames = ref<string[]>([])

const {
  selectedRange, customDateRange, isDatePickerOpen,
  dateRange, formatDateRangeDisplay, applyCustomRange: baseApplyCustomRange,
} = useDateRange({ defaultPreset: '7days' })
const minDate = today(getLocalTimeZone()).subtract({ days: LOG_RETENTION_DAYS })

const canOpenChat = authStore.hasPermission('chat')
const canExport = authStore.hasPermission('message_logs', 'export')
const router = useRouter()
const isExporting = ref(false)

// Queues a background export of exactly what the filters show. The file's
// times are written in the viewer's timezone.
async function requestExport() {
  isExporting.value = true
  try {
    const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
    await messageLogsService.export({ ...buildParams(), timezone })
    toast.success(t('exports.startedTitle'), {
      description: t('exports.startedDescription'),
      action: { label: t('exports.viewDownloads'), onClick: () => router.push('/downloads') },
    })
  } catch (e) {
    toast.error(getErrorMessage(e, t('exports.startFailed')))
  } finally {
    isExporting.value = false
  }
}

const hasActiveFilters = computed(() =>
  filterAccount.value !== 'all' || filterStatus.value !== 'all' || filterDirection.value !== 'all' ||
  filterSource.value !== 'all' || filterTemplate.value !== '' || filterPhone.value !== '')

const columns = computed<Column<MessageLog>[]>(() => [
  { key: 'created_at', label: t('messageLogs.columns.time') },
  { key: 'contact', label: t('messageLogs.columns.customer') },
  { key: 'direction', label: t('messageLogs.columns.direction') },
  { key: 'status', label: t('messageLogs.columns.status') },
  { key: 'template', label: t('messageLogs.columns.template') },
  { key: 'source', label: t('messageLogs.columns.source') },
  { key: 'content', label: t('messageLogs.columns.content') },
])

function buildParams(): MessageLogListParams {
  const { from, to } = dateRange.value
  const params: MessageLogListParams = {
    from: localDayBoundary(from, false),
    to: localDayBoundary(to, true),
  }
  if (filterAccount.value !== 'all') params.account = filterAccount.value
  if (filterStatus.value !== 'all') params.status = filterStatus.value as MessageLogStatus
  if (filterDirection.value !== 'all') params.direction = filterDirection.value as 'incoming' | 'outgoing'
  if (filterSource.value !== 'all') params.source = filterSource.value as MessageLogSource
  if (filterTemplate.value) params.template = filterTemplate.value
  if (filterPhone.value.trim()) params.phone = filterPhone.value.trim()
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
    const response = await messageLogsService.list(params)
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
    toast.error(getErrorMessage(e, t('messageLogs.loadFailed')))
  } finally {
    if (seq === requestSeq) isLoading.value = false
  }
}

function applyFilter() {
  fetchLogs()
}

const debouncedPhoneFilter = useDebounceFn(applyFilter, 400)
watch(filterPhone, () => debouncedPhoneFilter())

watch(selectedRange, (val) => {
  if (val !== 'custom') applyFilter()
})

function applyCustomRange() {
  baseApplyCustomRange()
  applyFilter()
}

function selectTemplate(name: string) {
  filterTemplate.value = name
  templateComboboxOpen.value = false
  applyFilter()
}

function clearFilters() {
  filterAccount.value = 'all'
  filterStatus.value = 'all'
  filterDirection.value = 'all'
  filterSource.value = 'all'
  filterTemplate.value = ''
  if (filterPhone.value !== '') {
    // Clearing the phone triggers its debounced fetch, which picks up the
    // other cleared filters too.
    filterPhone.value = ''
    return
  }
  applyFilter()
}

async function fetchAccounts() {
  try {
    const response = await accountsService.list()
    const data = (response.data as any).data || response.data
    accounts.value = data.accounts || []
  } catch {
    // The account filter is optional
  }
}

async function fetchTemplateNames() {
  const names = new Set<string>()
  try {
    for (let page = 1; page <= TEMPLATE_MAX_PAGES; page++) {
      const response = await templatesService.list({ page, limit: TEMPLATE_PAGE_SIZE })
      const data = (response.data as any).data || response.data
      const templates: { name: string }[] = data.templates || []
      templates.forEach(tpl => names.add(tpl.name))
      if (templates.length < TEMPLATE_PAGE_SIZE) break
    }
  } catch {
    // The template filter is optional
  }
  templateNames.value = [...names].sort((a, b) => a.localeCompare(b))
}

function statusClass(status: MessageLogStatus): string {
  switch (status) {
    case 'read': return 'bg-blue-500/10 text-blue-500 border-blue-500/20'
    case 'delivered': return 'bg-green-500/10 text-green-500 border-green-500/20'
    case 'sent': return 'bg-slate-500/10 text-slate-500 border-slate-500/20'
    case 'failed': return 'bg-red-500/10 text-red-500 border-red-500/20'
    case 'pending': return 'bg-amber-500/10 text-amber-500 border-amber-500/20'
    case 'received': return 'bg-purple-500/10 text-purple-500 border-purple-500/20'
    default: return ''
  }
}

function sourceLabel(source: MessageLog['source']): string {
  return t(`messageLogs.sources.${source || 'unknown'}`)
}

// Detail panel
const detailOpen = ref(false)
const selectedLog = ref<MessageLog | null>(null)
const isLoadingDetail = ref(false)

async function openDetail(log: MessageLog) {
  // Show the row's data at once; the detail call adds full content and sender.
  selectedLog.value = log
  detailOpen.value = true
  isLoadingDetail.value = true
  try {
    const response = await messageLogsService.get(log.id)
    if (selectedLog.value?.id !== log.id) return
    selectedLog.value = (response.data as any).data || response.data
  } catch (e) {
    toast.error(getErrorMessage(e, t('messageLogs.loadFailed')))
  } finally {
    isLoadingDetail.value = false
  }
}

async function copyToClipboard(value: string) {
  try {
    await navigator.clipboard.writeText(value)
    toast.success(t('common.copiedToClipboard'))
  } catch {
    toast.error(t('common.clipboardFailed'))
  }
}

const timeline = computed(() => {
  const log = selectedLog.value
  if (!log) return []
  return [
    { label: t('messageLogs.detail.created'), value: log.created_at },
    { label: t('messageLogs.detail.sent'), value: log.sent_at },
    { label: t('messageLogs.detail.delivered'), value: log.delivered_at },
    { label: t('messageLogs.detail.read'), value: log.read_at },
  ]
})

onMounted(() => {
  fetchLogs()
  fetchAccounts()
  fetchTemplateNames()
})
</script>

<template>
  <div class="flex flex-col h-full bg-[#0a0a0b] light:bg-gray-50">
    <PageHeader
      :title="t('messageLogs.title')"
      :description="t('messageLogs.description')"
      :icon="ListChecks"
      icon-gradient="bg-gradient-to-br from-sky-500 to-indigo-600 shadow-sky-500/20"
    >
      <template v-if="canExport" #actions>
        <Button variant="outline" size="sm" :loading="isExporting" @click="requestExport">
          <Download class="h-4 w-4 mr-1" />
          {{ t('exports.exportButton') }}
        </Button>
      </template>
    </PageHeader>

    <ScrollArea class="flex-1">
      <div class="p-6">
        <Card>
          <CardHeader>
            <div class="flex flex-col gap-4">
              <div>
                <CardTitle>{{ t('messageLogs.allMessages') }}</CardTitle>
                <CardDescription>{{ t('messageLogs.allMessagesDesc', { days: LOG_RETENTION_DAYS }) }}</CardDescription>
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
                <Select v-model="filterAccount" @update:model-value="applyFilter">
                  <SelectTrigger class="w-[160px]">
                    <SelectValue :placeholder="t('messageLogs.allAccounts')" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">{{ t('messageLogs.allAccounts') }}</SelectItem>
                    <SelectItem v-for="account in accounts" :key="account.id" :value="account.name">
                      {{ account.name }}
                    </SelectItem>
                  </SelectContent>
                </Select>
                <Select v-model="filterStatus" @update:model-value="applyFilter">
                  <SelectTrigger class="w-[140px]">
                    <SelectValue :placeholder="t('messageLogs.allStatuses')" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">{{ t('messageLogs.allStatuses') }}</SelectItem>
                    <SelectItem v-for="status in STATUSES" :key="status" :value="status">
                      {{ t(`messageLogs.statuses.${status}`) }}
                    </SelectItem>
                  </SelectContent>
                </Select>
                <Select v-model="filterDirection" @update:model-value="applyFilter">
                  <SelectTrigger class="w-[140px]">
                    <SelectValue :placeholder="t('messageLogs.allDirections')" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">{{ t('messageLogs.allDirections') }}</SelectItem>
                    <SelectItem value="outgoing">{{ t('messageLogs.directions.outgoing') }}</SelectItem>
                    <SelectItem value="incoming">{{ t('messageLogs.directions.incoming') }}</SelectItem>
                  </SelectContent>
                </Select>
                <Select v-model="filterSource" @update:model-value="applyFilter">
                  <SelectTrigger class="w-[140px]">
                    <SelectValue :placeholder="t('messageLogs.allSources')" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">{{ t('messageLogs.allSources') }}</SelectItem>
                    <SelectItem v-for="source in SOURCES" :key="source" :value="source">
                      {{ sourceLabel(source) }}
                    </SelectItem>
                  </SelectContent>
                </Select>
                <Popover v-model:open="templateComboboxOpen">
                  <PopoverTrigger as-child>
                    <Button variant="outline" role="combobox" :aria-expanded="templateComboboxOpen" class="w-[200px] justify-between font-normal">
                      <span class="truncate">{{ filterTemplate || t('messageLogs.allTemplates') }}</span>
                      <ChevronsUpDown class="ml-2 h-4 w-4 shrink-0 opacity-50" />
                    </Button>
                  </PopoverTrigger>
                  <PopoverContent class="w-[260px] p-0" align="start">
                    <Command>
                      <CommandInput :placeholder="t('messageLogs.searchTemplate')" />
                      <CommandList>
                        <CommandEmpty>{{ t('messageLogs.noTemplateFound') }}</CommandEmpty>
                        <CommandGroup>
                          <CommandItem value="__all__" @select="selectTemplate('')">
                            <Check :class="['mr-2 h-4 w-4', filterTemplate === '' ? 'opacity-100' : 'opacity-0']" />
                            {{ t('messageLogs.allTemplates') }}
                          </CommandItem>
                          <CommandItem v-for="name in templateNames" :key="name" :value="name" @select="selectTemplate(name)">
                            <Check :class="['mr-2 h-4 w-4', filterTemplate === name ? 'opacity-100' : 'opacity-0']" />
                            <span class="truncate">{{ name }}</span>
                          </CommandItem>
                        </CommandGroup>
                      </CommandList>
                    </Command>
                  </PopoverContent>
                </Popover>
                <SearchInput v-model="filterPhone" :placeholder="t('messageLogs.phonePlaceholder')" class="w-[200px]" />
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
              :empty-icon="ListChecks"
              :empty-title="t('messageLogs.noLogs')"
              :empty-description="t('messageLogs.noLogsDesc')"
              clickable-rows
              @row-click="openDetail"
            >
              <template #cell-created_at="{ item: log }">
                <span class="text-sm whitespace-nowrap">{{ formatLogTime(log.created_at) }}</span>
              </template>

              <template #cell-contact="{ item: log }">
                <div class="min-w-0">
                  <p class="text-sm font-medium truncate max-w-[180px]">{{ log.contact_name || '—' }}</p>
                  <p class="text-xs text-muted-foreground">{{ log.contact_phone }}</p>
                </div>
              </template>

              <template #cell-direction="{ item: log }">
                <span class="inline-flex items-center gap-1 text-sm text-muted-foreground">
                  <ArrowUpRight v-if="log.direction === 'outgoing'" class="h-4 w-4 text-sky-500" />
                  <ArrowDownLeft v-else class="h-4 w-4 text-purple-500" />
                  {{ t(`messageLogs.directions.${log.direction}`) }}
                </span>
              </template>

              <template #cell-status="{ item: log }">
                <div class="max-w-[220px]">
                  <Badge variant="outline" :class="[statusClass(log.status), 'text-xs']">
                    {{ t(`messageLogs.statuses.${log.status}`) }}
                  </Badge>
                  <p v-if="log.status === 'failed' && log.error_message" class="mt-1 text-xs text-red-500 truncate" :title="log.error_message">
                    {{ log.error_message }}
                  </p>
                </div>
              </template>

              <template #cell-template="{ item: log }">
                <span class="text-sm truncate block max-w-[200px]" :title="log.template_name">
                  {{ log.template_name || formatLabel(log.message_type) }}
                </span>
              </template>

              <template #cell-source="{ item: log }">
                <span class="text-sm text-muted-foreground">{{ sourceLabel(log.source) }}</span>
              </template>

              <template #cell-content="{ item: log }">
                <span class="text-sm text-muted-foreground truncate block max-w-[280px]" :title="log.content">
                  {{ log.content || '—' }}
                </span>
              </template>
            </DataTable>

            <div v-if="!isLoading && (prevCursor || nextCursor)" class="flex items-center justify-between border-t pt-4 mt-2">
              <span class="text-sm text-muted-foreground">{{ t('messageLogs.perPage', { count: 50 }) }}</span>
              <div class="flex items-center gap-2">
                <Button variant="outline" size="sm" :disabled="!prevCursor" @click="fetchLogs(prevCursor, 'newer')">
                  <ChevronLeft class="h-4 w-4 mr-1" />
                  {{ t('messageLogs.newer') }}
                </Button>
                <Button variant="outline" size="sm" :disabled="!nextCursor" @click="fetchLogs(nextCursor, 'older')">
                  {{ t('messageLogs.older') }}
                  <ChevronRight class="h-4 w-4 ml-1" />
                </Button>
              </div>
            </div>
          </CardContent>
        </Card>
      </div>
    </ScrollArea>

    <Sheet v-model:open="detailOpen">
      <SheetContent class="w-full sm:max-w-lg overflow-y-auto">
        <SheetHeader>
          <SheetTitle>{{ t('messageLogs.detail.title') }}</SheetTitle>
          <SheetDescription>{{ selectedLog ? formatLogTime(selectedLog.created_at) : '' }}</SheetDescription>
        </SheetHeader>

        <div v-if="selectedLog" class="mt-6 space-y-6 text-sm">
          <div class="flex items-center gap-2 flex-wrap">
            <Badge variant="outline" :class="statusClass(selectedLog.status)">
              {{ t(`messageLogs.statuses.${selectedLog.status}`) }}
            </Badge>
            <Badge variant="outline">{{ t(`messageLogs.directions.${selectedLog.direction}`) }}</Badge>
            <Badge variant="outline">{{ sourceLabel(selectedLog.source) }}</Badge>
          </div>

          <div v-if="selectedLog.status === 'failed' && selectedLog.error_message" class="rounded-md border border-red-500/20 bg-red-500/5 p-3">
            <p class="text-xs font-medium text-red-500 mb-1">{{ t('messageLogs.detail.failureReason') }}</p>
            <p class="text-red-500">{{ selectedLog.error_message }}</p>
          </div>

          <div v-if="selectedLog.direction === 'outgoing'">
            <p class="text-xs font-medium text-muted-foreground mb-2">{{ t('messageLogs.detail.timeline') }}</p>
            <ol class="space-y-2">
              <li v-for="step in timeline" :key="step.label" class="flex justify-between gap-4">
                <span class="text-muted-foreground">{{ step.label }}</span>
                <span :class="step.value ? '' : 'text-muted-foreground'">{{ formatLogTime(step.value) }}</span>
              </li>
            </ol>
          </div>

          <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2">
            <dt class="text-muted-foreground">{{ t('messageLogs.columns.customer') }}</dt>
            <dd class="text-right">{{ selectedLog.contact_name || '—' }}<br><span class="text-muted-foreground">{{ selectedLog.contact_phone }}</span></dd>
            <dt class="text-muted-foreground">{{ t('messageLogs.detail.account') }}</dt>
            <dd class="text-right">{{ selectedLog.whatsapp_account || '—' }}</dd>
            <dt class="text-muted-foreground">{{ t('messageLogs.detail.messageType') }}</dt>
            <dd class="text-right">{{ formatLabel(selectedLog.message_type) }}</dd>
            <template v-if="selectedLog.template_name">
              <dt class="text-muted-foreground">{{ t('messageLogs.columns.template') }}</dt>
              <dd class="text-right break-all">{{ selectedLog.template_name }}</dd>
            </template>
            <template v-if="selectedLog.campaign_name || selectedLog.campaign_id">
              <dt class="text-muted-foreground">{{ t('messageLogs.detail.campaign') }}</dt>
              <dd class="text-right">
                <RouterLink :to="`/campaigns/${selectedLog.campaign_id}`" class="hover:underline">
                  {{ selectedLog.campaign_name || selectedLog.campaign_id }}
                </RouterLink>
              </dd>
            </template>
            <template v-if="selectedLog.pricing_category">
              <dt class="text-muted-foreground">{{ t('messageLogs.detail.pricingCategory') }}</dt>
              <dd class="text-right">{{ formatLabel(selectedLog.pricing_category) }}</dd>
            </template>
            <template v-if="selectedLog.sent_by_user_name">
              <dt class="text-muted-foreground">{{ t('messageLogs.detail.sentBy') }}</dt>
              <dd class="text-right">{{ selectedLog.sent_by_user_name }}</dd>
            </template>
            <template v-if="selectedLog.whatsapp_message_id">
              <dt class="text-muted-foreground">{{ t('messageLogs.detail.whatsappMessageId') }}</dt>
              <dd class="flex items-start justify-end gap-1 min-w-0">
                <span class="break-all text-xs font-mono">{{ selectedLog.whatsapp_message_id }}</span>
                <Button variant="ghost" size="icon" class="h-6 w-6 shrink-0" :aria-label="t('common.copy')" @click="copyToClipboard(selectedLog.whatsapp_message_id)">
                  <Copy class="h-3 w-3" />
                </Button>
              </dd>
            </template>
          </dl>

          <div>
            <p class="text-xs font-medium text-muted-foreground mb-2">{{ t('messageLogs.columns.content') }}</p>
            <p class="whitespace-pre-wrap break-words rounded-md bg-muted/50 p-3" :class="isLoadingDetail && 'opacity-60'">
              {{ selectedLog.content || '—' }}
            </p>
          </div>

          <Button v-if="canOpenChat" variant="outline" class="w-full" as-child>
            <RouterLink :to="`/chat/${selectedLog.contact_id}`">
              <MessageSquare class="h-4 w-4 mr-2" />
              {{ t('messageLogs.detail.openConversation') }}
            </RouterLink>
          </Button>
        </div>
      </SheetContent>
    </Sheet>
  </div>
</template>
