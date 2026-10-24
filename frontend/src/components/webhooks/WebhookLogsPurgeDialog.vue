<script setup lang="ts">
import { ref, shallowRef, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { toast } from 'vue-sonner'
import { today, getLocalTimeZone, type DateValue } from '@internationalized/date'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Calendar } from '@/components/ui/calendar'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { webhookLogsService, type Webhook } from '@/services/api'
import { getErrorMessage } from '@/lib/api-utils'
import { localDayBoundary } from '@/lib/logs'
import { CalendarIcon } from 'lucide-vue-next'

// The API only purges logs older than 7 days.
const PURGE_MIN_AGE_DAYS = 7

const props = defineProps<{ webhooks: Webhook[] }>()
const open = defineModel<boolean>('open', { default: false })
const emit = defineEmits<{ purged: [] }>()

const { t } = useI18n()

const maxDate = today(getLocalTimeZone()).subtract({ days: PURGE_MIN_AGE_DAYS })
// shallowRef: DateValue is a class instance with private fields, which ref()
// would deep-unwrap into an incompatible plain-object type.
const before = shallowRef<DateValue>(maxDate)
const webhookId = ref('all')
const isDatePickerOpen = ref(false)
// Deliveries matching the current choice; null until checked.
const matching = ref<number | null>(null)
const isChecking = ref(false)
const isPurging = ref(false)

watch(open, (isOpen) => {
  if (isOpen) {
    before.value = maxDate
    webhookId.value = 'all'
    matching.value = null
  }
})
// Any change invalidates the previous count.
watch([before, webhookId], () => { matching.value = null })

const webhookName = computed(() =>
  webhookId.value === 'all' ? t('webhookLogs.allWebhooks') : props.webhooks.find(w => w.id === webhookId.value)?.name ?? '')

function purgeRequest(confirm: boolean) {
  const day = `${before.value.year}-${String(before.value.month).padStart(2, '0')}-${String(before.value.day).padStart(2, '0')}`
  return {
    // Start of the chosen local day: deliveries created before it are purged.
    before: localDayBoundary(day, false),
    webhook_id: webhookId.value === 'all' ? undefined : webhookId.value,
    confirm,
  }
}

async function check() {
  isChecking.value = true
  try {
    const response = await webhookLogsService.purge(purgeRequest(false))
    const data = (response.data as any).data || response.data
    matching.value = data.matching ?? 0
  } catch (e) {
    toast.error(getErrorMessage(e, t('webhookLogs.purge.failed')))
  } finally {
    isChecking.value = false
  }
}

async function purge() {
  isPurging.value = true
  try {
    await webhookLogsService.purge(purgeRequest(true))
    toast.success(t('webhookLogs.purge.started', { count: matching.value ?? 0 }))
    open.value = false
    emit('purged')
  } catch (e) {
    toast.error(getErrorMessage(e, t('webhookLogs.purge.failed')))
  } finally {
    isPurging.value = false
  }
}

function formatDay(d: DateValue): string {
  return d.toDate(getLocalTimeZone()).toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' })
}
</script>

<template>
  <Dialog v-model:open="open">
    <DialogContent class="sm:max-w-md">
      <DialogHeader>
        <DialogTitle>{{ t('webhookLogs.purge.title') }}</DialogTitle>
        <DialogDescription>{{ t('webhookLogs.purge.description', { days: PURGE_MIN_AGE_DAYS }) }}</DialogDescription>
      </DialogHeader>

      <div class="space-y-4">
        <div class="space-y-1.5">
          <Label class="text-xs">{{ t('webhookLogs.purge.before') }}</Label>
          <Popover v-model:open="isDatePickerOpen">
            <PopoverTrigger as-child>
              <Button variant="outline" class="w-full justify-start font-normal">
                <CalendarIcon class="h-4 w-4 mr-2" />
                {{ formatDay(before) }}
              </Button>
            </PopoverTrigger>
            <PopoverContent class="w-auto p-0" align="start">
              <Calendar
                :model-value="before"
                :max-value="maxDate"
                @update:model-value="(v) => { if (v) { before = v; isDatePickerOpen = false } }"
              />
            </PopoverContent>
          </Popover>
        </div>
        <div class="space-y-1.5">
          <Label class="text-xs">{{ t('webhookLogs.columns.webhook') }}</Label>
          <Select v-model="webhookId">
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">{{ t('webhookLogs.allWebhooks') }}</SelectItem>
              <SelectItem v-for="webhook in webhooks" :key="webhook.id" :value="webhook.id">{{ webhook.name }}</SelectItem>
            </SelectContent>
          </Select>
        </div>

        <p v-if="matching !== null" class="rounded-md border p-3 text-sm" :class="matching > 0 ? 'border-red-500/20 bg-red-500/5 text-red-500' : 'text-muted-foreground'">
          {{ matching > 0
            ? t('webhookLogs.purge.willDelete', { count: matching.toLocaleString(), webhook: webhookName, date: formatDay(before) })
            : t('webhookLogs.purge.nothingToDelete') }}
        </p>
      </div>

      <DialogFooter>
        <Button variant="outline" @click="open = false">{{ t('common.cancel') }}</Button>
        <Button v-if="matching === null || matching === 0" :loading="isChecking" @click="check">
          {{ t('webhookLogs.purge.check') }}
        </Button>
        <Button v-else variant="destructive" :loading="isPurging" @click="purge">
          {{ t('webhookLogs.purge.confirm', { count: matching.toLocaleString() }) }}
        </Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>
