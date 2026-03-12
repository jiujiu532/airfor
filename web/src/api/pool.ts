import http from "@/utils/http";

export interface PoolStats {
  active_count: number;
  cooling_count: number;
  standby_count: number;
  invalid_count: number;
  total_count: number;
  waiting_queue: number;
  active_cap: number;
  total_served: number;
}

export interface PoolConfig {
  active_pool_size: number;
  rate_limit_count: number;
  rate_limit_window_seconds: number;
  cooldown_seconds: number;
  max_total_requests: number;
  request_wait_timeout_seconds: number;
  standby_refill_threshold: number;
  auto_register_count: number;
  auto_register_concurrency: number;
  auto_register_referral_code: string;
  solver_url: string;
  cooling_pool_multiplier: number;
  enabled: boolean;
}

export interface RegistrationStats {
  total_attempts: number;
  successes: number;
  failures: number;
  is_running: boolean;
}

export interface PoolStatsResponse {
  enabled: boolean;
  pool?: PoolStats;
  config?: PoolConfig;
  registrar?: RegistrationStats;
}

export interface LogEntry {
  time: string;
  level: string;
  message: string;
}

export interface LogsResponse {
  logs: LogEntry[];
  next_index: number;
}

export const poolApi = {
  async getStats(): Promise<PoolStatsResponse> {
    const response = await http.get("/pool/stats", { hideMessage: true });
    return response.data || { enabled: false };
  },

  async getConfig(): Promise<PoolConfig> {
    const response = await http.get("/pool/config", { hideMessage: true });
    return response.data;
  },

  async updateConfig(config: PoolConfig): Promise<void> {
    return http.put("/pool/config", config);
  },

  async startRegistration(count: number, concurrency: number): Promise<void> {
    return http.post("/pool/register/start", { count, concurrency });
  },

  async stopRegistration(): Promise<void> {
    return http.post("/pool/register/stop");
  },

  async getRegistrationStats(): Promise<RegistrationStats> {
    const response = await http.get("/pool/register/stats", { hideMessage: true });
    return response.data || { total_attempts: 0, successes: 0, failures: 0, is_running: false };
  },

  async getRegistrationLogs(sinceIndex: number = 0): Promise<LogsResponse> {
    const response = await http.get(`/pool/register/logs?since=${sinceIndex}`, { hideMessage: true });
    return response.data || { logs: [], next_index: 0 };
  },
};

