<template>
  <AppLayout>
    <div class="space-y-6">
      <!-- Loading State -->
      <div v-if="loading" class="flex justify-center py-12">
        <div
          class="h-8 w-8 animate-spin rounded-full border-2 border-primary-500 border-t-transparent"
        ></div>
      </div>

      <!-- Empty State -->
      <div v-else-if="subscriptions.length === 0" class="card p-12 text-center">
        <div
          class="mx-auto mb-4 flex h-16 w-16 items-center justify-center rounded-full bg-gray-100 dark:bg-dark-700"
        >
          <Icon name="creditCard" size="xl" class="text-gray-400" />
        </div>
        <h3 class="mb-2 text-lg font-semibold text-gray-900 dark:text-white">
          {{ t('userSubscriptions.noActiveSubscriptions') }}
        </h3>
        <p class="text-gray-500 dark:text-dark-400">
          {{ t('userSubscriptions.noActiveSubscriptionsDesc') }}
        </p>
      </div>

      <!-- Subscriptions Grid -->
      <div v-else class="grid gap-6 lg:grid-cols-2">
        <div
          v-for="subscription in subscriptions"
          :key="subscription.id"
          class="overflow-hidden rounded-2xl border bg-white dark:bg-dark-800"
          :class="platformBorderClass(subscription.group?.platform || '')"
        >
          <!-- Header -->
          <div
            class="flex items-center justify-between border-b border-gray-100 p-4 dark:border-dark-700"
          >
            <div class="flex items-center gap-3">
              <div :class="['h-1.5 w-1.5 shrink-0 rounded-full', platformAccentDotClass(subscription.group?.platform || '')]" />
              <div>
                <div class="flex items-center gap-2">
                  <h3 class="font-semibold text-gray-900 dark:text-white">
                    {{ subscription.group?.name || `Group #${subscription.group_id}` }}
                  </h3>
                  <span :class="['rounded-md border px-2 py-0.5 text-[11px] font-medium', platformBadgeClass(subscription.group?.platform || '')]">
                    {{ platformLabel(subscription.group?.platform || '') }}
                  </span>
                </div>
                <p v-if="subscription.group?.description" class="mt-0.5 text-xs text-gray-500 dark:text-dark-400">
                  {{ subscription.group.description }}
                </p>
              </div>
            </div>
            <div class="flex items-center gap-2">
              <span
                :class="[
                  'rounded-full px-2 py-0.5 text-xs font-medium',
                  subscription.status === 'active'
                    ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300'
                    : subscription.status === 'expired'
                      ? 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-400'
                      : 'bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-300'
                ]"
              >
                {{ t(`userSubscriptions.status.${subscription.status}`) }}
              </span>
              <button
                v-if="subscription.status === 'active'"
                :class="['rounded-lg px-3 py-1.5 text-xs font-semibold text-white transition-colors', platformButtonClass(subscription.group?.platform || '')]"
                @click="router.push({ path: '/purchase', query: { tab: 'subscription', group: String(subscription.group_id) } })"
              >
                {{ t('payment.renewNow') }}
              </button>
            </div>
          </div>

          <!-- Usage Progress -->
          <div class="space-y-4 p-4">
            <!-- Expiration Info -->
            <div v-if="subscription.expires_at" class="flex items-center justify-between text-sm">
              <span class="text-gray-500 dark:text-dark-400">{{
                t('userSubscriptions.expires')
              }}</span>
              <span :class="getExpirationClass(subscription.expires_at)">
                {{ formatExpirationDate(subscription.expires_at) }}
              </span>
            </div>
            <div v-else class="flex items-center justify-between text-sm">
              <span class="text-gray-500 dark:text-dark-400">{{
                t('userSubscriptions.expires')
              }}</span>
              <span class="text-gray-700 dark:text-gray-300">{{
                t('userSubscriptions.noExpiration')
              }}</span>
            </div>

            <!-- 用量窗口（透传绑定号真实 5h / 7d；方向2私有视图：只显百分比+重置，藏美元）-->
            <div
              v-for="w in usageWindowsFor(subscription)"
              :key="w.key"
              class="space-y-2"
            >
              <div class="flex items-center justify-between">
                <span class="text-sm font-medium text-gray-700 dark:text-gray-300">
                  {{ w.label }}
                </span>
                <span class="text-sm text-gray-500 dark:text-dark-400">
                  {{ t('userSubscriptions.usedPercent', { pct: w.percent }) }}
                </span>
              </div>
              <div class="relative h-2 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-600">
                <div
                  class="absolute inset-y-0 left-0 rounded-full transition-all duration-300"
                  :class="w.barClass"
                  :style="{ width: w.percent + '%' }"
                ></div>
              </div>
              <p v-if="w.resetText" class="text-xs text-gray-500 dark:text-dark-400">
                {{ w.resetText }}
              </p>
            </div>

            <!-- 进度尚未就绪（无反推数据 / 账号未采样）-->
            <div
              v-if="usageWindowsFor(subscription).length === 0"
              class="flex items-center justify-center rounded-xl bg-gray-50 py-6 dark:bg-dark-700/40"
            >
              <p class="text-sm text-gray-500 dark:text-dark-400">
                {{ t('userSubscriptions.usageNotReady') }}
              </p>
            </div>
          </div>
        </div>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { useAppStore } from '@/stores/app'
import subscriptionsAPI from '@/api/subscriptions'
import type { UserSubscription, SubscriptionProgress, UsageWindowProgress } from '@/types'
import AppLayout from '@/components/layout/AppLayout.vue'
import Icon from '@/components/icons/Icon.vue'
import { formatDateOnly } from '@/utils/format'
import { platformBorderClass, platformBadgeClass, platformButtonClass, platformLabel } from '@/utils/platformColors'

function platformAccentDotClass(p: string): string {
  switch (p) {
    case 'anthropic': return 'bg-orange-500'
    case 'openai': return 'bg-emerald-500'
    case 'antigravity': return 'bg-purple-500'
    case 'gemini': return 'bg-blue-500'
    default: return 'bg-gray-400'
  }
}

const { t } = useI18n()
const router = useRouter()
const appStore = useAppStore()

const subscriptions = ref<UserSubscription[]>([])
const progressMap = ref<Record<number, SubscriptionProgress>>({})
const loading = ref(true)

async function loadSubscriptions() {
  try {
    loading.value = true
    const [subs, infos] = await Promise.all([
      subscriptionsAPI.getMySubscriptions(),
      subscriptionsAPI.getSubscriptionsProgress().catch(() => [])
    ])
    subscriptions.value = subs
    progressMap.value = Object.fromEntries(infos.map((i) => [i.subscription.id, i.progress]))
  } catch (error) {
    console.error('Failed to load subscriptions:', error)
    appStore.showError(t('userSubscriptions.failedToLoad'))
  } finally {
    loading.value = false
  }
}

interface UsageWindowView {
  key: string
  label: string
  percent: number
  barClass: string
  resetText: string
}

function barClassByPercent(pct: number): string {
  if (pct >= 90) return 'bg-red-500'
  if (pct >= 70) return 'bg-orange-500'
  return 'bg-green-500'
}

function formatResetText(secs: number): string {
  if (secs <= 0) return t('userSubscriptions.resetSoon')
  const days = Math.floor(secs / 86400)
  if (days >= 1) return t('userSubscriptions.resetIn', { time: t('userSubscriptions.daysShort', { days }) })
  const h = Math.floor(secs / 3600)
  const m = Math.floor((secs % 3600) / 60)
  return t('userSubscriptions.resetIn', { time: h >= 1 ? `${h}h${m}m` : `${m}m` })
}

function windowView(key: string, label: string, w?: UsageWindowProgress | null): UsageWindowView | null {
  if (!w) return null
  const percent = Math.round(w.percentage)
  return { key, label, percent, barClass: barClassByPercent(percent), resetText: formatResetText(w.resets_in_seconds) }
}

function usageWindowsFor(sub: UserSubscription): UsageWindowView[] {
  const p = progressMap.value[sub.id]
  if (!p) return []
  const out: UsageWindowView[] = []
  const fh = windowView('5h', t('userSubscriptions.fiveHourUsage'), p.five_hour)
  if (fh) out.push(fh)
  const wk = windowView('7d', t('userSubscriptions.sevenDayUsage'), p.weekly)
  if (wk) out.push(wk)
  return out
}

function formatExpirationDate(expiresAt: string): string {
  const now = new Date()
  const expires = new Date(expiresAt)
  const diff = expires.getTime() - now.getTime()
  const days = Math.ceil(diff / (1000 * 60 * 60 * 24))

  if (days < 0) {
    return t('userSubscriptions.status.expired')
  }

  const dateStr = formatDateOnly(expires)

  if (days === 0) {
    return `${dateStr} (${t('common.today')})`
  }
  if (days === 1) {
    return `${dateStr} (${t('common.tomorrow')})`
  }

  return t('userSubscriptions.daysRemaining', { days }) + ` (${dateStr})`
}

function getExpirationClass(expiresAt: string): string {
  const now = new Date()
  const expires = new Date(expiresAt)
  const diff = expires.getTime() - now.getTime()
  const days = Math.ceil(diff / (1000 * 60 * 60 * 24))

  if (days <= 0) return 'text-red-600 dark:text-red-400 font-medium'
  if (days <= 3) return 'text-red-600 dark:text-red-400'
  if (days <= 7) return 'text-orange-600 dark:text-orange-400'
  return 'text-gray-700 dark:text-gray-300'
}

onMounted(() => {
  loadSubscriptions()
})
</script>
