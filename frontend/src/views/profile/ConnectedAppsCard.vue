<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { toast } from 'vue-sonner'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { DeleteConfirmDialog } from '@/components/shared'
import { mcpService, type MCPConnection } from '@/services/api'
import { getErrorMessage } from '@/lib/api-utils'
import { formatLogTime } from '@/lib/logs'
import { Bot, Loader2, Unplug } from 'lucide-vue-next'

const { t } = useI18n()

const connections = ref<MCPConnection[]>([])
const isLoading = ref(true)
const revokeTarget = ref<MCPConnection | null>(null)
const revokeOpen = ref(false)
const isRevoking = ref(false)

onMounted(async () => {
  try {
    const response = await mcpService.listMyConnections()
    const data = (response.data as any).data || response.data
    connections.value = data.connections || []
  } catch (e) {
    toast.error(getErrorMessage(e, t('mcp.connectionsLoadFailed')))
  } finally {
    isLoading.value = false
  }
})

function confirmRevoke(conn: MCPConnection) {
  revokeTarget.value = conn
  revokeOpen.value = true
}

async function revoke() {
  if (!revokeTarget.value) return
  isRevoking.value = true
  try {
    await mcpService.revokeMyConnection(revokeTarget.value.id)
    connections.value = connections.value.filter(c => c.id !== revokeTarget.value?.id)
    revokeOpen.value = false
    toast.success(t('mcp.disconnected'))
  } catch (e) {
    toast.error(getErrorMessage(e, t('mcp.disconnectFailed')))
  } finally {
    isRevoking.value = false
  }
}
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle>{{ t('profile.connectedApps') }}</CardTitle>
      <CardDescription>{{ t('profile.connectedAppsDesc') }}</CardDescription>
    </CardHeader>
    <CardContent>
      <div v-if="isLoading" class="flex justify-center py-4">
        <Loader2 class="h-5 w-5 animate-spin text-muted-foreground" />
      </div>
      <p v-else-if="connections.length === 0" class="text-sm text-muted-foreground">{{ t('profile.noConnectedApps') }}</p>
      <div v-else class="divide-y rounded-lg border">
        <div v-for="conn in connections" :key="conn.id" class="flex items-start gap-3 p-3">
          <Bot class="h-5 w-5 mt-0.5 shrink-0 text-muted-foreground" />
          <div class="flex-1 min-w-0 space-y-1">
            <p class="text-sm font-medium truncate">
              {{ conn.client_name || conn.client_id }}
              <span class="font-normal text-muted-foreground">· {{ conn.organization_name }}</span>
            </p>
            <div class="flex flex-wrap gap-1">
              <Badge v-for="scope in conn.scopes" :key="scope" variant="outline" class="text-[11px]">
                {{ t(`mcp.scopes.${scope}.label`) }}
              </Badge>
            </div>
            <p class="text-xs text-muted-foreground">
              {{ t('mcp.columns.connected') }} {{ formatLogTime(conn.created_at) }} ·
              {{ t('mcp.columns.lastUsed') }} {{ conn.last_used_at ? formatLogTime(conn.last_used_at) : t('mcp.never') }}
            </p>
          </div>
          <Button variant="ghost" size="sm" @click="confirmRevoke(conn)">
            <Unplug class="h-4 w-4 mr-1" />
            {{ t('mcp.disconnect') }}
          </Button>
        </div>
      </div>
    </CardContent>
  </Card>

  <DeleteConfirmDialog
    v-model:open="revokeOpen"
    :title="t('mcp.disconnectTitle')"
    :description="t('mcp.disconnectDesc')"
    :is-submitting="isRevoking"
    @confirm="revoke"
  />
</template>
