// Package ratelimitpool provides the PoolService that integrates the rate-limited pool
// with gpt-load's database, key provider, and auto-registration system.
package ratelimitpool

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"gpt-load/internal/autoregister"
	"gpt-load/internal/encryption"
	"gpt-load/internal/models"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// PoolService bridges the rate-limited pool engine with gpt-load's data layer.
type PoolService struct {
	db            *gorm.DB
	encryptionSvc encryption.Service
	pool          *Pool
	registrar     *autoregister.Registrar

	mu          sync.Mutex
	registering bool // prevents concurrent auto-register triggers
}

// NewPoolService creates a new PoolService.
func NewPoolService(db *gorm.DB, encryptionSvc encryption.Service) *PoolService {
	return &PoolService{
		db:            db,
		encryptionSvc: encryptionSvc,
		registrar:     autoregister.NewRegistrar(),
	}
}

// Initialize loads configuration from the database and starts the pool.
func (s *PoolService) Initialize() error {
	// Always set up auto-registration callback so keys are saved to DB
	// regardless of whether the pool engine is enabled
	s.registrar.SetOnKeyRegistered(func(result autoregister.RegistrationResult) {
		s.onNewKeyRegistered(result)
	})

	config := s.loadConfigFromDB()

	if !config.Enabled {
		logrus.Info("[PoolService] Rate-limited pool is disabled (registration callback still active)")
		return nil
	}

	s.pool = NewPool(config)

	// Set up standby-low callback
	s.pool.SetOnStandbyLow(func(currentCount int) {
		s.triggerAutoRegister(currentCount)
	})

	// Load existing keys from database
	if err := s.loadKeysFromDB(); err != nil {
		return fmt.Errorf("failed to load keys into rate-limited pool: %w", err)
	}

	s.pool.Start()
	logrus.Info("[PoolService] Rate-limited pool initialized and started")

	return nil
}

// Stop gracefully stops the pool and registrar.
func (s *PoolService) Stop(ctx context.Context) {
	if s.pool != nil {
		s.pool.Stop()
	}
	if s.registrar != nil {
		s.registrar.StopRegistration()
	}
	logrus.Info("[PoolService] Stopped")
}

// IsEnabled returns whether the rate-limited pool is active.
func (s *PoolService) IsEnabled() bool {
	return s.pool != nil
}

// ReloadKeys reloads all keys from the database into the pool.
// Call this after manually adding/removing/restoring keys so the pool stays in sync.
func (s *PoolService) ReloadKeys() {
	if s.pool == nil {
		return
	}
	if err := s.loadKeysFromDB(); err != nil {
		logrus.WithError(err).Error("[PoolService] Failed to reload keys")
	} else {
		logrus.Info("[PoolService] 密钥池已重新加载")
	}
}

// GetPool returns the underlying pool (can be nil if disabled).
func (s *PoolService) GetPool() *Pool {
	return s.pool
}

// GetRegistrar returns the auto-registrar.
func (s *PoolService) GetRegistrar() *autoregister.Registrar {
	return s.registrar
}

// AcquireKey selects a key from the rate-limited pool.
// Returns the key value (decrypted) and key ID, or an error.
func (s *PoolService) AcquireKey(groupID uint) (keyValue string, keyID uint, err error) {
	if s.pool == nil {
		return "", 0, fmt.Errorf("rate-limited pool is not enabled")
	}

	slot, err := s.pool.AcquireKey()
	if err != nil {
		return "", 0, err
	}

	return slot.KeyValue, slot.KeyID, nil
}

// ReportKeyResult reports the outcome of a request that used a key.
// Call this after the upstream request completes.
func (s *PoolService) ReportKeyResult(keyID uint, success bool, statusCode int) {
	if s.pool == nil {
		return
	}

	// For permanent failures (401, 403), mark the key as invalid
	markInvalid := !success && (statusCode == 401 || statusCode == 403)

	// Find the slot by keyID
	s.pool.mu.Lock()
	var targetSlot *KeySlot

	for _, slot := range s.pool.activeKeys {
		if slot.KeyID == keyID {
			targetSlot = slot
			break
		}
	}
	if targetSlot == nil {
		for _, slot := range s.pool.coolingKeys {
			if slot.KeyID == keyID {
				targetSlot = slot
				break
			}
		}
	}
	s.pool.mu.Unlock()

	if targetSlot != nil && markInvalid {
		s.pool.ReleaseKey(targetSlot, true)
		// Also update the database
		go s.markKeyInvalidInDB(keyID)
	}
}

// Stats returns pool and registrar statistics.
func (s *PoolService) Stats() map[string]interface{} {
	result := make(map[string]interface{})

	if s.pool != nil {
		result["pool"] = s.pool.Stats()
		result["config"] = s.pool.GetConfig()
	}
	if s.registrar != nil {
		result["registrar"] = s.registrar.GetStats()
	}

	return result
}

// GetConfigFromDB loads and returns the pool configuration from the database.
func (s *PoolService) GetConfigFromDB() PoolConfig {
	return s.loadConfigFromDB()
}

// UpdateConfig updates the pool configuration and persists to database.
func (s *PoolService) UpdateConfig(config PoolConfig) error {
	// Persist to database
	configJSON, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	setting := models.SystemSetting{
		SettingKey:   "rate_limit_pool_config",
		SettingValue: string(configJSON),
		Description:  "Rate-limited pool configuration for api.airforce key rotation",
	}

	result := s.db.Where("setting_key = ?", "rate_limit_pool_config").
		Assign(models.SystemSetting{
			SettingValue: setting.SettingValue,
			Description:  setting.Description,
		}).
		FirstOrCreate(&setting)
	if result.Error != nil {
		return fmt.Errorf("failed to save config to DB: %w", result.Error)
	}

	// Hot-reload if pool is running
	if s.pool != nil {
		s.pool.UpdateConfig(config)
	}

	return nil
}

// StartManualRegistration triggers a manual registration batch.
// Manual has highest priority and will preempt any running auto-registration.
func (s *PoolService) StartManualRegistration(count, concurrency int) error {
	config := s.loadConfigFromDB()

	return s.registrar.ForceStart(autoregister.RegistrarConfig{
		SolverURL:    config.SolverURL,
		ReferralCode: config.AutoRegisterReferralCode,
		Count:        count,
		Concurrency:  concurrency,
		Source:       "manual",
	})
}

// --- Internal methods ---

// loadConfigFromDB loads pool configuration from the system_settings table.
func (s *PoolService) loadConfigFromDB() PoolConfig {
	config := DefaultPoolConfig()

	var setting models.SystemSetting
	result := s.db.Where("setting_key = ?", "rate_limit_pool_config").First(&setting)
	if result.Error != nil {
		return config
	}

	if err := json.Unmarshal([]byte(setting.SettingValue), &config); err != nil {
		logrus.WithError(err).Warn("[PoolService] Failed to parse pool config from DB, using defaults")
		return DefaultPoolConfig()
	}

	return config
}

// loadKeysFromDB loads all active API keys into the pool, deduplicated by actual key value.
// Since airforce rate-limits by physical key, the same key across multiple groups counts as ONE key.
func (s *PoolService) loadKeysFromDB() error {
	var apiKeys []models.APIKey
	if err := s.db.Where("status = ?", models.KeyStatusActive).Find(&apiKeys).Error; err != nil {
		return fmt.Errorf("failed to query active keys: %w", err)
	}

	if len(apiKeys) == 0 {
		logrus.Warn("[PoolService] No active keys found in database")
		return nil
	}

	// Deduplicate by key_hash — same physical key only enters pool once
	seen := make(map[string]bool)
	slots := make([]KeySlot, 0)
	for _, key := range apiKeys {
		hash := key.KeyHash
		if hash == "" {
			hash = key.KeyValue // fallback for old records without hash
		}
		if seen[hash] {
			continue // skip duplicate
		}
		seen[hash] = true

		// Decrypt the key value
		decrypted, err := s.encryptionSvc.Decrypt(key.KeyValue)
		if err != nil {
			decrypted = key.KeyValue
		}

		slots = append(slots, KeySlot{
			KeyID:         key.ID,
			KeyValue:      decrypted,
			GroupID:       key.GroupID,
			TotalRequests: key.RequestCount,
		})
	}

	logrus.Infof("[PoolService] 加载密钥: DB记录=%d, 去重后唯一key=%d", len(apiKeys), len(slots))
	s.pool.LoadKeys(slots)
	return nil
}

// onNewKeyRegistered is called when auto-registration produces a new key.
func (s *PoolService) onNewKeyRegistered(result autoregister.RegistrationResult) {
	if result.APIKey == "" {
		return
	}

	// Find ALL standard (non-aggregate) groups
	var groups []models.Group
	if err := s.db.Where("group_type != ? OR group_type IS NULL", "aggregate").
		Find(&groups).Error; err != nil {
		logrus.WithError(err).Error("[PoolService] Cannot find target groups for new key")
		return
	}

	if len(groups) == 0 {
		logrus.Warn("[PoolService] No standard groups found, cannot add new key")
		return
	}

	// Encrypt the key once
	encryptedKey, err := s.encryptionSvc.Encrypt(result.APIKey)
	if err != nil {
		logrus.WithError(err).Error("[PoolService] Failed to encrypt new API key")
		encryptedKey = result.APIKey
	}
	keyHash := s.encryptionSvc.Hash(result.APIKey)

	// Add the key to every standard group in DB
	addedCount := 0
	var firstKeyID uint
	for _, group := range groups {
		apiKey := models.APIKey{
			KeyValue: encryptedKey,
			KeyHash:  keyHash,
			GroupID:  group.ID,
			Status:   models.KeyStatusActive,
			Notes:    fmt.Sprintf("auto-registered: %s", result.Username),
		}

		if err := s.db.Create(&apiKey).Error; err != nil {
			logrus.WithError(err).Errorf("[PoolService] Failed to save key to group %d (%s)", group.ID, group.Name)
			continue
		}

		// Record the first DB record ID for pool entry
		if addedCount == 0 {
			firstKeyID = apiKey.ID
		}
		addedCount++
	}

	// Add to the rate-limited pool engine ONCE (same physical key = 1 pool slot)
	if s.pool != nil && addedCount > 0 {
		slot := KeySlot{
			KeyID:    firstKeyID,
			KeyValue: result.APIKey,
			GroupID:  groups[0].ID,
		}
		if result.Source == "manual" {
			// Manual registration: key goes to cooling pool (0 cooldown) → active on next tick
			s.pool.AddKeyToCooling(slot)
			logrus.Infof("[PoolService] 新key已注册 [手动]: user=%s, 写入%d个分组, 入冷却池", result.Username, addedCount)
		} else {
			// Auto registration: key goes to standby pool for reserve
			s.pool.AddKey(slot)
			logrus.Infof("[PoolService] 新key已注册 [自动]: user=%s, 写入%d个分组, 入备用池", result.Username, addedCount)
		}
	}
}

// triggerAutoRegister starts auto-registration when standby pool is low.
func (s *PoolService) triggerAutoRegister(currentStandbyCount int) {
	s.mu.Lock()
	if s.registering {
		s.mu.Unlock()
		return
	}
	s.registering = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.registering = false
		s.mu.Unlock()
	}()

	config := s.loadConfigFromDB()
	if config.AutoRegisterCount <= 0 || config.SolverURL == "" {
		logrus.Warn("[PoolService] Auto-register triggered but not configured")
		return
	}

	logrus.WithFields(logrus.Fields{
		"standby_count": currentStandbyCount,
		"threshold":     config.StandbyRefillThreshold,
		"register_count": config.AutoRegisterCount,
	}).Info("[PoolService] Standby pool low, triggering auto-registration")

	err := s.registrar.StartRegistration(autoregister.RegistrarConfig{
		SolverURL:    config.SolverURL,
		ReferralCode: config.AutoRegisterReferralCode,
		Count:        config.AutoRegisterCount,
		Concurrency:  config.AutoRegisterConcurrency,
		Source:       "auto",
	})
	if err != nil {
		logrus.WithError(err).Warn("[PoolService] Failed to start auto-registration")
	}
}

// markKeyInvalidInDB updates the key status to invalid in the database.
func (s *PoolService) markKeyInvalidInDB(keyID uint) {
	if err := s.db.Model(&models.APIKey{}).Where("id = ?", keyID).
		Updates(map[string]interface{}{
			"status": models.KeyStatusInvalid,
		}).Error; err != nil {
		logrus.WithError(err).Errorf("[PoolService] Failed to mark key %d as invalid in DB", keyID)
	}
}

