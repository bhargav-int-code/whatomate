<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { toast } from 'vue-sonner'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { ScrollArea } from '@/components/ui/scroll-area'
import { PageHeader, DataTable, DeleteConfirmDialog, type Column } from '@/components/shared'
import { organizationService, mcpService, MCP_SCOPES, type MCPConnection } from '@/services/api'
import { useAuthStore } from '@/stores/auth'
import { getErrorMessage } from '@/lib/api-utils'
import { formatLogTime } from '@/lib/logs'
import { Plug, Copy, Unplug, AlertTriangle } from 'lucide-vue-next'

const { t } = useI18n()
const authStore = useAuthStore()

const canWrite = computed(() => authStore.hasPermission('settings.general', 'write'))
const serverUrl = mcpService.endpointUrl()
const claudeCodeCommand = `claude mcp add --transport http whatomate ${serverUrl}`
// Claude.ai and ChatGPT connect from the internet and require HTTPS
const notPublicHTTPS = !serverUrl.startsWith('https://')

const enabled = ref(false)
const isLoadingSettings = ref(true)
const isSaving = ref(false)

const connections = ref<MCPConnection[]>([])
const isLoadingConnections = ref(false)
const revokeTarget = ref<MCPConnection | null>(null)
const revokeOpen = ref(false)
const isRevoking = ref(false)

const columns = computed<Column<MCPConnection>[]>(() => [
  { key: 'client_name', label: t('mcp.columns.app') },
  { key: 'user', label: t('mcp.columns.user') },
  { key: 'scopes', label: t('mcp.columns.permissions') },
  { key: 'created_at', label: t('mcp.columns.connected') },
  { key: 'last_used_at', label: t('mcp.columns.lastUsed') },
  { key: 'actions', label: '', align: 'right' },
])

async function loadSettings() {
  try {
    const response = await organizationService.getSettings()
    const data = response.data.data || response.data
    enabled.value = !!data.settings?.mcp_enabled
  } catch (e) {
    toast.error(getErrorMessage(e, t('mcp.loadFailed')))
  } finally {
    isLoadingSettings.value = false
  }
}

async function loadConnections() {
  if (!canWrite.value) return
  isLoadingConnections.value = true
  try {
    const response = await mcpService.listConnections()
    const data = (response.data as any).data || response.data
    connections.value = data.connections || []
  } catch (e) {
    toast.error(getErrorMessage(e, t('mcp.connectionsLoadFailed')))
  } finally {
    isLoadingConnections.value = false
  }
}

async function setEnabled(value: boolean) {
  const previous = enabled.value
  enabled.value = value
  isSaving.value = true
  try {
    await organizationService.updateSettings({ mcp_enabled: value })
    toast.success(value ? t('mcp.enabled') : t('mcp.disabled'))
  } catch (e) {
    enabled.value = previous
    toast.error(getErrorMessage(e, t('mcp.saveFailed')))
  } finally {
    isSaving.value = false
  }
}

async function copy(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    toast.success(t('mcp.copied'))
  } catch {
    // Clipboard access can be blocked (e.g. plain http); the text stays selectable
  }
}

function confirmRevoke(conn: MCPConnection) {
  revokeTarget.value = conn
  revokeOpen.value = true
}

async function revoke() {
  if (!revokeTarget.value) return
  isRevoking.value = true
  try {
    await mcpService.revokeConnection(revokeTarget.value.id)
    connections.value = connections.value.filter(c => c.id !== revokeTarget.value?.id)
    revokeOpen.value = false
    toast.success(t('mcp.disconnected'))
  } catch (e) {
    toast.error(getErrorMessage(e, t('mcp.disconnectFailed')))
  } finally {
    isRevoking.value = false
  }
}

onMounted(() => {
  loadSettings()
  loadConnections()
})
</script>

<template>
  <div class="flex flex-col h-full bg-[#0a0a0b] light:bg-gray-50">
    <PageHeader
      :title="t('mcp.title')"
      :description="t('mcp.description')"
      :icon="Plug"
      icon-gradient="bg-gradient-to-br from-violet-500 to-indigo-600 shadow-violet-500/20"
    />

    <ScrollArea class="flex-1">
      <div class="p-6 space-y-6 max-w-5xl">
        <Card>
          <CardHeader>
            <CardTitle>{{ t('mcp.access') }}</CardTitle>
            <CardDescription>{{ t('mcp.accessDesc') }}</CardDescription>
          </CardHeader>
          <CardContent>
            <div class="flex items-center justify-between gap-4">
              <div>
                <p class="font-medium">{{ t('mcp.enable') }}</p>
                <p class="text-sm text-muted-foreground">{{ canWrite ? t('mcp.enableDesc') : t('mcp.adminOnly') }}</p>
              </div>
              <Switch
                :checked="enabled"
                :disabled="!canWrite || isLoadingSettings || isSaving"
                @update:checked="setEnabled"
              />
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>{{ t('mcp.connect') }}</CardTitle>
            <CardDescription>{{ t('mcp.connectDesc') }}</CardDescription>
          </CardHeader>
          <CardContent class="space-y-6">
            <div class="space-y-2">
              <p class="text-sm font-medium">{{ t('mcp.serverUrl') }}</p>
              <div class="flex gap-2">
                <Input :model-value="serverUrl" readonly class="font-mono text-sm" @focus="($event.target as HTMLInputElement).select()" />
                <Button variant="outline" @click="copy(serverUrl)">
                  <Copy class="h-4 w-4 mr-1" />
                  {{ t('mcp.copy') }}
                </Button>
              </div>
              <p v-if="notPublicHTTPS" class="flex gap-1.5 text-xs text-amber-500">
                <AlertTriangle class="h-3.5 w-3.5 shrink-0 mt-0.5" />
                {{ t('mcp.guides.publicUrlNote') }}
              </p>
            </div>

            <div class="grid gap-4 md:grid-cols-3">
              <div class="rounded-lg border p-4 space-y-1.5">
                <p class="font-medium text-sm">{{ t('mcp.guides.claude') }}</p>
                <p class="text-xs text-muted-foreground">{{ t('mcp.guides.claudeSteps') }}</p>
              </div>
              <div class="rounded-lg border p-4 space-y-1.5">
                <p class="font-medium text-sm">{{ t('mcp.guides.chatgpt') }}</p>
                <p class="text-xs text-muted-foreground">{{ t('mcp.guides.chatgptSteps') }}</p>
              </div>
              <div class="rounded-lg border p-4 space-y-1.5 min-w-0">
                <p class="font-medium text-sm">{{ t('mcp.guides.claudeCode') }}</p>
                <p class="text-xs text-muted-foreground">{{ t('mcp.guides.claudeCodeSteps') }}</p>
                <div class="flex items-start gap-1">
                  <code class="flex-1 min-w-0 break-all rounded bg-muted px-2 py-1 text-[11px]">{{ claudeCodeCommand }}</code>
                  <Button variant="ghost" size="icon" class="h-7 w-7 shrink-0" :aria-label="t('mcp.copy')" @click="copy(claudeCodeCommand)">
                    <Copy class="h-3.5 w-3.5" />
                  </Button>
                </div>
              </div>
            </div>

            <div class="space-y-2">
              <div v-for="scope in MCP_SCOPES" :key="scope" class="flex flex-wrap items-baseline gap-x-2 text-sm">
                <Badge variant="outline" class="font-mono text-[11px]">{{ scope }}</Badge>
                <span class="font-medium">{{ t(`mcp.scopes.${scope}.label`) }}</span>
                <span class="text-muted-foreground text-xs">{{ t(`mcp.scopes.${scope}.desc`) }}</span>
              </div>
            </div>
          </CardContent>
        </Card>

        <Card v-if="canWrite">
          <CardHeader>
            <CardTitle>{{ t('mcp.connections') }}</CardTitle>
            <CardDescription>{{ t('mcp.connectionsDesc') }}</CardDescription>
          </CardHeader>
          <CardContent>
            <DataTable
              :items="connections"
              :columns="columns"
              :is-loading="isLoadingConnections"
              :empty-icon="Plug"
              :empty-title="t('mcp.noConnections')"
              :empty-description="t('mcp.noConnectionsDesc')"
            >
              <template #cell-client_name="{ item: conn }">
                <span class="text-sm font-medium">{{ conn.client_name || conn.client_id }}</span>
              </template>
              <template #cell-user="{ item: conn }">
                <div class="min-w-0">
                  <p class="text-sm truncate">{{ conn.user_name }}</p>
                  <p class="text-xs text-muted-foreground truncate">{{ conn.user_email }}</p>
                </div>
              </template>
              <template #cell-scopes="{ item: conn }">
                <div class="flex flex-wrap gap-1 max-w-[280px]">
                  <Badge v-for="scope in conn.scopes" :key="scope" variant="outline" class="text-[11px]" :title="t(`mcp.scopes.${scope}.desc`)">
                    {{ t(`mcp.scopes.${scope}.label`) }}
                  </Badge>
                </div>
              </template>
              <template #cell-created_at="{ item: conn }">
                <span class="text-sm whitespace-nowrap">{{ formatLogTime(conn.created_at) }}</span>
              </template>
              <template #cell-last_used_at="{ item: conn }">
                <span class="text-sm text-muted-foreground whitespace-nowrap">{{ conn.last_used_at ? formatLogTime(conn.last_used_at) : t('mcp.never') }}</span>
              </template>
              <template #cell-actions="{ item: conn }">
                <Button variant="ghost" size="sm" @click="confirmRevoke(conn)">
                  <Unplug class="h-4 w-4 mr-1" />
                  {{ t('mcp.disconnect') }}
                </Button>
              </template>
            </DataTable>
          </CardContent>
        </Card>
      </div>
    </ScrollArea>

    <DeleteConfirmDialog
      v-model:open="revokeOpen"
      :title="t('mcp.disconnectTitle')"
      :description="t('mcp.disconnectDesc')"
      :is-submitting="isRevoking"
      @confirm="revoke"
    />
  </div>
</template>
