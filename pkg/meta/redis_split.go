package meta

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/redis/go-redis/v9"
)

func init() {
	Register("redis-split", newRedisSplitMeta)
}

type redisSplitMeta struct {
	*redisMeta
	masterURI  string
	replicaURI string
	master     redis.UniversalClient
	replica    redis.UniversalClient
}

var _ Meta = (*redisSplitMeta)(nil)
var _ engine = (*redisSplitMeta)(nil)

var redisSplitMasterFactory = newRedisMeta

var redisSplitReplicaFactory = func(uri string, conf *Config) (redis.UniversalClient, error) {
	if conf == nil {
		conf = DefaultConf()
	}
	opt, err := redis.ParseURL(uri)
	if err != nil {
		return nil, fmt.Errorf("redis parse %s: %w", uri, err)
	}
	if opt.Password == "" {
		opt.Password = os.Getenv("REDIS_PASSWORD")
	}
	if opt.Password == "" {
		opt.Password = os.Getenv("META_PASSWORD")
	}
	opt.MaxRetries = conf.Retries
	if opt.MaxRetries == 0 {
		opt.MaxRetries = -1
	}
	return redis.NewClient(opt), nil
}

func parseRedisSplitURL(raw string) (string, string, error) {
	const scheme = "redis-split://"
	if raw == "" {
		return "", "", errors.New("empty redis-split url")
	}
	if !strings.HasPrefix(raw, scheme) {
		return "", "", fmt.Errorf("invalid redis-split url, missing prefix %q", scheme)
	}
	rest := strings.TrimPrefix(raw, scheme)
	masterPart := rest
	queryPart := ""
	if idx := strings.Index(rest, "?"); idx >= 0 {
		masterPart = rest[:idx]
		queryPart = rest[idx+1:]
	}
	masterPart = strings.TrimSpace(masterPart)
	if masterPart == "" {
		return "", "", errors.New("master uri is required")
	}
	if !strings.HasPrefix(masterPart, "redis://") && !strings.HasPrefix(masterPart, "rediss://") {
		return "", "", fmt.Errorf("invalid master uri %q", masterPart)
	}
	var replica string
	if queryPart != "" {
		vals, err := url.ParseQuery(queryPart)
		if err != nil {
			return "", "", fmt.Errorf("invalid query parameters: %w", err)
		}
		replica = strings.TrimSpace(vals.Get("replica"))
	}
	if replica == "" {
		return "", "", errors.New("replica uri is required")
	}
	if !strings.HasPrefix(replica, "redis://") && !strings.HasPrefix(replica, "rediss://") {
		return "", "", fmt.Errorf("invalid replica uri %q", replica)
	}
	return masterPart, replica, nil
}

func newRedisSplitMeta(driver, addr string, conf *Config) (Meta, error) {
	if conf == nil {
		conf = DefaultConf()
	}
	masterURI, replicaURI, err := parseRedisSplitURL(driver + "://" + addr)
	if err != nil {
		return nil, err
	}
	masterDriver, masterAddr, err := splitRedisURI(masterURI)
	if err != nil {
		return nil, err
	}
	meta, err := redisSplitMasterFactory(masterDriver, masterAddr, conf)
	if err != nil {
		return nil, err
	}
	rm, ok := meta.(*redisMeta)
	if !ok {
		return nil, fmt.Errorf("unexpected redis meta type %T", meta)
	}
	replicaClient, err := redisSplitReplicaFactory(replicaURI, conf)
	if err != nil {
		rm.Shutdown()
		return nil, err
	}
	split := &redisSplitMeta{
		redisMeta:  rm,
		masterURI:  masterURI,
		replicaURI: replicaURI,
		master:     rm.rdb,
		replica:    replicaClient,
	}
	split.en = split
	return split, nil
}

func splitRedisURI(uri string) (string, string, error) {
	idx := strings.Index(uri, "://")
	if idx <= 0 {
		return "", "", fmt.Errorf("invalid redis uri: %s", uri)
	}
	driver := uri[:idx]
	addr := uri[idx+3:]
	if addr == "" {
		return "", "", fmt.Errorf("empty redis addr: %s", uri)
	}
	return driver, addr, nil
}

func (m *redisSplitMeta) Name() string {
	return "redis-split"
}
