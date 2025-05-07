func (m *redisMeta) setupClientSideCaching(expiry time.Duration) error {
	ctx := Background()
	
	// For cluster clients, we need a separate connection for tracking
	var client redis.Cmdable
	if _, ok := m.rdb.(*redis.ClusterClient); ok {
		if c, err := m.rdb.(*redis.ClusterClient).MasterForKey(ctx, m.prefix); err == nil {
			client = c
		} else {
			return err
		}
	} else {
		client = m.rdb
	}
	
	// Enable tracking
	mode := "ON"
	if m.clientCacheBcast {
		mode = "ON BCAST"
	} else {
		mode = "ON OPTIN"
	}
	
	if err := client.Do(ctx, "CLIENT", "TRACKING", mode, "REDIRECT", 0).Err(); err != nil {
		return err
	}
	
	// Subscribe to invalidation messages
	m.cacheSubscription = m.rdb.Subscribe(ctx, "__redis__:invalidate")
	
	// Start a goroutine to handle invalidation messages
	go m.handleCacheInvalidation()
	
	return nil
}

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

// Override methods with client-side caching versions
func (m *redisMeta) overrideWithCachedMethods() {
	// No need to override methods at runtime since we have conditional logic in each method
	logger.Debugf("Redis client-side caching methods are ready")
}

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
