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
	"syscall"
	"time"
)

// Wrappers for Redis API methods that use client-side caching when enabled

// Write wraps the write operation with client-side caching support
func (m *redisMeta) Write(ctx Context, inode Ino, indx uint32, off uint32, slice Slice, mtime time.Time) syscall.Errno {
	if m.clientCache {
		return m.writeCached(ctx, inode, indx, off, slice, mtime)
	}
	return m.baseMeta.Write(ctx, inode, indx, off, slice, mtime)
}

// Truncate wraps the truncate operation with client-side caching support
func (m *redisMeta) Truncate(ctx Context, inode Ino, flags uint8, length uint64, attr *Attr, skipPermCheck bool) syscall.Errno {
	if m.clientCache {
		return m.truncateCached(ctx, inode, flags, length, attr, skipPermCheck)
	}
	return m.baseMeta.Truncate(ctx, inode, flags, length, attr, skipPermCheck)
}

// Create wraps the create operation with client-side caching support
func (m *redisMeta) Create(ctx Context, parent Ino, name string, mode uint16, cumask uint16, flags uint32, inode *Ino, attr *Attr) syscall.Errno {
	if m.clientCache {
		return m.createFileCached(ctx, parent, name, mode, cumask, flags, inode, attr)
	}
	return m.baseMeta.Create(ctx, parent, name, mode, cumask, flags, inode, attr)
}
