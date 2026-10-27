<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { toast } from 'vue-sonner'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet'
import { webhookLogsService, type WebhookLog, type WebhookLogDetail } from '@/services/api'
import { getErrorMessage } from '@/lib/api-utils'
import { formatLogTime, prettyJSON, webhookDeliveryStatusClass } from '@/lib/logs'
import { Copy } from 'lucide-vue-next'

// Shows one webhook delivery. Pass the row from a list as `log` so the panel
// opens with it at once; the full snapshot is fetched on open.
const props = defineProps<{ log: WebhookLog | null }>()
const open = defineModel<boolean>('open', { default: false })

const { t } = useI18n()

const detail = ref<WebhookLogDetail | null>(null)
const isLoading = ref(false)

watch(() => [open.value, props.log?.id] as const, async ([isOpen, id]) => {
  if (!isOpen || !id) return
  detail.value = null
  isLoading.value = true
  try {
    const response = await webhookLogsService.get(id)
    if (props.log?.id !== id) return
    detail.value = (response.data as any).data || response.data
  } catch (e) {
    toast.error(getErrorMessage(e, t('webhookLogs.loadFailed')))
  } finally {
    isLoading.value = false
  }
}, { immediate: true })

const shown = computed<WebhookLog | null>(() => detail.value ?? props.log)

const requestBody = computed(() =>
  detail.value ? JSON.stringify(detail.value.request.body, null, 2) : '')
const responseBody = computed(() =>
  detail.value?.response ? prettyJSON(detail.value.response.body) : '')

async function copyToClipboard(value: string) {
  try {
    await navigator.clipboard.writeText(value)
    toast.success(t('common.copiedToClipboard'))
  } catch {
    toast.error(t('common.clipboardFailed'))
  }
}
</script>


<template>
  <Sheet v-model:open="open">
    <SheetContent class="w-full sm:max-w-2xl overflow-y-auto">
      <SheetHeader>
        <SheetTitle>{{ t('webhookLogs.detail.title') }}</SheetTitle>
        <SheetDescription>{{ shown ? formatLogTime(shown.created_at) : '' }}</SheetDescription>
      </SheetHeader>

      <div v-if="shown" class="mt-6 space-y-6 text-sm">
        <div class="flex items-center gap-2 flex-wrap">
          <Badge variant="outline" :class="webhookDeliveryStatusClass(shown.status)">
            {{ t(`webhookLogs.statuses.${shown.status}`) }}
          </Badge>
          <Badge variant="outline" class="font-mono">{{ shown.event }}</Badge>
          <Badge variant="outline">{{ t('webhookLogs.attemptCount', { count: shown.attempt_count }, shown.attempt_count) }}</Badge>
        </div>

        <div v-if="shown.status === 'failed' && shown.failure_reason" class="rounded-md border border-red-500/20 bg-red-500/5 p-3">
          <p class="text-xs font-medium text-red-500 mb-1">{{ t('webhookLogs.detail.failureReason') }}</p>
          <p class="text-red-500 break-words">{{ shown.failure_reason }}</p>
        </div>

        <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2">
          <dt class="text-muted-foreground">{{ t('webhookLogs.columns.webhook') }}</dt>
          <dd class="text-right">
            <RouterLink :to="`/settings/webhooks/${shown.webhook_id}`" class="hover:underline">{{ shown.webhook_name }}</RouterLink>
          </dd>
          <template v-if="shown.reference_id">
            <dt class="text-muted-foreground">{{ t('webhookLogs.columns.reference') }}</dt>
            <dd class="flex items-start justify-end gap-1 min-w-0">
              <span class="break-all text-xs font-mono">{{ shown.reference_id }}</span>
              <Button variant="ghost" size="icon" class="h-6 w-6 shrink-0" :aria-label="t('common.copy')" @click="copyToClipboard(shown.reference_id)">
                <Copy class="h-3 w-3" />
              </Button>
            </dd>
          </template>
          <template v-if="shown.contact_phone">
            <dt class="text-muted-foreground">{{ t('webhookLogs.detail.customer') }}</dt>
            <dd class="text-right">{{ shown.contact_phone }}</dd>
          </template>
          <dt class="text-muted-foreground">{{ t('webhookLogs.detail.completed') }}</dt>
          <dd class="text-right">{{ formatLogTime(shown.completed_at) }}</dd>
        </dl>

        <p v-if="isLoading" class="text-muted-foreground">{{ t('common.loading') }}…</p>

        <template v-if="detail">
          <div>
            <p class="text-xs font-medium text-muted-foreground mb-2">{{ t('webhookLogs.detail.attempts') }}</p>
            <ol class="space-y-2">
              <li v-for="attempt in detail.attempts" :key="attempt.attempt" class="flex items-start justify-between gap-4 rounded-md border p-2">
                <div class="min-w-0">
                  <p class="font-medium">{{ t('webhookLogs.detail.attemptN', { n: attempt.attempt }) }}</p>
                  <p class="text-xs text-muted-foreground">{{ formatLogTime(attempt.at) }}</p>
                  <p v-if="attempt.error" class="text-xs text-red-500 break-words mt-1">{{ attempt.error }}</p>
                </div>
                <div class="text-right shrink-0">
                  <Badge variant="outline" :class="attempt.error ? webhookDeliveryStatusClass('failed') : webhookDeliveryStatusClass('success')">
                    {{ attempt.status_code || t('webhookLogs.detail.noResponse') }}
                  </Badge>
                  <p class="text-xs text-muted-foreground mt-1">{{ attempt.duration_ms }} ms</p>
                </div>
              </li>
            </ol>
          </div>

          <div>
            <p class="text-xs font-medium text-muted-foreground mb-2">{{ t('webhookLogs.detail.request') }}</p>
            <p class="font-mono text-xs break-all mb-2"><span class="font-semibold">{{ detail.request.method }}</span> {{ detail.request.url }}</p>
            <table class="w-full text-xs mb-3">
              <tbody>
                <tr v-for="(value, key) in detail.request.headers" :key="key" class="border-b last:border-0">
                  <td class="py-1 pr-3 font-mono text-muted-foreground align-top whitespace-nowrap">{{ key }}</td>
                  <td class="py-1 font-mono break-all">{{ value }}</td>
                </tr>
              </tbody>
            </table>
            <div class="relative">
              <Button variant="ghost" size="icon" class="absolute right-1 top-1 h-6 w-6" :aria-label="t('common.copy')" @click="copyToClipboard(requestBody)">
                <Copy class="h-3 w-3" />
              </Button>
              <pre class="max-h-96 overflow-auto rounded-md bg-muted/50 p-3 text-xs">{{ requestBody }}</pre>
            </div>
          </div>

          <div>
            <p class="text-xs font-medium text-muted-foreground mb-2">{{ t('webhookLogs.detail.response') }}</p>
            <template v-if="detail.response">
              <p class="text-xs mb-2">{{ t('webhookLogs.detail.statusCode', { code: detail.response.status_code }) }}</p>
              <pre v-if="responseBody" class="max-h-64 overflow-auto rounded-md bg-muted/50 p-3 text-xs">{{ responseBody }}</pre>
              <p v-else class="text-xs text-muted-foreground">{{ t('webhookLogs.detail.emptyBody') }}</p>
            </template>
            <p v-else class="text-xs text-muted-foreground">{{ t('webhookLogs.detail.noResponseReceived') }}</p>
          </div>
        </template>
      </div>
    </SheetContent>
  </Sheet>
</template>
