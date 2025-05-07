//go:build !noredis
// +build !noredis

/*
 * JuiceFS, Copyright 2020 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package meta

import (
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// setupClientSideCaching configures Redis client-side caching
func (m *redisMeta) setupClientSideCaching(expiry time.Duration) error {
	ctx := Background()
	
	// For cluster clients, we need a separate connection for tracking
	// Note: we'll use m.rdb directly which has the Do method
	if _, ok := m.rdb.(*redis.ClusterClient); ok {
		// For cluster mode, we should get the master node for our key
		_, err := m.rdb.(*redis.ClusterClient).MasterForKey(ctx, m.prefix)
		if err != nil {
			return err
		}
	}
		// Enable tracking
	mode := "ON"
	if m.clientCacheBcast {
		mode = "ON BCAST"
	} else {
		mode = "ON OPTIN"
	}
	
	// Use Do() to execute arbitrary Redis commands
	err := m.rdb.Do(ctx, "CLIENT", "TRACKING", mode, "REDIRECT", "0").Err()
	if err != nil {
		return err
	}
	
	// Subscribe to invalidation messages
	m.cacheSubscription = m.rdb.Subscribe(ctx, "__redis__:invalidate")
	
	// Start a goroutine to handle invalidation messages
	go m.handleCacheInvalidation()
	
	return nil
}

// handleCacheInvalidation processes invalidation messages from Redis
func (m *redisMeta) handleCacheInvalidation() {
	ch := m.cacheSubscription.Channel()
	for msg := range ch {
		key := msg.Payload
		if key == "" || !strings.HasPrefix(key, m.prefix) {
			continue
		}
		
		m.cacheMu.Lock()
		if strings.HasPrefix(key, m.prefix+"i") {
			// Invalidate inode cache
			inodeStr := key[len(m.prefix)+1:]
			inode, err := strconv.ParseUint(inodeStr, 10, 64)
			if err == nil {
				m.inodeCache.Remove(Ino(inode))
				logger.Debugf("Invalidated inode cache for %s", inodeStr)
			}
		} else if strings.HasPrefix(key, m.prefix+"d") {
			// Invalidate entry cache
			parentStr := key[len(m.prefix)+1:]
			parent, err := strconv.ParseUint(parentStr, 10, 64)
			if err == nil {
				m.entryCache.Remove(Ino(parent))
				logger.Debugf("Invalidated entry cache for %s", parentStr)
			}
		}
		m.cacheMu.Unlock()
	}
}

// overrideWithCachedMethods replaces standard methods with cached versions
func (m *redisMeta) overrideWithCachedMethods() {
	// No need to override methods at runtime, we use conditionals in each method
	logger.Debugf("Redis client-side caching methods are ready")
}