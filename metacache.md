Отличная идея. Ты, по сути, предлагаешь сделать `rueidis.go` "переключателем" (router), который может работать либо в режиме "кэширование в памяти" (текущее), либо в режиме "локальный write-back" (`fast-meta`).

Это самый чистый подход. Он сохраняет всю твою работу по `rueidis` (инвалидация, пакетная обработка) и расширяет ее.

Вот пошаговый план реализации, учитывающий все наши обсуждения:

-----

### 🏛️ Фаза 1: Инициализация и Интеграция BadgerDB

Цель: "Научить" `rueidis.go` работать с двумя режимами и инициализировать BadgerDB.

  * **Шаг 1: Модификация `rueidisMeta` (в `rueidis.go`)**
      * Добавь в структуру `rueidisMeta` два новых поля:
        ```go
        type rueidisMeta struct {
            *redisMeta
            // ... (все существующие поля) ...
            
            fastMetaEnabled bool      // Флаг, что ?fast-meta=1 включен
            localCache      tkvClient // Интерфейс для локального BadgerDB
            oplogQueue      chan *metaOplog // Очередь "грязных" транзакций
        }
        ```
  * **Шаг 2: Модификация `newRueidisMeta` (в `rueidis.go`)**
      * Добавь парсинг `?fast-meta=1` из URI (аналогично тому, как ты парсишь `?ttl=`, строка 149). Сохрани результат в `m.fastMetaEnabled`.
      * **Если `m.fastMetaEnabled == true`:**
        1.  **Инициализация `localCache`:**
              * Получи путь к кэшу данных: `conf.CacheDir`.
              * Создай путь для кэша метаданных: `localPath := filepath.Join(conf.CacheDir, "meta")`.
              * Инициализируй Badger: `m.localCache, err = newTkvClient("badger", localPath)`.
        2.  **Инициализация Очереди:** `m.oplogQueue = make(chan *metaOplog, 1024)` (размер подберешь).
        3.  **Запуск `syncWorker`:** `go m.runSyncWorker()` (этот воркер мы напишем в Фазе 4).
  * **Шаг 3: Тестирование Фазы 1**
      * Запусти `juicefs mount` с `rueidis://...` -\> Убедись, что `m.localCache == nil`.
      * Запусти `juicefs mount` с `rueidis://...?fast-meta=1` -\> Убедись, что `m.localCache != nil` и папка `/meta` создана в `CacheDir`.

-----

### 📖 Фаза 2: Логика Чтения (Read-Through Cache) и Инвалидация

Цель: Заставить все операции чтения работать через Badger, используя Redis как бэкенд, и автоматически инвалидировать кэш.

  * **Шаг 4: Настройка Инвалидации `CLIENT TRACKING`**

      * В `newRueidisMeta`, при создании `rueidis.ClientOption` (строка 413):
      * **Если `m.fastMetaEnabled == true`:**
        1.  **Определи коллбэк `OnInvalidation`:**
            ```go
            invalidationCallback := func(messages []rueidis.RedisMessage) {
                keys := parseKeysFromMsg(messages) // (вспомогательная функция для парсинга)
                if len(keys) > 0 {
                    // Асинхронно удаляем из Badger, чтобы не блокировать сеть
                    go m.localCache.txn(context.Background(), func(tx *kvTxn) error {
                        for _, key := range keys {
                            tx.delete(key)
                        }
                        return nil
                    })
                }
            }
            ```
        2.  Установи опции клиента:
              * `opt.OnInvalidation = invalidationCallback` (передаем наш коллбэк).
              * `opt.DisableCache = true` (**Важно\!** Отключаем *внутренний* кэш `rueidis` в памяти, т.к. у нас теперь есть Badger).

  * **Шаг 5: Модификация всех операций ЧТЕНИЯ**

      * Тебе нужно будет модифицировать **все** функции, которые читают данные (например, `doGetAttr`, `doLookup`, `doRead`, `GetXattr`, `doReaddir` и т.д.).
      * Каждая функция должна начинаться с "маршрутизатора":
        ```go
        func (m *rueidisMeta) doGetAttr(ctx Context, inode Ino, attr *Attr) syscall.Errno {
            if !m.fastMetaEnabled {
                // Старая логика (как сейчас в rueidis.go)
                return m.redisMeta.doGetAttr(ctx, inode, attr)
            }

            // Новая логика ?fast-meta=1:
            // 1. Пытаемся прочитать из Badger
            key := m.inodeKey(inode)
            err := m.localCache.simpleTxn(ctx, func(tx *kvTxn) error {
                data := tx.get(key)
                if data == nil { return syscall.ENOENT } // cache miss
                m.parseAttr(data, attr)
                return nil
            }, 0)
            
            if err == nil { return 0 } // Нашли в Badger, успех!

            // 2. Cache Miss: читаем из Redis
            data, err := m.compat.Get(ctx, key).Bytes() // m.compat - это rueidis
            if err != nil { return errno(err) }

            // 3. Сохраняем в Badger
            m.localCache.txn(ctx, func(tx *kvTxn) error {
                tx.set(key, data)
                return nil
            })
            
            m.parseAttr(data, attr)
            return 0
        }
        ```

  * **Шаг 6: Тестирование Фазы 2**

      * Запусти 2 клиента (А и Б) с `?fast-meta=1`.
      * Клиент А: `ls /` (кэш Badger наполняется).
      * Клиент Б: `mkdir /newdir`.
      * Клиент А: `ls /` (должен *немедленно* увидеть `newdir`, т.к. коллбэк `OnInvalidation` очистил старый кэш `ls`).

-----

### ⚡ Фаза 3: Логика Записи (Write-Back Cache)

Цель: Заставить `create`, `rename`, `unlink` отвечать мгновенно.

  * **Шаг 7: Создание `oplog` и `recordingTxn`**

      * Определи структуры `metaOp` (`{op, key, value, delta}`) и `metaOplog` (`[]*metaOp`).
      * Создай обертку `recordingTxn` (для `kvtxn`), которая при вызове `set/delete/incrBy` одновременно:
        1.  Выполняет операцию на реальном `localTxn` (Badger).
        2.  Добавляет операцию в срез `ops` (`metaOplog`).

  * **Шаг 8: Модификация `rueidisMeta.txn()` (Главный маршрутизатор записи)**

      * Перепиши `rueidisMeta.txn` кардинально:
        ```go
        func (m *rueidisMeta) txn(ctx Context, txf func(tx rueidiscompat.Tx) error, keys ...string) error {
            if !m.fastMetaEnabled {
                // Старая логика: синхронный WATCH/MULTI/EXEC в Redis
                return m.redisMeta.txn(ctx, txf, keys...)
            }

            // Новая логика ?fast-meta=1:
            var oplog *metaOplog
            
            // 1. Выполняем транзакцию ЛОКАЛЬНО в Badger
            err := m.localCache.txn(ctx, func(tx *kvTxn) error {
                // Создаем "записывающий" kvtxn
                recorder := &recordingTxn{localTxn: tx, ops: make(metaOplog, 0)}
                
                // Обертываем его в kvTxn, чтобы передать в redisMeta-функции
                kvTxnWrapper := &kvTxn{kvtxn: recorder, retry: 0}

                // !!! Хак: твои doMknod/doRename и т.д. в redis.go 
                // ожидают `rueidiscompat.Tx`. Нам нужно их обмануть
                // или переписать их, чтобы они принимали `kvtxn`.
                // Проще всего будет скопировать логику doMknod/doRename и т.д.
                // из redis.go и заставить их работать с `kvTxnWrapper`.
                
                // ВМЕСТО f(txf) мы должны будем вызывать нашу
                // обертку, которая вызывает txf с поддельным Tx
                // ... (Это самая сложная "грязная" часть) ...
                
                // ---- УПРОЩЕННЫЙ ВАРИАНТ ----
                // Предположим, мы переписали/адаптировали `txf` 
                // чтобы он работал с нашим `kvTxnWrapper`
                
                // err := txf(kvTxnWrapper) // (Это псевдокод)
                
                // ---- РЕАЛИСТИЧНЫЙ ВАРИАНТ ----
                // Функции типа doMknod/doRename/doUnlink уже реализованы 
                // в `redisMeta` и принимают `rueidiscompat.Tx`.
                // Мы не можем их вызвать.
                // Нам нужно будет скопировать их реализацию из `redis.go`
                // и адаптировать для работы с `kvtxn` (как в `tkv.go`).
                // `m.txn` в `tkv.go` (строка 1046) - идеальный пример.
                
                // ... (Выполняем txf, используя recorder)
                
                // 7. Сохраняем Oplog в Badger АТОМАРНО
                oplogKey := []byte(fmt.Sprintf("oplog:%d", time.Now().UnixNano()))
                serializedOps, _ := json.Marshal(recorder.ops)
                tx.set(oplogKey, serializedOps)
                
                // Захватываем oplog для отправки в очередь
                oplog = &recorder.ops 
                return nil // Коммитим Badger
            }, 0) // 0 ретраев, т.к. это локально
            
            if err != nil {
                return err // Локальная ошибка
            }

            // 8. Отправляем oplog в очередь (неблокирующе)
            select {
            case m.oplogQueue <- oplog:
            default:
                logger.Warnf("Oplog queue is full! Write will be delayed.")
                // Воркер все равно подхватит его из Badger при рестарте,
                // но мы можем добавить его в отдельную очередь
            }
            
            // 9. Немедленно возвращаем успех
            return nil 
        }
        ```

  * **Шаг 9: Тестирование Фазы 3**

      * `?fast-meta=1`: `time touch /mnt/jfs/testfile` должен выполняться мгновенно.
      * Проверь Badger: там должны появиться `A:ino`, `D:parent/testfile` и `oplog:<id>`.
      * `ls` на *другом* клиенте не должен видеть `testfile` (пока).

-----

### 🛰️ Фаза 4: Фоновый Синхронизатор (`syncWorker`)

Цель: Отправлять `oplog` из Badger в Redis и обрабатывать конфликты.

  * **Шаг 10: Реализация `runSyncWorker()`**

      * Это `for oplog := range m.oplogQueue { ... }`.
      * Внутри цикла вызови `m.syncOplog(oplog)`.

  * **Шаг 11: Реализация `syncOplog(oplog *metaOplog)`**

      * `keysToWatch := extractKeysFromOplog(oplog)`
      * `err := m.redisMeta.txn(ctx, func(tx rueidiscompat.Tx) error { ... }, keysToWatch...)`
          * Внутри `txn` используй `TxPipelined` и примени все `op` из `oplog` в Redis (как в `rueidis_batch_write.go`).
      * **Обработка ошибок (Важно\!):**
          * **`if err == nil` (Успех):**
              * Удали `oplog:<id>` из Badger.
          * **`if err == rueidiscompat.TxFailedErr` (Конфликт\!):**
              * **"Жертвуем" записью.**
              * Log `ERROR`: "Конфликт синхронизации, откатываем локальные изменения".
              * Запусти **компенсирующую транзакцию** в `m.localCache` (Badger), чтобы *удалить* данные, которые мы создали (`A:ino`, `D:parent/testfile`).
              * Удали `oplog:<id>` из Badger.
          * **`if err == network_error` (Сеть упала):**
              * **Не** удаляй `oplog`.
              * Log `WARN`: "Сеть недоступна, повтор позже".
              * Переведи ФС в `ReadOnly` (см. Шаг 14).
              * `requeue` (повторная попытка через N секунд).

  * **Шаг 12: Тестирование Фазы 4**

      * `?fast-meta=1`: `touch file1`. Убедись, что `oplog` удален из Badger, а `file1` появился в Redis.
      * Протестируй "жертву" (Шаг 19 из прошлого ответа).

-----

### 🛡️ Фаза 5: Отказоустойчивость (Блокировки и "Отпуск")

Цель: Сделать систему надежной.

  * **Шаг 13: Обработка Блокировок (`flock`/`plock`)**

      * Найди реализации `Getlk`, `Setlk`, `Setlkw` (они в `pkg/meta/rueidis_lock.go`).
      * Убедись, что они **не** используют `m.localCache`.
      * Они *всегда* должны вызывать `m.redisMeta.Getlk/Setlk`, чтобы идти напрямую в Redis.

  * **Шаг 14: Read-Only при разрыве связи**

      * В `syncOplog` (Шаг 11), если `err == network_error`:
        1.  Немедленно вызывай `m.conf.SetReadOnly(true)` (это в `baseMeta`).
        2.  Все новые `create/rm/mv` от FUSE теперь будут получать `EROFS`.
      * `syncWorker` должен продолжать попытки `syncOplog` в фоне.
      * Когда `syncOplog` *наконец-то* пройдет (сеть восстановлена):
        1.  Запусти **полную инвалидацию кэша чтения** (Шаг 15), т.к. мы пропустили `CLIENT TRACKING`.
        2.  После *запуска* инвалидации, `m.conf.SetReadOnly(false)`.

  * **Шаг 15: Bootstrap (Запуск `mount` после "отпуска")**

      * В `newRueidisMeta`, если `m.fastMetaEnabled == true`:
      * Сразу после `go m.runSyncWorker()`, запусти `go m.bootstrapFastMeta()`.
      * **Реализация `bootstrapFastMeta()`:**
        1.  `m.conf.SetReadOnly(true)`.
        2.  **Задача 1: Восстановление Oplog.**
              * Просканируй `m.localCache` (Badger) по префиксу `oplog:*`.
              * Для каждого `oplog`'а: `m.oplogQueue <- oplog`.
        3.  **Задача 2: Полная Инвалидация Кэша Чтения.**
              * Просканируй `m.localCache` по *всем* ключам (кроме `oplog:*`).
              * Удали их *всех* (например, `tx.deleteKeys(prefix)` из `tkv.go`).
        4.  **Задача 3: Ожидание синхронизации.**
              * Нам нужно дождаться, пока `m.oplogQueue` опустеет (все старые записи синхронизированы).
              * *Только после этого* `m.conf.SetReadOnly(false)`.

  * **Шаг 16: Тестирование Фазы 5**

      * Проверь `flock`.
      * `?fast-meta=1`: `touch file1`, `kill -9 juicefs`. Перезапусти `mount`. Убедись, что `file1` синхронизировался.
      * `?fast-meta=1` (Клиент А) + Стандартный клиент (Клиент Б).
      * А: `ls /` (кэширует).
      * Б: `mkdir /newdir_while_A_was_offline`.
      * Перезапусти Клиента А. `ls /` должен показать `newdir_while_A_was_offline` (т.е. кэш был сброшен).