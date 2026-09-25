package shop

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type attemptWindow struct {
	count int
	until time.Time
}

type attemptLimiter struct {
	mu      sync.Mutex
	windows map[string]attemptWindow
}

func newAttemptLimiter() *attemptLimiter {
	return &attemptLimiter{windows: make(map[string]attemptWindow)}
}

func (l *attemptLimiter) allow(key string, limit int, duration time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.windows) >= 10000 {
		for name, window := range l.windows {
			if now.After(window.until) {
				delete(l.windows, name)
			}
		}
		if len(l.windows) >= 10000 {
			return false
		}
	}
	window := l.windows[key]
	if !now.Before(window.until) {
		window = attemptWindow{until: now.Add(duration)}
	}
	if window.count >= limit {
		return false
	}
	window.count++
	l.windows[key] = window
	return true
}

func accountLimitKey(email string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return "login:" + hex.EncodeToString(digest[:])
}

func clientIP(r *http.Request, trustedProxies []net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "unknown"
	}
	trusted := ip.IsLoopback()
	for _, network := range trustedProxies {
		if network.Contains(ip) {
			trusted = true
			break
		}
	}
	if trusted {
		if real := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); real != nil {
			return real.String()
		}
	}
	return ip.String()
}
