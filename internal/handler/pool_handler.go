package handler

import (
	"fmt"
	"net/http"

	"gpt-load/internal/autoregister"
	"gpt-load/internal/ratelimitpool"
	"gpt-load/internal/response"

	"github.com/gin-gonic/gin"
)

// GetPoolStats returns the current rate-limited pool statistics.
func (s *Server) GetPoolStats(c *gin.Context) {
	if s.PoolService == nil {
		response.Success(c, gin.H{
			"enabled": false,
			"message": "Rate-limited pool is not initialized",
		})
		return
	}

	stats := s.PoolService.Stats()
	stats["enabled"] = s.PoolService.IsEnabled()
	response.Success(c, stats)
}

// GetPoolConfig returns the current pool configuration.
func (s *Server) GetPoolConfig(c *gin.Context) {
	if s.PoolService == nil {
		response.Success(c, ratelimitpool.DefaultPoolConfig())
		return
	}

	pool := s.PoolService.GetPool()
	if pool != nil {
		response.Success(c, pool.GetConfig())
		return
	}

	// Pool not running, load config from DB via service
	response.Success(c, s.PoolService.GetConfigFromDB())
}

// UpdatePoolConfig updates the rate-limited pool configuration.
func (s *Server) UpdatePoolConfig(c *gin.Context) {
	var config ratelimitpool.PoolConfig
	if err := c.ShouldBindJSON(&config); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid configuration: " + err.Error()})
		return
	}

	// Validate
	if config.ActivePoolSize <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "active_pool_size must be > 0"})
		return
	}
	if config.RateLimitCount <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "rate_limit_count must be > 0"})
		return
	}
	if config.RateLimitWindowSeconds <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "rate_limit_window_seconds must be > 0"})
		return
	}
	if config.CooldownSeconds < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cooldown_seconds must be >= 0"})
		return
	}

	if s.PoolService == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Pool service not initialized"})
		return
	}

	if err := s.PoolService.UpdateConfig(config); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	response.Success(c, gin.H{"message": "Configuration updated"})
}

// StartRegistration triggers a manual registration batch.
func (s *Server) StartRegistration(c *gin.Context) {
	var req struct {
		Count       int `json:"count" binding:"required"`
		Concurrency int `json:"concurrency"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	if req.Concurrency <= 0 {
		req.Concurrency = 2
	}

	if s.PoolService == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Pool service not initialized"})
		return
	}

	if err := s.PoolService.StartManualRegistration(req.Count, req.Concurrency); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}

	response.Success(c, gin.H{"message": "Registration started"})
}

// StopRegistration stops the currently running registration batch.
func (s *Server) StopRegistration(c *gin.Context) {
	if s.PoolService == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Pool service not initialized"})
		return
	}

	s.PoolService.GetRegistrar().StopRegistration()
	response.Success(c, gin.H{"message": "Registration stop signal sent"})
}

// GetRegistrationStats returns auto-registration statistics.
func (s *Server) GetRegistrationStats(c *gin.Context) {
	if s.PoolService == nil {
		response.Success(c, gin.H{"is_running": false})
		return
	}

	response.Success(c, s.PoolService.GetRegistrar().GetStats())
}

// GetRegistrationLogs returns buffered registration log entries.
func (s *Server) GetRegistrationLogs(c *gin.Context) {
	if s.PoolService == nil {
		response.Success(c, gin.H{"logs": []any{}, "next_index": 0})
		return
	}

	sinceIndex := 0
	if v := c.Query("since"); v != "" {
		fmt.Sscanf(v, "%d", &sinceIndex)
	}

	logs, nextIndex := s.PoolService.GetRegistrar().GetLogs(sinceIndex)
	if logs == nil {
		logs = []autoregister.LogEntry{}
	}
	response.Success(c, gin.H{"logs": logs, "next_index": nextIndex})
}
