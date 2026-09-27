// Package mobile is the WebSocket transport for the AIxodia Android client
// (REQ-046). It plugs into the harness exactly like the other transports — an
// sdk.InputSource feeding sdk.MergeInputSources and an sdk.Display fed by
// sdk.DispatchDisplay — so no core loop code changes.
//
// Authentication is two steps:
//
//  1. the daemon is only reachable through a random Cloudflare quick-tunnel
//     hostname, so nothing is published;
//  2. the phone sends its Cloudflare API token in the WebSocket handshake header
//     `Authorization: Bearer <token>`, which is verified against Cloudflare
//     (GET /user/tokens/verify) BEFORE the socket is upgraded (REQ-046(2)).
//
// A missing header is 401 and is not counted. A wrong token is 401 and counted:
// five failures lock that client address for 30s, then 60/120/240/300s. A
// verifier outage is 503 and is not counted, because the daemon cannot confirm
// the token and must fail closed. The daemon itself owns no credential: the
// verified token lives in memory and is gone after a restart (CON-012).
package mobile

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Verifier answers "is this Cloudflare API token good?".
type Verifier interface {
	VerifyToken(ctx context.Context, token string) error
}

// TokenCache is the RAM copy of the one token a phone has already handed over.
// A token that matches it is trusted without another Cloudflare round trip, so a
// verified phone keeps working while the Worker is briefly unreachable and the
// daemon still owns nothing on disk (REQ-046(3), CON-012).
type TokenCache interface {
	Get() string
	Adopt(string)
}

// ErrTokenRejected marks a wrong credential, which is the only case that counts
// toward the lockout.
var ErrTokenRejected = errors.New("mobile: token rejected")

const (
	defaultMaxFails    = 5
	defaultBasePenalty = 30 * time.Second
	defaultMaxPenalty  = 5 * time.Minute
)

// GateConfig tunes the handshake check and the lockout schedule.
type GateConfig struct {
	Verify       Verifier
	Cache        TokenCache
	MaxFails     int
	BasePenalty  time.Duration
	MaxPenalty   time.Duration
	HeaderName   string
	HeaderScheme string
}

func (c GateConfig) withDefaults() GateConfig {
	if c.Verify == nil {
		panic("mobile: GateConfig.Verify is required")
	}
	if c.MaxFails <= 0 {
		c.MaxFails = defaultMaxFails
	}
	if c.BasePenalty <= 0 {
		c.BasePenalty = defaultBasePenalty
	}
	if c.MaxPenalty <= 0 {
		c.MaxPenalty = defaultMaxPenalty
	}
	if c.HeaderName == "" {
		c.HeaderName = "Authorization"
	}
	if c.HeaderScheme == "" {
		c.HeaderScheme = "Bearer"
	}
	return c
}

// Decision is the result of one handshake attempt.
type Decision struct {
	Allowed    bool
	Status     int // 200, 401, 429, 503
	Token      string
	RetryAfter time.Duration
	Reason     string
	Fails      int
}

// Gate checks the Authorization header and enforces the progressive lockout.
// It stores counters only — no credentials, no disk state.
type Gate struct {
	cfg GateConfig

	mu            sync.Mutex
	fails         map[string]int
	penalties     map[string]time.Time
	globalFails   int
	globalPenalty time.Time
}

func NewGate(cfg GateConfig) *Gate {
	return &Gate{
		cfg:       cfg.withDefaults(),
		fails:     map[string]int{},
		penalties: map[string]time.Time{},
	}
}

// Check must run before the WebSocket upgrade: a rejection must never become a
// socket.
func (g *Gate) Check(r *http.Request) Decision {
	header := r.Header.Get(g.cfg.HeaderName)
	if header == "" {
		return Decision{Status: http.StatusUnauthorized, Reason: "missing " + g.cfg.HeaderName + " header"}
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], g.cfg.HeaderScheme) || strings.TrimSpace(parts[1]) == "" {
		return Decision{Status: http.StatusUnauthorized, Reason: "malformed " + g.cfg.HeaderName + " header"}
	}
	token := strings.TrimSpace(parts[1])
	key := clientKey(r)
	if wait := g.lockedUntil(key); wait > 0 {
		return Decision{Status: http.StatusTooManyRequests, RetryAfter: wait, Reason: "too many failed token attempts"}
	}
	if cached := g.cfg.Cache.Get(); cached != "" && subtle.ConstantTimeCompare([]byte(cached), []byte(token)) == 1 {
		g.succeed(key)
		return Decision{Allowed: true, Status: http.StatusOK, Token: token, Reason: "cached token"}
	}

	err := g.cfg.Verify.VerifyToken(r.Context(), token)
	switch {
	case err == nil:
		g.succeed(key)
		return Decision{Allowed: true, Status: http.StatusOK, Token: token, Reason: "ok"}
	case errors.Is(err, ErrTokenRejected):
		fails := g.fail(key)
		logRejected(key, token, fails, "token ไม่ผ่าน")
		if fails >= g.cfg.MaxFails {
			return Decision{Status: http.StatusTooManyRequests, RetryAfter: g.PenaltyFor(fails), Fails: fails,
				Reason: "token ผิดเกินลิมิต — ลองอีกครั้งหลังหมดเวลาล็อก"}
		}
		return Decision{Status: http.StatusUnauthorized, Fails: fails, Reason: "token ไม่ผ่าน"}
	default:
		logRejected(key, token, 0, "ตรวจ token ไม่ได้: "+err.Error())
		return Decision{Status: http.StatusServiceUnavailable, Reason: "ตรวจ token ไม่ได้ (ข้อมูลชั่วคราว)"}
	}
}

// logRejected records who was turned away and why, so an operator can see a
// 401/503 loop in the daemon log. Only a digest of the token is ever written.
func logRejected(key, token string, fails int, reason string) {
	sum := sha256.Sum256([]byte(token))
	log.Printf("mobile: handshake rejected addr=%s token=%s… fails=%d: %s",
		key, hex.EncodeToString(sum[:4]), fails, reason)
}

// CachedTokens is the RAM copy the gate trusts, so the REST surface can adopt a
// token it just verified through the very same path the socket uses.
func (g *Gate) CachedTokens() TokenCache { return g.cfg.Cache }

// Write renders a rejection with Retry-After. The body never echoes the token.
func (g *Gate) Write(w http.ResponseWriter, d Decision) {
	if d.RetryAfter > 0 {
		seconds := int(d.RetryAfter.Seconds())
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconvItoa(seconds))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(d.Status)
	_, _ = w.Write([]byte(`{"error":"` + d.Reason + `","status":` + strconvItoa(d.Status) + `}`))
}

// PenaltyFor is exported for tests and for operator-facing documentation of the
// schedule: 30 → 60 → 120 → 240 → 300 seconds.
func (g *Gate) PenaltyFor(fails int) time.Duration {
	steps := fails - g.cfg.MaxFails
	if steps < 0 {
		steps = 0
	}
	penalty := g.cfg.BasePenalty
	for i := 0; i < steps && penalty < g.cfg.MaxPenalty; i++ {
		penalty *= 2
	}
	if penalty > g.cfg.MaxPenalty {
		penalty = g.cfg.MaxPenalty
	}
	return penalty
}

// Fails reports the current failure count for an address (operator/debug aid).
func (g *Gate) Fails(addr string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fails[addr]
}

func (g *Gate) lockedUntil(key string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	var wait time.Duration
	if until, ok := g.penalties[key]; ok && until.After(now) {
		wait = time.Until(until)
	}
	if until := g.globalPenalty; until.After(now) && time.Until(until) > wait {
		wait = time.Until(until)
	}
	return wait
}

func (g *Gate) fail(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fails[key]++
	g.globalFails++
	n := g.fails[key]
	if n >= g.cfg.MaxFails {
		g.penalties[key] = time.Now().Add(g.PenaltyFor(n))
		if g.globalFails >= g.cfg.MaxFails*3 {
			g.globalPenalty = time.Now().Add(g.cfg.BasePenalty)
			g.globalFails = 0
		}
	}
	return n
}

func (g *Gate) succeed(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.fails, key)
	delete(g.penalties, key)
	g.globalFails = 0
	g.globalPenalty = time.Time{}
}

// clientKey is the address only. A key that included the token would let an
// attacker dodge the lockout by rotating credentials, which defeats the rule.
func clientKey(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
		return ip
	}
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		if i := strings.IndexByte(forwarded, ','); i >= 0 {
			forwarded = forwarded[:i]
		}
		return strings.TrimSpace(forwarded)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	// Last resort: a stable, non-reversible identifier for the presented token
	// so distinct clients do not share one counter.
	sum := sha256.Sum256([]byte(r.RemoteAddr))
	return "addr:" + hex.EncodeToString(sum[:6])
}

func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
