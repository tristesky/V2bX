package conf

type LimitConfig struct {
	EnableRealtime          bool                     `json:"EnableRealtime"`
	SpeedLimit              int                      `json:"SpeedLimit"`
	IPLimit                 int                      `json:"DeviceLimit"`
	ConnLimit               int                      `json:"ConnLimit"`
	EnableIpRecorder        bool                     `json:"EnableIpRecorder"`
	IpRecorderConfig        *IpReportConfig          `json:"IpRecorderConfig"`
	OnlineIPLimit           *OnlineIPLimitConfig     `json:"OnlineIPLimit"`
	EnableDynamicSpeedLimit bool                     `json:"EnableDynamicSpeedLimit"`
	DynamicSpeedLimitConfig *DynamicSpeedLimitConfig `json:"DynamicSpeedLimitConfig"`
}

type RecorderConfig struct {
	Url     string `json:"Url"`
	Token   string `json:"Token"`
	Timeout int    `json:"Timeout"`
}

type RedisConfig struct {
	Address          string   `json:"Address"`
	Addresses        []string `json:"Addresses"`
	MasterName       string   `json:"MasterName"`
	Username         string   `json:"Username"`
	Password         string   `json:"Password"`
	SentinelUsername string   `json:"SentinelUsername"`
	SentinelPassword string   `json:"SentinelPassword"`
	Db               int      `json:"Db"`
	Expiry           int      `json:"Expiry"`
	TLS              bool     `json:"TLS"`
	PoolSize         int      `json:"PoolSize"`
	MinIdleConns     int      `json:"MinIdleConns"`
}

type IpReportConfig struct {
	Periodic       int             `json:"Periodic"`
	Type           string          `json:"Type"`
	RecorderConfig *RecorderConfig `json:"RecorderConfig"`
	RedisConfig    *RedisConfig    `json:"RedisConfig"`
	EnableIpSync   bool            `json:"EnableIpSync"`
}

type DynamicSpeedLimitConfig struct {
	Periodic   int   `json:"Periodic"`
	Traffic    int64 `json:"Traffic"`
	SpeedLimit int   `json:"SpeedLimit"`
	ExpireTime int   `json:"ExpireTime"`
}

type OnlineIPLimitConfig struct {
	Enable          bool         `json:"Enable"`
	Type            string       `json:"Type"`
	Scope           string       `json:"Scope"`
	KeyPrefix       string       `json:"KeyPrefix"`
	TTL             int          `json:"TTL"`
	RefreshInterval int          `json:"RefreshInterval"`
	RejectCacheTTL  int          `json:"RejectCacheTTL"`
	Timeout         int          `json:"Timeout"`
	FailureCooldown int          `json:"FailureCooldown"`
	IPv6Prefix      int          `json:"IPv6Prefix"`
	RedisConfig     *RedisConfig `json:"RedisConfig"`
}
