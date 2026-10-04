package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed openapi.json
var openapiSpec []byte

const docsHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>ANAF CUI Lookup API</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="stylesheet" href="https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.17.14/swagger-ui.min.css"></head>
<body><div id="ui"></div>
<script src="https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.17.14/swagger-ui-bundle.min.js"></script>
<script>SwaggerUIBundle({url:"openapi.json",dom_id:"#ui"})</script></body></html>`

const docsCSP = "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdnjs.cloudflare.com; " +
	"style-src 'self' 'unsafe-inline' https://cdnjs.cloudflare.com; img-src 'self' data:"

// ---- cache ----

type cacheEntry struct {
	company *Company // nil means "not found"
	expires time.Time
}

type cache struct {
	mu      sync.Mutex
	items   map[string]cacheEntry
	ttl     time.Duration
	negTTL  time.Duration
	maxSize int
}

func newCache(ttl, negTTL time.Duration, maxSize int) *cache {
	return &cache{items: map[string]cacheEntry{}, ttl: ttl, negTTL: negTTL, maxSize: maxSize}
}

func (c *cache) get(cui string) (*Company, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[cui]
	if !ok || time.Now().After(e.expires) {
		delete(c.items, cui)
		return nil, false
	}
	return e.company, true
}

func (c *cache) set(cui string, co *Company) {
	if c.ttl <= 0 {
		return
	}
	ttl := c.ttl
	if co == nil {
		ttl = c.negTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) >= c.maxSize {
		now := time.Now()
		for k, v := range c.items {
			if now.After(v.expires) {
				delete(c.items, k)
			}
		}
		if len(c.items) >= c.maxSize { // still full, reset (simple and bounded)
			c.items = map[string]cacheEntry{}
		}
	}
	c.items[cui] = cacheEntry{company: co, expires: time.Now().Add(ttl)}
}

// ---- rate limiter (fixed window) ----

type window struct {
	start time.Time
	count int
}

type limiter struct {
	mu    sync.Mutex
	hits  map[string]*window
	limit int
	per   time.Duration
}

func newLimiter(limit int, per time.Duration) *limiter {
	l := &limiter{hits: map[string]*window{}, limit: limit, per: per}
	go func() {
		for range time.Tick(time.Minute) {
			l.mu.Lock()
			for k, w := range l.hits {
				if time.Since(w.start) > l.per {
					delete(l.hits, k)
				}
			}
			l.mu.Unlock()
		}
	}()
	return l
}

// allow reports whether the key may proceed, plus seconds until the window resets.
func (l *limiter) allow(key string) (bool, int) {
	if l.limit <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	w := l.hits[key]
	if w == nil || now.Sub(w.start) >= l.per {
		w = &window{start: now}
		l.hits[key] = w
	}
	w.count++
	retry := int(l.per.Seconds() - now.Sub(w.start).Seconds())
	if retry < 1 {
		retry = 1
	}
	return w.count <= l.limit, retry
}

// ---- server ----

type server struct {
	anaf       *anafClient
	cache      *cache
	keys       [][32]byte
	rate       *limiter // per API key
	authFail   *limiter // per client IP, failed auth attempts
	trustProxy bool
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(openapiSpec)
	})
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", docsCSP)
		w.Write([]byte(docsHTML))
	})
	mux.Handle("GET /v1/companies/{cui}", s.auth(http.HandlerFunc(s.getCompany)))
	mux.Handle("POST /v1/companies", s.auth(http.HandlerFunc(s.batchCompanies)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, &apiError{404, "route_not_found", "Route not found."})
	})
	return s.recoverer(s.secure(s.logging(mux)))
}

func (s *server) getCompany(w http.ResponseWriter, r *http.Request) {
	cui := normalizeCui(r.PathValue("cui"))
	if cui == "" {
		writeErr(w, &apiError{400, "invalid_cui", "Invalid CUI. Use 2 to 10 digits, optionally prefixed with RO."})
		return
	}
	found, aerr := s.resolve(r.Context(), []string{cui})
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	co, ok := found[cui]
	if !ok {
		writeErr(w, &apiError{404, "not_found", "CUI not found in the ANAF registry."})
		return
	}
	writeJSON(w, 200, co)
}

func (s *server) batchCompanies(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var in struct {
		CUIs []string `json:"cuis"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.CUIs) == 0 {
		writeErr(w, &apiError{400, "invalid_request", `Body must be JSON like {"cuis": ["123456"]}.`})
		return
	}
	if len(in.CUIs) > maxBatch {
		writeErr(w, &apiError{400, "too_many_cuis", "At most " + strconv.Itoa(maxBatch) + " CUIs per request."})
		return
	}
	valid, invalid := []string{}, []string{}
	seen := map[string]bool{}
	for _, raw := range in.CUIs {
		c := normalizeCui(raw)
		if c == "" {
			invalid = append(invalid, raw)
		} else if !seen[c] {
			seen[c] = true
			valid = append(valid, c)
		}
	}
	found, aerr := s.resolve(r.Context(), valid)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	companies, notFound := []Company{}, []string{}
	for _, c := range valid {
		if co, ok := found[c]; ok {
			companies = append(companies, co)
		} else {
			notFound = append(notFound, c)
		}
	}
	writeJSON(w, 200, map[string]any{"found": companies, "notFound": notFound, "invalid": invalid})
}

// resolve serves CUIs from cache and fetches the rest from ANAF in one call.
func (s *server) resolve(ctx context.Context, cuis []string) (map[string]Company, *apiError) {
	out := map[string]Company{}
	var missing []string
	for _, c := range cuis {
		if co, hit := s.cache.get(c); hit {
			if co != nil {
				out[c] = *co
			}
		} else {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	fetched, aerr := s.anaf.lookup(ctx, missing)
	if aerr != nil {
		return nil, aerr
	}
	for _, c := range missing {
		if co, ok := fetched[c]; ok {
			cp := co
			out[c] = co
			s.cache.set(c, &cp)
		} else {
			s.cache.set(c, nil)
		}
	}
	return out, nil
}

// ---- middleware ----

func (s *server) clientIP(r *http.Request) string {
	if s.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key == "" {
			if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
				key = strings.TrimSpace(h[7:])
			}
		}
		idx := s.matchKey(key)
		if idx < 0 {
			if ok, retry := s.authFail.allow(s.clientIP(r)); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(retry))
				writeErr(w, &apiError{429, "rate_limited", "Too many failed attempts."})
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="anaf-cui-lookup"`)
			writeErr(w, &apiError{401, "unauthorized", "Missing or invalid API key."})
			return
		}
		if ok, retry := s.rate.allow("key" + strconv.Itoa(idx)); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeErr(w, &apiError{429, "rate_limited", "Rate limit exceeded."})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// matchKey compares against every configured key in constant time.
func (s *server) matchKey(key string) int {
	if key == "" {
		return -1
	}
	h := sha256.Sum256([]byte(key))
	match := -1
	for i, k := range s.keys {
		if subtle.ConstantTimeCompare(h[:], k[:]) == 1 {
			match = i
		}
	}
	return match
}

func (s *server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'")
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(c int) { w.status = c; w.ResponseWriter.WriteHeader(c) }

func (s *server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/healthz" {
			return
		}
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"ms", time.Since(start).Milliseconds(), "ip", s.clientIP(r))
	})
}

func (s *server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic", "err", rec)
				writeErr(w, &apiError{500, "internal_error", "Internal server error."})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.Status, map[string]any{"error": e})
}
