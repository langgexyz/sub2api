<template>
  <AuthLayout>
    <div class="rounded-lg bg-white p-6 shadow-xl dark:bg-dark-secondary">
      <h2 class="mb-2 text-lg font-semibold text-gray-900 dark:text-white">
        {{ t('deviceAuthorization.title') }}
      </h2>
      <p class="mb-5 text-sm text-gray-600 dark:text-gray-300">
        {{ t('deviceAuthorization.description') }}
      </p>

      <!-- user_code 输入 -->
      <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-200">
        {{ t('deviceAuthorization.codeLabel') }}
      </label>
      <input
        v-model="userCode"
        type="text"
        autocapitalize="characters"
        spellcheck="false"
        :placeholder="t('deviceAuthorization.codePlaceholder')"
        class="w-full rounded-lg border border-gray-300 px-3 py-2 text-center font-mono text-lg tracking-widest uppercase focus:border-blue-500 focus:outline-none dark:border-dark-border dark:bg-dark-primary dark:text-white"
        :disabled="state === 'approved'"
        @keyup.enter="handleApprove"
      />

      <!-- 状态提示 -->
      <p v-if="message" class="mt-3 text-sm" :class="messageClass">{{ message }}</p>

      <div class="mt-6 flex gap-3">
        <button
          type="button"
          class="flex-1 rounded-lg border border-gray-300 px-4 py-2 text-gray-700 hover:bg-gray-50 dark:border-dark-border dark:text-gray-200 dark:hover:bg-dark-primary"
          :disabled="loading || state === 'approved'"
          @click="goHome"
        >
          {{ t('common.cancel') }}
        </button>
        <button
          type="button"
          class="flex-1 rounded-lg bg-blue-600 px-4 py-2 font-medium text-white hover:bg-blue-700 disabled:opacity-50"
          :disabled="loading || !userCode || state === 'approved'"
          @click="handleApprove"
        >
          {{ loading ? t('common.verifying') : t('deviceAuthorization.approveButton') }}
        </button>
      </div>
    </div>
  </AuthLayout>
</template>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import { AuthLayout } from '@/components/layout'
import { authAPI } from '@/api/auth'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()

const userCode = ref('')
const loading = ref(false)
const state = ref<'idle' | 'approved' | 'error'>('idle')
const message = ref('')

const messageClass = computed(() =>
  state.value === 'approved'
    ? 'text-green-600 dark:text-green-400'
    : 'text-red-600 dark:text-red-400',
)

// edge 打开的是 verification_uri_complete（带 ?user_code=），这里预填。
onMounted(() => {
  const fromQuery = route.query.user_code
  if (typeof fromQuery === 'string' && fromQuery) {
    userCode.value = fromQuery.toUpperCase()
  }
})

function normalize(code: string): string {
  return code.trim().toUpperCase()
}

async function handleApprove() {
  const code = normalize(userCode.value)
  if (!code) return
  loading.value = true
  message.value = ''
  try {
    // 先校验码有效（给出更友好的错误），再批准。
    const valid = await authAPI.verifyDeviceCode(code)
    if (!valid) {
      state.value = 'error'
      message.value = t('deviceAuthorization.invalidCode')
      return
    }
    await authAPI.approveDevice(code)
    state.value = 'approved'
    message.value = t('deviceAuthorization.approved')
  } catch {
    state.value = 'error'
    message.value = t('deviceAuthorization.approveFailed')
  } finally {
    loading.value = false
  }
}

function goHome() {
  router.push('/dashboard')
}
</script>
