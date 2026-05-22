package limiter

import (
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/InazumaV/V2bX/conf"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
)

const defaultOnlineIPKeyPrefix = "v2bx:online_ip"

var redisOnlineIPScript = redis.NewScript(`
local key = KEYS[1]
local order_key = KEYS[2]
local now = tonumber(ARGV[1])
local admitted_at = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
local expire = tonumber(ARGV[4])
local limit = tonumber(ARGV[5])
local ip = ARGV[6]

local expired = redis.call("ZRANGEBYSCORE", key, 0, now - ttl)
for _, member in ipairs(expired) do
	redis.call("ZREM", order_key, member)
end
redis.call("ZREMRANGEBYSCORE", key, 0, now - ttl)

if redis.call("ZSCORE", key, ip) then
	redis.call("ZADD", key, now, ip)
	redis.call("ZADD", order_key, "NX", admitted_at, ip)
	redis.call("PEXPIRE", key, expire)
	redis.call("PEXPIRE", order_key, expire)
	return {1, redis.call("ZCARD", key)}
end

local count = redis.call("ZCARD", key)
if count < limit then
	redis.call("ZADD", key, now, ip)
	redis.call("ZADD", order_key, admitted_at, ip)
	redis.call("PEXPIRE", key, expire)
	redis.call("PEXPIRE", order_key, expire)
	return {1, count + 1}
end

redis.call("PEXPIRE", key, expire)
redis.call("PEXPIRE", order_key, expire)
return {0, count}
`)

var redisOnlineIPRenewScript = redis.NewScript(`
local key = KEYS[1]
local order_key = KEYS[2]
local now = tonumber(ARGV[1])
local admitted_at = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
local expire = tonumber(ARGV[4])
local limit = tonumber(ARGV[5])
local ip = ARGV[6]

local expired = redis.call("ZRANGEBYSCORE", key, 0, now - ttl)
for _, member in ipairs(expired) do
	redis.call("ZREM", order_key, member)
end
redis.call("ZREMRANGEBYSCORE", key, 0, now - ttl)
redis.call("ZADD", key, now, ip)
redis.call("ZADD", order_key, "NX", admitted_at, ip)

local allowed = 1
local count = redis.call("ZCARD", key)
if limit > 0 and count > limit then
	local excess = count - limit
	local dropped = redis.call("ZREVRANGE", order_key, 0, excess - 1)
	for _, member in ipairs(dropped) do
		redis.call("ZREM", key, member)
		redis.call("ZREM", order_key, member)
		if member == ip then
			allowed = 0
		end
	end
end

redis.call("PEXPIRE", key, expire)
redis.call("PEXPIRE", order_key, expire)
return {allowed, redis.call("ZCARD", key)}
`)

type onlineIPIdentity struct {
	UID  int
	UUID string
}

type onlineIPStore interface {
	Allow(identity onlineIPIdentity, ip string, limit int) bool
	Renew(identity onlineIPIdentity, ip string, limit int, admittedAtUnixMicro int64) bool
	RenewInterval() time.Duration
	Close() error
}

type failOpenOnlineIPStore struct{}

func (failOpenOnlineIPStore) Allow(identity onlineIPIdentity, ip string, limit int) bool {
	return true
}

func (failOpenOnlineIPStore) Renew(identity onlineIPIdentity, ip string, limit int, admittedAtUnixMicro int64) bool {
	return true
}

func (failOpenOnlineIPStore) RenewInterval() time.Duration {
	return 0
}

func (failOpenOnlineIPStore) Close() error {
	return nil
}

type onlineIPCacheEntry struct {
	Allowed      bool
	ExpireUnixNS int64
}

type redisOnlineIPStore struct {
	client          redis.UniversalClient
	keyPrefix       string
	scopeHash       string
	ttl             time.Duration
	expire          time.Duration
	refreshInterval time.Duration
	rejectCacheTTL  time.Duration
	timeout         time.Duration
	failureCooldown time.Duration
	ipv6Prefix      int
	cache           sync.Map
	failUntil       atomic.Int64
	lastFailLog     atomic.Int64
}

func newOnlineIPStore(c *conf.OnlineIPLimitConfig, defaultScope string) (onlineIPStore, error) {
	if c == nil || !c.Enable {
		return nil, nil
	}
	if c.Type != "" && !strings.EqualFold(c.Type, "redis") {
		return nil, fmt.Errorf("unsupported online ip limit type: %s", c.Type)
	}
	if c.RedisConfig == nil {
		return nil, errors.New("online ip redis config is empty")
	}
	return newRedisOnlineIPStore(c, defaultScope)
}

func newRedisOnlineIPStore(c *conf.OnlineIPLimitConfig, defaultScope string) (*redisOnlineIPStore, error) {
	rc := c.RedisConfig
	addrs := make([]string, 0, len(rc.Addresses)+1)
	if rc.Address != "" {
		addrs = append(addrs, rc.Address)
	}
	addrs = append(addrs, rc.Addresses...)
	if len(addrs) == 0 {
		return nil, errors.New("online ip redis address is empty")
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
		keyPrefix = defaultOnlineIPKeyPrefix
	}

	ipv6Prefix := c.IPv6Prefix
	if ipv6Prefix <= 0 || ipv6Prefix > 128 {
		ipv6Prefix = 128
	}

	return &redisOnlineIPStore{
		client:          client,
		keyPrefix:       keyPrefix,
		scopeHash:       shortHash(scope),
		ttl:             ttl,
		expire:          ttl + 30*time.Second,
		refreshInterval: refreshInterval,
		rejectCacheTTL:  durationWithDefaultSeconds(c.RejectCacheTTL, 3),
		timeout:         timeout,
		failureCooldown: durationWithDefaultSeconds(c.FailureCooldown, 30),
		ipv6Prefix:      ipv6Prefix,
	}, nil
}

func (s *redisOnlineIPStore) Allow(identity onlineIPIdentity, ip string, limit int) bool {
	if limit <= 0 {
		return true
	}
	if time.Now().UnixNano() < s.failUntil.Load() {
		return true
	}

	normalizedIP := normalizeOnlineIP(ip, s.ipv6Prefix)
	key := s.redisKey(identity)
	cacheKey := s.allowCacheKey(key, normalizedIP, limit)
	if allowed, ok := s.cacheGet(cacheKey); ok {
		return allowed
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	now := time.Now().UnixMilli()
	res, err := redisOnlineIPScript.Run(ctx, s.client, []string{key, s.redisOrderKey(identity)},
		now,
		time.Now().UnixMicro(),
		s.ttl.Milliseconds(),
		s.expire.Milliseconds(),
		limit,
		normalizedIP,
	).Result()
	if err != nil {
		s.markFailure(err)
		return true
	}

	allowed, err := parseRedisOnlineIPResult(res)
	if err != nil {
		s.markFailure(err)
		return true
	}

	if allowed {
		s.cacheStore(cacheKey, true, s.refreshInterval)
	} else {
		s.cacheStore(cacheKey, false, s.rejectCacheTTL)
	}
	return allowed
}

func (s *redisOnlineIPStore) Renew(identity onlineIPIdentity, ip string, limit int, admittedAtUnixMicro int64) bool {
	if time.Now().UnixNano() < s.failUntil.Load() {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	res, err := redisOnlineIPRenewScript.Run(ctx, s.client, []string{s.redisKey(identity), s.redisOrderKey(identity)},
		time.Now().UnixMilli(),
		admittedAtUnixMicro,
		s.ttl.Milliseconds(),
		s.expire.Milliseconds(),
		limit,
		normalizeOnlineIP(ip, s.ipv6Prefix),
	).Result()
	if err != nil {
		s.markFailure(err)
		return true
	}

	allowed, err := parseRedisOnlineIPResult(res)
	if err != nil {
		s.markFailure(err)
		return true
	}
	if !allowed {
		s.cache.Delete(s.allowCacheKey(s.redisKey(identity), normalizeOnlineIP(ip, s.ipv6Prefix), limit))
	}
	return allowed
}

func (s *redisOnlineIPStore) RenewInterval() time.Duration {
	return s.refreshInterval
}

func (s *redisOnlineIPStore) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

func (s *redisOnlineIPStore) redisKey(identity onlineIPIdentity) string {
	userKey := strconv.Itoa(identity.UID)
	if identity.UID == 0 {
		userKey = shortHash(identity.UUID)
	}
	return s.keyPrefix + ":" + s.scopeHash + ":" + userKey
}

func (s *redisOnlineIPStore) redisOrderKey(identity onlineIPIdentity) string {
	return s.redisKey(identity) + ":order"
}

func (s *redisOnlineIPStore) allowCacheKey(key string, normalizedIP string, limit int) string {
	return key + "|" + normalizedIP + "|" + strconv.Itoa(limit)
}

func (s *redisOnlineIPStore) cacheGet(key string) (bool, bool) {
	v, ok := s.cache.Load(key)
	if !ok {
		return false, false
	}
	entry := v.(onlineIPCacheEntry)
	if time.Now().UnixNano() >= entry.ExpireUnixNS {
		s.cache.Delete(key)
		return false, false
	}
	return entry.Allowed, true
}

func (s *redisOnlineIPStore) cacheStore(key string, allowed bool, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	s.cache.Store(key, onlineIPCacheEntry{
		Allowed:      allowed,
		ExpireUnixNS: time.Now().Add(ttl).UnixNano(),
	})
}

func (s *redisOnlineIPStore) markFailure(err error) {
	now := time.Now()
	s.failUntil.Store(now.Add(s.failureCooldown).UnixNano())
	nowNS := now.UnixNano()
	last := s.lastFailLog.Load()
	if nowNS-last < s.failureCooldown.Nanoseconds() {
		return
	}
	if s.lastFailLog.CompareAndSwap(last, nowNS) {
		log.WithError(err).Warn("online ip redis unavailable, fail-open")
	}
}

func parseRedisOnlineIPResult(res interface{}) (bool, error) {
	values, ok := res.([]interface{})
	if !ok || len(values) == 0 {
		return false, fmt.Errorf("unexpected redis online ip result: %T", res)
	}
	switch v := values[0].(type) {
	case int64:
		return v == 1, nil
	case int:
		return v == 1, nil
	case string:
		return v == "1", nil
	case []byte:
		return string(v) == "1", nil
	default:
		return false, fmt.Errorf("unexpected redis online ip allow value: %T", values[0])
	}
}

func normalizeOnlineIP(ip string, ipv6Prefix int) string {
	ip = strings.TrimPrefix(ip, "::ffff:")
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	if addr.Is6() && ipv6Prefix < 128 {
		prefix, err := addr.Prefix(ipv6Prefix)
		if err == nil {
			return prefix.Masked().String()
		}
	}
	return addr.String()
}

func durationWithDefaultSeconds(value, fallback int) time.Duration {
	if value <= 0 {
		value = fallback
	}
	return time.Duration(value) * time.Second
}

func durationWithDefaultMS(value, fallback int) time.Duration {
	if value <= 0 {
		value = fallback
	}
	return time.Duration(value) * time.Millisecond
}

func shortHash(value string) string {
	sum := sha1.Sum([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}
