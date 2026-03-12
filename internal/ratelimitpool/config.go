// Package ratelimitpool implements a three-pool key rotation system with sliding window rate limiting.
// It manages keys across Active, Cooling, and Standby pools for api.airforce integration.
package ratelimitpool

import "time"

// PoolConfig holds all configurable parameters for the rate-limited key pool.
type PoolConfig struct {
	// ActivePoolSize is the maximum number of keys in the active pool.
	// Keys beyond this count are placed in the standby pool.
	ActivePoolSize int `json:"active_pool_size"`

	// RateLimitCount is the maximum number of requests a single key can serve
	// within the sliding window period before entering the cooling pool.
	RateLimitCount int `json:"rate_limit_count"`

	// RateLimitWindowSeconds is the sliding window duration in seconds.
	// The system looks back this many seconds to count recent requests per key.
	RateLimitWindowSeconds int `json:"rate_limit_window_seconds"`

	// CooldownSeconds is the fixed duration (in seconds) a key stays in the
	// cooling pool after hitting the rate limit.
	CooldownSeconds int `json:"cooldown_seconds"`

	// MaxTotalRequests is the lifetime request cap for a single key.
	// Once a key reaches this count, it is permanently invalidated.
	// 0 means no lifetime limit.
	MaxTotalRequests int64 `json:"max_total_requests"`

	// RequestWaitTimeoutSeconds is the maximum time (in seconds) a request will
	// wait for an available key when all active keys are in cooling.
	// After this timeout, the request receives HTTP 429.
	RequestWaitTimeoutSeconds int `json:"request_wait_timeout_seconds"`

	// StandbyRefillThreshold triggers auto-registration when the standby pool
	// drops below this number of keys.
	StandbyRefillThreshold int `json:"standby_refill_threshold"`

	// AutoRegisterCount is the number of new keys to register per auto-fill cycle.
	AutoRegisterCount int `json:"auto_register_count"`

	// AutoRegisterConcurrency is the number of concurrent registration workers.
	AutoRegisterConcurrency int `json:"auto_register_concurrency"`

	// AutoRegisterReferralCode is the referral code used when registering new accounts.
	AutoRegisterReferralCode string `json:"auto_register_referral_code"`

	// SolverURL is the Turnstile CAPTCHA solver service URL.
	SolverURL string `json:"solver_url"`

	// CoolingPoolMultiplier sets the target cooling pool size as a multiple of ActivePoolSize.
	// For example, with ActivePoolSize=10 and CoolingPoolMultiplier=2, the cooling pool
	// will be pre-filled with 20 reserve keys from standby. These reserves ensure
	// the active pool stays full even when keys are rate-limited.
	// Default: 1 (cooling target = active pool size)
	CoolingPoolMultiplier int `json:"cooling_pool_multiplier"`

	// Enabled controls whether the rate-limited pool is active.
	// When disabled, the system falls back to the standard gpt-load key rotation.
	Enabled bool `json:"enabled"`
}

// DefaultPoolConfig returns sensible default configuration values.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		ActivePoolSize:            10,
		RateLimitCount:            2,
		RateLimitWindowSeconds:    60,
		CooldownSeconds:           60,
		MaxTotalRequests:          10,
		RequestWaitTimeoutSeconds: 30,
		StandbyRefillThreshold:    10,
		AutoRegisterCount:         10,
		AutoRegisterConcurrency:   2,
		AutoRegisterReferralCode:  "",
		SolverURL:                 "http://localhost:5072",
		CoolingPoolMultiplier:     1,
		Enabled:                   false,
	}
}

// RateLimitWindow returns the sliding window duration as time.Duration.
func (c *PoolConfig) RateLimitWindow() time.Duration {
	return time.Duration(c.RateLimitWindowSeconds) * time.Second
}

// CooldownDuration returns the cooldown duration as time.Duration.
func (c *PoolConfig) CooldownDuration() time.Duration {
	return time.Duration(c.CooldownSeconds) * time.Second
}

// RequestWaitTimeout returns the wait timeout as time.Duration.
func (c *PoolConfig) RequestWaitTimeout() time.Duration {
	return time.Duration(c.RequestWaitTimeoutSeconds) * time.Second
}

// CoolingPoolTarget returns the target number of keys in the cooling pool.
func (c *PoolConfig) CoolingPoolTarget() int {
	m := c.CoolingPoolMultiplier
	if m < 1 {
		m = 1
	}
	return c.ActivePoolSize * m
}
