<script setup lang="ts">
import {
  poolApi,
  type PoolConfig,
  type PoolStats,
  type RegistrationStats,
} from "@/api/pool";
import {
  NButton,
  NCard,
  NForm,
  NFormItem,
  NGrid,
  NGridItem,
  NInput,
  NInputNumber,
  NSpace,
  NStatistic,
  NSwitch,
  NTag,
  useMessage,
} from "naive-ui";
import { onBeforeUnmount, onMounted, ref } from "vue";

const message = useMessage();

const poolEnabled = ref(false);
const stats = ref<PoolStats | null>(null);
const config = ref<PoolConfig>({
  active_pool_size: 1000,
  rate_limit_count: 2,
  rate_limit_window_seconds: 60,
  cooldown_seconds: 60,
  max_total_requests: 1000,
  request_wait_timeout_seconds: 30,
  standby_refill_threshold: 100,
  auto_register_count: 50,
  auto_register_concurrency: 2,
  auto_register_referral_code: "",
  solver_url: "http://localhost:5072",
  cooling_pool_multiplier: 1,
  enabled: false,
});
const regStats = ref<RegistrationStats>({
  total_attempts: 0,
  successes: 0,
  failures: 0,
  is_running: false,
});
const isSaving = ref(false);
const regCount = ref(50);
const regConcurrency = ref(2);

let pollTimer: ReturnType<typeof setInterval> | null = null;

onMounted(async () => {
  await fetchConfig();
  await fetchStats();
  // Poll stats every 3 seconds
  pollTimer = setInterval(fetchStats, 3000);
});

onBeforeUnmount(() => {
  if (pollTimer) clearInterval(pollTimer);
});

async function fetchStats() {
  try {
    const data = await poolApi.getStats();
    poolEnabled.value = data.enabled;
    if (data.pool) stats.value = data.pool;
    if (data.registrar) regStats.value = data.registrar;
  } catch (_e) {
    /* silent */
  }
  // Always fetch registration stats independently
  try {
    const rs = await poolApi.getRegistrationStats();
    regStats.value = rs;
  } catch (_e) {
    /* silent */
  }
}


async function fetchConfig() {
  try {
    const data = await poolApi.getConfig();
    config.value = data;
  } catch (_e) {
    /* silent */
  }
}

async function saveConfig() {
  isSaving.value = true;
  try {
    await poolApi.updateConfig(config.value);
    await fetchStats();
  } finally {
    isSaving.value = false;
  }
}

async function startRegistration() {
  try {
    await poolApi.startRegistration(regCount.value, regConcurrency.value);
    // Immediately update stats to catch is_running=true
    await fetchStats();
    // Poll faster during registration (every 1 second)
    if (pollTimer) clearInterval(pollTimer);
    pollTimer = setInterval(fetchStats, 1000);
  } catch (e: any) {
    message.error(e?.response?.data?.error || "Failed to start registration");
  }
}

async function stopRegistration() {
  try {
    await poolApi.stopRegistration();
    message.success("停止信号已发送");
    await fetchStats();
    // Slow back down to normal polling
    if (pollTimer) clearInterval(pollTimer);
    pollTimer = setInterval(fetchStats, 3000);
  } catch (_e) {
    /* silent */
  }
}

function activePercent(): number {
  if (!stats.value || stats.value.active_cap === 0) return 0;
  return Math.round((stats.value.active_count / stats.value.active_cap) * 100);
}
</script>

<template>
  <div class="pool-container">
    <n-space vertical size="large">
      <!-- Pool Status Cards -->
      <div class="pool-status-header">
        <h2 class="section-title">
          🏊 密钥池状态
          <n-tag v-if="poolEnabled" type="success" size="small" round>运行中</n-tag>
          <n-tag v-else type="warning" size="small" round>未启用</n-tag>
        </h2>
      </div>

      <div class="stats-grid">
        <n-card class="stat-card active-card" size="small" hoverable>
          <div class="stat-inner">
            <div class="stat-icon">🟢</div>
            <n-statistic label="活跃池" :value="stats?.active_count ?? 0">
              <template #suffix>
                <span class="stat-suffix">/ {{ stats?.active_cap ?? 0 }}</span>
              </template>
            </n-statistic>
            <div class="stat-bar">
              <div class="stat-bar-fill active-fill" :style="{ width: activePercent() + '%' }" />
            </div>
          </div>
        </n-card>

        <n-card class="stat-card cooling-card" size="small" hoverable>
          <div class="stat-inner">
            <div class="stat-icon">❄️</div>
            <n-statistic label="冷却池" :value="stats?.cooling_count ?? 0" />
          </div>
        </n-card>

        <n-card class="stat-card standby-card" size="small" hoverable>
          <div class="stat-inner">
            <div class="stat-icon">⏳</div>
            <n-statistic label="备用池" :value="stats?.standby_count ?? 0" />
          </div>
        </n-card>

        <n-card class="stat-card invalid-card" size="small" hoverable>
          <div class="stat-inner">
            <div class="stat-icon">🚫</div>
            <n-statistic label="已失效" :value="stats?.invalid_count ?? 0" />
          </div>
        </n-card>

        <n-card class="stat-card queue-card" size="small" hoverable>
          <div class="stat-inner">
            <div class="stat-icon">🔄</div>
            <n-statistic label="等待队列" :value="stats?.waiting_queue ?? 0" />
          </div>
        </n-card>

        <n-card class="stat-card total-card" size="small" hoverable>
          <div class="stat-inner">
            <div class="stat-icon">📈</div>
            <n-statistic label="累计请求" :value="stats?.total_served ?? 0" />
          </div>
        </n-card>
      </div>

      <!-- Configuration -->
      <n-card title="⚙️ 池配置" size="small" hoverable bordered>
        <n-form label-placement="top" :model="config">
          <n-grid :x-gap="24" :y-gap="0" responsive="screen" cols="1 s:2 m:3 l:4">
            <n-grid-item>
              <n-form-item label="启用速率限制池">
                <n-switch v-model:value="config.enabled" />
              </n-form-item>
            </n-grid-item>
            <n-grid-item>
              <n-form-item label="活跃池大小">
                <n-input-number
                  v-model:value="config.active_pool_size"
                  :min="1"
                  style="width: 100%"
                  size="small"
                />
              </n-form-item>
            </n-grid-item>
            <n-grid-item>
              <n-form-item label="速率限制（次/窗口）">
                <n-input-number
                  v-model:value="config.rate_limit_count"
                  :min="1"
                  style="width: 100%"
                  size="small"
                />
              </n-form-item>
            </n-grid-item>
            <n-grid-item>
              <n-form-item label="滑动窗口（秒）">
                <n-input-number
                  v-model:value="config.rate_limit_window_seconds"
                  :min="1"
                  style="width: 100%"
                  size="small"
                />
              </n-form-item>
            </n-grid-item>
            <n-grid-item>
              <n-form-item label="冷却时间（秒）">
                <n-input-number
                  v-model:value="config.cooldown_seconds"
                  :min="0"
                  style="width: 100%"
                  size="small"
                />
              </n-form-item>
            </n-grid-item>
            <n-grid-item>
              <n-form-item label="冷却池倍数">
                <n-input-number
                  v-model:value="config.cooling_pool_multiplier"
                  :min="1"
                  :max="10"
                  style="width: 100%"
                  size="small"
                />
                <template #feedback>
                  冷却池目标 = 活跃池 × {{ config.cooling_pool_multiplier || 1 }}
                  = {{ (config.active_pool_size || 0) * (config.cooling_pool_multiplier || 1) }} 个key
                </template>
              </n-form-item>
            </n-grid-item>
            <n-grid-item>
              <n-form-item label="生命周期上限（次）">
                <n-input-number
                  v-model:value="config.max_total_requests"
                  :min="0"
                  style="width: 100%"
                  size="small"
                  placeholder="0 = 无限"
                />
              </n-form-item>
            </n-grid-item>
            <n-grid-item>
              <n-form-item label="等待超时（秒）">
                <n-input-number
                  v-model:value="config.request_wait_timeout_seconds"
                  :min="1"
                  style="width: 100%"
                  size="small"
                />
              </n-form-item>
            </n-grid-item>
            <n-grid-item>
              <n-form-item label="备用池补充阈值">
                <n-input-number
                  v-model:value="config.standby_refill_threshold"
                  :min="0"
                  style="width: 100%"
                  size="small"
                />
              </n-form-item>
            </n-grid-item>
          </n-grid>

          <!-- Auto-Register Config -->
          <n-grid :x-gap="24" :y-gap="0" responsive="screen" cols="1 s:2 m:3 l:4">
            <n-grid-item>
              <n-form-item label="自动注册数量">
                <n-input-number
                  v-model:value="config.auto_register_count"
                  :min="1"
                  style="width: 100%"
                  size="small"
                />
              </n-form-item>
            </n-grid-item>
            <n-grid-item>
              <n-form-item label="注册并发数">
                <n-input-number
                  v-model:value="config.auto_register_concurrency"
                  :min="1"
                  style="width: 100%"
                  size="small"
                />
              </n-form-item>
            </n-grid-item>
            <n-grid-item :span="2">
              <n-form-item label="邀请码">
                <n-input
                  v-model:value="config.auto_register_referral_code"
                  placeholder="注册用邀请码"
                  size="small"
                />
              </n-form-item>
            </n-grid-item>
            <n-grid-item :span="2">
              <n-form-item label="打码服务地址">
                <n-input
                  v-model:value="config.solver_url"
                  placeholder="http://localhost:5072"
                  size="small"
                />
              </n-form-item>
            </n-grid-item>
          </n-grid>



          <div style="display: flex; justify-content: center; padding-top: 8px">
            <n-button
              type="primary"
              :loading="isSaving"
              @click="saveConfig"
              style="min-width: 180px"
            >
              💾 保存配置
            </n-button>
          </div>
        </n-form>
      </n-card>

      <!-- Manual Registration -->
      <n-card title="🤖 自动注册" size="small" hoverable bordered>
        <n-space vertical>
          <!-- Registration Stats -->
          <div class="reg-stats-row">
            <n-tag :type="regStats.is_running ? 'success' : 'default'" round>
              {{ regStats.is_running ? '🔄 注册中...' : '⏹ 空闲' }}
            </n-tag>
            <span class="reg-stat-item">尝试: {{ regStats.total_attempts }}</span>
            <span class="reg-stat-item success">✅ {{ regStats.successes }}</span>
            <span class="reg-stat-item fail">❌ {{ regStats.failures }}</span>
          </div>

          <!-- Controls -->
          <n-grid :x-gap="12" cols="3">
            <n-grid-item>
              <n-form-item label="注册数量" size="small">
                <n-input-number v-model:value="regCount" :min="1" style="width: 100%" size="small" />
              </n-form-item>
            </n-grid-item>
            <n-grid-item>
              <n-form-item label="并发数" size="small">
                <n-input-number v-model:value="regConcurrency" :min="1" :max="10" style="width: 100%" size="small" />
              </n-form-item>
            </n-grid-item>
            <n-grid-item>
              <n-form-item label=" " size="small">
                <n-space>
                  <n-button
                    type="primary"
                    :disabled="regStats.is_running"
                    @click="startRegistration"
                    size="small"
                  >
                    🚀 开始注册
                  </n-button>
                  <n-button
                    type="error"
                    @click="stopRegistration"
                    size="small"
                  >
                    ⏹ 停止
                  </n-button>
                </n-space>
              </n-form-item>
            </n-grid-item>
          </n-grid>
        </n-space>
      </n-card>
    </n-space>
  </div>
</template>

<style scoped>
.pool-container {
  animation: fadeInUp 0.3s ease-out;
}

.section-title {
  font-size: 1.5rem;
  font-weight: 700;
  margin: 0;
  display: flex;
  align-items: center;
  gap: 12px;
}

.stats-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(170px, 1fr));
  gap: 12px;
}

.stat-card {
  border-radius: var(--border-radius-lg);
  transition: all 0.3s ease;
  overflow: hidden;
}

.stat-card:hover {
  transform: translateY(-3px);
  box-shadow: var(--shadow-lg);
}

.stat-inner {
  display: flex;
  flex-direction: column;
  align-items: center;
  text-align: center;
  gap: 4px;
}

.stat-icon {
  font-size: 1.8rem;
  margin-bottom: 2px;
}

.stat-suffix {
  font-size: 0.85rem;
  opacity: 0.6;
  font-weight: 400;
}

.stat-bar {
  width: 100%;
  height: 4px;
  background: rgba(128, 128, 128, 0.15);
  border-radius: 2px;
  overflow: hidden;
  margin-top: 4px;
}

.stat-bar-fill {
  height: 100%;
  border-radius: 2px;
  transition: width 0.5s ease;
}

.active-fill {
  background: linear-gradient(90deg, #18a058, #36ad6a);
}

.active-card {
  border-left: 3px solid #18a058;
}

.cooling-card {
  border-left: 3px solid #2080f0;
}

.standby-card {
  border-left: 3px solid #f0a020;
}

.invalid-card {
  border-left: 3px solid #d03050;
}

.queue-card {
  border-left: 3px solid #8b5cf6;
}

.total-card {
  border-left: 3px solid #06b6d4;
}

.reg-stats-row {
  display: flex;
  align-items: center;
  gap: 16px;
  flex-wrap: wrap;
  padding: 8px 0;
}

.reg-stat-item {
  font-size: 0.9rem;
  font-weight: 500;
  opacity: 0.8;
}

.reg-stat-item.success {
  color: #18a058;
}

.reg-stat-item.fail {
  color: #d03050;
}

@keyframes fadeInUp {
  from {
    opacity: 0;
    transform: translateY(20px);
  }
  to {
    opacity: 1;
    transform: translateY(0);
  }
}

@media (max-width: 768px) {
  .stats-grid {
    grid-template-columns: repeat(2, 1fr);
  }
}
</style>
