<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { useAuthStore } from '@/stores/auth'
import { mcpService, type OAuthConsentRequest, type MCPScope } from '@/services/api'
import { getErrorMessage } from '@/lib/api-utils'
import { toast } from 'vue-sonner'
import { MessageSquare, Bot, ArrowLeftRight, Loader2, AlertTriangle, ExternalLink } from 'lucide-vue-next'

const { t } = useI18n()
const route = useRoute()
const authStore = useAuthStore()

const requestId = String(route.query.request || '')
const request = ref<OAuthConsentRequest | null>(null)
const isLoading = ref(true)
const notFound = ref(false)
const submitting = ref<'allow' | 'deny' | null>(null)
const redirecting = ref(false)
const logoFailed = ref(false)

const orgId = ref('')
const selectedScopes = ref<MCPScope[]>([])

const appName = computed(() => request.value?.client_name || t('oauthConsent.unnamedApp'))
const selectedOrg = computed(() => request.value?.organizations.find(o => o.id === orgId.value))
const orgDisabled = computed(() => !!selectedOrg.value && !selectedOrg.value.mcp_enabled)
const canAllow = computed(() => !!selectedOrg.value && !orgDisabled.value && selectedScopes.value.length > 0 && !submitting.value)

function toggleScope(scope: MCPScope, checked: boolean | 'indeterminate') {
  if (checked === true) {
    if (!selectedScopes.value.includes(scope)) selectedScopes.value.push(scope)
  } else {
    selectedScopes.value = selectedScopes.value.filter(s => s !== scope)
  }
}

onMounted(async () => {
  if (!requestId) {
    notFound.value = true
    isLoading.value = false
    return
  }
  try {
    const response = await mcpService.getConsentRequest(requestId)
    const data = (response.data as any).data || response.data
    request.value = data
    selectedScopes.value = [...data.scopes]
    const orgs = data.organizations as OAuthConsentRequest['organizations']
    orgId.value = (orgs.find(o => o.is_current && o.mcp_enabled) || orgs.find(o => o.mcp_enabled) || orgs.find(o => o.is_current) || orgs[0])?.id || ''
  } catch {
    notFound.value = true
  } finally {
    isLoading.value = false
  }
})

async function answer(allow: boolean) {
  if (!request.value) return
  if (allow && selectedScopes.value.length === 0) {
    toast.error(t('oauthConsent.chooseScope'))
    return
  }
  submitting.value = allow ? 'allow' : 'deny'
  try {
    const response = allow
      ? await mcpService.approve(requestId, { organization_id: orgId.value, scopes: selectedScopes.value })
      : await mcpService.deny(requestId)
    const data = (response.data as any).data || response.data
    redirecting.value = true
    window.location.href = data.redirect_url
  } catch (e: any) {
    if (e?.response?.status === 404) {
      notFound.value = true
    } else {
      toast.error(getErrorMessage(e, t('oauthConsent.failed')))
    }
    submitting.value = null
  }
}
</script>

<template>
  <div class="min-h-screen flex items-center justify-center bg-[#0a0a0b] light:bg-gradient-to-br light:from-gray-50 light:to-gray-100 p-4">
    <div class="w-full max-w-md rounded-2xl border border-white/[0.08] bg-white/[0.02] backdrop-blur light:bg-white light:border-gray-200 light:shadow-xl">
      <div v-if="isLoading" class="p-12 flex justify-center">
        <Loader2 class="h-6 w-6 animate-spin text-white/50 light:text-gray-400" />
      </div>

      <div v-else-if="notFound" class="p-8 text-center space-y-2">
        <AlertTriangle class="h-8 w-8 mx-auto text-amber-500" />
        <h2 class="text-lg font-semibold text-white light:text-gray-900">{{ $t('oauthConsent.notFound') }}</h2>
        <p class="text-sm text-white/50 light:text-gray-500">{{ $t('oauthConsent.notFoundDesc') }}</p>
      </div>

      <div v-else-if="redirecting" class="p-12 text-center space-y-3">
        <Loader2 class="h-6 w-6 mx-auto animate-spin text-white/50 light:text-gray-400" />
        <p class="text-sm text-white/60 light:text-gray-600">{{ $t('oauthConsent.redirecting', { app: appName }) }}</p>
      </div>

      <template v-else-if="request">
        <div class="p-8 pb-4 space-y-3 text-center">
          <div class="flex items-center justify-center gap-3 mb-2">
            <div class="h-12 w-12 rounded-xl border border-white/[0.08] bg-white/[0.04] light:border-gray-200 light:bg-gray-50 flex items-center justify-center overflow-hidden">
              <img
                v-if="request.logo_uri && !logoFailed"
                :src="request.logo_uri"
                :alt="appName"
                class="h-8 w-8 object-contain"
                referrerpolicy="no-referrer"
                @error="logoFailed = true"
              />
              <Bot v-else class="h-6 w-6 text-white/70 light:text-gray-600" />
            </div>
            <ArrowLeftRight class="h-4 w-4 text-white/30 light:text-gray-400" />
            <div class="h-12 w-12 rounded-xl bg-gradient-to-br from-emerald-500 to-green-600 flex items-center justify-center shadow-lg shadow-emerald-500/20">
              <MessageSquare class="h-6 w-6 text-white" />
            </div>
          </div>
          <h2 class="text-xl font-bold text-white light:text-gray-900">{{ $t('oauthConsent.title', { app: appName }) }}</h2>
          <p class="text-sm text-white/50 light:text-gray-500">{{ $t('oauthConsent.signedInAs', { email: authStore.user?.email || '' }) }}</p>
          <a
            v-if="request.client_uri"
            :href="request.client_uri"
            target="_blank"
            rel="noopener noreferrer"
            class="inline-flex items-center gap-1 text-xs text-white/40 hover:text-white/70 light:text-gray-500 light:hover:text-gray-700"
          >
            {{ $t('oauthConsent.appWebsite') }} <ExternalLink class="h-3 w-3" />
          </a>
        </div>

        <div class="px-8 pb-4 space-y-5">
          <div v-if="request.organizations.length > 1" class="space-y-2">
            <Label class="text-white/70 light:text-gray-700">{{ $t('oauthConsent.organization') }}</Label>
            <Select v-model="orgId">
              <SelectTrigger class="bg-white/[0.04] border-white/[0.1] text-white/80 light:bg-white light:border-gray-200 light:text-gray-700">
                <SelectValue />
              </SelectTrigger>
              <SelectContent class="bg-[#141414] border-white/[0.08] light:bg-white light:border-gray-200">
                <SelectItem
                  v-for="org in request.organizations"
                  :key="org.id"
                  :value="org.id"
                  class="text-white/70 focus:bg-white/[0.08] focus:text-white light:text-gray-700 light:focus:bg-gray-100"
                >
                  {{ org.name }}<span v-if="!org.mcp_enabled" class="ml-1 text-white/40 light:text-gray-400">({{ $t('oauthConsent.accessOff') }})</span>
                </SelectItem>
              </SelectContent>
            </Select>
          </div>

          <div v-if="orgDisabled" class="flex gap-2 rounded-lg border border-amber-500/30 bg-amber-500/10 p-3 text-sm text-amber-200 light:text-amber-800">
            <AlertTriangle class="h-4 w-4 shrink-0 mt-0.5" />
            <span>{{ $t('oauthConsent.orgDisabled', { org: selectedOrg?.name }) }}</span>
          </div>

          <div class="space-y-2">
            <p class="text-sm font-medium text-white/80 light:text-gray-800">{{ $t('oauthConsent.permissions') }}</p>
            <div class="rounded-lg border border-white/[0.08] light:border-gray-200 divide-y divide-white/[0.06] light:divide-gray-100">
              <div v-for="scope in request.scopes" :key="scope" class="flex items-start gap-3 p-3">
                <Checkbox
                  :id="`scope-${scope}`"
                  :checked="selectedScopes.includes(scope)"
                  :disabled="!!submitting"
                  class="mt-0.5"
                  @update:checked="(checked) => toggleScope(scope, checked)"
                />
                <div class="grid gap-0.5">
                  <Label :for="`scope-${scope}`" class="cursor-pointer text-white/90 light:text-gray-900">{{ $t(`mcp.scopes.${scope}.label`) }}</Label>
                  <p class="text-xs text-white/40 light:text-gray-500">{{ $t(`mcp.scopes.${scope}.desc`) }}</p>
                </div>
              </div>
            </div>
            <p class="text-xs text-white/40 light:text-gray-500">{{ $t('oauthConsent.roleNote') }}</p>
          </div>
        </div>

        <div class="px-8 pb-8 space-y-3">
          <div class="flex gap-3">
            <Button variant="outline" class="flex-1" :disabled="!!submitting" @click="answer(false)">
              <Loader2 v-if="submitting === 'deny'" class="mr-2 h-4 w-4 animate-spin" />
              {{ $t('oauthConsent.deny') }}
            </Button>
            <Button class="flex-1" :disabled="!canAllow" @click="answer(true)">
              <Loader2 v-if="submitting === 'allow'" class="mr-2 h-4 w-4 animate-spin" />
              {{ $t('oauthConsent.allow') }}
            </Button>
          </div>
          <p v-if="request.redirect_host" class="text-center text-xs text-white/30 light:text-gray-400">
            {{ $t('oauthConsent.redirectNote', { host: request.redirect_host }) }}
          </p>
        </div>
      </template>
    </div>
  </div>
</template>
