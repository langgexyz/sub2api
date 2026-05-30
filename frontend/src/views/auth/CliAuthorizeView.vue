<template>
  <AuthLayout>
    <div class="rounded-lg bg-white p-6 shadow-xl dark:bg-dark-secondary">
      <h2 class="mb-4 text-lg font-semibold text-gray-900 dark:text-white">
        {{ t('cliAuthorize.title') }}
      </h2>

      <p class="mb-4 text-sm text-gray-700 dark:text-gray-200">
        {{ t('cliAuthorize.prompt') }}
      </p>

      <!-- Requesting redirect host -->
      <div
        v-if="redirectHost"
        class="mb-2 flex items-center justify-between gap-3 text-sm"
      >
        <span class="text-gray-500 dark:text-gray-400">{{ t('cliAuthorize.deviceLabel') }}</span>
        <span class="font-mono text-gray-900 dark:text-white">{{ redirectHost }}</span>
      </div>

      <!-- Optional device name -->
      <div
        v-if="deviceName"
        class="mb-2 flex items-center justify-between gap-3 text-sm"
      >
        <span class="text-gray-500 dark:text-gray-400">{{ t('cliAuthorize.deviceLabel') }}</span>
        <span class="font-mono text-gray-900 dark:text-white">{{ deviceName }}</span>
      </div>

      <!-- Error message -->
      <p v-if="errorMessage" class="mt-3 text-sm text-red-600 dark:text-red-400">
        {{ errorMessage }}
      </p>

      <div class="mt-6 flex gap-3">
        <button
          type="button"
          class="flex-1 rounded-lg border border-gray-300 px-4 py-2 text-gray-700 hover:bg-gray-50 dark:border-dark-border dark:text-gray-200 dark:hover:bg-dark-primary"
          :disabled="loading"
          @click="handleCancel"
        >
          {{ t('cliAuthorize.cancelButton') }}
        </button>
        <button
          type="button"
          class="flex-1 rounded-lg bg-blue-600 px-4 py-2 font-medium text-white hover:bg-blue-700 disabled:opacity-50"
          :disabled="loading"
          @click="handleAuthorize"
        >
          {{ loading ? t('cliAuthorize.authorizing') : t('cliAuthorize.authorizeButton') }}
        </button>
      </div>
    </div>
  </AuthLayout>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import { AuthLayout } from '@/components/layout'
import { authAPI } from '@/api/auth'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()

const loading = ref(false)
const errorMessage = ref('')

function queryString(key: string): string {
  const value = route.query[key]
  if (typeof value === 'string') {
    return value
  }
  if (Array.isArray(value) && typeof value[0] === 'string') {
    return value[0]
  }
  return ''
}

const responseType = computed(() => queryString('response_type'))
const codeChallenge = computed(() => queryString('code_challenge'))
const codeChallengeMethod = computed(() => queryString('code_challenge_method'))
const redirectUri = computed(() => queryString('redirect_uri'))
const state = computed(() => queryString('state'))
const devicePubkey = computed(() => queryString('device_pubkey'))
const deviceName = computed(() => queryString('name'))

const redirectHost = computed(() => {
  if (!redirectUri.value) {
    return ''
  }
  try {
    return new URL(redirectUri.value).host
  } catch {
    return redirectUri.value
  }
})

async function handleAuthorize(): Promise<void> {
  errorMessage.value = ''
  loading.value = true
  try {
    const { redirect_to } = await authAPI.authorizeCli({
      response_type: responseType.value,
      code_challenge: codeChallenge.value,
      code_challenge_method: codeChallengeMethod.value,
      redirect_uri: redirectUri.value,
      state: state.value,
      device_pubkey: devicePubkey.value,
      name: deviceName.value || undefined
    })
    // Hand the authorization code to the edge's loopback server.
    window.location.href = redirect_to
  } catch {
    errorMessage.value = t('cliAuthorize.failed')
    loading.value = false
  }
}

function handleCancel(): void {
  router.push('/dashboard')
}
</script>
