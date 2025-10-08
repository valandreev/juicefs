package meta

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
)

type redisLikeTarget struct {
	Scheme string
	URI    string
	Addr   string
	label  string
}

func (t redisLikeTarget) displayName() string {
	if t.label != "" {
		return t.label
	}
	return fmt.Sprintf("%s:%s", t.Scheme, t.Addr)
}

func redisLikeTestTargets() []redisLikeTarget {
	raw := os.Getenv("JFS_TEST_REDIS_URIS")
	if raw == "" {
		raw = os.Getenv("JFS_TEST_REDIS_URI")
	}
	if raw == "" {
		raw = "redis://127.0.0.1:6379/10"
	}
	parts := strings.Split(raw, ",")
	targets := make([]redisLikeTarget, 0, len(parts))
	for _, entry := range parts {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		scheme, addr, label, err := parseRedisLikeURI(entry)
		if err != nil {
			continue
		}
		targets = append(targets, redisLikeTarget{
			Scheme: scheme,
			URI:    entry,
			Addr:   addr,
			label:  label,
		})
	}
	return targets
}

func parseRedisLikeURI(uri string) (scheme, addr, label string, err error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", "", "", err
	}
	scheme = strings.ToLower(u.Scheme)
	userInfo := ""
	if u.User != nil {
		userInfo = u.User.String() + "@"
	}
	addrBuilder := strings.Builder{}
	addrBuilder.WriteString(userInfo)
	switch scheme {
	case "redis", "rediss", "rueidis", "ruediss", "unix":
		if scheme == "unix" {
			addrBuilder.WriteString(strings.TrimPrefix(uri, scheme+"://"))
		} else {
			addrBuilder.WriteString(u.Host)
			addrBuilder.WriteString(u.Path)
		}
	default:
		addrBuilder.WriteString(strings.TrimPrefix(uri, scheme+"://"))
	}
	if u.RawQuery != "" {
		addrBuilder.WriteString("?")
		addrBuilder.WriteString(u.RawQuery)
	}
	addr = addrBuilder.String()
	labelParts := []string{scheme}
	if host := u.Host; host != "" {
		labelParts = append(labelParts, host)
	}
	if path := strings.Trim(u.Path, "/"); path != "" {
		labelParts = append(labelParts, path)
	}
	if len(labelParts) > 0 {
		label = strings.Join(labelParts, "-")
	}
	return scheme, addr, label, nil
}

func forEachRedisLike(t *testing.T, suite string, fn func(t *testing.T, target redisLikeTarget)) {
	t.Helper()
	targets := redisLikeTestTargets()
	if len(targets) == 0 {
		t.Skip("no redis-like targets configured")
		return
	}
	for idx, target := range targets {
		caseName := fmt.Sprintf("%s[%d]-%s", suite, idx, target.displayName())
		t.Run(caseName, func(t *testing.T) {
			t.Helper()
			fn(t, target)
		})
	}
}

func createMetaFromURI(t *testing.T, uri string, conf *Config) Meta {
	t.Helper()
	scheme, addr, _, err := parseRedisLikeURI(uri)
	if err != nil {
		t.Fatalf("invalid meta uri %q: %v", uri, err)
	}
	creator, ok := metaDrivers[scheme]
	if !ok {
		t.Skipf("meta driver %s not registered", scheme)
		return nil
	}
	var cfg *Config
	if conf != nil {
		copyConf := *conf
		cfg = &copyConf
	} else {
		cfg = DefaultConf()
	}
	m, err := creator(scheme, addr, cfg)
	if err != nil {
		t.Fatalf("create meta %s: %v", uri, err)
	}
	return m
}

func createMetaForTarget(t *testing.T, target redisLikeTarget, conf *Config) Meta {
	t.Helper()
	creator, ok := metaDrivers[target.Scheme]
	if !ok {
		t.Skipf("meta driver %s not registered", target.Scheme)
		return nil
	}
	var cfg *Config
	if conf != nil {
		copyConf := *conf
		cfg = &copyConf
	} else {
		cfg = testConfig()
	}
	m, err := creator(target.Scheme, target.Addr, cfg)
	if err != nil {
		t.Fatalf("create meta %s: %v", target.URI, err)
	}
	return m
}
