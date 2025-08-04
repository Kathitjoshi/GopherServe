// Enhanced Production-Ready Go Web Server - Complete Implementation
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug" // Added for debug.Stack()
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//-----------------------------------------------------------------------------
// Configuration & Data Structures
//-----------------------------------------------------------------------------

// Config holds all configurable parameters for the web server.
// Values can be set via environment variables for cloud-native deployment.
type Config struct {
	Port           string        // Port for the server to listen on (default: "8080")
	Host           string        // Host interface to bind to (e.g., "0.0.0.0" for all, "localhost")
	ReadTimeout    time.Duration // Maximum duration for reading the entire request
	WriteTimeout   time.Duration // Maximum duration before timing out writes of the response
	IdleTimeout    time.Duration // Maximum amount of time to wait for the next request when keep-alives are enabled
	MaxHeaderBytes int           // Maximum size of request headers
	EnableTLS      bool          // Flag to enable HTTPS/TLS
	CertFile       string        // Path to TLS certificate file (e.g., server.crt)
	KeyFile        string        // Path to TLS private key file (e.g., server.key)

	EnableRateLimit bool // Flag to enable per-IP request rate limiting
	RateLimit       int  // Max requests per minute per IP if rate limiting is enabled

	StaticDir   string // Directory for serving static files (CSS, JS, images)
	TemplateDir string // Directory containing HTML templates

	LogFile        string   // Path to a file for server logs (empty for stdout only)
	EnableMetrics  bool     // Flag to enable the /metrics endpoint and internal metrics collection
	EnableCORS     bool     // Flag to enable Cross-Origin Resource Sharing (CORS) headers
	TrustedProxies []string // List of trusted proxy IPs for correct client IP detection
}

// Server represents the main web server application instance.
type Server struct {
	config      *Config
	templates   *template.Template
	logger      *log.Logger
	rateLimiter *RateLimiter
	metrics     *Metrics
	mu          sync.RWMutex // Mutex for server-level state (though rarely needed for this design)
	startTime   time.Time    // Time when the server started
}

// Metrics tracks various server statistics and performance indicators.
type Metrics struct {
	RequestCount        int64                    `json:"request_count"`         // Total number of requests served
	ErrorCount          int64                    `json:"error_count"`           // Total number of errors encountered
	StartTime           time.Time                `json:"start_time"`            // Server start time
	Uptime              string                   `json:"uptime"`                // Formatted server uptime
	EndpointStats       map[string]*EndpointStat `json:"endpoint_stats"`        // Statistics per API endpoint
	ResponseTimes       []time.Duration          `json:"-"`                     // Recent response times (not marshaled to JSON)
	ActiveUsers         int64                    `json:"active_users"`          // Placeholder for tracking active users (requires more logic)
	MemoryUsage         uint64                   `json:"memory_usage"`          // Current memory allocated by the Go process
	CPUUsage            float64                  `json:"cpu_usage"`             // Placeholder for CPU usage (requires more advanced monitoring)
	AverageResponseTime string                   `json:"average_response_time"` // Calculated average response time for JSON output
	mu                  sync.RWMutex             // Mutex to protect concurrent access to metrics
}

// EndpointStat tracks statistics for a specific API endpoint.
type EndpointStat struct {
	Count       int64         `json:"count"`        // Number of times this endpoint was accessed
	TotalTime   time.Duration `json:"total_time"`   // Sum of all request durations for this endpoint
	AverageTime time.Duration `json:"average_time"` // Average request duration for this endpoint
	LastAccess  time.Time     `json:"last_access"`  // Last time this endpoint was accessed
	ErrorCount  int64         `json:"error_count"`  // Number of errors for this endpoint
	StatusCodes map[int]int64 `json:"status_codes"` // Count of each HTTP status code returned
}

// RateLimiter implements a token bucket algorithm for rate limiting client requests.
type RateLimiter struct {
	clients map[string]*ClientBucket // Map of client IP to their token bucket
	mu      sync.RWMutex             // Mutex for accessing the clients map
	rate    int                      // Max tokens (requests) per minute
}

// ClientBucket represents a single client's token bucket for rate limiting.
type ClientBucket struct {
	tokens     int        // Current number of available tokens
	lastRefill time.Time  // Last time the bucket was refilled
	mu         sync.Mutex // Mutex to protect this specific bucket
}

// APIResponse is a standardized structure for JSON API responses.
type APIResponse struct {
	Success   bool        `json:"success"`         // Indicates if the request was successful
	Data      interface{} `json:"data,omitempty"`  // Payload data on success
	Error     string      `json:"error,omitempty"` // Error message on failure
	Meta      interface{} `json:"meta,omitempty"`  // Optional metadata
	Timestamp time.Time   `json:"timestamp"`       // Server timestamp of the response
	Version   string      `json:"version"`         // API version
}

// TemplateData holds dynamic data passed to HTML templates for rendering.
type TemplateData struct {
	Title      string                 // Main title for the page
	Subtitle   string                 // Subtitle/description
	Message    string                 // A general message to display
	Content    string                 // Main content text/HTML
	Data       map[string]interface{} // Generic data map for displaying key-value pairs
	ServerTime string                 // Formatted current server time
	GoVersion  string                 // Go runtime version
	ServerInfo map[string]interface{} // Server specific information
	Metrics    *Metrics               // Full metrics object for metrics page
	Error      string                 // Error message to display on the page
}

// responseWriter is a wrapper around http.ResponseWriter to capture the HTTP status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int // Stores the status code written by the handler
}

// WriteHeader captures the status code before calling the underlying WriteHeader.
func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

//-----------------------------------------------------------------------------
// Configuration Initialization & Helpers
//-----------------------------------------------------------------------------

// NewConfig creates a new server configuration with default values,
// allowing overrides via environment variables.
func NewConfig() *Config {
	return &Config{
		Port:            getEnv("PORT", "8080"),
		Host:            getEnv("HOST", "0.0.0.0"), // Default to 0.0.0.0 to bind to all interfaces
		ReadTimeout:     30 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     120 * time.Second,
		MaxHeaderBytes:  1 << 20, // 1 MB
		EnableTLS:       getEnvBool("ENABLE_TLS", false),
		CertFile:        getEnv("CERT_FILE", "server.crt"),
		KeyFile:         getEnv("KEY_FILE", "server.key"),
		EnableRateLimit: getEnvBool("ENABLE_RATE_LIMIT", true), // Sensible default
		RateLimit:       getEnvInt("RATE_LIMIT", 100),          // 100 requests per minute
		StaticDir:       getEnv("STATIC_DIR", "./static"),
		TemplateDir:     getEnv("TEMPLATE_DIR", "./templates"),
		LogFile:         getEnv("LOG_FILE", ""), // Empty string means log to stdout only
		EnableMetrics:   getEnvBool("ENABLE_METRICS", true),
		EnableCORS:      getEnvBool("ENABLE_CORS", true),
		TrustedProxies:  strings.Split(getEnv("TRUSTED_PROXIES", "127.0.0.1,::1"), ","),
	}
}

// getEnv retrieves an environment variable, returning a defaultValue if not found.
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvInt retrieves an environment variable as an integer, returning a defaultValue if not found or invalid.
func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}

// getEnvBool retrieves an environment variable as a boolean, returning a defaultValue if not found or invalid.
func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolValue, err := strconv.ParseBool(value); err == nil {
			return boolValue
		}
	}
	return defaultValue
}

// formatBytes formats a byte count into a human-readable string (e.g., "1.2 MB").
func formatBytes(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

//-----------------------------------------------------------------------------
// Rate Limiter Implementation
//-----------------------------------------------------------------------------

// NewRateLimiter creates and initializes a new RateLimiter instance.
func NewRateLimiter(rate int) *RateLimiter {
	return &RateLimiter{
		clients: make(map[string]*ClientBucket),
		rate:    rate, // Rate in requests per minute
	}
}

// Allow checks if a client's request is allowed based on the token bucket algorithm.
// It refills tokens periodically and consumes one token per allowed request.
func (rl *RateLimiter) Allow(clientIP string) bool {
	rl.mu.RLock()
	bucket, exists := rl.clients[clientIP]
	rl.mu.RUnlock()

	if !exists {
		// If bucket doesn't exist, create it. Double-check after acquiring write lock.
		rl.mu.Lock()
		defer rl.mu.Unlock() // Ensure unlock if creating a new bucket
		if _, exists = rl.clients[clientIP]; !exists {
			bucket = &ClientBucket{
				tokens:     rl.rate, // Initialize with full tokens
				lastRefill: time.Now(),
			}
			rl.clients[clientIP] = bucket
		}
	}

	bucket.mu.Lock()
	defer bucket.mu.Unlock()

	// Refill tokens: The rate is per minute, so calculate tokens per second and refill based on elapsed time.
	// For simplicity, we assume refill happens instantly per second.
	now := time.Now()
	elapsedSeconds := now.Sub(bucket.lastRefill).Seconds()
	// Tokens to add: (rate per minute / 60 seconds) * elapsed seconds
	tokensToAdd := int(elapsedSeconds * float64(rl.rate) / 60.0)

	if tokensToAdd > 0 {
		bucket.tokens += tokensToAdd
		if bucket.tokens > rl.rate { // Cap tokens at max rate
			bucket.tokens = rl.rate
		}
		bucket.lastRefill = now // Update last refill time
	}

	// Consume a token if available
	if bucket.tokens > 0 {
		bucket.tokens--
		return true
	}

	return false // No tokens left, request is denied
}

//-----------------------------------------------------------------------------
// Metrics Implementation
//-----------------------------------------------------------------------------

// NewMetrics creates and initializes a new Metrics tracker.
// It also starts a background goroutine to periodically update system metrics.
func NewMetrics() *Metrics {
	m := &Metrics{
		StartTime:     time.Now(),
		EndpointStats: make(map[string]*EndpointStat),
		ResponseTimes: make([]time.Duration, 0, 1000), // Pre-allocate capacity for efficiency
	}
	// Start a background goroutine to periodically update system metrics
	go func() {
		for {
			m.updateSystemMetrics()
			time.Sleep(5 * time.Second) // Update every 5 seconds
		}
	}()
	return m
}

// RecordRequest updates metrics for a single incoming HTTP request.
func (m *Metrics) RecordRequest(endpoint string, duration time.Duration, statusCode int) {
	m.mu.Lock() // Acquire write lock for all metrics updates
	defer m.mu.Unlock()

	m.RequestCount++
	// Uptime is calculated on demand for display, but can be updated here too
	m.Uptime = time.Since(m.StartTime).Truncate(time.Second).String()

	// Add response time to a circular buffer of last 1000 response times
	m.ResponseTimes = append(m.ResponseTimes, duration)
	if len(m.ResponseTimes) > 1000 {
		m.ResponseTimes = m.ResponseTimes[1:] // Drop the oldest entry
	}

	// Update endpoint-specific statistics
	stat, exists := m.EndpointStats[endpoint]
	if !exists {
		stat = &EndpointStat{
			StatusCodes: make(map[int]int64),
		}
		m.EndpointStats[endpoint] = stat
	}

	stat.Count++
	stat.TotalTime += duration
	// Calculate average time; handle division by zero for first request
	if stat.Count > 0 {
		stat.AverageTime = stat.TotalTime / time.Duration(stat.Count)
	}
	stat.LastAccess = time.Now()
	stat.StatusCodes[statusCode]++

	// Increment error count for status codes 400 or higher
	if statusCode >= 400 {
		stat.ErrorCount++
		m.ErrorCount++
	}
}

// updateSystemMetrics gathers runtime memory statistics.
// CPU usage requires more complex OS-specific calls or libraries.
func (m *Metrics) updateSystemMetrics() {
	m.mu.Lock()
	defer m.mu.Unlock()

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	m.MemoryUsage = memStats.Alloc // Bytes allocated and still in use

	// Update uptime here as well, although it's also done in RecordRequest
	m.Uptime = time.Since(m.StartTime).Truncate(time.Second).String()

	// CPUUsage is currently not implemented due to platform-specific complexities.
	// For production, consider using external libraries like "github.com/shirou/gopsutil".
	m.CPUUsage = 0.0 // Placeholder
}

// GetAverageResponseTime calculates the average response time from the collected samples.
func (m *Metrics) GetAverageResponseTime() time.Duration {
	m.mu.RLock() // Acquire read lock
	defer m.mu.RUnlock()

	if len(m.ResponseTimes) == 0 {
		return 0
	}

	var total time.Duration
	for _, rt := range m.ResponseTimes {
		total += rt
	}
	return total / time.Duration(len(m.ResponseTimes))
}

//-----------------------------------------------------------------------------
// Server Initialization
//-----------------------------------------------------------------------------

// NewServer creates and initializes a new Server instance.
// It sets up logging, rate limiting, and loads HTML templates.
func NewServer(config *Config) (*Server, error) {
	server := &Server{
		config:    config,
		metrics:   NewMetrics(),
		startTime: time.Now(),
	}

	// 1. Setup Logger: Determines where logs are written (stdout or file).
	var logOutput io.Writer = os.Stdout
	if config.LogFile != "" {
		logFile, err := os.OpenFile(config.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return nil, fmt.Errorf("failed to open log file %q: %w", config.LogFile, err)
		}
		// Use a multi-writer to output to both console and file
		logOutput = io.MultiWriter(os.Stdout, logFile)
	}
	// Configure the logger with a prefix and standard flags (date, time, file:line)
	server.logger = log.New(logOutput, "[SERVER] ", log.LstdFlags|log.Lshortfile)

	// 2. Setup Rate Limiter: Only if enabled in configuration.
	if config.EnableRateLimit {
		server.rateLimiter = NewRateLimiter(config.RateLimit)
		server.logger.Printf("Rate limiting enabled: %d requests/minute per IP", config.RateLimit)
	} else {
		server.logger.Println("Rate limiting disabled.")
	}

	// 3. Load Templates: Parse HTML templates from the specified directory.
	if err := server.loadTemplates(); err != nil {
		return nil, fmt.Errorf("failed to load templates: %w", err)
	}

	// 4. Create Static Directory: Ensure the static assets directory exists.
	if err := os.MkdirAll(config.StaticDir, 0755); err != nil {
		server.logger.Printf("Warning: failed to create static directory %q: %v", config.StaticDir, err)
	}

	server.logger.Println("✅ Server initialized successfully.")
	return server, nil
}

// loadTemplates loads HTML templates from the configured directory.
// It also creates a default template if none exists and adds custom functions.
func (s *Server) loadTemplates() error {
	// Ensure the template directory exists
	if err := os.MkdirAll(s.config.TemplateDir, 0755); err != nil {
		return fmt.Errorf("failed to create template directory %q: %w", s.config.TemplateDir, err)
	}

	// Check for a default template; create if missing
	defaultTemplatePath := filepath.Join(s.config.TemplateDir, "default.html")
	if _, err := os.Stat(defaultTemplatePath); os.IsNotExist(err) {
		s.logger.Printf("Default template %q not found. Creating one...", defaultTemplatePath)
		if err := s.createDefaultTemplate(defaultTemplatePath); err != nil {
			return fmt.Errorf("failed to create default template: %w", err)
		}
	}

	// Initialize template parser with custom functions
	tmpl := template.New("").Funcs(template.FuncMap{
		"title":          strings.ToTitle,
		"upper":          strings.ToUpper,
		"lower":          strings.ToLower,
		"formatTime":     func(t time.Time) string { return t.Format("2006-01-02 15:04:05 MST (UTC)") }, // Add MST for timezone
		"formatDuration": func(d time.Duration) string { return d.Round(time.Millisecond).String() },    // Use .String() with rounding
		"formatBytes":    formatBytes,                                                                   // Use the standalone helper function
		"add":            func(a, b int) int { return a + b },
		"formatNumber": func(n int64) string { // Format large numbers (e.g., 1234567 -> 1.2M)
			if n < 1000 {
				return fmt.Sprintf("%d", n)
			}
			if n < 1000000 {
				return fmt.Sprintf("%.1fK", float64(n)/1000)
			}
			return fmt.Sprintf("%.1fM", float64(n)/1000000)
		},
	})

	// Parse all .html files in the template directory
	var err error
	s.templates, err = tmpl.ParseGlob(filepath.Join(s.config.TemplateDir, "*.html"))
	if err != nil {
		return fmt.Errorf("failed to parse templates from %q: %w", s.config.TemplateDir, err)
	}

	s.logger.Printf("✅ Templates loaded successfully from: %s (%d files)", s.config.TemplateDir, len(s.templates.Templates()))
	return nil
}

// createDefaultTemplate writes a comprehensive default HTML template to the specified filename.
// This is useful if no templates exist initially, providing a basic functional UI.
func (s *Server) createDefaultTemplate(filename string) error {
	content := `<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<title>{{.Title}}</title>
	<link rel="preconnect" href="https://fonts.googleapis.com">
	<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
	<link href="https://fonts.googleapis.com/css2?family=Inter:ital,opsz,wght@0,14..32,100..900;1,14..32,100..900&display=swap" rel="stylesheet">
	<style>
		*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
		:root { --primary: #3b82f6; --primary-dark: #2563eb; --secondary: #8b5cf6; --accent: #10b981; --background: #0f172a; --surface: #1e293b; --surface-light: #334155; --surface-hover: #475569; --text: #f8fafc; --text-dim: #cbd5e1; --text-muted: #94a3b8; --border: #475569; --border-light: #64748b; --success: #22c55e; --warning: #f59e0b; --error: #ef4444; --shadow: rgba(0, 0, 0, 0.1); --shadow-lg: rgba(0, 0, 0, 0.25); --gradient: linear-gradient(135deg, var(--primary), var(--secondary)); }
		html { scroll-behavior: smooth; }
		body { font-family: 'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', 'Roboto', sans-serif; background: var(--background); color: var(--text); line-height: 1.6; min-height: 100vh; overflow-x: hidden; position: relative; }
		body::before { content: ''; position: fixed; top: 0; left: 0; width: 100%; height: 100%; background: radial-gradient(circle at 25% 25%, rgba(59, 130, 246, 0.1) 0%, transparent 50%), radial-gradient(circle at 75% 75%, rgba(139, 92, 246, 0.08) 0%, transparent 50%), radial-gradient(circle at 50% 50%, rgba(16, 185, 129, 0.05) 0%, transparent 50%); z-index: -2; animation: backgroundShift 20s ease-in-out infinite alternate; }
		@keyframes backgroundShift { 0% { transform: translateX(0) translateY(0) scale(1); } 100% { transform: translateX(-10px) translateY(-10px) scale(1.02); } }
		.container { max-width: 1400px; margin: 0 auto; padding: 2rem 1rem; position: relative; z-index: 1; }
		.header { text-align: center; margin-bottom: 3rem; padding: 2rem 0; }
		.header h1 { font-size: clamp(2.5rem, 8vw, 5rem); font-weight: 900; background: var(--gradient); -webkit-background-clip: text; -webkit-text-fill-color: transparent; background-clip: text; margin-bottom: 1rem; letter-spacing: -0.05em; line-height: 1.1; animation: slideInUp 0.8s ease-out; }
		.header p { color: var(--text-dim); font-size: clamp(1rem, 2vw, 1.25rem); margin-bottom: 1.5rem; max-width: 600px; margin-left: auto; margin-right: auto; animation: slideInUp 0.8s ease-out 0.2s both; }
		.status-badge { display: inline-flex; align-items: center; gap: 0.75rem; background: rgba(34, 197, 94, 0.1); border: 2px solid var(--success); color: var(--success); padding: 0.75rem 1.5rem; border-radius: 50px; font-size: 1rem; font-weight: 600; backdrop-filter: blur(10px); animation: slideInUp 0.8s ease-out 0.4s both, pulse 3s ease-in-out infinite; }
		@keyframes slideInUp { from { opacity: 0; transform: translateY(30px); } to { opacity: 1; transform: translateY(0); } }
		@keyframes pulse { 0%, 100% { box-shadow: 0 0 0 0 rgba(34, 197, 94, 0.4); } 50% { box-shadow: 0 0 0 10px rgba(34, 197, 94, 0.1); } }
		.nav { display: grid; grid-template-columns: repeat(auto-fit, minmax(280px, 1fr)); gap: 1.5rem; margin: 3rem 0; max-width: 1200px; margin-left: auto; margin-right: auto; }
		.nav-item { background: rgba(30, 41, 59, 0.8); border: 1px solid var(--border); border-radius: 20px; padding: 2rem 1.5rem; text-decoration: none; color: var(--text); text-align: center; transition: all 0.3s cubic-bezier(0.4, 0, 0.2, 1); backdrop-filter: blur(10px); position: relative; overflow: hidden; animation: fadeInScale 0.6s ease-out both; }
		.nav-item:nth-child(n) { animation-delay: calc(0.1s * var(--i, 1)); }
		@keyframes fadeInScale { from { opacity: 0; transform: scale(0.9) translateY(20px); } to { opacity: 1; transform: scale(1) translateY(0); } }
		.nav-item::before { content: ''; position: absolute; top: 0; left: -100%; width: 100%; height: 100%; background: linear-gradient(90deg, transparent, rgba(255, 255, 255, 0.1), transparent); transition: left 0.5s; }
		.nav-item:hover::before { left: 100%; }
		.nav-item:hover { background: rgba(51, 65, 85, 0.9); border-color: var(--primary); transform: translateY(-8px) scale(1.02); box-shadow: 0 20px 40px rgba(0, 0, 0, 0.3), 0 0 0 1px rgba(59, 130, 246, 0.2); }
		.nav-item .icon { font-size: 2.5rem; margin-bottom: 1rem; display: block; filter: drop-shadow(0 2px 4px rgba(0, 0, 0, 0.3)); }
		.nav-item .title { font-weight: 700; font-size: 1.25rem; margin-bottom: 0.5rem; color: var(--text); }
		.nav-item .desc { font-size: 0.95rem; color: var(--text-dim); line-height: 1.4; }
		.stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(250px, 1fr)); gap: 1.5rem; margin: 2rem 0 3rem 0; max-width: 1200px; margin-left: auto; margin-right: auto; }
		.stat-card { background: rgba(30, 41, 59, 0.6); border: 1px solid var(--border); border-radius: 16px; padding: 2rem; backdrop-filter: blur(10px); transition: all 0.3s ease; position: relative; overflow: hidden; }
		.stat-card::before { content: ''; position: absolute; top: 0; left: 0; right: 0; height: 3px; background: var(--gradient); transform: scaleX(0); transition: transform 0.3s ease; }
		.stat-card:hover::before { transform: scaleX(1); }
		.stat-card:hover { border-color: var(--primary); transform: translateY(-5px); box-shadow: 0 15px 35px rgba(0, 0, 0, 0.2); }
		.stat-card h3 { font-size: 0.9rem; color: var(--text-muted); margin-bottom: 0.75rem; text-transform: uppercase; letter-spacing: 0.1em; font-weight: 600; }
		.stat-card .value { font-size: 2.25rem; font-weight: 800; color: var(--text); margin-bottom: 0.5rem; line-height: 1; }
		.stat-card .change { font-size: 0.9rem; color: var(--success); display: flex; align-items: center; gap: 0.5rem; font-weight: 500; }
		.content { margin: 4rem 0; text-align: center; }
		.content h2 { font-size: clamp(1.75rem, 4vw, 2.5rem); margin-bottom: 2rem; color: var(--primary); font-weight: 700; }
		.content-text { background: rgba(30, 41, 59, 0.6); border: 1px solid var(--border); border-radius: 16px; padding: 2.5rem; margin: 3rem auto; max-width: 800px; color: var(--text); font-size: 1.1rem; line-height: 1.7; backdrop-filter: blur(10px); text-align: left; }
		.error-message { background: rgba(239, 68, 68, 0.1); border: 1px solid var(--error); color: var(--error); padding: 1rem 1.5rem; border-radius: 12px; margin: 1rem 0; text-align: center; font-weight: 500; }
		.footer { text-align: center; margin-top: 4rem; padding: 2rem; border-top: 1px solid var(--border); background: rgba(15, 23, 42, 0.8); backdrop-filter: blur(10px); }
		.footer-content { display: flex; flex-wrap: wrap; justify-content: center; align-items: center; gap: 2rem; }
		.footer-item { display: flex; align-items: center; gap: 0.75rem; color: var(--text-dim); font-size: 0.95rem; }
		.tech-badge { background: var(--gradient); color: white; padding: 0.4rem 1rem; border-radius: 20px; font-size: 0.9rem; font-weight: 600; border: 1px solid rgba(255, 255, 255, 0.2); }
		#server-time { cursor: pointer; padding: 0.25rem 0.5rem; border-radius: 8px; transition: background-color 0.2s; }
		#server-time:hover { background-color: rgba(59, 130, 246, 0.1); }
		@media (max-width: 768px) { .container { padding: 1rem; } .nav { grid-template-columns: 1fr; gap: 1rem; } .nav-item { padding: 1.5rem 1rem; } .stats { grid-template-columns: 1fr; gap: 1rem; } .stat-card { padding: 1.5rem; } .footer-content { flex-direction: column; gap: 1rem; } .content-text { padding: 1.5rem; margin: 2rem auto; } }
		.loading { opacity: 0; animation: fadeIn 0.6s ease-out forwards; }
		@keyframes fadeIn { to { opacity: 1; } }
	</style>
</head>
<body>
	<div class="container loading">
		<header class="header">
			<h1>{{.Title}}</h1>
			{{if .Subtitle}}<p>{{.Subtitle}}</p>{{end}}
			<div class="status-badge"><span>🟢</span><span>Server Online</span></div>
		</header>
		<nav class="nav">
			<a href="/" class="nav-item" style="--i: 1"><span class="icon">🏠</span><div class="title">Home</div><div class="desc">Server dashboard and overview</div></a>
			<a href="/about" class="nav-item" style="--i: 2"><span class="icon">ℹ️</span><div class="title">About</div><div class="desc">Server information and details</div></a>
			<a href="/time" class="nav-item" style="--i: 3"><span class="icon">🕒</span><div class="title">Time</div><div class="desc">Current server time and timezone</div></a>
			<a href="/day" class="nav-item" style="--i: 4"><span class="icon">📅</span><div class="title">Day</div><div class="desc">Current day and date information</div></a>
			<a href="/health" class="nav-item" style="--i: 5"><span class="icon">❤️</span><div class="title">Health</div><div class="desc">System health and status check</div></a>
			<a href="/metrics" class="nav-item" style="--i: 6"><span class="icon">📊</span><div class="title">Metrics</div><div class="desc">Performance statistics and analytics</div></a>
			<a href="/api/status" class="nav-item" style="--i: 7"><span class="icon">🔌</span><div class="title">API</div><div class="desc">Check the JSON API status</div></a>
		</nav>
		<main class="content">
			{{if .Error}}<div class="error-message">⚠️ {{.Error}}</div>{{end}}
			{{if .Message}}<h2>{{.Message}}</h2>{{end}}
			{{if .Data}}
				<div class="stats">
					{{range $key, $value := .Data}}
					<div class="stat-card">
						<h3>{{$key | title}}</h3>
						<div class="value">{{$value}}</div>
					</div>
					{{end}}
				</div>
			{{end}}
			{{if .Content}}<div class="content-text">{{.Content}}</div>{{end}}
		</main>
		<footer class="footer">
			<div class="footer-content">
				<div class="footer-item"><span class="tech-badge">Go v{{.GoVersion}}</span></div>
				<div class="footer-item"><span>Server Time:</span><span id="server-time">{{.ServerTime}}</span></div>
			</div>
		</footer>
	</div>
	<script>
		// Simple script to update time locally to avoid constant requests
		const timeEl = document.getElementById('server-time');
		if (timeEl) {
			let serverTime = new Date('{{.ServerTime}}');
			setInterval(() => {
				serverTime.setSeconds(serverTime.getSeconds() + 1);
				timeEl.textContent = serverTime.toISOString().slice(0, 19).replace('T', ' ') + ' UTC'; // Add UTC back
			}, 1000);
		}
	</script>
</body>
</html>
`
	return os.WriteFile(filename, []byte(content), 0644)
}

//-----------------------------------------------------------------------------
// Middleware Functions
//-----------------------------------------------------------------------------

// loggingMiddleware logs details of incoming HTTP requests.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r) // Call the next handler in the chain
		s.logger.Printf("INFO: %s %s %s %s %s", r.Method, r.RequestURI, r.Proto, time.Since(start).Round(time.Millisecond), r.RemoteAddr)
	})
}

// metricsMiddleware collects and records request-specific metrics.
func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Wrap the response writer to capture the HTTP status code
		rw := &responseWriter{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		duration := time.Since(start)

		// Sanitize the URL path for consistent endpoint naming in metrics
		// e.g., /users/123 becomes /users/:id
		endpoint := r.Method + " " + regexp.MustCompile(`/\d+`).ReplaceAllString(r.URL.Path, "/:id")
		s.metrics.RecordRequest(endpoint, duration, rw.statusCode)
	})
}

// rateLimitMiddleware applies rate limiting based on the client's IP address.
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.config.EnableRateLimit {
			clientIP := s.getClientIP(r) // Get the true client IP, considering proxies
			if !s.rateLimiter.Allow(clientIP) {
				s.logger.Printf("RATE_LIMIT: Client %q exceeded rate limit for %s %s", clientIP, r.Method, r.URL.Path)
				s.respondError(w, http.StatusTooManyRequests, "Too many requests. Please try again later.")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// corsMiddleware sets appropriate CORS headers if enabled.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.config.EnableCORS {
			w.Header().Set("Access-Control-Allow-Origin", "*") // For production, specify allowed origins
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, HEAD")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
			w.Header().Set("Access-Control-Max-Age", "86400") // Cache preflight response for 24 hours
		}
		// Handle preflight requests
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeadersMiddleware adds various security-related HTTP headers.
func (s *Server) securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")                                                                                                                        // Prevents browser from MIME-sniffing a response away from the declared content-type
		w.Header().Set("X-Frame-Options", "DENY")                                                                                                                                  // Prevents clickjacking by forbidding embedding in iframes
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com;") // CSP for basic XSS protection
		w.Header().Set("X-XSS-Protection", "1; mode=block")                                                                                                                        // Enables XSS filter in browsers
		w.Header().Set("Referrer-Policy", "no-referrer-when-downgrade")                                                                                                            // Controls referrer information
		next.ServeHTTP(w, r)
	})
}

// recoverPanicMiddleware recovers from panics in HTTP handlers to prevent server crashes.
func (s *Server) recoverPanicMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				s.logger.Printf("CRITICAL: PANIC Recovered: %v. Stack: %s", err, debug.Stack()) // Log stack trace
				s.respondError(w, http.StatusInternalServerError, "Internal Server Error. Our team has been notified.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

//-----------------------------------------------------------------------------
// HTTP Handler Functions
//-----------------------------------------------------------------------------

// handleHome renders the main dashboard page with server uptime and request count.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.handleNotFound(w, r) // Handle routes not specifically defined
		return
	}

	data := TemplateData{
		Title:    "Go Web Server Dashboard",
		Subtitle: "A concurrent, production-ready web server with metrics and security features.",
		Data: map[string]interface{}{
			"Server Uptime":   s.metrics.Uptime,
			"Requests Served": s.metrics.RequestCount,
			"Errors Count":    s.metrics.ErrorCount,
			"Memory Usage":    formatBytes(s.metrics.MemoryUsage),
			"Go Routines":     runtime.NumGoroutine(),
		},
	}
	s.renderTemplate(w, "default.html", data)
}

// handleAbout renders the about page with general server information.
func (s *Server) handleAbout(w http.ResponseWriter, r *http.Request) {
	data := TemplateData{
		Title:   "About This Server",
		Message: "Server Information & Features",
		Content: "This server is a demonstration of a robust, production-grade web service written in Go. It features a modern UI, middleware for logging, metrics, security, rate-limiting, and graceful shutdown capabilities. It's designed for high concurrency and resilience.",
		Data: map[string]interface{}{
			"Go Version":      runtime.Version(),
			"OS / Arch":       fmt.Sprintf("%s / %s", runtime.GOOS, runtime.GOARCH),
			"Max Procs (CPU)": runtime.GOMAXPROCS(0), // Number of OS threads Go can use
		},
	}
	s.renderTemplate(w, "default.html", data)
}

// handleTime renders a page displaying current server time in various formats.
func (s *Server) handleTime(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	data := TemplateData{
		Title:   "Server Time",
		Message: "The current server time is:",
		Data: map[string]interface{}{
			"UTC Time":       now.UTC().Format("2006-01-02 15:04:05 MST (UTC)"),
			"Local Time":     now.Format("2006-01-02 15:04:05 MST (Local)"),
			"Unix Timestamp": now.Unix(),
			"RFC1123":        now.Format(time.RFC1123),
		},
	}
	s.renderTemplate(w, "default.html", data)
}

// handleDay renders a page displaying current date-related information.
func (s *Server) handleDay(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	data := TemplateData{
		Title:   "Day Information",
		Message: "Today's Date Information:",
		Data: map[string]interface{}{
			"Current Date": now.Format("Monday, January 2, 2006"),
			"Day of Week":  now.Weekday().String(),
			"Day of Year":  now.YearDay(),
			"Week of Year": (now.YearDay() / 7) + 1, // Simple week calculation
			"Month":        now.Month().String(),
		},
	}
	s.renderTemplate(w, "default.html", data)
}

// handleHealth provides a simple JSON health check endpoint.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.respondJSON(w, http.StatusOK, map[string]string{"status": "healthy", "message": "Server is operational"})
}

// handleMetrics provides a JSON endpoint with current server performance metrics.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.config.EnableMetrics {
		s.respondError(w, http.StatusNotFound, "Metrics endpoint is disabled")
		return
	}
	s.metrics.mu.RLock() // Acquire read lock before marshaling metrics
	defer s.metrics.mu.RUnlock()

	// Update uptime just before sending to ensure it's fresh
	s.metrics.Uptime = time.Since(s.metrics.StartTime).Truncate(time.Second).String()

	// Create a copy of the metrics to work with (avoids modifying the live metrics under read lock)
	metricsCopy := *s.metrics

	// Calculate average response time and assign it to the new field in the copy
	avgRTDuration := s.metrics.GetAverageResponseTime()
	metricsCopy.AverageResponseTime = avgRTDuration.Round(time.Microsecond).String() // Format it nicely

	s.respondJSON(w, http.StatusOK, metricsCopy)
}

// handleAPIStatus provides a simple JSON endpoint to confirm API functionality.
func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	s.respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    map[string]string{"status": "API is online and running"},
	})
}

// handleNotFound serves a custom 404 page for unmatched routes.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	data := TemplateData{
		Title:   "404 Not Found",
		Message: "Page Not Found",
		Error:   fmt.Sprintf("The requested URL %q was not found on this server.", r.URL.Path),
	}
	s.renderTemplate(w, "default.html", data) // Render the default template for 404
}

//-----------------------------------------------------------------------------
// General HTTP Response Helpers
//-----------------------------------------------------------------------------

// renderTemplate executes the specified HTML template with the given data.
func (s *Server) renderTemplate(w http.ResponseWriter, name string, data TemplateData) {
	// Populate common data fields for all templates
	data.GoVersion = strings.TrimPrefix(runtime.Version(), "go")
	data.ServerTime = time.Now().UTC().Format("2006-01-02 15:04:05") + " UTC" // Standardize time format

	// Look up the specific template by name
	tmpl := s.templates.Lookup(name)
	if tmpl == nil {
		s.logger.Printf("ERROR: Template %q not found or not parsed.", name)
		s.respondError(w, http.StatusInternalServerError, "Server error: Template not found.")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		s.logger.Printf("ERROR: Failed to execute template %q: %v", name, err)
		s.respondError(w, http.StatusInternalServerError, "Server error: Failed to render page.")
	}
}

// respondError sends a JSON error response to the client.
func (s *Server) respondError(w http.ResponseWriter, code int, message string) {
	s.respondJSON(w, code, APIResponse{Success: false, Error: message})
}

// respondJSON sends a JSON response to the client.
func (s *Server) respondJSON(w http.ResponseWriter, code int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// If the payload is an APIResponse, ensure timestamp and version are set.
	if resp, ok := payload.(APIResponse); ok {
		resp.Timestamp = time.Now().UTC()
		if resp.Version == "" {
			resp.Version = "v1.0" // Default API version
		}
		payload = resp
	} else if respErr, ok := payload.(struct {
		Success bool
		Error   string
	}); ok && !respErr.Success {
		// If it's an anonymous error struct (e.g., from respondError), wrap it in APIResponse
		payload = APIResponse{
			Success:   false,
			Error:     respErr.Error,
			Timestamp: time.Now().UTC(),
			Version:   "v1.0",
		}
	}

	responseBytes, err := json.MarshalIndent(payload, "", "  ") // Use MarshalIndent for pretty-printing JSON
	if err != nil {
		s.logger.Printf("ERROR: JSON marshaling error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		// Fallback error message if JSON marshaling fails
		w.Write([]byte(`{"success":false,"error":"Internal Server Error - JSON encoding failed"}`))
		return
	}

	w.WriteHeader(code)
	w.Write(responseBytes)
}

// getClientIP extracts the true client IP address, handling X-Forwarded-For and X-Real-IP headers.
func (s *Server) getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header first (common in load balancers/proxies)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For can contain a comma-separated list of IPs.
		// The first IP is usually the original client IP.
		ips := strings.Split(xff, ",")
		for _, ip := range ips {
			trimmedIP := strings.TrimSpace(ip)
			// Validate if the IP is from a trusted proxy if trusted_proxies is configured
			if len(s.config.TrustedProxies) > 0 {
				isTrusted := false
				for _, trusted := range s.config.TrustedProxies {
					if trimmedIP == trusted { // Simplified check, could use CIDR matching
						isTrusted = true
						break
					}
				}
				if isTrusted {
					continue // Skip trusted proxy IPs, look for the actual client behind it
				}
			}
			return trimmedIP // Return the first non-trusted IP
		}
	}

	// Check X-Real-IP header (another common proxy header)
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}

	// Fallback to r.RemoteAddr if no proxy headers are found
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return ip
	}
	return r.RemoteAddr // Return raw RemoteAddr if parsing fails
}

//-----------------------------------------------------------------------------
// Server Lifecycle Management
//-----------------------------------------------------------------------------

// Run starts the HTTP server, sets up graceful shutdown, and handles TLS if enabled.
func (s *Server) Run() error {
	// Create a new ServeMux for routing HTTP requests
	mux := http.NewServeMux()

	// 1. Static File Server: Serve files from the configured static directory.
	staticFS := http.FileServer(http.Dir(s.config.StaticDir))
	mux.Handle("/static/", http.StripPrefix("/static/", staticFS))

	// 2. Register HTTP Handlers for specific routes.
	mux.HandleFunc("/", s.handleHome)
	mux.HandleFunc("/about", s.handleAbout)
	mux.HandleFunc("/time", s.handleTime)
	mux.HandleFunc("/day", s.handleDay)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/api/status", s.handleAPIStatus)
	// Add a catch-all for unmatched routes (404)
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(s.config.StaticDir, "favicon.ico"))
	})
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(s.config.StaticDir, "robots.txt"))
	})
	// Generic handler for anything not matched by specific routes
	mux.HandleFunc("/_/*", s.handleNotFound) // Catch-all for other sub-paths not defined

	// 3. Chain Middleware: Apply middleware in the desired order (outermost to innermost).
	// Recovery from panics should be the outermost to catch errors from all subsequent middleware/handlers.
	var handler http.Handler = mux
	handler = s.recoverPanicMiddleware(handler)    // Catches panics
	handler = s.loggingMiddleware(handler)         // Logs requests
	handler = s.metricsMiddleware(handler)         // Collects metrics
	handler = s.rateLimitMiddleware(handler)       // Applies rate limiting
	handler = s.corsMiddleware(handler)            // Adds CORS headers
	handler = s.securityHeadersMiddleware(handler) // Adds security headers

	// 4. Configure the standard Go HTTP server instance.
	httpServer := &http.Server{
		Addr:           net.JoinHostPort(s.config.Host, s.config.Port),
		Handler:        handler,
		ReadTimeout:    s.config.ReadTimeout,
		WriteTimeout:   s.config.WriteTimeout,
		IdleTimeout:    s.config.IdleTimeout,
		MaxHeaderBytes: s.config.MaxHeaderBytes,
	}

	// 5. Setup Graceful Shutdown: Listen for OS signals to gracefully stop the server.
	shutdownChan := make(chan os.Signal, 1)
	signal.Notify(shutdownChan, syscall.SIGINT, syscall.SIGTERM) // Listen for Ctrl+C and termination signals

	// Channel to receive errors from the HTTP server's ListenAndServe call.
	serverErrors := make(chan error, 1)

	// Start the HTTP server in a non-blocking goroutine.
	go func() {
		// Log the URL for easy access. If listening on 0.0.0.0, provide localhost link.
		scheme := "http"
		if s.config.EnableTLS {
			scheme = "https"
		}
		displayHost := s.config.Host
		if displayHost == "0.0.0.0" || displayHost == "::" {
			displayHost = "localhost" // For local clickable link
		}
		serverURL := fmt.Sprintf("%s://%s:%s", scheme, displayHost, s.config.Port)

		s.logger.Printf("🚀 Server starting on %s (TLS: %t, Rate Limit: %t)", httpServer.Addr, s.config.EnableTLS, s.config.EnableRateLimit)
		s.logger.Printf("✅ Server is listening. Access it at: %s", serverURL)

		// Start listening for incoming requests.
		if s.config.EnableTLS {
			serverErrors <- httpServer.ListenAndServeTLS(s.config.CertFile, s.config.KeyFile)
		} else {
			serverErrors <- httpServer.ListenAndServe()
		}
	}()

	// Block main goroutine until a shutdown signal is received or server encounters an error.
	select {
	case err := <-serverErrors:
		// An error occurred during server startup/operation (e.g., port in use)
		return fmt.Errorf("server startup/runtime error: %w", err)
	case sig := <-shutdownChan:
		// Received an OS signal (SIGINT/SIGTERM) for graceful shutdown
		s.logger.Printf("Received OS signal: %v. Initiating graceful shutdown...", sig)
		// Create a context with a timeout for graceful shutdown.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second) // 15 seconds to shut down
		defer cancel()

		// Attempt to gracefully shut down the server.
		if err := httpServer.Shutdown(ctx); err != nil {
			// Force close the server if graceful shutdown fails
			s.logger.Printf("ERROR: Force closing server due to graceful shutdown failure: %v", err)
			httpServer.Close()
			return fmt.Errorf("server failed graceful shutdown: %w", err)
		}
		s.logger.Println("✅ Server stopped gracefully.")
	}

	return nil // Server exited cleanly
}

//-----------------------------------------------------------------------------
// Main Entry Point
//-----------------------------------------------------------------------------

func main() {
	// Initialize logging for the main function before server setup
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags | log.Lshortfile) // Keep standard flags for main func fatal errors

	// Load server configuration
	config := NewConfig()

	// Create a new server instance
	server, err := NewServer(config)
	if err != nil {
		log.Fatalf("❌ FATAL: Failed to initialize server: %v", err)
	}

	// Run the server. The Run method blocks until shutdown.
	if err := server.Run(); err != nil && err != http.ErrServerClosed {
		// Log fatal error if server fails to run for reasons other than graceful shutdown
		server.logger.Fatalf("❌ FATAL: Server failed to run: %v", err)
	}

	// Program exits after server stops
	log.Println("👋 Server application exited.")
}
