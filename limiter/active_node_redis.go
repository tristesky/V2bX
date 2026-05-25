package limiter

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/InazumaV/V2bX/conf"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
)

const defaultActiveNodeKeyPrefix = "v2bx:active_node"

type activeNodeDecision uint8

const (
	activeNodeRejected activeNodeDecision = iota
	activeNodeAdmitted
	activeNodeObserve
)

var redisActiveNodeCheckScript = redis.NewScript(`
local key = KEYS[1]
local order_key = KEYS[2]
local blocked_key = KEYS[3]
local enforced_key = KEYS[4]
local now = tonumber(ARGV[1])
local admitted_at = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
local expire = tonumber(ARGV[4])
local block_ttl = tonumber(ARGV[5])
local limit = tonumber(ARGV[6])
local node = ARGV[7]

local expired = redis.call("ZRANGEBYSCORE", key, 0, now - ttl)
for _, member in ipairs(expired) do
	redis.call("ZREM", order_key, member)
end
redis.call("ZREMRANGEBYSCORE", key, 0, now - ttl)
redis.call("ZREMRANGEBYSCORE", blocked_key, 0, now)

if redis.call("ZSCORE", key, node) then
	return {1, redis.call("ZCARD", key)}
end

if redis.call("ZSCORE", blocked_key, node) then
	local count = redis.call("ZCARD", key)
	if count < limit then
		redis.call("ZREM", blocked_key, node)
		redis.call("DEL", enforced_key)
		return {2, count}
	end
	redis.call("ZADD", blocked_key, now + block_ttl, node)
	redis.call("PEXPIRE", blocked_key, block_ttl + 30000)
	redis.call("SET", enforced_key, "1", "PX", block_ttl)
	return {0, count}
end

if redis.call("EXISTS", enforced_key) == 1 then
	local count = redis.call("ZCARD", key)
	if count < limit then
		redis.call("DEL", enforced_key)
		return {2, count}
	end
	redis.call("ZADD", blocked_key, now + block_ttl, node)
	redis.call("PEXPIRE", blocked_key, block_ttl + 30000)
	redis.call("PEXPIRE", enforced_key, block_ttl)
	return {0, count}
end

return {2, redis.call("ZCARD", key)}
`)

var redisActiveNodeActivateScript = redis.NewScript(`
local key = KEYS[1]
local order_key = KEYS[2]
local blocked_key = KEYS[3]
local enforced_key = KEYS[4]
local now = tonumber(ARGV[1])
local admitted_at = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
local expire = tonumber(ARGV[4])
local block_ttl = tonumber(ARGV[5])
local limit = tonumber(ARGV[6])
local node = ARGV[7]

local expired = redis.call("ZRANGEBYSCORE", key, 0, now - ttl)
for _, member in ipairs(expired) do
	redis.call("ZREM", order_key, member)
end
redis.call("ZREMRANGEBYSCORE", key, 0, now - ttl)
redis.call("ZREMRANGEBYSCORE", blocked_key, 0, now)

if redis.call("ZSCORE", key, node) then
	redis.call("ZADD", key, now, node)
	redis.call("ZREM", blocked_key, node)
	redis.call("PEXPIRE", key, expire)
	redis.call("PEXPIRE", order_key, expire)
	return {1, redis.call("ZCARD", key)}
end

local count = redis.call("ZCARD", key)
if count < limit then
	redis.call("ZADD", key, now, node)
	redis.call("ZADD", order_key, admitted_at, node)
	redis.call("ZREM", blocked_key, node)
	redis.call("DEL", enforced_key)
	redis.call("PEXPIRE", key, expire)
	redis.call("PEXPIRE", order_key, expire)
	return {1, count + 1}
end

redis.call("ZADD", blocked_key, now + block_ttl, node)
redis.call("PEXPIRE", blocked_key, block_ttl + 30000)
redis.call("SET", enforced_key, "1", "PX", block_ttl)
return {0, count}
`)

var redisActiveNodeRenewScript = redis.NewScript(`
local key = KEYS[1]
local order_key = KEYS[2]
local blocked_key = KEYS[3]
local enforced_key = KEYS[4]
local now = tonumber(ARGV[1])
local admitted_at = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
local expire = tonumber(ARGV[4])
local block_ttl = tonumber(ARGV[5])
local limit = tonumber(ARGV[6])
local node = ARGV[7]

local expired = redis.call("ZRANGEBYSCORE", key, 0, now - ttl)
for _, member in ipairs(expired) do
	redis.call("ZREM", order_key, member)
end
redis.call("ZREMRANGEBYSCORE", key, 0, now - ttl)
redis.call("ZREMRANGEBYSCORE", blocked_key, 0, now)
redis.call("ZADD", key, now, node)
redis.call("ZADD", order_key, "NX", admitted_at, node)

local allowed = 1
local count = redis.call("ZCARD", key)
if limit > 0 and count > limit then
	local excess = count - limit
	local dropped = redis.call("ZREVRANGE", order_key, 0, excess - 1)
	for _, member in ipairs(dropped) do
		redis.call("ZREM", key, member)
		redis.call("ZREM", order_key, member)
		redis.call("ZADD", blocked_key, now + block_ttl, member)
		if member == node then
			allowed = 0
		end
	end
	redis.call("SET", enforced_key, "1", "PX", block_ttl)
end

if allowed == 1 then
	redis.call("ZREM", blocked_key, node)
end
redis.call("PEXPIRE", key, expire)
redis.call("PEXPIRE", order_key, expire)
redis.call("PEXPIRE", blocked_key, block_ttl + 30000)
return {allowed, redis.call("ZCARD", key)}
`)

var redisActiveNodeReleaseScript = redis.NewScript(`
redis.call("ZREM", KEYS[1], ARGV[1])
redis.call("ZREM", KEYS[2], ARGV[1])
return 1
`)

type activeNodeStore interface {
	Check(identity onlineIPIdentity, nodeID string, limit int) activeNodeDecision
	Activate(identity onlineIPIdentity, nodeID string, limit int, admittedAtUnixMicro int64) activeNodeDecision
	Renew(identity onlineIPIdentity, nodeID string, limit int, admittedAtUnixMicro int64) bool
	Release(identity onlineIPIdentity, nodeID string)
	ActivationDelay() time.Duration
	RenewInterval() time.Duration
	Close() error
}

type failOpenActiveNodeStore struct {
	activationDelay time.Duration
}

func (s failOpenActiveNodeStore) Check(identity onlineIPIdentity, nodeID string, limit int) activeNodeDecision {
	return activeNodeObserve
}

func (s failOpenActiveNodeStore) Activate(identity onlineIPIdentity, nodeID string, limit int, admittedAtUnixMicro int64) activeNodeDecision {
	return activeNodeObserve
}

func (s failOpenActiveNodeStore) Renew(identity onlineIPIdentity, nodeID string, limit int, admittedAtUnixMicro int64) bool {
	return true
}

func (s failOpenActiveNodeStore) Release(identity onlineIPIdentity, nodeID string) {}

func (s failOpenActiveNodeStore) ActivationDelay() time.Duration {
	return s.activationDelay
}

func (s failOpenActiveNodeStore) RenewInterval() time.Duration {
	return 0
}

func (s failOpenActiveNodeStore) Close() error {
	return nil
}

type activeNodeCacheEntry struct {
	decision     activeNodeDecision
	expireUnixNS int64
}

type redisActiveNodeStore struct {
	client          redis.UniversalClient
	keyPrefix       string
	scopeHash       string
	ttl             time.Duration
	expire          time.Duration
	activationDelay time.Duration
	blockTTL        time.Duration
	refreshInterval time.Duration
	rejectCacheTTL  time.Duration
	timeout         time.Duration
	failureCooldown time.Duration
	cache           sync.Map
	failUntil       atomic.Int64
	lastFailLog     atomic.Int64
}

func newActiveNodeStore(c *conf.ActiveNodeLimitConfig, fallback *conf.OnlineIPLimitConfig, defaultScope string) (activeNodeStore, error) {
	if c == nil || !c.Enable {
		return nil, nil
	}

	resolved := *c
	if fallback != nil {
		if resolved.Type == "" {
			resolved.Type = fallback.Type
		}
		if resolved.Scope == "" {
			resolved.Scope = fallback.Scope
		}
		if resolved.TTL == 0 {
			resolved.TTL = fallback.TTL
		}
		if resolved.RefreshInterval == 0 {
			resolved.RefreshInterval = fallback.RefreshInterval
		}
		if resolved.RejectCacheTTL == 0 {
			resolved.RejectCacheTTL = fallback.RejectCacheTTL
		}
		if resolved.Timeout == 0 {
			resolved.Timeout = fallback.Timeout
		}
		if resolved.FailureCooldown == 0 {
			resolved.FailureCooldown = fallback.FailureCooldown
		}
		if resolved.RedisConfig == nil {
			resolved.RedisConfig = fallback.RedisConfig
		}
	}

	if resolved.Type != "" && !strings.EqualFold(resolved.Type, "redis") {
		return nil, fmt.Errorf("unsupported active node limit type: %s", resolved.Type)
	}
	if resolved.RedisConfig == nil {
		return nil, errors.New("active node redis config is empty")
	}
	return newRedisActiveNodeStore(&resolved, defaultScope)
}

func newRedisActiveNodeStore(c *conf.ActiveNodeLimitConfig, defaultScope string) (*redisActiveNodeStore, error) {
	rc := c.RedisConfig
	addrs := make([]string, 0, len(rc.Addresses)+1)
	if rc.Address != "" {
		addrs = append(addrs, rc.Address)
	}
	addrs = append(addrs, rc.Addresses...)
	if len(addrs) == 0 {
		return nil, errors.New("active node redis address is empty")
	}

	timeout := durationWithDefaultMS(c.Timeout, 200)
	var tlsConfig *tls.Config
	if rc.TLS {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	client := redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:            addrs,
		MasterName:       rc.MasterName,
		Username:         rc.Username,
		Password:         rc.Password,
		SentinelUsername: rc.SentinelUsername,
		SentinelPassword: rc.SentinelPassword,
		DB:               rc.Db,
		DialTimeout:      timeout,
		ReadTimeout:      timeout,
		WriteTimeout:     timeout,
		PoolSize:         rc.PoolSize,
		MinIdleConns:     rc.MinIdleConns,
		TLSConfig:        tlsConfig,
	})

	scope := c.Scope
	if scope == "" {
		scope = defaultScope
	}
	if scope == "" {
		scope = "default"
	}
	ttl := durationWithDefaultSeconds(c.TTL, 120)
	refreshInterval := durationWithDefaultSeconds(c.RefreshInterval, 20)
	if refreshInterval >= ttl {
		refreshInterval = ttl / 3
	}
	if refreshInterval <= 0 {
		refreshInterval = 20 * time.Second
	}
	keyPrefix := strings.TrimSuffix(c.KeyPrefix, ":")
	if keyPrefix == "" {
		keyPrefix = defaultActiveNodeKeyPrefix
	}

	return &redisActiveNodeStore{
		client:          client,
		keyPrefix:       keyPrefix,
		scopeHash:       shortHash(scope),
		ttl:             ttl,
		expire:          ttl + 30*time.Second,
		activationDelay: durationWithDefaultSeconds(c.ActivationDelay, 60),
		blockTTL:        durationWithDefaultSeconds(c.BlockTTL, 600),
		refreshInterval: refreshInterval,
		rejectCacheTTL:  durationWithDefaultSeconds(c.RejectCacheTTL, 3),
		timeout:         timeout,
		failureCooldown: durationWithDefaultSeconds(c.FailureCooldown, 30),
	}, nil
}

func (s *redisActiveNodeStore) Check(identity onlineIPIdentity, nodeID string, limit int) activeNodeDecision {
	if limit <= 0 || nodeID == "" {
		return activeNodeObserve
	}
	if time.Now().UnixNano() < s.failUntil.Load() {
		return activeNodeObserve
	}
	key := s.redisKey(identity)
	cacheKey := s.cacheKey(key, nodeID, limit)
	if decision, ok := s.cacheGet(cacheKey); ok {
		return decision
	}

	result, err := s.runDecisionScript(redisActiveNodeCheckScript, identity, nodeID, limit, time.Now().UnixMicro())
	if err != nil {
		s.markFailure(err)
		return activeNodeObserve
	}
	s.cacheDecision(cacheKey, result)
	return result
}

func (s *redisActiveNodeStore) Activate(identity onlineIPIdentity, nodeID string, limit int, admittedAtUnixMicro int64) activeNodeDecision {
	if limit <= 0 || nodeID == "" {
		return activeNodeObserve
	}
	if time.Now().UnixNano() < s.failUntil.Load() {
		return activeNodeObserve
	}

	result, err := s.runDecisionScript(redisActiveNodeActivateScript, identity, nodeID, limit, admittedAtUnixMicro)
	if err != nil {
		s.markFailure(err)
		return activeNodeObserve
	}
	s.cacheDecision(s.cacheKey(s.redisKey(identity), nodeID, limit), result)
	return result
}

func (s *redisActiveNodeStore) Renew(identity onlineIPIdentity, nodeID string, limit int, admittedAtUnixMicro int64) bool {
	if time.Now().UnixNano() < s.failUntil.Load() {
		return true
	}
	result, err := s.runDecisionScript(redisActiveNodeRenewScript, identity, nodeID, limit, admittedAtUnixMicro)
	if err != nil {
		s.markFailure(err)
		return true
	}
	s.cacheDecision(s.cacheKey(s.redisKey(identity), nodeID, limit), result)
	return result != activeNodeRejected
}

func (s *redisActiveNodeStore) Release(identity onlineIPIdentity, nodeID string) {
	if nodeID == "" || time.Now().UnixNano() < s.failUntil.Load() {
		return
	}
	key := s.redisKey(identity)
	s.deleteCachedNodeDecisions(key, nodeID)
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	if _, err := redisActiveNodeReleaseScript.Run(ctx, s.client, []string{key, s.orderKey(identity)}, nodeID).Result(); err != nil {
		s.markFailure(err)
	}
}

func (s *redisActiveNodeStore) runDecisionScript(script *redis.Script, identity onlineIPIdentity, nodeID string, limit int, admittedAtUnixMicro int64) (activeNodeDecision, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	result, err := script.Run(ctx, s.client, []string{s.redisKey(identity), s.orderKey(identity), s.blockedKey(identity), s.enforcedKey(identity)},
		time.Now().UnixMilli(),
		admittedAtUnixMicro,
		s.ttl.Milliseconds(),
		s.expire.Milliseconds(),
		s.blockTTL.Milliseconds(),
		limit,
		nodeID,
	).Result()
	if err != nil {
		return activeNodeObserve, err
	}
	return parseActiveNodeDecision(result)
}

func (s *redisActiveNodeStore) ActivationDelay() time.Duration {
	return s.activationDelay
}

func (s *redisActiveNodeStore) RenewInterval() time.Duration {
	return s.refreshInterval
}

func (s *redisActiveNodeStore) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

func (s *redisActiveNodeStore) redisKey(identity onlineIPIdentity) string {
	userKey := strconv.Itoa(identity.UID)
	if identity.UID == 0 {
		userKey = shortHash(identity.UUID)
	}
	return s.keyPrefix + ":" + s.scopeHash + ":" + userKey
}

func (s *redisActiveNodeStore) orderKey(identity onlineIPIdentity) string {
	return s.redisKey(identity) + ":order"
}

func (s *redisActiveNodeStore) blockedKey(identity onlineIPIdentity) string {
	return s.redisKey(identity) + ":blocked"
}

func (s *redisActiveNodeStore) enforcedKey(identity onlineIPIdentity) string {
	return s.redisKey(identity) + ":enforced"
}

func (s *redisActiveNodeStore) cacheKey(key, nodeID string, limit int) string {
	return key + "|" + nodeID + "|" + strconv.Itoa(limit)
}

func (s *redisActiveNodeStore) cacheGet(key string) (activeNodeDecision, bool) {
	value, ok := s.cache.Load(key)
	if !ok {
		return activeNodeObserve, false
	}
	entry := value.(activeNodeCacheEntry)
	if time.Now().UnixNano() >= entry.expireUnixNS {
		s.cache.Delete(key)
		return activeNodeObserve, false
	}
	return entry.decision, true
}

func (s *redisActiveNodeStore) cacheDecision(key string, decision activeNodeDecision) {
	ttl := s.rejectCacheTTL
	if decision == activeNodeAdmitted {
		ttl = s.refreshInterval
	}
	if ttl <= 0 {
		return
	}
	s.cache.Store(key, activeNodeCacheEntry{decision: decision, expireUnixNS: time.Now().Add(ttl).UnixNano()})
}

func (s *redisActiveNodeStore) deleteCachedNodeDecisions(key, nodeID string) {
	prefix := key + "|" + nodeID + "|"
	s.cache.Range(func(cacheKey, value interface{}) bool {
		if strings.HasPrefix(cacheKey.(string), prefix) {
			s.cache.Delete(cacheKey)
		}
		return true
	})
}

func (s *redisActiveNodeStore) markFailure(err error) {
	now := time.Now()
	s.failUntil.Store(now.Add(s.failureCooldown).UnixNano())
	nowNS := now.UnixNano()
	last := s.lastFailLog.Load()
	if nowNS-last >= s.failureCooldown.Nanoseconds() && s.lastFailLog.CompareAndSwap(last, nowNS) {
		log.WithError(err).Warn("active node redis unavailable, fail-open")
	}
}

func parseActiveNodeDecision(result interface{}) (activeNodeDecision, error) {
	values, ok := result.([]interface{})
	if !ok || len(values) == 0 {
		return activeNodeObserve, fmt.Errorf("unexpected redis active node result: %T", result)
	}
	var decision int64
	switch value := values[0].(type) {
	case int64:
		decision = value
	case int:
		decision = int64(value)
	case string:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return activeNodeObserve, err
		}
		decision = parsed
	case []byte:
		parsed, err := strconv.ParseInt(string(value), 10, 64)
		if err != nil {
			return activeNodeObserve, err
		}
		decision = parsed
	default:
		return activeNodeObserve, fmt.Errorf("unexpected redis active node decision: %T", values[0])
	}
	if decision < int64(activeNodeRejected) || decision > int64(activeNodeObserve) {
		return activeNodeObserve, fmt.Errorf("unexpected redis active node decision value: %d", decision)
	}
	return activeNodeDecision(decision), nil
}
