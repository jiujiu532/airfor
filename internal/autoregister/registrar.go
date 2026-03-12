// Package autoregister implements automatic api.airforce account registration.
// It handles Turnstile CAPTCHA solving, account signup, API key retrieval, and model toggling.
package autoregister

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	signupURL            = "https://api.airforce/auth/signup"
	meURL                = "https://api.airforce/api/me"
	siteURL              = "https://panel.api.airforce/signup/"
	turnstileSitekey     = "0x4AAAAAACY9xSVz3RBFYucU"
	modelToggleURLFmt    = "https://api.airforce/api/models/%s/toggle"
	defaultReferralCode  = ""
)

// toggleModels lists models to enable upon registration with a referral code.
var toggleModels = []string{
	"gemini-3-pro",
	"gemini-3.1-pro",
	"gemini-3-flash",
	"gemini-3.1-flash-lite",
	"nano-banana-pro",
	"nano-banana-2",
	"gpt-5.1-chat",
	"gpt-5.2-chat",
	"gpt-5.2-codex",
	"gpt-5.3-codex",
	"gpt-5.3-codex-openai-compact",
	"gpt-5-4-pro",
	"gpt-5.4",
	"gpt-5.2",
	"gpt-5.1",
	"claude-sonnet-4.5-uncensored",
	"claude-opus-4.5-uncensored",
	"claude-opus-4.6-uncensored",
	"claude-sonnet-4.6-uncensored",
}

// RegistrationResult holds the outcome of a single registration attempt.
type RegistrationResult struct {
	Username string `json:"username"`
	Password string `json:"password"`
	APIKey   string `json:"api_key"`
	Error    string `json:"error,omitempty"`
	Source   string `json:"source"` // "manual" or "auto"
}

// RegistrationStats tracks aggregate registration statistics.
type RegistrationStats struct {
	TotalAttempts int32 `json:"total_attempts"`
	Successes     int32 `json:"successes"`
	Failures      int32 `json:"failures"`
	IsRunning     bool  `json:"is_running"`
}

// RegistrarConfig holds configuration for the auto-registrar.
type RegistrarConfig struct {
	SolverURL    string `json:"solver_url"`
	ReferralCode string `json:"referral_code"`
	Count        int    `json:"count"`        // number of accounts to register
	Concurrency  int    `json:"concurrency"`  // concurrent registration workers
	Source       string `json:"source"`       // "manual" or "auto"
}

// LogEntry represents a single registration log line.
type LogEntry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

const maxLogEntries = 500

// Registrar handles automatic api.airforce account registration.
type Registrar struct {
	mu            sync.Mutex
	isRunning     bool
	currentSource string // "manual" or "auto"
	stopCh        chan struct{}

	stats RegistrationStats

	// Callback when a new key is registered successfully
	onKeyRegistered func(result RegistrationResult)

	httpClient *http.Client

	// Log buffer for frontend streaming
	logMu   sync.Mutex
	logBuf  []LogEntry
}

// NewRegistrar creates a new Registrar instance.
func NewRegistrar() *Registrar {
	return &Registrar{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logBuf: make([]LogEntry, 0, maxLogEntries),
	}
}

// addLog appends a log entry to the ring buffer.
func (r *Registrar) addLog(level, msg string) {
	entry := LogEntry{
		Time:    time.Now().Format("15:04:05"),
		Level:   level,
		Message: msg,
	}
	r.logMu.Lock()
	if len(r.logBuf) >= maxLogEntries {
		r.logBuf = r.logBuf[1:]
	}
	r.logBuf = append(r.logBuf, entry)
	r.logMu.Unlock()
}

// logInfo logs an info message to both logrus and the buffer.
func (r *Registrar) logInfo(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	logrus.Info(msg)
	r.addLog("info", msg)
}

// logWarn logs a warning message to both logrus and the buffer.
func (r *Registrar) logWarn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	logrus.Warn(msg)
	r.addLog("warn", msg)
}

// logDebug logs a debug message to both logrus and the buffer.
func (r *Registrar) logDebug(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	logrus.Debug(msg)
	r.addLog("debug", msg)
}

// GetLogs returns all buffered log entries and optionally clears them.
func (r *Registrar) GetLogs(sinceIndex int) ([]LogEntry, int) {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	if sinceIndex >= len(r.logBuf) {
		return nil, len(r.logBuf)
	}
	if sinceIndex < 0 {
		sinceIndex = 0
	}
	entries := make([]LogEntry, len(r.logBuf)-sinceIndex)
	copy(entries, r.logBuf[sinceIndex:])
	return entries, len(r.logBuf)
}

// SetOnKeyRegistered sets the callback fired when a key is successfully registered.
func (r *Registrar) SetOnKeyRegistered(fn func(result RegistrationResult)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onKeyRegistered = fn
}

// IsRunning returns whether a registration task is currently active.
func (r *Registrar) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.isRunning
}

// GetStats returns current registration statistics.
func (r *Registrar) GetStats() RegistrationStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stats
	s.IsRunning = r.isRunning
	return s
}

// StartRegistration begins an asynchronous registration batch.
// Returns immediately; registration runs in background goroutines.
func (r *Registrar) StartRegistration(config RegistrarConfig) error {
	r.mu.Lock()
	if r.isRunning {
		r.mu.Unlock()
		return fmt.Errorf("registration is already running (source: %s)", r.currentSource)
	}
	r.isRunning = true
	r.currentSource = config.Source
	r.stopCh = make(chan struct{})
	r.stats = RegistrationStats{IsRunning: true}
	r.mu.Unlock()

	go r.runRegistration(config)
	return nil
}

// ForceStart stops any running registration and starts a new one.
// Used by manual registration to preempt auto-registration.
func (r *Registrar) ForceStart(config RegistrarConfig) error {
	r.mu.Lock()
	if r.isRunning && r.stopCh != nil {
		r.logInfo("[AutoRegister] 强制停止当前 %s 注册，为 %s 注册让路", r.currentSource, config.Source)
		close(r.stopCh)
		r.isRunning = false
	}
	r.mu.Unlock()

	// Brief pause to let workers stop
	time.Sleep(100 * time.Millisecond)

	return r.StartRegistration(config)
}

// StopRegistration signals the current registration batch to stop.
func (r *Registrar) StopRegistration() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isRunning && r.stopCh != nil {
		close(r.stopCh)
	}
}

// GetSource returns the source of the currently running registration.
func (r *Registrar) GetSource() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentSource
}

// runRegistration executes the registration batch with worker goroutines.
func (r *Registrar) runRegistration(config RegistrarConfig) {
	defer func() {
		r.mu.Lock()
		r.isRunning = false
		r.mu.Unlock()
	}()

	concurrency := config.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	count := config.Count
	if count <= 0 {
		count = 10
	}

	r.logInfo("[AutoRegister] === 批量注册开始 === 总数=%d 并发=%d", count, concurrency)

	var wg sync.WaitGroup
	var totalIndex int32

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		workerID := i
		go func() {
			defer wg.Done()
			for {
				select {
				case <-r.stopCh:
					r.logInfo("[AutoRegister] W%d 收到停止信号", workerID)
					return
				default:
				}

				idx := atomic.AddInt32(&totalIndex, 1)
				if int(idx) > count {
					return
				}

				result := r.registerOne(config, workerID)
				result.Source = config.Source
				atomic.AddInt32(&r.stats.TotalAttempts, 1)

				if result.Error != "" {
					atomic.AddInt32(&r.stats.Failures, 1)
					r.logWarn("[AutoRegister] [%d/%d] ❌ 失败: %s", idx, count, result.Error)
				} else {
					atomic.AddInt32(&r.stats.Successes, 1)
					r.logInfo("[AutoRegister] [%d/%d] ✅ 成功 key=%s", idx, count, maskKey(result.APIKey))

					r.mu.Lock()
					cb := r.onKeyRegistered
					r.mu.Unlock()
					if cb != nil {
						cb(result)
					}
				}
			}
		}()
	}

	wg.Wait()
	r.logInfo("[AutoRegister] === 批量注册完成 === 成功=%d 失败=%d", r.stats.Successes, r.stats.Failures)
}

// registerOne executes a single complete registration flow.
func (r *Registrar) registerOne(config RegistrarConfig, workerID int) RegistrationResult {
	username := generateUsername(10)
	password := generatePassword(14)

	// Use default referral code if not configured
	referralCode := config.ReferralCode
	if referralCode == "" {
		referralCode = defaultReferralCode
	}

	r.logDebug("[AutoRegister] W%d 打码中...", workerID)
	captchaToken, err := r.solveTurnstile(config.SolverURL, siteURL, turnstileSitekey, 120)
	if err != nil {
		return RegistrationResult{Error: fmt.Sprintf("打码失败: %v", err)}
	}

	r.logDebug("[AutoRegister] W%d 注册账号 %s...", workerID, username)
	jwtToken, err := r.signup(username, password, captchaToken, referralCode)
	if err != nil {
		return RegistrationResult{Error: fmt.Sprintf("注册失败: %v", err)}
	}

	enabled := r.toggleModels(jwtToken, workerID)
	r.logDebug("[AutoRegister] W%d 启用模型 %d/%d", workerID, enabled, len(toggleModels))

	apiKey, err := r.getAPIKey(jwtToken)
	if err != nil {
		return RegistrationResult{
			Username: username,
			Password: password,
			Error:    fmt.Sprintf("获取Key失败: %v", err),
		}
	}

	return RegistrationResult{
		Username: username,
		Password: password,
		APIKey:   apiKey,
	}
}

// solveTurnstile calls the local Turnstile solver service.
func (r *Registrar) solveTurnstile(solverURL, siteurl, sitekey string, timeout int) (string, error) {
	if solverURL == "" {
		solverURL = "http://localhost:5072"
	}

	params := url.Values{
		"url":     {siteurl},
		"sitekey": {sitekey},
	}

	// Create task
	resp, err := r.httpClient.Get(fmt.Sprintf("%s/turnstile?%s", solverURL, params.Encode()))
	if err != nil {
		return "", fmt.Errorf("solver request failed: %w", err)
	}
	defer resp.Body.Close()

	var taskResult map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&taskResult); err != nil {
		return "", fmt.Errorf("solver response decode failed: %w", err)
	}

	taskID := ""
	if id, ok := taskResult["task_id"].(string); ok {
		taskID = id
	} else if id, ok := taskResult["taskId"].(string); ok {
		taskID = id
	}
	if taskID == "" {
		return "", fmt.Errorf("no task_id in solver response: %v", taskResult)
	}

	// Poll for result
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-r.stopCh:
			return "", fmt.Errorf("registration stopped")
		default:
		}

		time.Sleep(3 * time.Second)

		resp, err := r.httpClient.Get(fmt.Sprintf("%s/result?id=%s", solverURL, taskID))
		if err != nil {
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		raw := strings.TrimSpace(string(body))
		if raw == "" || raw == "CAPTCHA_NOT_READY" {
			continue
		}

		var result map[string]interface{}
		if err := json.Unmarshal(body, &result); err != nil {
			continue
		}

		// Parse token from various response formats
		token := extractToken(result)
		if token != "" {
			failMarkers := []string{"CAPTCHA_FAIL", "FAIL", "ERROR", "TIMEOUT"}
			upper := strings.ToUpper(token)
			for _, m := range failMarkers {
				if upper == m {
					return "", fmt.Errorf("solver returned failure: %s", token)
				}
			}
			if !strings.HasPrefix(token, "0.") {
				return "", fmt.Errorf("invalid token format: %s", token[:min(50, len(token))])
			}
			return token, nil
		}
	}

	return "", fmt.Errorf("solver timeout (%ds)", timeout)
}

// signup registers a new account and returns the JWT token.
func (r *Registrar) signup(username, password, captchaToken, referralCode string) (string, error) {
	payload := map[string]string{
		"username":      username,
		"password":      password,
		"captcha_token": captchaToken,
	}
	if referralCode != "" {
		payload["referral_code"] = referralCode
	}

	jsonBody, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", signupURL, strings.NewReader(string(jsonBody)))
	if err != nil {
		return "", err
	}

	r.setHeaders(req, "", referralCode)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("signup HTTP %d: %s", resp.StatusCode, string(body)[:min(300, len(body))])
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	token, ok := result["token"].(string)
	if !ok || token == "" {
		return "", fmt.Errorf("no token in signup response")
	}
	return token, nil
}

// getAPIKey retrieves the API key using a JWT token.
func (r *Registrar) getAPIKey(jwtToken string) (string, error) {
	req, err := http.NewRequest("GET", meURL, nil)
	if err != nil {
		return "", err
	}
	r.setHeaders(req, jwtToken, "")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("me HTTP %d: %s", resp.StatusCode, string(body)[:min(300, len(body))])
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	apiKey, ok := result["api_key"].(string)
	if !ok || apiKey == "" {
		return "", fmt.Errorf("no api_key in me response")
	}
	return apiKey, nil
}

// toggleModels enables all configured models for the account. Returns count of successfully enabled models.
func (r *Registrar) toggleModels(jwtToken string, workerID int) int {
	enabled := 0
	for _, model := range toggleModels {
		if r.toggleModel(jwtToken, model) {
			enabled++
		}
	}
	return enabled
}

// toggleModel enables a single model. Returns true if successful.
func (r *Registrar) toggleModel(jwtToken, modelName string) bool {
	toggleURL := fmt.Sprintf(modelToggleURLFmt, modelName)
	req, err := http.NewRequest("POST", toggleURL, nil)
	if err != nil {
		return false
	}

	req.Header.Set("accept", "*/*")
	req.Header.Set("authorization", "Bearer "+jwtToken)
	req.Header.Set("content-length", "0")
	req.Header.Set("origin", "https://api.airforce")
	req.Header.Set("referer", "https://api.airforce/dashboard/")
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err == nil {
		if on, ok := result["enabled"].(bool); ok && on {
			return true
		}
	}
	return false
}

// setHeaders sets common request headers for api.airforce requests.
func (r *Registrar) setHeaders(req *http.Request, jwtToken, referralCode string) {
	referer := "https://panel.api.airforce/signup/"
	if referralCode != "" {
		referer = fmt.Sprintf("https://panel.api.airforce/signup/?ref=%s", referralCode)
	}

	req.Header.Set("accept", "*/*")
	req.Header.Set("accept-language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("origin", "https://panel.api.airforce")
	req.Header.Set("referer", referer)
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36")

	if jwtToken != "" {
		req.Header.Set("authorization", "Bearer "+jwtToken)
	}
}

// --- Utility functions ---

const letterBytes = "abcdefghijklmnopqrstuvwxyz"
const alphanumBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func generateUsername(length int) string {
	b := make([]byte, length)
	b[0] = letterBytes[rand.Intn(len(letterBytes))]
	for i := 1; i < length; i++ {
		b[i] = alphanumBytes[rand.Intn(len(alphanumBytes))]
	}
	return string(b)
}

func generatePassword(length int) string {
	b := make([]byte, length)
	for i := range b {
		b[i] = alphanumBytes[rand.Intn(len(alphanumBytes))]
	}
	return string(b)
}

func extractToken(result map[string]interface{}) string {
	// Format 1: {"solution": {"token": "0.xxx"}}
	if solution, ok := result["solution"].(map[string]interface{}); ok {
		if token, ok := solution["token"].(string); ok && token != "" {
			return token
		}
		if token, ok := solution["value"].(string); ok && token != "" {
			return token
		}
	}
	// Format 2: {"value": "0.xxx"} or {"token": "0.xxx"}
	if token, ok := result["value"].(string); ok && token != "" {
		return token
	}
	if token, ok := result["token"].(string); ok && token != "" {
		return token
	}
	return ""
}

func maskKey(key string) string {
	if len(key) <= 20 {
		return key[:min(10, len(key))] + "..."
	}
	return key[:10] + "..." + key[len(key)-6:]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
