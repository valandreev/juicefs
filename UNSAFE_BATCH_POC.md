# Unsafe Batch PoC - План Реализации

## 🎯 Цель

Проверить гипотезу: батчинг операций в Redis даёт **100-1000× ускорение** для создания/удаления мелких файлов.

## 📋 Задачи (1-2 дня)

### Задача 1: Добавить URI параметр `?unsafe_batch=1`

**Файл:** `pkg/meta/rueidis.go` (lines ~140-200)

**Изменения:**
```go
// После существующих параметров (ttl, prime, subscribe, etc.)
unsafeBatchMode := false

if u.Query().Get("unsafe_batch") == "1" {
    unsafeBatchMode = true
    logger.Warnf("⚠️  UNSAFE BATCH MODE ENABLED - NO ATOMICITY GUARANTEES!")
    logger.Warnf("⚠️  Use ONLY for isolated testing or single-client scenarios!")
}
```

**Передать в rueidisMeta:**
```go
m := &rueidisMeta{
    redisMeta:        base,
    // ... existing fields
    unsafeBatchMode:  unsafeBatchMode,  // ДОБАВИТЬ
}
```

**Добавить поле в struct (lines ~50-130):**
```go
type rueidisMeta struct {
    *redisMeta
    // ... existing fields
    unsafeBatchMode  bool  // Enable unsafe batching (no transactions)
}
```

---

### Задача 2: Создать `pkg/meta/rueidis_unsafe_batch.go`

**Новый файл:** `pkg/meta/rueidis_unsafe_batch.go`

**Содержимое:**

```go
// Copyright 2024 Juicedata Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");

package meta

import (
    "syscall"
    "time"

    "github.com/redis/rueidis"
)

// doMknodUnsafeBatch creates a file/directory WITHOUT transaction atomicity.
//
// ⚠️ WARNING: This is UNSAFE and should ONLY be used for:
//   - Performance testing (PoC)
//   - Single-client scenarios (no concurrent access)
//   - Non-critical data (can tolerate inconsistencies)
//
// Race conditions WILL occur with multiple clients:
//   - Orphaned inodes
//   - Incorrect counters
//   - Duplicate file entries
//
// DO NOT USE IN PRODUCTION unless you understand the risks!
func (m *rueidisMeta) doMknodUnsafeBatch(ctx Context, parent Ino, name string, _type uint8, mode, cumask uint16, path string, inode *Ino, attr *Attr) syscall.Errno {
    // Step 1: Generate inode if not provided
    if *inode == 0 {
        var err error
        *inode, err = m.nextInode()
        if err != nil {
            return errno(err)
        }
    }

    // Step 2: Read parent inode (WITHOUT WATCH - no locking!)
    data, err := m.rdb.Get(ctx, m.inodeKey(parent)).Bytes()
    if err != nil {
        return errno(err)
    }

    var pattr Attr
    m.parseAttr(data, &pattr)

    // Step 3: Update parent timestamps
    now := time.Now()
    pattr.Mtime = now.Unix()
    pattr.Mtimensec = uint32(now.Nanosecond())
    pattr.Ctime = now.Unix()
    pattr.Ctimensec = uint32(now.Nanosecond())

    // Step 4: Initialize new inode attributes
    attr.Typ = _type
    attr.Mode = mode & ^cumask
    attr.Uid = pattr.Uid
    attr.Gid = pattr.Gid
    attr.Parent = parent
    attr.Nlink = 1
    attr.Atime = now.Unix()
    attr.Mtime = now.Unix()
    attr.Ctime = now.Unix()
    attr.Atimensec = uint32(now.Nanosecond())
    attr.Mtimensec = uint32(now.Nanosecond())
    attr.Ctimensec = uint32(now.Nanosecond())

    // Step 5: Build pipeline commands (NO transaction!)
    cmds := []rueidis.Completed{
        // Create new inode
        m.client.B().Set().
            Key(m.inodeKey(*inode)).
            Value(string(m.marshal(attr))).
            Build(),

        // Update parent inode
        m.client.B().Set().
            Key(m.inodeKey(parent)).
            Value(string(m.marshal(&pattr))).
            Build(),

        // Add directory entry (NO existence check!)
        m.client.B().Hset().
            Key(m.entryKey(parent)).
            FieldValue().
            FieldValue(name, string(m.packEntry(_type, *inode))).
            Build(),

        // Update used space counter
        m.client.B().Incrby().
            Key(m.usedSpaceKey()).
            Increment(align4K(0)).
            Build(),

        // Update total inodes counter
        m.client.B().Incr().
            Key(m.totalInodesKey()).
            Build(),
    }

    // Step 6: Execute pipeline (ONE RTT!)
    results := m.client.DoMulti(ctx, cmds...)

    // Step 7: Check for errors
    for i, res := range results {
        if err := res.Error(); err != nil {
            logger.Errorf("Unsafe batch mknod operation %d failed: %v", i, err)
            return errno(err)
        }
    }

    return 0
}

// doUnlinkUnsafeBatch removes a file WITHOUT transaction atomicity.
//
// ⚠️ WARNING: Same risks as doMknodUnsafeBatch - UNSAFE for concurrent access!
func (m *rueidisMeta) doUnlinkUnsafeBatch(ctx Context, parent Ino, name string) syscall.Errno {
    // Step 1: Read entry (WITHOUT WATCH)
    entryBuf, err := m.rdb.HGet(ctx, m.entryKey(parent), name).Bytes()
    if err == redis.Nil {
        return syscall.ENOENT
    }
    if err != nil {
        return errno(err)
    }

    _type, inode := m.parseEntry(entryBuf)

    // Step 2: Read inode (WITHOUT WATCH)
    data, err := m.rdb.Get(ctx, m.inodeKey(inode)).Bytes()
    if err != nil {
        return errno(err)
    }

    var attr Attr
    m.parseAttr(data, &attr)

    // Step 3: Read parent (WITHOUT WATCH)
    pdata, err := m.rdb.Get(ctx, m.inodeKey(parent)).Bytes()
    if err != nil {
        return errno(err)
    }

    var pattr Attr
    m.parseAttr(pdata, &pattr)

    // Step 4: Update timestamps
    now := time.Now()
    attr.Ctime = now.Unix()
    attr.Ctimensec = uint32(now.Nanosecond())
    attr.Nlink--

    pattr.Mtime = now.Unix()
    pattr.Mtimensec = uint32(now.Nanosecond())
    pattr.Ctime = now.Unix()
    pattr.Ctimensec = uint32(now.Nanosecond())

    // Step 5: Build pipeline commands (NO transaction!)
    cmds := []rueidis.Completed{
        // Remove directory entry
        m.client.B().Hdel().
            Key(m.entryKey(parent)).
            Field(name).
            Build(),

        // Update parent inode
        m.client.B().Set().
            Key(m.inodeKey(parent)).
            Value(string(m.marshal(&pattr))).
            Build(),

        // Update file inode
        m.client.B().Set().
            Key(m.inodeKey(inode)).
            Value(string(m.marshal(&attr))).
            Build(),

        // Decrement used space counter
        m.client.B().Incrby().
            Key(m.usedSpaceKey()).
            Increment(-int64(align4K(attr.Length))).
            Build(),

        // Decrement total inodes counter (if nlink=0)
        m.client.B().Decr().
            Key(m.totalInodesKey()).
            Build(),
    }

    // Step 6: Execute pipeline (ONE RTT!)
    results := m.client.DoMulti(ctx, cmds...)

    // Step 7: Check for errors
    for i, res := range results {
        if err := res.Error(); err != nil {
            logger.Errorf("Unsafe batch unlink operation %d failed: %v", i, err)
            return errno(err)
        }
    }

    return 0
}
```

---

### Задача 3: Интегрировать в `doMknod` и `doUnlink`

**Файл:** `pkg/meta/rueidis.go`

**Найти функцию `doMknod` (около line 4790):**

```go
func (m *rueidisMeta) doMknod(ctx Context, parent Ino, name string, _type uint8, mode, cumask uint16, path string, inode *Ino, attr *Attr) syscall.Errno {
    // ДОБАВИТЬ В НАЧАЛО ФУНКЦИИ:
    if m.unsafeBatchMode {
        return m.doMknodUnsafeBatch(ctx, parent, name, _type, mode, cumask, path, inode, attr)
    }

    // Существующий код с транзакциями...
    err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
        // ...
    })
}
```

**Найти функцию `doUnlink` (около line 4962):**

```go
func (m *rueidisMeta) doUnlink(ctx Context, parent Ino, name string, attr *Attr) syscall.Errno {
    // ДОБАВИТЬ В НАЧАЛО ФУНКЦИИ:
    if m.unsafeBatchMode {
        return m.doUnlinkUnsafeBatch(ctx, parent, name)
    }

    // Существующий код с транзакциями...
    err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
        // ...
    })
}
```

---

### Задача 4: Тестирование

**Скрипт для тестирования:** `test_unsafe_batch.sh`

```bash
#!/bin/bash

# 1. Start Redis
docker run -d --name redis-test -p 6379:6379 redis:latest

# 2. Format FS with unsafe batch mode
./juicefs format \
    --storage file \
    "rueidis://localhost:6379/1?unsafe_batch=1&metaprime=1" \
    /tmp/jfs-unsafe-test

# 3. Mount
./juicefs mount \
    "rueidis://localhost:6379/1?unsafe_batch=1&metaprime=1" \
    /tmp/jfs-mount &

sleep 2

# 4. Benchmark: Create 10,000 files
echo "=== Creating 10,000 files with UNSAFE mode ==="
time {
    for i in {1..10000}; do
        touch /tmp/jfs-mount/file$i.txt
    done
}

# 5. Benchmark: Delete 10,000 files
echo "=== Deleting 10,000 files with UNSAFE mode ==="
time {
    for i in {1..10000}; do
        rm /tmp/jfs-mount/file$i.txt
    done
}

# 6. Cleanup
./juicefs umount /tmp/jfs-mount
docker stop redis-test
docker rm redis-test
```

**Сравнение с SAFE mode:**

```bash
# Тест БЕЗ unsafe_batch (текущий режим с транзакциями)
./juicefs format \
    "rueidis://localhost:6379/2" \
    /tmp/jfs-safe-test

./juicefs mount "rueidis://localhost:6379/2" /tmp/jfs-mount-safe &

echo "=== Creating 10,000 files with SAFE mode (transactions) ==="
time {
    for i in {1..10000}; do
        touch /tmp/jfs-mount-safe/file$i.txt
    done
}
```

---

## 📊 Ожидаемые Результаты

### SAFE mode (с транзакциями):
```
Creating 10,000 files: ~10-20 секунд
Deleting 10,000 files: ~10-20 секунд
Throughput: ~500-1000 files/sec
```

### UNSAFE mode (без транзакций):
```
Creating 10,000 files: ~0.02-0.1 секунды
Deleting 10,000 files: ~0.02-0.1 секунды
Throughput: ~100,000-500,000 files/sec
```

### Ускорение: **100-500×** 🚀

---

## ✅ Критерий Успеха PoC

1. ✅ Ускорение >= 50× для создания файлов
2. ✅ Ускорение >= 50× для удаления файлов
3. ✅ Файловая система работает корректно (при одном клиенте)
4. ✅ Нет crash'ей или corruption'а данных

**Если PoC успешен:**
→ Переходим к реализации **Lua Scripts** (безопасная версия с той же производительностью)

**Если PoC не даёт ускорения:**
→ Проблема не в транзакциях, ищем другие bottleneck'и (network latency, Redis performance, VFS overhead)

---

## 🎯 Следующие Шаги После PoC

1. **Измерить производительность** с разными параметрами:
   - `?metaprime=1&inode_batch=1000` (ID batching)
   - `?ttl=1h` (client-side caching)
   - Разные network latency (localhost vs remote Redis)

2. **Профилирование:**
   - `pprof` для определения bottleneck'ов
   - Redis `SLOWLOG` для медленных команд
   - `strace` для syscall overhead

3. **Если ускорение подтверждено:**
   - Реализовать Lua Scripts версию (2-3 недели)
   - Добавить transparent batching accumulator
   - Production-ready feature с feature flag

---

**Статус:** 📝 Ready to implement  
**Оценка времени:** 1-2 дня  
**Риск:** Низкий (только для тестирования)
