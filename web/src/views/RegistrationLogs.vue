<script setup lang="ts">
import { poolApi, type LogEntry } from "@/api/pool";
import { ref, onMounted, onUnmounted, nextTick, watch } from "vue";

const logs = ref<LogEntry[]>([]);
const nextIndex = ref(0);
const autoScroll = ref(true);
const logContainerRef = ref<HTMLDivElement | null>(null);
let pollTimer: ReturnType<typeof setInterval> | null = null;

async function fetchLogs() {
  try {
    const res = await poolApi.getRegistrationLogs(nextIndex.value);
    if (res.logs && res.logs.length > 0) {
      logs.value.push(...res.logs);
      // Keep max 1000 entries in frontend
      if (logs.value.length > 1000) {
        logs.value = logs.value.slice(-1000);
      }
    }
    nextIndex.value = res.next_index;
  } catch {
    // ignore polling errors
  }
}

function scrollToBottom() {
  if (!autoScroll.value || !logContainerRef.value) return;
  nextTick(() => {
    const el = logContainerRef.value;
    if (el) el.scrollTop = el.scrollHeight;
  });
}

watch(logs, () => scrollToBottom(), { deep: true });

function clearLogs() {
  logs.value = [];
}

function levelColor(level: string): string {
  switch (level) {
    case "warn":
      return "#f0a020";
    case "error":
      return "#d03050";
    case "debug":
      return "#8a8a8a";
    default:
      return "#18a058";
  }
}

onMounted(() => {
  fetchLogs();
  pollTimer = setInterval(fetchLogs, 2000);
});

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer);
});
</script>

<template>
  <div class="reg-logs-page">
    <div class="logs-header">
      <h2 class="page-title">📝 注册日志</h2>
      <div class="header-actions">
        <n-switch v-model:value="autoScroll" size="small">
          <template #checked>自动滚动</template>
          <template #unchecked>暂停滚动</template>
        </n-switch>
        <n-button size="small" quaternary @click="clearLogs">清空</n-button>
        <n-tag :type="logs.length > 0 ? 'success' : 'default'" size="small" round>
          {{ logs.length }} 条
        </n-tag>
      </div>
    </div>

    <div ref="logContainerRef" class="log-container">
      <div v-if="logs.length === 0" class="log-empty">
        暂无注册日志，启动注册任务后日志将显示在此处
      </div>
      <div v-for="(entry, i) in logs" :key="i" class="log-line">
        <span class="log-time">{{ entry.time }}</span>
        <span class="log-level" :style="{ color: levelColor(entry.level) }">
          [{{ entry.level.toUpperCase() }}]
        </span>
        <span class="log-msg">{{ entry.message }}</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.reg-logs-page {
  display: flex;
  flex-direction: column;
  gap: 12px;
  height: calc(100vh - 150px);
}

.logs-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-shrink: 0;
}

.page-title {
  font-size: 1.2rem;
  font-weight: 700;
  margin: 0;
  background: var(--primary-gradient);
  -webkit-background-clip: text;
  -webkit-text-fill-color: transparent;
  background-clip: text;
}

.header-actions {
  display: flex;
  align-items: center;
  gap: 12px;
}

.log-container {
  flex: 1;
  overflow-y: auto;
  background: var(--card-bg, #1a1a2e);
  border-radius: var(--border-radius-lg, 12px);
  border: 1px solid var(--border-color-light, rgba(255,255,255,0.08));
  padding: 12px 16px;
  font-family: "JetBrains Mono", "Fira Code", "Cascadia Code", monospace;
  font-size: 13px;
  line-height: 1.7;
}

.log-empty {
  color: #666;
  text-align: center;
  padding: 40px 0;
  font-family: inherit;
}

.log-line {
  display: flex;
  gap: 8px;
  padding: 1px 0;
  word-break: break-all;
}

.log-line:hover {
  background: rgba(102, 126, 234, 0.06);
  border-radius: 4px;
}

.log-time {
  color: #999;
  flex-shrink: 0;
  min-width: 64px;
}

.log-level {
  flex-shrink: 0;
  font-weight: 600;
  min-width: 60px;
}

.log-msg {
  color: var(--text-color, #333);
}

:root.dark .log-time {
  color: #aaa;
}

:root.dark .log-msg {
  color: #e0e0e0;
}
</style>
