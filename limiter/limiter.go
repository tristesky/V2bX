package limiter

import (
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/InazumaV/V2bX/api/panel"
	"github.com/InazumaV/V2bX/common/format"
	"github.com/InazumaV/V2bX/conf"
	"github.com/juju/ratelimit"
	log "github.com/sirupsen/logrus"
)

var limitLock sync.RWMutex
var limiter map[string]*Limiter

func Init() {
	limiter = map[string]*Limiter{}
}

type Limiter struct {
	DomainRules    []*regexp.Regexp
	ProtocolRules  []string
	SpeedLimit     int
	DeviceLimit    int
	UserOnlineIP   *sync.Map      // Key: TagUUID, value: {Key: Ip, value: Uid}
	OldUserOnline  *sync.Map      // Key: Ip, value: Uid
	UUIDtoUID      map[string]int // Key: UUID, value: Uid
	UserLimitInfo  *sync.Map      // Key: TagUUID value: UserLimitInfo
	SpeedLimiter   *sync.Map      // key: TagUUID, value: *ratelimit.Bucket
	AliveList      map[int]int    // Key: Uid, value: alive_ip
	OnlineIPStore  onlineIPStore
	onlineIPLeases *onlineIPLeaseTracker
}

type UserLimitInfo struct {
	UID               int
	UUID              string
	SpeedLimit        int
	DeviceLimit       int
	DynamicSpeedLimit int
	ExpireTime        int64
	OverLimit         bool
}

func AddLimiter(tag string, l *conf.LimitConfig, users []panel.UserInfo, aliveList map[int]int, defaultScopes ...string) *Limiter {
	defaultScope := ""
	if len(defaultScopes) > 0 {
		defaultScope = defaultScopes[0]
	}
	info := &Limiter{
		SpeedLimit:    l.SpeedLimit,
		DeviceLimit:   l.IPLimit,
		UserOnlineIP:  new(sync.Map),
		UserLimitInfo: new(sync.Map),
		SpeedLimiter:  new(sync.Map),
		AliveList:     aliveList,
		OldUserOnline: new(sync.Map),
	}
	uuidmap := make(map[string]int)
	for i := range users {
		uuidmap[users[i].Uuid] = users[i].Id
		userLimit := &UserLimitInfo{}
		userLimit.UID = users[i].Id
		userLimit.UUID = users[i].Uuid
		if users[i].SpeedLimit != 0 {
			userLimit.SpeedLimit = users[i].SpeedLimit
		}
		if users[i].DeviceLimit != 0 {
			userLimit.DeviceLimit = users[i].DeviceLimit
		}
		userLimit.OverLimit = false
		info.UserLimitInfo.Store(format.UserTag(tag, users[i].Uuid), userLimit)
	}
	info.UUIDtoUID = uuidmap
	store, err := newOnlineIPStore(l.OnlineIPLimit, defaultScope)
	if err != nil {
		log.WithField("tag", tag).WithError(err).Warn("init online ip limiter failed, fail-open")
		store = failOpenOnlineIPStore{}
	}
	info.OnlineIPStore = store
	info.onlineIPLeases = newOnlineIPLeaseTracker(store)
	limitLock.Lock()
	limiter[tag] = info
	limitLock.Unlock()
	return info
}

func GetLimiter(tag string) (info *Limiter, err error) {
	limitLock.RLock()
	info, ok := limiter[tag]
	limitLock.RUnlock()
	if !ok {
		return nil, errors.New("not found")
	}
	return info, nil
}

func DeleteLimiter(tag string) {
	limitLock.Lock()
	info := limiter[tag]
	delete(limiter, tag)
	limitLock.Unlock()
	if info != nil {
		if info.onlineIPLeases != nil {
			info.onlineIPLeases.Close()
		}
	}
	if info != nil && info.OnlineIPStore != nil {
		if err := info.OnlineIPStore.Close(); err != nil {
			log.WithField("tag", tag).WithError(err).Warn("close online ip limiter failed")
		}
	}
}

func (l *Limiter) UpdateUser(tag string, added []panel.UserInfo, deleted []panel.UserInfo) {
	for i := range deleted {
		l.UserLimitInfo.Delete(format.UserTag(tag, deleted[i].Uuid))
		l.UserOnlineIP.Delete(format.UserTag(tag, deleted[i].Uuid))
		l.SpeedLimiter.Delete(format.UserTag(tag, deleted[i].Uuid))
		delete(l.UUIDtoUID, deleted[i].Uuid)
		delete(l.AliveList, deleted[i].Id)
	}
	for i := range added {
		userLimit := &UserLimitInfo{
			UID:  added[i].Id,
			UUID: added[i].Uuid,
		}
		if added[i].SpeedLimit != 0 {
			userLimit.SpeedLimit = added[i].SpeedLimit
			userLimit.ExpireTime = 0
		}
		if added[i].DeviceLimit != 0 {
			userLimit.DeviceLimit = added[i].DeviceLimit
		}
		userLimit.OverLimit = false
		l.UserLimitInfo.Store(format.UserTag(tag, added[i].Uuid), userLimit)
		l.UUIDtoUID[added[i].Uuid] = added[i].Id
	}
}

func (l *Limiter) UpdateDynamicSpeedLimit(tag, uuid string, limit int, expire time.Time) error {
	if v, ok := l.UserLimitInfo.Load(format.UserTag(tag, uuid)); ok {
		info := v.(*UserLimitInfo)
		info.DynamicSpeedLimit = limit
		info.ExpireTime = expire.Unix()
	} else {
		return errors.New("not found")
	}
	return nil
}

func (l *Limiter) CheckLimit(taguuid string, ip string, isTcp bool, noSSUDP bool) (Bucket *ratelimit.Bucket, Reject bool) {
	// check if ipv4 mapped ipv6
	ip = strings.TrimPrefix(ip, "::ffff:")

	// check and gen speed limit Bucket
	nodeLimit := l.SpeedLimit
	userLimit := 0
	deviceLimit := 0
	var uid int
	if v, ok := l.UserLimitInfo.Load(taguuid); ok {
		u := v.(*UserLimitInfo)
		deviceLimit = u.DeviceLimit
		uid = u.UID
		if deviceLimit == 0 {
			deviceLimit = l.DeviceLimit
		}
		if u.ExpireTime < time.Now().Unix() && u.ExpireTime != 0 {
			if u.SpeedLimit != 0 {
				userLimit = u.SpeedLimit
				u.DynamicSpeedLimit = 0
				u.ExpireTime = 0
			} else {
				l.UserLimitInfo.Delete(taguuid)
			}
		} else {
			userLimit = determineSpeedLimit(u.SpeedLimit, u.DynamicSpeedLimit)
		}
	} else {
		return nil, true
	}
	if noSSUDP {
		if l.OnlineIPStore != nil && deviceLimit > 0 {
			uuid := ""
			if v, ok := l.UserLimitInfo.Load(taguuid); ok {
				uuid = v.(*UserLimitInfo).UUID
			}
			if !l.OnlineIPStore.Allow(onlineIPIdentity{UID: uid, UUID: uuid}, ip, deviceLimit) {
				return nil, true
			}
			l.storeOnlineIP(taguuid, ip, uid)
		} else if l.checkLocalOnlineIPLimit(taguuid, ip, uid, deviceLimit) {
			return nil, true
		}
	}

	limit := int64(determineSpeedLimit(nodeLimit, userLimit)) * 1000000 / 8 // If you need the Speed limit
	if limit > 0 {
		Bucket = ratelimit.NewBucketWithQuantum(time.Second, limit, limit) // Byte/s
		if v, ok := l.SpeedLimiter.LoadOrStore(taguuid, Bucket); ok {
			return v.(*ratelimit.Bucket), false
		} else {
			l.SpeedLimiter.Store(taguuid, Bucket)
			return Bucket, false
		}
	} else {
		return nil, false
	}
}

func (l *Limiter) storeOnlineIP(taguuid string, ip string, uid int) {
	newipMap := new(sync.Map)
	newipMap.Store(ip, uid)
	if v, loaded := l.UserOnlineIP.LoadOrStore(taguuid, newipMap); loaded {
		v.(*sync.Map).Store(ip, uid)
	}
}

func (l *Limiter) AcquireOnlineIPLease(taguuid string, ip string, disconnect func()) func() {
	if l.onlineIPLeases == nil {
		return noopOnlineIPLeaseRelease
	}

	v, ok := l.UserLimitInfo.Load(taguuid)
	if !ok {
		return noopOnlineIPLeaseRelease
	}

	u := v.(*UserLimitInfo)
	deviceLimit := u.DeviceLimit
	if deviceLimit == 0 {
		deviceLimit = l.DeviceLimit
	}
	if deviceLimit <= 0 {
		return noopOnlineIPLeaseRelease
	}

	return l.onlineIPLeases.Acquire(onlineIPIdentity{UID: u.UID, UUID: u.UUID}, strings.TrimPrefix(ip, "::ffff:"), deviceLimit, disconnect)
}

func (l *Limiter) checkLocalOnlineIPLimit(taguuid string, ip string, uid int, deviceLimit int) (reject bool) {
	newipMap := new(sync.Map)
	newipMap.Store(ip, uid)
	aliveIp := l.AliveList[uid]
	if v, loaded := l.UserOnlineIP.LoadOrStore(taguuid, newipMap); loaded {
		oldipMap := v.(*sync.Map)
		if _, loaded := oldipMap.LoadOrStore(ip, uid); !loaded {
			if v, loaded := l.OldUserOnline.Load(ip); loaded {
				if v.(int) == uid {
					l.OldUserOnline.Delete(ip)
				}
			} else if deviceLimit > 0 && deviceLimit <= aliveIp {
				oldipMap.Delete(ip)
				return true
			}
		}
	} else if v, ok := l.OldUserOnline.Load(ip); ok {
		if v.(int) == uid {
			l.OldUserOnline.Delete(ip)
		}
	} else if deviceLimit > 0 && deviceLimit <= aliveIp {
		l.UserOnlineIP.Delete(taguuid)
		return true
	}
	return false
}

func (l *Limiter) GetOnlineDevice() (*[]panel.OnlineUser, error) {
	if l.onlineIPLeases != nil {
		onlineUser := l.onlineIPLeases.OnlineDevices()
		l.clearLocalOnlineDeviceSnapshot()
		return &onlineUser, nil
	}
	return l.getLocalOnlineDevice()
}

func (l *Limiter) clearLocalOnlineDeviceSnapshot() {
	l.OldUserOnline = new(sync.Map)
	l.UserOnlineIP.Range(func(key, value interface{}) bool {
		l.UserOnlineIP.Delete(key)
		return true
	})
}

func (l *Limiter) getLocalOnlineDevice() (*[]panel.OnlineUser, error) {
	var onlineUser []panel.OnlineUser
	l.OldUserOnline = new(sync.Map)
	l.UserOnlineIP.Range(func(key, value interface{}) bool {
		taguuid := key.(string)
		ipMap := value.(*sync.Map)
		ipMap.Range(func(key, value interface{}) bool {
			uid := value.(int)
			ip := key.(string)
			l.OldUserOnline.Store(ip, uid)
			onlineUser = append(onlineUser, panel.OnlineUser{UID: uid, IP: ip})
			return true
		})
		l.UserOnlineIP.Delete(taguuid) // Reset online device
		return true
	})

	return &onlineUser, nil
}

type UserIpList struct {
	Uid    int      `json:"Uid"`
	IpList []string `json:"Ips"`
}
