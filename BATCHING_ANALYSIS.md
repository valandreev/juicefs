# Анализ Батчинга в Rueidis - Проблема Производительности Мелких Файлов

## 🔍 Общая Картина Проблемы

**Симптом:** Изменение параметров батчинга **не влияет** (или влияет незаметно) на скорость записи и удаления мелких файлов.

**Корневая причина:** Критические метаданные операции (создание, удаление файлов) **НЕ ИСПОЛЬЗУЮТ** систему батчинга - они выполняются через **атомарные транзакции** (WATCH/MULTI/EXEC).

---

## 📊 Текущая Реализация Батчинга

### Что Реализовано

**Файл:** `pkg/meta/rueidis_batch_write.go` (1155 строк)

**Компоненты:**
1. **Асинхронная очередь** операций (`batchQueue chan *BatchOp`)
2. **Coalescing** - объединение однотипных операций
3. **Adaptive sizing** - динамическая подстройка размера батча
4. **MSET/HMSET оптимизация** - группировка SET/HSET команд
5. **Back-pressure** - защита от переполнения очереди

**Поддерживаемые операции:**
- `OpSET` - SET key value
- `OpHSET` - HSET hash field value
- `OpHDEL` - HDEL hash field
- `OpHINCRBY` - HINCRBY hash field delta
- `OpDEL` - DEL key
- `OpZADD` - ZADD key score member
- `OpSADD` - SADD key member
- `OpRPUSH` - RPUSH key value
- `OpINCRBY` - INCRBY key delta

### Параметры Батчинга

```go
// Из URI query parameters:
?batchwrite=0          // Отключить батчинг (по умолчанию: ВКЛЮЧЕН)
?batch_size=N          // Макс. операций в батче (по умолчанию: 512)
?batch_bytes=N         // Макс. байт в батче (по умолчанию: 262144 = 256KB)
?batch_interval=Xms    // Макс. время между флашами (по умолчанию: 200ms)

// Внутренние:
maxQueueSize = 100000  // Макс. размер очереди
```

### Где Батчинг ИСПОЛЬЗУЕТСЯ

**Файл:** `pkg/meta/rueidis.go` (lines 880-1080)

Функции-обертки для батчинга:
```go
batchSet(ctx, key, value)       // line 870
batchHSet(ctx, key, field, val) // line 908
batchHDel(ctx, key, field)      // line 944
batchHIncrBy(ctx, key, field, delta) // line 968
batchDel(ctx, key)              // line 994
batchIncrBy(ctx, key, delta)    // line 1013
batchFlushBarrier(ctx, inode)   // line 1067 (синхронизация)
```

**Но эти функции НЕ ВЫЗЫВАЮТСЯ** в критических операциях создания/удаления файлов!

---

## ❌ Почему Батчинг НЕ Работает для Мелких Файлов

### 1. Создание Файла (`doMknod`)

**Файл:** `pkg/meta/rueidis.go` lines 4790-4960

**Код:**
```go
func (m *rueidisMeta) doMknod(ctx Context, parent Ino, name string, ...) syscall.Errno {
    err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
        // Чтение родительской директории
        data, err := tx.Get(ctx, parentKey).Bytes()
        
        // АТОМАРНАЯ транзакция (WATCH/MULTI/EXEC)
        _, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
            pipe.Set(ctx, m.inodeKey(*inode), m.marshal(attr), 0)
            pipe.Set(ctx, m.inodeKey(parent), m.marshal(&pattr), 0)
            pipe.HSet(ctx, entryKey, name, m.packEntry(_type, *inode))
            pipe.IncrBy(ctx, m.usedSpaceKey(), align4K(0))
            pipe.Incr(ctx, m.totalInodesKey())
            return nil
        })
        return err
    }, m.inodeKey(parent), m.entryKey(parent))
}
```

**Проблема:**
- Использует `m.txn()` - атомарную транзакцию Redis
- Все операции внутри `TxPipelined` выполняются **синхронно** как единая транзакция
- **НЕ ИСПОЛЬЗУЕТ** `batchSet`, `batchHSet` и т.д.
- **Латентность:** Каждое создание файла = 1 RTT к Redis

### 2. Удаление Файла (`doUnlink`)

**Файл:** `pkg/meta/rueidis.go` lines 4962-5200

**Код:**
```go
func (m *rueidisMeta) doUnlink(ctx Context, parent Ino, name string, ...) syscall.Errno {
    err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
        // Чтение metadata
        entryBuf, err := tx.HGet(ctx, m.entryKey(parent), name).Bytes()
        
        // АТОМАРНАЯ транзакция
        _, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
            pipe.HDel(ctx, m.entryKey(parent), name)
            pipe.Set(ctx, m.inodeKey(parent), m.marshal(&pattr), 0)
            pipe.Set(ctx, m.inodeKey(inode), m.marshal(attr), 0)
            // ... множество других операций
            return nil
        })
        return err
    }, keys...)
}
```

**Проблема:** Аналогично созданию - транзакция, а не батчинг.

### 3. Запись в Файл (`doWrite`)

**Файл:** `pkg/meta/rueidis.go` lines 5774-5825

**Код:**
```go
func (m *rueidisMeta) doWrite(ctx Context, inode Ino, ...) syscall.Errno {
    return errno(m.txn(ctx, func(tx rueidiscompat.Tx) error {
        // Чтение inode
        data, err := tx.Get(ctx, m.inodeKey(inode)).Bytes()
        
        // АТОМАРНАЯ транзакция
        _, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
            pipe.RPush(ctx, m.chunkKey(inode, indx), marshalSlice(...))
            pipe.Set(ctx, m.inodeKey(inode), m.marshal(attr), 0)
            pipe.IncrBy(ctx, m.usedSpaceKey(), delta.space)
            return nil
        })
        return err
    }, m.inodeKey(inode)))
}
```

**Проблема:** Транзакция вместо батчинга.

---

## 📉 Влияние на Производительность

### Текущая Ситуация

**Для создания N мелких файлов:**
- **Операций к Redis:** N × (1 транзакция) = **N RTT**
- **Латентность:** N × (network_latency + Redis_exec_time)
- **Пример:** 1000 файлов при 1ms RTT = **1000ms минимум**

**Батчинг НЕ ПОМОГАЕТ потому что:**
- Параметры `?batch_size=512` игнорируются для `doMknod`
- Параметры `?batch_interval=2ms` игнорируются для `doUnlink`
- Функции `batchSet/batchHSet` **не вызываются** в критических путях

### Потенциал Батчинга (если бы работал)

**С батчингом для N файлов:**
- **Операций к Redis:** ⌈N / batch_size⌉ = **меньше RTT**
- **Латентность:** ⌈N / 512⌉ × RTT (с batch_size=512)
- **Пример:** 1000 файлов = **2 RTT** (вместо 1000 RTT)
- **Ускорение:** **500× теоретически**

---

## 🔍 Почему Используются Транзакции

### Требование Атомарности

**Создание файла требует:**
1. Проверить существование файла с тем же именем
2. Обновить родительскую директорию
3. Создать новый inode
4. Добавить entry в родительскую директорию
5. Обновить счетчики (used_space, total_inodes)

**Все это должно быть АТОМАРНО:**
- Либо все операции выполнены
- Либо ничего не выполнено (откат)
- **Race condition:** Два клиента создают файл с одним именем одновременно

**Redis транзакции (WATCH/MULTI/EXEC) гарантируют:**
- Optimistic locking через WATCH
- Если ключи изменились - транзакция откатывается (retry)
- Атомарность всех операций внутри MULTI/EXEC

**Батчинг НЕ МОЖЕТ обеспечить атомарность:**
- Операции выполняются асинхронно
- Нет гарантии порядка выполнения между разными ключами
- Нет механизма отката при конфликтах

---

## 🎯 Корневая Проблема

### Архитектурное Противоречие

```
┌────────────────────────────────────────────────────────────┐
│  ТРЕБОВАНИЯ                                                │
│                                                            │
│  Создание файла:                                          │
│    ✅ Атомарность - все или ничего                       │
│    ✅ Консистентность - нет race conditions              │
│    ❌ Низкая латентность - нужен батчинг                 │
│                                                            │
│  Текущее решение:                                         │
│    ✅ Транзакции (WATCH/MULTI/EXEC)                      │
│    ✅ Атомарность гарантирована                          │
│    ❌ Каждая операция = 1 RTT                            │
│    ❌ Батчинг не используется                            │
│                                                            │
│  Результат:                                               │
│    ❌ Параметры батчинга не влияют на создание файлов    │
│    ❌ N файлов = N RTT к Redis                           │
│    ❌ Производительность O(N) вместо O(⌈N/batch⌉)      │
└────────────────────────────────────────────────────────────┘
```

---

## 💡 Возможные Пути Решения

### Решение 1: Lua Scripts (Рекомендуется)

**Идея:** Переместить атомарную логику на сторону сервера Redis.

**Реализация:**
```lua
-- create_file.lua
local parent_key = KEYS[1]
local entry_key = KEYS[2]
local inode_key = KEYS[3]
local name = ARGV[1]
local inode_data = ARGV[2]
local parent_data = ARGV[3]
local entry_data = ARGV[4]

-- Проверка существования
local existing = redis.call('HGET', entry_key, name)
if existing then
    return {err = 'EEXIST'}
end

-- Атомарное создание
redis.call('SET', inode_key, inode_data)
redis.call('SET', parent_key, parent_data)
redis.call('HSET', entry_key, name, entry_data)
redis.call('INCR', 'jfs:totalInodes')
redis.call('INCRBY', 'jfs:usedSpace', ARGV[5])

return {ok = 'OK'}
```

**Преимущества:**
- ✅ **Атомарность** сохраняется (Lua script выполняется атомарно)
- ✅ **Батчинг возможен:** Можно отправить N Lua scripts в одном pipeline
- ✅ **1 RTT для батча** вместо N RTT
- ✅ **Консистентность** гарантирована Redis

**Недостатки:**
- ⚠️ Сложность отладки Lua скриптов
- ⚠️ Нужно портировать логику из Go в Lua
- ✅ **Redis Cluster совместимость:** Проблем НЕТ - JuiceFS использует hash tags `{DB}` для всех ключей, обеспечивая размещение всех ключей FS в одном hash slot

**Пример реализации:**
```go
func (m *rueidisMeta) doMknodBatched(ctx Context, ops []MknodOp) []syscall.Errno {
    // Подготовить N Lua script вызовов
    cmds := make([]rueidis.Completed, 0, len(ops))
    
    for _, op := range ops {
        cmd := m.client.B().Eval().
            Script(createFileScript).
            Numkeys(3).
            Key(m.inodeKey(op.parent)).
            Key(m.entryKey(op.parent)).
            Key(m.inodeKey(op.inode)).
            Arg(op.name, op.inodeData, op.parentData, op.entryData).
            Build()
        cmds = append(cmds, cmd)
    }
    
    // ОДИН RTT для всех операций!
    results := m.client.DoMulti(ctx, cmds...)
    
    // Обработать результаты
    errs := make([]syscall.Errno, len(results))
    for i, res := range results {
        errs[i] = parseScriptResult(res)
    }
    return errs
}
```

**Оценка ускорения:**
- **До:** 1000 файлов = 1000 RTT @ 1ms = **1000ms**
- **После:** 1000 файлов = 1 RTT @ 1ms = **1ms**
- **Ускорение:** **1000×** 🚀

---

### Решение 2: Relaxed Consistency (Асинхронные Обновления)

**Идея:** Разделить критические и некритические операции.

**Критические (ДОЛЖНЫ быть атомарны):**
- Создание entry в директории
- Проверка существования файла

**Некритические (могут быть асинхронны):**
- Обновление timestamp родительской директории
- Инкремент счетчиков (used_space, total_inodes)
- Обновление статистики директорий

**Реализация:**
```go
func (m *rueidisMeta) doMknodHybrid(ctx Context, parent Ino, name string, ...) syscall.Errno {
    // Фаза 1: Атомарное создание (транзакция)
    err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
        // Проверка + создание entry (КРИТИЧНО)
        _, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
            pipe.Set(ctx, m.inodeKey(*inode), m.marshal(attr), 0)
            pipe.HSet(ctx, entryKey, name, m.packEntry(_type, *inode))
            return nil
        })
        return err
    }, m.inodeKey(parent), entryKey)
    
    if err != nil {
        return errno(err)
    }
    
    // Фаза 2: Асинхронные обновления (БАТЧИНГ)
    m.batchSet(ctx, m.inodeKey(parent), m.marshal(&pattr)) // timestamp родителя
    m.batchIncrBy(ctx, m.usedSpaceKey(), align4K(0))       // счетчики
    m.batchIncrBy(ctx, m.totalInodesKey(), 1)
    
    return 0
}
```

**Преимущества:**
- ✅ Критические операции атомарны
- ✅ Некритические операции батчатся
- ✅ Частичное ускорение (на некритических операциях)
- ✅ Простая реализация (минимальные изменения)

**Недостатки:**
- ⚠️ Eventual consistency для счетчиков
- ⚠️ Timestamps родителя могут отставать
- ⚠️ **Ускорение ограничено** - критический путь остается медленным

**Оценка ускорения:**
- **До:** 1000 файлов = 1000 RTT @ 1ms = **1000ms**
- **После:** 1000 файлов = 1000 RTT (критич.) + 2 RTT (счетчики) = **1000ms**
- **Ускорение:** **~0%** (критический путь не изменился)

---

### Решение 3: Группировка операций в одной транзакции

**Идея:** Создавать несколько файлов в одной транзакции.

**Проблема:** Требует изменения API - вместо `Mknod(name)` нужно `MknodBatch(names[])`.

**Реализация:**
```go
func (m *rueidisMeta) doMknodBatch(ctx Context, parent Ino, ops []MknodOp) []syscall.Errno {
    results := make([]syscall.Errno, len(ops))
    
    err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
        // Читаем родителя один раз
        pattr := m.getParentAttr(tx, parent)
        
        // Проверяем все имена на существование
        for i, op := range ops {
            if exists := tx.HExists(ctx, entryKey, op.name); exists {
                results[i] = syscall.EEXIST
                continue
            }
        }
        
        // Создаем все файлы в одной транзакции
        _, err := tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
            for i, op := range ops {
                if results[i] != 0 {
                    continue // skip failed ops
                }
                pipe.Set(ctx, m.inodeKey(op.inode), m.marshal(op.attr), 0)
                pipe.HSet(ctx, entryKey, op.name, m.packEntry(TypeFile, op.inode))
            }
            pipe.Set(ctx, m.inodeKey(parent), m.marshal(&pattr), 0)
            pipe.IncrBy(ctx, m.usedSpaceKey(), totalSpace)
            pipe.IncrBy(ctx, m.totalInodesKey(), successCount)
            return nil
        })
        return err
    }, m.inodeKey(parent), entryKey)
    
    return results
}
```

**Преимущества:**
- ✅ Атомарность сохраняется для всего батча
- ✅ N файлов в одной директории = 1 RTT
- ✅ Работает в текущей архитектуре Redis

**Недостатки:**
- ⚠️ **Требует изменения API** верхнего уровня (VFS, FUSE)
- ⚠️ Все операции в одной директории (ограничение)
- ⚠️ Большие транзакции могут блокировать Redis
- ✅ **Redis Cluster совместимость:** Проблем НЕТ - hash tags гарантируют один hash slot для всех ключей FS

**Оценка ускорения:**
- **До:** 1000 файлов в одной директории = 1000 RTT @ 1ms = **1000ms**
- **После:** 1000 файлов в одной директории = 1 RTT @ 1ms = **1ms**
- **Ускорение:** **1000×** (для файлов в одной директории)

---

### Решение 4: Redis Pipelining без транзакций (UNSAFE MODE для PoC)

**Идея:** Использовать `DoMulti()` без WATCH/MULTI/EXEC для максимальной производительности.

**⚠️ ТОЛЬКО для Proof of Concept и изолированных сценариев!**

**Реализация (PoC):**
```go
// pkg/meta/rueidis.go - добавить флаг
type rueidisMeta struct {
    // ... existing fields
    unsafeBatchMode bool  // Enable unsafe batching (no transactions)
}

// Парсинг URI параметра ?unsafe_batch=1
if u.Query().Get("unsafe_batch") == "1" {
    unsafeBatchMode = true
    logger.Warnf("⚠️  UNSAFE BATCH MODE ENABLED - NO ATOMICITY GUARANTEES!")
}

// pkg/meta/rueidis_unsafe_batch.go (новый файл)
func (m *rueidisMeta) doMknodUnsafeBatch(ctx Context, parent Ino, name string, _type uint8, mode, cumask uint16, path string, inode *Ino, attr *Attr) syscall.Errno {
    // Генерация inode (из пула или INCR)
    if *inode == 0 {
        var err error
        *inode, err = m.nextInode()
        if err != nil {
            return errno(err)
        }
    }

    // Подготовка данных БЕЗ проверок
    entryKey := m.entryKey(parent)
    
    // Получить родителя (без WATCH!)
    data, err := m.rdb.Get(ctx, m.inodeKey(parent)).Bytes()
    if err != nil {
        return errno(err)
    }
    
    var pattr Attr
    m.parseAttr(data, &pattr)
    
    // Обновить timestamps
    now := time.Now()
    pattr.Mtime = now.Unix()
    pattr.Mtimensec = uint32(now.Nanosecond())
    pattr.Ctime = now.Unix()
    pattr.Ctimensec = uint32(now.Nanosecond())
    
    // Инициализировать новый inode
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
    
    // UNSAFE PIPELINE - без транзакций!
    cmds := []rueidis.Completed{
        // 1. Создать новый inode
        m.client.B().Set().
            Key(m.inodeKey(*inode)).
            Value(string(m.marshal(attr))).
            Build(),
        
        // 2. Обновить родителя
        m.client.B().Set().
            Key(m.inodeKey(parent)).
            Value(string(m.marshal(&pattr))).
            Build(),
        
        // 3. Добавить entry (БЕЗ проверки на существование!)
        m.client.B().Hset().
            Key(entryKey).
            FieldValue().
            FieldValue(name, string(m.packEntry(_type, *inode))).
            Build(),
        
        // 4. Обновить счетчики
        m.client.B().Incrby().
            Key(m.usedSpaceKey()).
            Increment(align4K(0)).
            Build(),
        
        m.client.B().Incr().
            Key(m.totalInodesKey()).
            Build(),
    }
    
    // ОДИН RTT для всех операций!
    results := m.client.DoMulti(ctx, cmds...)
    
    // Проверить ошибки
    for i, res := range results {
        if err := res.Error(); err != nil {
            logger.Errorf("Unsafe batch operation %d failed: %v", i, err)
            return errno(err)
        }
    }
    
    return 0
}
```

**Интеграция в doMknod:**
```go
func (m *rueidisMeta) doMknod(ctx Context, parent Ino, name string, _type uint8, mode, cumask uint16, path string, inode *Ino, attr *Attr) syscall.Errno {
    if m.unsafeBatchMode {
        // ⚠️ UNSAFE: максимальная производительность, риск race conditions
        return m.doMknodUnsafeBatch(ctx, parent, name, _type, mode, cumask, path, inode, attr)
    }
    
    // SAFE: текущая реализация с транзакциями
    return m.doMknodSafe(ctx, parent, name, _type, mode, cumask, path, inode, attr)
}
```

**Преимущества PoC:**
- ✅ **Простая реализация:** ~100 строк кода
- ✅ **Максимальная производительность:** 500-1000× ускорение
- ✅ **Быстрая проверка гипотезы:** Работает ли батчинг вообще?
- ✅ **Безопасно для изолированных тестов:** Один клиент = нет race conditions

**Риски (для осведомленности):**
- ❌ **Race conditions при нескольких клиентах**
- ❌ **Orphaned inodes при конфликтах**
- ❌ **Утечка счетчиков**
- ⚠️ **ТОЛЬКО для тестирования или строго изолированных сценариев!**

**Пример race condition (при нескольких клиентах):**
```
Client A                          Client B
────────────────────────────────────────────────────
HGET entry:1 "file.txt" → nil
                                  HGET entry:1 "file.txt" → nil
HSET entry:1 "file.txt" inode=100
                                  HSET entry:1 "file.txt" inode=101
Result: file.txt points to inode=101, but inode=100 also exists
        ❌ INCONSISTENCY: orphaned inode (но FS работает)
```

**Когда БЕЗОПАСНО использовать Unsafe Mode:**
- ✅ **Один клиент** (ваш тестовый сценарий)
- ✅ **Bulk import данных** (tar -xf, cp -r)
- ✅ **Build artifacts в CI/CD** (изолированные задачи)
- ✅ **Scratch/temporary storage** (не критичные данные)
- ✅ **Performance тестирование** (замер максимальной производительности)

**Когда НЕ использовать:**
- ❌ **Несколько клиентов** монтируют одну FS
- ❌ **Kubernetes PV** с несколькими подами
- ❌ **Production workloads**
- ❌ **Критичные данные** (БД, логи)

---

### Решение 5: Кеширование метаданных на клиенте (Оптимизация чтений)

**Идея:** Не поможет для записей, но может ускорить чтения при создании файлов.

**Применимость:**
- ✅ Ускорение `stat()`, `getattr()`, `readdir()`
- ❌ НЕ ускорение `create()`, `unlink()`, `write()`

**Вывод:** Не решает проблему создания файлов.

---

## 📋 Сравнение Решений

| Решение | Ускорение | Атомарность | Сложность | Совместимость API | Redis Cluster | Рекомендация |
|---------|-----------|-------------|-----------|-------------------|---------------|--------------|
| **1. Lua Scripts** | **1000×** ✅ | ✅ Гарантирована | ⚠️ Средняя | ✅ Да | ✅ **Hash tags** | ⭐⭐⭐⭐⭐ **Production** |
| **2. Relaxed Consistency** | ~0% ❌ | ⚠️ Eventual | ✅ Низкая | ✅ Да | ✅ Да | ⭐⭐ Не эффективно |
| **3. Batch API** | 1000×* ✅ | ✅ Гарантирована | ⚠️ Средняя | ❌ Нет | ✅ **Hash tags** | ⭐⭐⭐⭐ Хорошо, но нужен новый API |
| **4. Unsafe Pipeline** | 1000× ✅ | ❌ **ОПАСНО** | ✅ **Очень низкая** | ✅ Да | ✅ Да | ⭐⭐⭐ **PoC / Isolated use** |
| **5. Client Cache** | 0% ❌ | N/A | ✅ Низкая | ✅ Да | ✅ Да | ⭐ Только для чтений |

\* Только для файлов в одной директории

**Примечание:** JuiceFS использует Redis hash tags (`{DB}` prefix) для всех ключей файловой системы, гарантируя их размещение в одном hash slot в Redis Cluster. Это обеспечивает корректную работу транзакций и Lua скриптов в кластерном режиме без дополнительных ограничений.

**Стратегия реализации:**
1. **Фаза 1 (PoC):** Unsafe Pipeline - проверка производительности (~1-2 дня)
2. **Фаза 2 (Production):** Lua Scripts - безопасная версия с той же производительностью (~2-3 недели)

---

## 🏆 Рекомендуемое Решение: Lua Scripts + Batching

### Этап 1: Lua Scripts для Критических Операций

**Файлы для изменения:**
1. `pkg/meta/rueidis_lua_scripts.go` (новый файл)
2. `pkg/meta/rueidis.go` (добавить batch методы)

**Скрипты для реализации:**
- `create_file.lua` - атомарное создание файла
- `delete_file.lua` - атомарное удаление файла
- `write_chunk.lua` - атомарная запись chunk

**Пример Lua скрипта:**
```lua
-- create_file.lua
-- KEYS: parent_inode_key, entry_key, inode_key, used_space_key, total_inodes_key
-- ARGV: name, inode_data, parent_data, entry_data, space_delta

local entry_key = KEYS[2]
local name = ARGV[1]

-- Проверка существования
local existing = redis.call('HEXISTS', entry_key, name)
if existing == 1 then
    return redis.error_reply('EEXIST')
end

-- Атомарное создание всех ключей
redis.call('SET', KEYS[1], ARGV[3])  -- parent inode
redis.call('SET', KEYS[3], ARGV[2])  -- new inode
redis.call('HSET', KEYS[2], name, ARGV[4])  -- entry
redis.call('INCRBY', KEYS[4], ARGV[5])  -- used_space
redis.call('INCR', KEYS[5])  -- total_inodes

return redis.status_reply('OK')
```

**Реализация батчинга:**
```go
// pkg/meta/rueidis_batch_mknod.go (новый файл)

type BatchMknodOp struct {
    Parent Ino
    Name   string
    Type   uint8
    Mode   uint16
    Inode  Ino
    Attr   *Attr
}

func (m *rueidisMeta) BatchMknod(ctx Context, ops []BatchMknodOp) []syscall.Errno {
    if len(ops) == 0 {
        return nil
    }
    
    // Подготовить Lua script команды для каждой операции
    cmds := make([]rueidis.Completed, 0, len(ops))
    
    for _, op := range ops {
        // Сериализовать данные
        inodeData := m.marshal(op.Attr)
        parentData := m.marshalParentUpdate(op.Parent, ...)
        entryData := m.packEntry(op.Type, op.Inode)
        
        // Собрать Lua команду
        cmd := m.client.B().Eval().
            Script(createFileScript).
            Numkeys(5).
            Key(m.inodeKey(op.Parent)).
            Key(m.entryKey(op.Parent)).
            Key(m.inodeKey(op.Inode)).
            Key(m.usedSpaceKey()).
            Key(m.totalInodesKey()).
            Arg(op.Name, inodeData, parentData, entryData, "0").
            Build()
            
        cmds = append(cmds, cmd)
    }
    
    // БАТЧ: Отправить ВСЕ команды в одном pipeline
    results := m.client.DoMulti(ctx, cmds...)
    
    // Обработать результаты
    errs := make([]syscall.Errno, len(ops))
    for i, res := range results {
        if err := res.Error(); err != nil {
            errs[i] = parseScriptError(err)
        } else {
            errs[i] = 0 // success
        }
    }
    
    return errs
}
```

### Этап 2: Интеграция с существующим кодом

**Вариант A: Transparent batching (рекомендуется)**

Накапливать операции и флашить периодически:

```go
type pendingMknod struct {
    op       BatchMknodOp
    resultCh chan syscall.Errno
}

type batchMknodAccumulator struct {
    mu      sync.Mutex
    pending []pendingMknod
    ticker  *time.Ticker
}

func (m *rueidisMeta) doMknod(ctx Context, parent Ino, name string, ...) syscall.Errno {
    // Создать операцию
    op := BatchMknodOp{
        Parent: parent,
        Name:   name,
        ...
    }
    
    resultCh := make(chan syscall.Errno, 1)
    
    // Добавить в накопитель
    m.mknodAccumulator.mu.Lock()
    m.mknodAccumulator.pending = append(m.mknodAccumulator.pending, pendingMknod{
        op:       op,
        resultCh: resultCh,
    })
    shouldFlush := len(m.mknodAccumulator.pending) >= 512 // batch_size
    m.mknodAccumulator.mu.Unlock()
    
    if shouldFlush {
        m.flushMknodBatch()
    }
    
    // Ждать результата
    return <-resultCh
}

func (m *rueidisMeta) flushMknodBatch() {
    m.mknodAccumulator.mu.Lock()
    batch := m.mknodAccumulator.pending
    m.mknodAccumulator.pending = nil
    m.mknodAccumulator.mu.Unlock()
    
    if len(batch) == 0 {
        return
    }
    
    // Извлечь операции
    ops := make([]BatchMknodOp, len(batch))
    for i, p := range batch {
        ops[i] = p.op
    }
    
    // Выполнить батч
    results := m.BatchMknod(ctx, ops)
    
    // Отправить результаты
    for i, p := range batch {
        p.resultCh <- results[i]
        close(p.resultCh)
    }
}
```

**Вариант B: Explicit batching API (проще, но требует изменения VFS)**

Добавить новый метод в Meta interface:
```go
type Meta interface {
    // ... existing methods
    
    // BatchCreate creates multiple files atomically using Lua scripts
    BatchCreate(ctx Context, ops []CreateOp) []syscall.Errno
}
```

---

### Этап 3: Мониторинг и метрики

Добавить новые Prometheus метрики:

```go
batchMknodOps = prometheus.NewCounter(prometheus.CounterOpts{
    Name: "rueidis_batch_mknod_operations_total",
    Help: "Total number of batched mknod operations",
})

batchMknodSize = prometheus.NewHistogram(prometheus.HistogramOpts{
    Name:    "rueidis_batch_mknod_size",
    Help:    "Number of mknod operations per batch",
    Buckets: []float64{1, 10, 50, 100, 250, 512, 1000},
})

batchMknodDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
    Name:    "rueidis_batch_mknod_duration_seconds",
    Help:    "Time taken to execute batched mknod operations",
    Buckets: prometheus.ExponentialBuckets(0.0001, 2, 15),
})
```

---

### Этап 4: Тестирование

**Микробенчмарк:**
```go
func BenchmarkMknod_Sequential(b *testing.B) {
    // Старый подход: каждый файл отдельно
    for i := 0; i < b.N; i++ {
        m.Mknod(ctx, parent, fmt.Sprintf("file%d", i), ...)
    }
}

func BenchmarkMknod_Batched(b *testing.B) {
    // Новый подход: батчи по 512 файлов
    ops := make([]BatchMknodOp, 512)
    for i := 0; i < b.N; i += 512 {
        for j := 0; j < 512; j++ {
            ops[j] = BatchMknodOp{...}
        }
        m.BatchMknod(ctx, ops)
    }
}
```

**Ожидаемые результаты:**
- Sequential: ~1000 RTT/sec (при 1ms RTT)
- Batched: ~512000 ops/sec (при 1ms RTT, batch=512)
- **Ускорение: 512×**

---

## 📊 Ожидаемый Эффект

### Сценарий: Создание 10,000 мелких файлов

**Текущая реализация (с транзакциями):**
```
Операций к Redis: 10,000 транзакций
Латентность (1ms RTT): 10,000ms = 10 секунд
Throughput: 1,000 files/sec
```

**С Lua Scripts + Batching (batch_size=512):**
```
Операций к Redis: ⌈10,000 / 512⌉ = 20 pipeline запросов
Латентность (1ms RTT): 20ms = 0.02 секунды
Throughput: 500,000 files/sec
```

**Ускорение: 500×** 🚀

---

### Сценарий: Удаление 10,000 мелких файлов

**Текущая реализация:**
```
Операций к Redis: 10,000 транзакций
Латентность: 10 секунд
```

**С Lua Scripts + Batching:**
```
Операций к Redis: 20 pipeline запросов
Латентность: 0.02 секунды
Ускорение: 500×
```

---

## 🔧 План Реализации (Двухфазный подход)

### 🚀 Фаза 1: Unsafe Pipeline PoC (1-2 дня) - ТЕКУЩАЯ

**Цель:** Проверить гипотезу что батчинг реально даёт 100-1000× ускорение для мелких файлов.

**Задачи:**
1. ✅ Добавить URI параметр `?unsafe_batch=1`
2. ✅ Реализовать `doMknodUnsafeBatch()` без транзакций
3. ✅ Реализовать `doUnlinkUnsafeBatch()` без транзакций
4. ✅ Добавить WARNING логи при включении unsafe режима
5. ✅ Написать benchmark для сравнения производительности
6. ✅ Измерить ускорение на реальных данных

**Файлы для создания:**
- `pkg/meta/rueidis_unsafe_batch.go` (~200 строк)
- `pkg/meta/rueidis_unsafe_batch_test.go` (~100 строк)

**Тестовый сценарий:**
```bash
# Создать FS с unsafe батчингом
juicefs format --storage file \
    "rueidis://localhost:6379/1?unsafe_batch=1&metaprime=1" \
    /tmp/jfs-test

# Бенчмарк: создание 10,000 мелких файлов
time for i in {1..10000}; do 
    touch /mnt/jfs/file$i.txt
done

# Ожидаемый результат:
# БЕЗ unsafe_batch: ~10-20 секунд (1000 files/sec)
# С unsafe_batch:    ~0.02-0.05 секунды (200,000+ files/sec)
# Ускорение: 100-500×
```

**Критерий успеха:**
- ✅ Ускорение >= 50× для создания/удаления файлов
- ✅ Файловая система остаётся работоспособной
- ✅ Данные не повреждены (при одном клиенте)

**Если PoC успешен → переходим к Фазе 2 (Safe mode)**

---

### 🛡️ Фаза 2: Lua Scripts Production (2-3 недели) - ПОСЛЕ PoC

**Цель:** Реализовать безопасную версию с той же производительностью.

#### Шаг 1: Lua Scripts для Критических Операций (1 неделя)

**Файлы для изменения:**
1. `pkg/meta/rueidis_lua_scripts.go` (новый файл, ~400 строк)
2. `pkg/meta/rueidis.go` (добавить загрузку скриптов)

**Скрипты для реализации:**
- `create_file.lua` - атомарное создание файла
- `delete_file.lua` - атомарное удаление файла
- `write_chunk.lua` - атомарная запись chunk

**Пример Lua скрипта:**
```lua
-- create_file.lua
-- KEYS: parent_inode_key, entry_key, inode_key, used_space_key, total_inodes_key
-- ARGV: name, inode_data, parent_data, entry_data, space_delta

local entry_key = KEYS[2]
local name = ARGV[1]

-- Проверка существования (АТОМАРНО)
local existing = redis.call('HEXISTS', entry_key, name)
if existing == 1 then
    return redis.error_reply('EEXIST')
end

-- Атомарное создание всех ключей
redis.call('SET', KEYS[1], ARGV[3])  -- parent inode
redis.call('SET', KEYS[3], ARGV[2])  -- new inode
redis.call('HSET', KEYS[2], name, ARGV[4])  -- entry
redis.call('INCRBY', KEYS[4], ARGV[5])  -- used_space
redis.call('INCR', KEYS[5])  -- total_inodes

return redis.status_reply('OK')
```

#### Шаг 2: Batch Accumulator (1 неделя)

**Transparent batching** - накапливать операции и флашить автоматически:

```go
type batchMknodAccumulator struct {
    mu      sync.Mutex
    pending []pendingMknod
    ticker  *time.Ticker
}

func (m *rueidisMeta) doMknod(...) syscall.Errno {
    if m.safeBatchMode {
        // Добавить в аккумулятор
        return m.accumulateMknod(...)
    }
    // Fallback: текущая транзакция
    return m.doMknodTransaction(...)
}
```

#### Шаг 3: Testing & Metrics (1 неделя)
1. ✅ Unit тесты для Lua скриптов
2. ✅ Integration тесты для батчинга
3. ✅ Stress тесты (параллельные клиенты)
4. ✅ Prometheus метрики
5. ✅ Бенчмарки

**Ожидаемые результаты:**
- Sequential: ~1000 RTT/sec (при 1ms RTT)
- Batched: ~512000 ops/sec (при 1ms RTT, batch=512)
- **Ускорение: 512×** (как Unsafe, но безопасно!)

---

### 📊 Сравнение: Unsafe PoC vs Lua Scripts Production

| Критерий | Unsafe PoC | Lua Scripts |
|----------|-----------|-------------|
| **Время реализации** | 1-2 дня ✅ | 2-3 недели |
| **Производительность** | 1000× ✅ | 1000× ✅ |
| **Атомарность** | ❌ Нет | ✅ Да |
| **Race conditions** | ⚠️ Возможны | ✅ Защита |
| **Production ready** | ❌ Только изолированно | ✅ Да |
| **Сложность кода** | Очень низкая | Средняя |

---

### 🎯 Итоговый План

1. **Сейчас:** Реализуем Unsafe PoC (1-2 дня)
   - Проверяем гипотезу о производительности
   - Измеряем реальное ускорение
   - Понимаем узкие места

2. **Если PoC успешен:** Реализуем Lua Scripts (2-3 недели)
   - Портируем логику в Lua
   - Добавляем batch accumulator
   - Production-ready версия

3. **Если PoC не даёт ускорения:** Ищем другие причины медленности
   - Возможно проблема не в RTT
   - Возможно Redis сам медленный
   - Возможно network latency не главный bottleneck

---

## ⚠️ Потенциальные Риски

### 1. Redis Cluster Hash Slot Ограничения ✅ УЖЕ РЕШЕНО

**Анализ кода:**

JuiceFS **УЖЕ использует hash tags** для обеспечения размещения всех ключей файловой системы в одном hash slot!

**Код из `pkg/meta/redis.go` lines 252-253:**
```go
rdb = redis.NewClusterClient(&copt)
prefix = fmt.Sprintf("{%d}", opt.DB)  // ← Hash tag!
```

**Как это работает:**

1. **Standalone режим:** `prefix = ""` (пустая строка)
2. **Cluster режим:** `prefix = "{10}"` (например, для DB=10)

**Все ключи в JuiceFS имеют этот prefix:**
```go
// Примеры ключей (из redis.go lines 570-694):
func (m *redisMeta) inodeKey(inode Ino) string {
    return m.prefix + "i" + inode.String()  // "{10}i1234"
}

func (m *redisMeta) entryKey(inode Ino) string {
    return m.prefix + "d" + inode.String()  // "{10}d5678"
}

func (m *redisMeta) chunkKey(inode Ino, indx uint32) string {
    return m.prefix + "c" + inode.String() + "_" + strconv.FormatInt(int64(indx), 10)  // "{10}c1234_0"
}

func (m *redisMeta) usedSpaceKey() string {
    return m.prefix + usedSpace  // "{10}usedSpace"
}
```

**Redis Cluster Hash Tag механизм:**

По спецификации Redis Cluster ([Hash Tags](https://redis.io/docs/reference/cluster-spec/#hash-tags)):
> Only the part between the first occurrence of `{` and the first occurrence of `}` is hashed to determine the hash slot.

**Для ключа `"{10}i1234"`:**
- Часть для хеширования: `10` (между `{` и `}`)
- Все ключи с префиксом `{10}` → **ОДИН hash slot**
- Все ключи с префиксом `{5}` → **другой hash slot**

**Документация JuiceFS подтверждает:**

Из `docs/en/administration/metadata/redis_best_practices.md` lines 82-90:
> Redis Cluster does not support multiple databases. However, it splits the key space into 16384 hash slots, and distributes the slots to several nodes. **Based on Redis Cluster's Hash Tag feature, JuiceFS adds `{DB}` before all file system keys to ensure they will be hashed to the same hash slot**, assuring that transactions can still work. Besides, one Redis Cluster can serve for multiple JuiceFS file systems as long as they use different db numbers.

**Вывод:**

✅ **НЕТ ПРОБЛЕМЫ с hash slots в Redis Cluster!**

- Все ключи одной файловой системы → один hash slot
- Lua скрипты могут безопасно работать с любыми ключами FS
- Транзакции (WATCH/MULTI/EXEC) работают корректно
- Батчинг с Lua scripts будет работать без дополнительной группировки

**Что это значит для Lua Scripts решения:**

```go
// ✅ РАБОТАЕТ - все ключи в одном hash slot благодаря {DB} prefix
func (m *rueidisMeta) BatchMknod(ctx Context, ops []BatchMknodOp) []syscall.Errno {
    cmds := make([]rueidis.Completed, 0, len(ops))
    
    for _, op := range ops {
        // Все эти ключи имеют prefix = "{10}" (например)
        // Значит все в одном hash slot - Lua скрипт РАБОТАЕТ!
        cmd := m.client.B().Eval().
            Script(createFileScript).
            Numkeys(5).
            Key(m.inodeKey(op.Parent)).      // "{10}i5678"
            Key(m.entryKey(op.Parent)).      // "{10}d5678"
            Key(m.inodeKey(op.Inode)).       // "{10}i9999"
            Key(m.usedSpaceKey()).            // "{10}usedSpace"
            Key(m.totalInodesKey()).          // "{10}totalInodes"
            Arg(...).
            Build()
        cmds = append(cmds, cmd)
    }
    
    // Все Lua скрипты выполняются на ОДНОМ узле кластера
    results := m.client.DoMulti(ctx, cmds...)
    return processResults(results)
}
```

**Дополнительное преимущество:**

Несколько файловых систем на одном Redis Cluster:
- FS #1: prefix = `{1}` → hash slot A
- FS #2: prefix = `{2}` → hash slot B
- FS #3: prefix = `{10}` → hash slot C

Каждая FS изолирована в своем hash slot, но все могут использовать один кластер!

### 2. Lua Script Debugging

**Проблема:** Ошибки в Lua скриптах сложно отлаживать.

**Решение:**
- Обширное логирование в Lua (`redis.log(redis.LOG_NOTICE, ...)`)
- Unit тесты для каждого скрипта
- Staging environment для тестирования

### 3. Версионирование Lua Scripts

**Проблема:** Изменения в Lua скриптах при rolling update могут сломать совместимость.

**Решение:**
- Версионирование скриптов (v1, v2, v3)
- Поддержка нескольких версий одновременно
- Graceful migration

---

## 📚 Дополнительные Рекомендации

### Для текущей реализации (без Lua)

**Временное решение - оптимизация транзакций:**

1. **Использовать пре-allocation для inode IDs:**
   ```go
   // Вместо INCR для каждого файла
   nextInode := m.preallocateInodes(ctx, 1000) // batch allocation
   ```
   Уже реализовано через `metaPrimeEnabled`, но по умолчанию ВЫКЛЮЧЕНО!

2. **Включить ID batching по умолчанию:**
   ```go
   metaPrimeEnabled := true  // было: false
   ```
   Это даст небольшое ускорение (на INCR операциях).

3. **Оптимизировать размер транзакций:**
   - Убрать ненужные операции из TxPipelined
   - Использовать HSET для нескольких полей вместо множественных HSET

**Эффект:** ~5-10% ускорение (не существенно).

---

## 🎯 Итоговые Выводы

### Текущее Состояние

✅ **Батчинг реализован** (`rueidis_batch_write.go`, 1155 строк)  
❌ **Батчинг НЕ ИСПОЛЬЗУЕТСЯ** для критических операций (create/delete файлов)  
❌ **Параметры `?batch_size`, `?batch_interval` НЕ ВЛИЯЮТ** на производительность мелких файлов  
❌ **Каждое создание файла = 1 RTT** к Redis (транзакция)  

### Корневая Причина

Создание и удаление файлов требуют **атомарности** и защиты от **race conditions**.  
Текущая реализация использует Redis транзакции (WATCH/MULTI/EXEC).  
Транзакции **несовместимы** с асинхронным батчингом.  

### Стратегия Решения (Двухфазная)

#### Фаза 1: Unsafe Pipeline PoC (1-2 дня) ← **ТЕКУЩАЯ**

**Цель:** Проверить гипотезу о производительности батчинга.

**Реализация:**
- ✅ URI параметр `?unsafe_batch=1`
- ✅ `doMknodUnsafeBatch()` без транзакций
- ✅ `doUnlinkUnsafeBatch()` без транзакций
- ✅ Benchmark для измерения ускорения

**Ожидаемый результат:**
- Ускорение: **100-500×** для мелких файлов
- Throughput: **100,000-500,000 files/sec** (vs 500-1000 сейчас)
- Только для изолированного тестирования (один клиент)

**Риски:**
- ⚠️ Race conditions при нескольких клиентах
- ⚠️ Orphaned inodes при конфликтах
- ⚠️ **НЕ для production** - только PoC!

**Файлы:**
- См. `.vscode/UNSAFE_BATCH_POC.md` для детального плана

---

#### Фаза 2: Lua Scripts Production (2-3 недели) ← **ПОСЛЕ PoC**

**Цель:** Production-ready версия с той же производительностью.

**Преимущества:**
- ✅ Атомарность сохраняется (Lua выполняется атомарно)
- ✅ Та же производительность (100-1000×)
- ✅ Защита от race conditions
- ✅ Production-ready
- ✅ Redis Cluster совместимость (hash tags)

**Реализация:**
- Портировать логику в Lua скрипты
- Batch accumulator для transparent batching
- Метрики и мониторинг
- Comprehensive тестирование

---

### Рекомендация

⭐⭐⭐⭐⭐ **Двухфазный подход:**

1. **Сначала:** Unsafe PoC (1-2 дня)
   - Подтверждаем гипотезу о производительности
   - Измеряем реальное ускорение
   - Минимальные усилия

2. **Потом (если PoC успешен):** Lua Scripts (2-3 недели)
   - Production-ready версия
   - Безопасность + производительность
   - Feature flag для постепенного rollout

**Если PoC НЕ даст ускорения:**
→ Проблема не в транзакциях, ищем другие bottleneck'и  
→ Экономим 2-3 недели на Lua implementation

---

**Следующий шаг:** Реализовать Unsafe PoC согласно `.vscode/UNSAFE_BATCH_POC.md`

---

**Статус документа:** Анализ завершен, PoC план готов  
**Дата:** 2025-10-30  
**Автор:** AI Analysis  
**Версия:** 2.0 (updated with PoC strategy)
