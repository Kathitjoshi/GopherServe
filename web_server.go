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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Server configuration
type Config struct {
	Port            string
	Host            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	MaxHeaderBytes  int
	EnableTLS       bool
	CertFile        string
	KeyFile         string
	EnableRateLimit bool
	RateLimit       int
	StaticDir       string
	TemplateDir     string
	LogFile         string
	EnableMetrics   bool
	EnableCORS      bool
	TrustedProxies  []string
}

// Server represents our enhanced web server
type Server struct {
	config      *Config
	templates   *template.Template
	logger      *log.Logger
	rateLimiter *RateLimiter
	metrics     *Metrics
	mu          sync.RWMutex
	startTime   time.Time
}

// Metrics tracks server statistics
type Metrics struct {
	RequestCount  int64                    `json:"request_count"`
	ErrorCount    int64                    `json:"error_count"`
	StartTime     time.Time                `json:"start_time"`
	Uptime        string                   `json:"uptime"`
	EndpointStats map[string]*EndpointStat `json:"endpoint_stats"`
	ResponseTimes []time.Duration          `json:"-"`
	ActiveUsers   int64                    `json:"active_users"`
	MemoryUsage   uint64                   `json:"memory_usage"`
	CPUUsage      float64                  `json:"cpu_usage"`
	mu            sync.RWMutex
}

// EndpointStat tracks individual endpoint statistics
type EndpointStat struct {
	Count       int64         `json:"count"`
	TotalTime   time.Duration `json:"total_time"`
	AverageTime time.Duration `json:"average_time"`
	LastAccess  time.Time     `json:"last_access"`
	ErrorCount  int64         `json:"error_count"`
	StatusCodes map[int]int64 `json:"status_codes"`
}

// RateLimiter implements token bucket rate limiting
type RateLimiter struct {
	clients map[string]*ClientBucket
	mu      sync.RWMutex
	rate    int
}

// ClientBucket represents a token bucket for a client
type ClientBucket struct {
	tokens     int
	lastRefill time.Time
	mu         sync.Mutex
}

// APIResponse represents a JSON API response
type APIResponse struct {
	Success   bool        `json:"success"`
	Data      interface{} `json:"data,omitempty"`
	Error     string      `json:"error,omitempty"`
	Meta      interface{} `json:"meta,omitempty"`
	Timestamp time.Time   `json:"timestamp"`
	Version   string      `json:"version"`
}

// TemplateData holds data for HTML templates
type TemplateData struct {
	Title      string
	Subtitle   string
	Message    string
	Content    string
	Data       map[string]interface{}
	ServerTime string
	GoVersion  string
	ServerInfo map[string]interface{}
	Metrics    *Metrics
	Error      string
}

// NewConfig creates a new server configuration with defaults
func NewConfig() *Config {
	return &Config{
		Port:            getEnv("PORT", "8080"),
		Host:            getEnv("HOST", "localhost"),
		ReadTimeout:     30 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     120 * time.Second,
		MaxHeaderBytes:  1 << 20, // 1 MB
		EnableTLS:       getEnv("ENABLE_TLS", "false") == "true",
		CertFile:        getEnv("CERT_FILE", "server.crt"),
		KeyFile:         getEnv("KEY_FILE", "server.key"),
		EnableRateLimit: getEnv("ENABLE_RATE_LIMIT", "true") == "true",
		RateLimit:       getEnvInt("RATE_LIMIT", 100),
		StaticDir:       getEnv("STATIC_DIR", "./static"),
		TemplateDir:     getEnv("TEMPLATE_DIR", "./templates"),
		LogFile:         getEnv("LOG_FILE", ""),
		EnableMetrics:   getEnv("ENABLE_METRICS", "true") == "true",
		EnableCORS:      getEnv("ENABLE_CORS", "true") == "true",
		TrustedProxies:  strings.Split(getEnv("TRUSTED_PROXIES", "127.0.0.1,::1"), ","),
	}
}

// Helper functions
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}

// =================================================================
// >> FIX 1: Create a standalone helper function for formatting bytes
// =================================================================
func formatBytesHelper(bytes uint64) string {
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

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(rate int) *RateLimiter {
	return &RateLimiter{
		clients: make(map[string]*ClientBucket),
		rate:    rate,
	}
}

// Allow checks if a client is allowed to make a request
func (rl *RateLimiter) Allow(clientIP string) bool {
	rl.mu.RLock()
	bucket, exists := rl.clients[clientIP]
	rl.mu.RUnlock()

	if !exists {
		rl.mu.Lock()
		// Double check after acquiring write lock
		if _, exists = rl.clients[clientIP]; !exists {
			bucket = &ClientBucket{
				tokens:     rl.rate,
				lastRefill: time.Now(),
			}
			rl.clients[clientIP] = bucket
		}
		rl.mu.Unlock()
	}

	bucket.mu.Lock()
	defer bucket.mu.Unlock()

	// Refill tokens based on time passed
	now := time.Now()
	elapsed := now.Sub(bucket.lastRefill)
	tokensToAdd := int(elapsed.Seconds()) * rl.rate / 60 // Add tokens per second

	if tokensToAdd > 0 {
		bucket.tokens += tokensToAdd
		if bucket.tokens > rl.rate {
			bucket.tokens = rl.rate
		}
		bucket.lastRefill = now
	}

	if bucket.tokens > 0 {
		bucket.tokens--
		return true
	}

	return false
}

// NewMetrics creates a new metrics tracker
func NewMetrics() *Metrics {
	m := &Metrics{
		StartTime:     time.Now(),
		EndpointStats: make(map[string]*EndpointStat),
		ResponseTimes: make([]time.Duration, 0, 1000),
	}
	// Start a background goroutine to periodically update system metrics
	go func() {
		for {
			m.updateSystemMetrics()
			time.Sleep(5 * time.Second)
		}
	}()
	return m
}

// RecordRequest records a request in metrics
func (m *Metrics) RecordRequest(endpoint string, duration time.Duration, statusCode int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.RequestCount++
	m.Uptime = time.Since(m.StartTime).Truncate(time.Second).String()

	// Add response time (keep only last 1000)
	m.ResponseTimes = append(m.ResponseTimes, duration)
	if len(m.ResponseTimes) > 1000 {
		m.ResponseTimes = m.ResponseTimes[1:]
	}

	stat, exists := m.EndpointStats[endpoint]
	if !exists {
		stat = &EndpointStat{
			StatusCodes: make(map[int]int64),
		}
		m.EndpointStats[endpoint] = stat
	}

	stat.Count++
	stat.TotalTime += duration
	stat.AverageTime = stat.TotalTime / time.Duration(stat.Count)
	stat.LastAccess = time.Now()
	stat.StatusCodes[statusCode]++

	if statusCode >= 400 {
		stat.ErrorCount++
		m.ErrorCount++
	}
}

// updateSystemMetrics updates system resource metrics
func (m *Metrics) updateSystemMetrics() {
	m.mu.Lock()
	defer m.mu.Unlock()
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	m.MemoryUsage = memStats.Alloc
	m.Uptime = time.Since(m.StartTime).Truncate(time.Second).String()
}

// GetAverageResponseTime calculates average response time
func (m *Metrics) GetAverageResponseTime() time.Duration {
	m.mu.RLock()
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

// NewServer creates a new enhanced server instance
func NewServer(config *Config) (*Server, error) {
	server := &Server{
		config:    config,
		metrics:   NewMetrics(),
		startTime: time.Now(),
	}

	// Setup logger
	var logOutput io.Writer = os.Stdout
	if config.LogFile != "" {
		logFile, err := os.OpenFile(config.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return nil, fmt.Errorf("failed to open log file: %v", err)
		}
		logOutput = io.MultiWriter(os.Stdout, logFile)
	}
	server.logger = log.New(logOutput, "[SERVER] ", log.LstdFlags|log.Lshortfile)

	// Setup rate limiter
	if config.EnableRateLimit {
		server.rateLimiter = NewRateLimiter(config.RateLimit)
	}

	// Load templates
	if err := server.loadTemplates(); err != nil {
		return nil, fmt.Errorf("failed to load templates: %v", err)
	}

	// Create static directory if it doesn't exist
	if err := os.MkdirAll(config.StaticDir, 0755); err != nil {
		server.logger.Printf("Warning: failed to create static directory: %v", err)
	}

	server.logger.Printf("✅ Server initialized successfully")
	return server, nil
}

// loadTemplates loads HTML templates with enhanced error handling
func (s *Server) loadTemplates() error {
	// Create template directory if it doesn't exist
	if err := os.MkdirAll(s.config.TemplateDir, 0755); err != nil {
		return err
	}

	// Create default template if none exist
	defaultTemplatePath := filepath.Join(s.config.TemplateDir, "default.html")
	if _, err := os.Stat(defaultTemplatePath); os.IsNotExist(err) {
		s.logger.Printf("Default template not found. Creating one at %s", defaultTemplatePath)
		if err := s.createDefaultTemplate(defaultTemplatePath); err != nil {
			return err
		}
	}

	// Parse templates with enhanced functions
	tmpl := template.New("").Funcs(template.FuncMap{
		"title": strings.ToTitle,
		"upper": strings.ToUpper,
		"lower": strings.ToLower,
		"formatTime": func(t time.Time) string {
			return t.Format("2006-01-02 15:04:05")
		},
		"formatDuration": func(d time.Duration) string {
			if d < time.Minute {
				return fmt.Sprintf("%.1fs", d.Seconds())
			}
			if d < time.Hour {
				return fmt.Sprintf("%.1fm", d.Minutes())
			}
			return fmt.Sprintf("%.1fh", d.Hours())
		},
		// =================================================================
		// >> FIX 2: Use the named helper function in the FuncMap
		// =================================================================
		"formatBytes": formatBytesHelper,
		"add":         func(a, b int) int { return a + b },
		"formatNumber": func(n int64) string {
			if n < 1000 {
				return fmt.Sprintf("%d", n)
			}
			if n < 1000000 {
				return fmt.Sprintf("%.1fK", float64(n)/1000)
			}
			return fmt.Sprintf("%.1fM", float64(n)/1000000)
		},
	})

	var err error
	s.templates, err = tmpl.ParseGlob(filepath.Join(s.config.TemplateDir, "*.html"))
	if err != nil {
		s.logger.Printf("Template parsing error: %v", err)
		return err
	}

	s.logger.Printf("✅ Templates loaded successfully from: %s", s.config.TemplateDir)
	return nil
}

// createDefaultTemplate creates a comprehensive default HTML template
func (s *Server) createDefaultTemplate(filename string) error {
	// (This function's content is correct and remains unchanged)
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
				timeEl.textContent = serverTime.toISOString().slice(0, 19).replace('T', ' ');
			}, 1000);
		}
	</script>
</body>
</html>
`
	return os.WriteFile(filename, []byte(content), 0644)
}

// --- Middleware ---

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.logger.Printf("%s %s %s %s", r.Method, r.RequestURI, time.Since(start), r.RemoteAddr)
	})
}

func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Use a response writer wrapper to capture status code
		rw := &responseWriter{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		duration := time.Since(start)

		// Create a sanitized endpoint name
		endpoint := r.Method + " " + regexp.MustCompile(`/\d+`).ReplaceAllString(r.URL.Path, "/:id")
		s.metrics.RecordRequest(endpoint, duration, rw.statusCode)
	})
}

// responseWriter is a wrapper to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.config.EnableRateLimit {
			clientIP := s.getClientIP(r)
			if !s.rateLimiter.Allow(clientIP) {
				s.respondError(w, http.StatusTooManyRequests, "Too many requests")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.config.EnableCORS {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		}
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com;")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverPanicMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				s.logger.Printf("PANIC: %v", err)
				s.respondError(w, http.StatusInternalServerError, "Internal Server Error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// --- Handlers ---

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	data := TemplateData{
		Title:    "Go Web Server",
		Subtitle: "A modern, production-ready web server.",
		Data: map[string]interface{}{
			"Uptime":          s.metrics.Uptime,
			"Requests Served": s.metrics.RequestCount,
			// =================================================================
			// >> FIX 3: Call the helper function directly from the handler
			// =================================================================
			"Memory Usage": formatBytesHelper(s.metrics.MemoryUsage),
			"Go Routines":  runtime.NumGoroutine(),
		},
	}
	s.renderTemplate(w, "default.html", data)
}

func (s *Server) handleAbout(w http.ResponseWriter, r *http.Request) {
	data := TemplateData{
		Title:   "About This Server",
		Message: "Server Information",
		Content: "This server is a demonstration of a robust, production-grade web service written in Go. It features a modern UI, middleware for logging, metrics, security, rate-limiting, and graceful shutdown capabilities.",
	}
	s.renderTemplate(w, "default.html", data)
}

func (s *Server) handleTime(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	data := TemplateData{
		Title:   "Server Time",
		Message: "The current server time is:",
		Data: map[string]interface{}{
			"Time (UTC)":   now.UTC().Format(time.RFC1123),
			"Time (Local)": now.Format(time.RFC1123),
			"Unix Time":    now.Unix(),
		},
	}
	s.renderTemplate(w, "default.html", data)
}

func (s *Server) handleDay(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	data := TemplateData{
		Title:   "Day Information",
		Message: "Today's Date Information:",
		Data: map[string]interface{}{
			"Date":        now.Format("January 2, 2006"),
			"Day of Week": now.Weekday().String(),
			"Day of Year": now.YearDay(),
		},
	}
	s.renderTemplate(w, "default.html", data)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.metrics.mu.RLock()
	defer s.metrics.mu.RUnlock()
	s.respondJSON(w, http.StatusOK, s.metrics)
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	s.respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    map[string]string{"status": "API is online and running"},
	})
}

// --- Helper Methods ---

func (s *Server) renderTemplate(w http.ResponseWriter, name string, data TemplateData) {
	// Add common data to all templates
	data.GoVersion = strings.TrimPrefix(runtime.Version(), "go")
	data.ServerTime = time.Now().UTC().Format("2006-01-02 15:04:05") + " UTC"

	tmpl := s.templates.Lookup(name)
	if tmpl == nil {
		s.logger.Printf("Template %s not found", name)
		s.respondError(w, http.StatusInternalServerError, "Template not found")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err := tmpl.Execute(w, data)
	if err != nil {
		s.logger.Printf("Error executing template %s: %v", name, err)
		s.respondError(w, http.StatusInternalServerError, "Error rendering page")
	}
}

func (s *Server) respondError(w http.ResponseWriter, code int, message string) {
	s.respondJSON(w, code, APIResponse{Success: false, Error: message})
}

func (s *Server) respondJSON(w http.ResponseWriter, code int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// Add timestamp and version to APIResponse if applicable
	if resp, ok := payload.(APIResponse); ok {
		resp.Timestamp = time.Now().UTC()
		resp.Version = "v1.0"
		payload = resp
	}

	response, err := json.Marshal(payload)
	if err != nil {
		s.logger.Printf("JSON marshaling error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"success":false,"error":"Internal Server Error"}`))
		return
	}

	w.WriteHeader(code)
	w.Write(response)
}

func (s *Server) getClientIP(r *http.Request) string {
	ip := r.Header.Get("X-Forwarded-For")
	ip = strings.TrimSpace(strings.Split(ip, ",")[0])
	if ip != "" {
		return ip
	}

	ip = r.Header.Get("X-Real-IP")
	if ip != "" {
		return ip
	}

	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return ip
	}
	return r.RemoteAddr
}

// --- Server Lifecycle ---

func (s *Server) Run() error {
	// Setup router and middleware chain
	mux := http.NewServeMux()

	// Static file server
	staticFS := http.FileServer(http.Dir(s.config.StaticDir))
	mux.Handle("/static/", http.StripPrefix("/static/", staticFS))

	// Register handlers
	mux.HandleFunc("/", s.handleHome)
	mux.HandleFunc("/about", s.handleAbout)
	mux.HandleFunc("/time", s.handleTime)
	mux.HandleFunc("/day", s.handleDay)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/api/status", s.handleAPIStatus)

	// Chain middleware
	var handler http.Handler = mux
	handler = s.recoverPanicMiddleware(handler)
	handler = s.loggingMiddleware(handler)
	handler = s.metricsMiddleware(handler)
	handler = s.rateLimitMiddleware(handler)
	handler = s.corsMiddleware(handler)
	handler = s.securityHeadersMiddleware(handler)

	// Configure the HTTP server
	httpServer := &http.Server{
		Addr:           net.JoinHostPort(s.config.Host, s.config.Port),
		Handler:        handler,
		ReadTimeout:    s.config.ReadTimeout,
		WriteTimeout:   s.config.WriteTimeout,
		IdleTimeout:    s.config.IdleTimeout,
		MaxHeaderBytes: s.config.MaxHeaderBytes,
	}

	// Graceful shutdown setup
	shutdownChan := make(chan os.Signal, 1)
	signal.Notify(shutdownChan, syscall.SIGINT, syscall.SIGTERM)

	serverErrors := make(chan error, 1)

	// Start the server in a goroutine
	// Inside the Run() method...

	go func() {
		// >> NEW: Logic to create a user-friendly, clickable URL
		scheme := "http"
		if s.config.EnableTLS {
			scheme = "https"
		}
		// If listening on all interfaces, use 'localhost' for the clickable link.
		displayHost := s.config.Host
		if displayHost == "0.0.0.0" || displayHost == "::" {
			displayHost = "localhost"
		}
		url := fmt.Sprintf("%s://%s:%s", scheme, displayHost, s.config.Port)

		addr := httpServer.Addr
		s.logger.Printf("🚀 Server starting on %s (TLS: %v)", addr, s.config.EnableTLS)
		// >> NEW: The log line with the clickable link
		s.logger.Printf("✅ Server is listening. Access it at: %s", url)

		if s.config.EnableTLS {
			serverErrors <- httpServer.ListenAndServeTLS(s.config.CertFile, s.config.KeyFile)
		} else {
			serverErrors <- httpServer.ListenAndServe()
		}
	}()

	// Block until a signal is received or an error occurs
	select {
	case err := <-serverErrors:
		return fmt.Errorf("server error: %w", err)
	case sig := <-shutdownChan:
		s.logger.Printf("Received signal: %v. Starting graceful shutdown...", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		if err := httpServer.Shutdown(ctx); err != nil {
			httpServer.Close()
			return fmt.Errorf("could not stop server gracefully: %w", err)
		}
		s.logger.Println("✅ Server stopped gracefully.")
	}

	return nil
}

func main() {
	config := NewConfig()
	server, err := NewServer(config)
	if err != nil {
		log.Fatalf("❌ Failed to initialize server: %v", err)
	}

	if err := server.Run(); err != nil && err != http.ErrServerClosed {
		server.logger.Fatalf("❌ Server failed to run: %v", err)
	}
}
