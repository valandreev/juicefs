//go:build !norueidis
// +build !norueidis

package meta

import (
	"fmt"
	"strings"
	"syscall"

	"github.com/redis/rueidis"
	"github.com/redis/rueidis/rueidiscompat"
)

type rueidisMeta struct {
	*redisMeta

	scheme    string
	canonical string
	option    rueidis.ClientOption
	client    rueidis.Client
	compat    rueidiscompat.Cmdable
}

// Temporary Rueidis registration that delegates to the Redis implementation.
// This keeps the Rueidis schemes usable during the driver bring-up and lets
// the registration test enforce that the schemes stay wired.
func init() {
	Register("rueidis", newRueidisMeta)
	Register("ruediss", newRueidisMeta)
}

func newRueidisMeta(driver, addr string, conf *Config) (Meta, error) {
	canonical := mapRueidisScheme(driver)
	uri := canonical + "://" + addr

	opt, err := rueidis.ParseURL(uri)
	if err != nil {
		return nil, fmt.Errorf("rueidis parse %s: %w", uri, err)
	}

	delegate, err := newRedisMeta(canonical, addr, conf)
	if err != nil {
		return nil, err
	}

	base, ok := delegate.(*redisMeta)
	if !ok {
		return nil, fmt.Errorf("unexpected meta implementation %T", delegate)
	}

	client, err := rueidis.NewClient(opt)
	if err != nil {
		return nil, fmt.Errorf("rueidis connect %s: %w", uri, err)
	}

	m := &rueidisMeta{
		redisMeta: base,
		scheme:    driver,
		canonical: canonical,
		option:    opt,
		client:    client,
		compat:    rueidiscompat.NewAdapter(client),
	}
	m.redisMeta.en = m
	return m, nil
}

func mapRueidisScheme(driver string) string {
	switch strings.ToLower(driver) {
	case "rueidis":
		return "redis"
	case "ruediss":
		return "rediss"
	default:
		return driver
	}
}

func (m *rueidisMeta) Name() string {
	return "rueidis"
}

func (m *rueidisMeta) Shutdown() error {
	if m.client != nil {
		m.client.Close()
	}
	return m.redisMeta.Shutdown()
}

func (m *rueidisMeta) doDeleteSlice(id uint64, size uint32) error {
	if m.compat == nil {
		return m.redisMeta.doDeleteSlice(id, size)
	}
	return m.compat.HDel(Background(), m.sliceRefs(), m.sliceKey(id, size)).Err()
}

func (m *rueidisMeta) doLoad() ([]byte, error) {
	if m.compat == nil {
		return m.redisMeta.doLoad()
	}
	cmd := m.compat.Get(Background(), m.setting())
	if err := cmd.Err(); err != nil {
		if err == rueidiscompat.Nil {
			return nil, nil
		}
		return nil, err
	}
	return cmd.Bytes()
}

func (m *rueidisMeta) getCounter(name string) (int64, error) {
	if m.compat == nil {
		return m.redisMeta.getCounter(name)
	}
	cmd := m.compat.Get(Background(), m.counterKey(name))
	v, err := cmd.Int64()
	if err == rueidiscompat.Nil {
		err = nil
	}
	return v, err
}

func (m *rueidisMeta) incrCounter(name string, value int64) (int64, error) {
	if m.compat == nil {
		return m.redisMeta.incrCounter(name, value)
	}
	if m.conf.ReadOnly {
		return 0, syscall.EROFS
	}
	key := m.counterKey(name)
	cmd := m.compat.IncrBy(Background(), key, value)
	v, err := cmd.Result()
	if err != nil {
		return v, err
	}
	if name == "nextInode" || name == "nextChunk" {
		return v + 1, nil
	}
	return v, nil
}
