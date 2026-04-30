package conf

type LimitConfig struct {
	EnableRealtime bool `json:"EnableRealtime"`
	SpeedLimit     int  `json:"SpeedLimit"`
	IPLimit        int  `json:"DeviceLimit"`
	ConnLimit      int  `json:"ConnLimit"`
	// EnableIpRecorder / IpRecorderConfig were declared on the original
	// upstream limiter but never wired into the data path of this fork.
	// Operators who set them got a silent no-op. They are kept here only
	// so that JSON unmarshal of pre-existing config.json files doesn't
	// fail; node startup logs a deprecation Warn whenever either is set.
	// Remove from your config when convenient.
	EnableIpRecorder        bool                     `json:"EnableIpRecorder"`
	IpRecorderConfig        *IpReportConfig          `json:"IpRecorderConfig"`
	EnableDynamicSpeedLimit bool                     `json:"EnableDynamicSpeedLimit"`
	DynamicSpeedLimitConfig *DynamicSpeedLimitConfig `json:"DynamicSpeedLimitConfig"`
	// MaxReportFailureRollbacks bounds how many consecutive
	// ReportUserTraffic failures will roll bytes back into the core's
	// per-user counter. After this many consecutive failures the bytes
	// are dropped on the floor and a loud Error is logged so the in-
	// core accumulator stays bounded — otherwise the counter grows
	// without limit while the panel is unreachable. Zero or unset means
	// "use the 5-cycle default"; set to a negative value to disable
	// the guard (legacy unbounded behavior).
	MaxReportFailureRollbacks int `json:"MaxReportFailureRollbacks"`
}

type RecorderConfig struct {
	Url     string `json:"Url"`
	Token   string `json:"Token"`
	Timeout int    `json:"Timeout"`
}

type RedisConfig struct {
	Address  string `json:"Address"`
	Password string `json:"Password"`
	Db       int    `json:"Db"`
	Expiry   int    `json:"Expiry"`
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
