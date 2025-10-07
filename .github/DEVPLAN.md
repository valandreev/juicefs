## DEVPLAN: TDD-план реализации движка метаданных `redis-split`

Ссылка: `.github/PRD.md` (использовать как спецификацию). Цель — реализовать MVP (FR1–FR4)
по методологии TDD: писать тест → убедиться, что тест падает → минимально реализовать код → повторить.

Общие правила для каждого шага:
- Каждая логическая задача должна иметь 1–3 теста (юнит/интеграция) с понятными именами.
- Тесты размещать рядом с кодом: `pkg/meta/xxxx_test.go`.
- Быстрые unit-тесты должны использоваться для логики парсинга, маршрутизации, принятия решений.
- Более дорогие сценарии (запуск Redis, интеграционные проверки) — отдельные integration tests, выполняемые вручную/CI.
- Команды для проверки локально:
  - Быстрый запуск конкретного теста: `go test ./pkg/meta -run TestName -v`
  - Запуск пакета meta: `make test.meta.core` (см. `Makefile`)

Структура файлов и короткие пояснения (куда добавлять код):
- Основная реализация: `pkg/meta/redis_split.go`
  - Регистрация движка: `Register("redis-split", newRedisSplitMeta)`
  - Функции: парсинг URL, создание двух клиентов (master, replica), маршрутизатор запросов.
- Unit-тесты маршрутизации и парсинга: `pkg/meta/redis_split_test.go`.
- Тесты интеграции: `pkg/meta/redis_split_integration_test.go` (используют реальный Redis, запускаются в CI/локально).

Пошаговый TDD-план (маленькие итерации):

1) Подготовка: парсинг URL *(✅ выполнено  – `TestParseRedisSplitURL`, `parseRedisSplitURL`)*
   - Тесты:
     - `TestParseRedisSplitURL` в `pkg/meta/redis_split_test.go`.
       - Ввод: `redis-split://redis://10.0.0.1:6379/1?replica=redis://10.0.0.2:16379/1`
       - Ожидаемое: правильно распарсенные masterURI и replicaURI, ошибки при отсутствии replica.
   - Ожидаемо-падающий тест: на старте нет функции `ParseRedisSplitURL`.
   - Реализация: добавить функцию `parseRedisSplitURL(uri string) (master string, replica string, err error)` и простые unit-тесты.
   - Acceptance: тесты проходят.

2) Скелет движка: регистрация и конструктор *(✅ выполнено – `TestRegisterRedisSplit`, `newRedisSplitMeta`, `redisSplitMeta`)*
   - Тесты:
     - `TestRegisterRedisSplit` (в `pkg/meta/redis_split_test.go`): вызов `newRedisSplitMeta("redis-split", <uri>, conf)` возвращает Meta без паники/с ошибкой при недопустимом URI.
   - Реализация: в `pkg/meta/redis_split.go` добавить `newRedisSplitMeta(driver, addr string, conf *Config) (Meta, error)` минимального возврата `&redisSplitMeta{}` и `Register("redis-split", newRedisSplitMeta)` в `init()`.
   - Acceptance: тест проходит (можно возвращать заглушку, пока не подключены real clients).

3) Подключение клиентов (мастер + реплика) — мокируемая обвязка *(✅ выполнено – `TestNewRedisSplitClients`, `TestNewRedisSplitClients_InvalidReplica`, фабрики `redisSplitMasterFactory`/`redisSplitReplicaFactory`)*
   - Тесты:
     - `TestNewRedisSplitClients` — проверяет, что при корректном URI создаются два redis.UniversalClient или их обёртки.
     - `TestNewRedisSplitClients_InvalidURI` — некорректный URI возвращает ошибку.
   - Реализация: использовать `redis.ParseURL` и `redis.NewClient` или `redis.NewFailoverClient` в конструкторе; сохранить в структуре поля `master redis.UniversalClient`, `replica redis.UniversalClient`.
   - Замечание: для тестов юнитов можно инжектировать интерфейс `type redisClient interface{ Do(...) }` и реализовать фейковый клиент.

4) Выделить и протестировать логику маршрутизации команд *(✅ выполнено – `chooseClientForOp`, `TestChooseClient_ReadOpsToReplica`, `TestChooseClient_WriteOpsToMaster`, `TestChooseClient_LockOpsToMaster`)*
   - Цель: разделить логику на читаемую функцию `chooseClientForOp(op string, ctx Context) (redis.UniversalClient, routeReason string)`.
   - Тесты (юниты):
     - `TestChooseClient_ReadOpsToReplica` — операции `doGetAttr`, `doReaddir`, `doReadlink` -> replica.
     - `TestChooseClient_WriteOpsToMaster` — операции `doMknod`, `doUnlink`, `txn()`-wrapped ops -> master.
     - `TestChooseClient_LockOpsToMaster` — lock-related ops -> master.
   - Реализация: добавить helper `isWriteOp(op)` or explicit switch; покрыть тестами edge-cases.

5) Тест для txn() — гарантия что транзакции идут на мастер *(✅ выполнено – `TestTxnUsesMaster`, перехват `trackingClient`)*
   - Тест: `TestTxnUsesMaster`.
     - Контекст: создать fake `master` и `replica` клиенты, которые инкрементируют счетчик вызовов; вызвать `m.txn(ctx, func(tx *redis.Tx) error { return nil }, keys...)` через новый движок.
     - Ожидаемое: только master получил транзакцию.
   - Реализация: модифицировать/скопировать существующую `txn()` логику из `pkg/meta/redis.go` в `redis_split.go`, но вызов `m.master` вместо `m.rdb`.

6) Покрыть unit-тестами реальные маршруты в минимальных методах *(✅ выполнено – `TestDoGetAttr_RoutesToReplica`, `TestDoMknod_RoutesToMaster`, переопределение `doGetAttr` для чтений)*
   - Тесты:
     - `TestDoGetAttr_RoutesToReplica` — вызвать public/internal `doGetAttr` (или вынести маршрутизацию в testable helper) и проверить, что read-клиент использован.
     - `TestDoMknod_RoutesToMaster` — для создания inode — используется master.
   - Реализация: при необходимости вынести общие куски кода (например, `handleLuaResult`) в библиотечные хелперы чтобы переиспользовать с минимальными дублированиями.

7) Интеграционный тест парой реальных Redis (локально / CI) *(✅ выполнено – `TestRedisSplitIntegration_BasicRouting`)*
    - Реализация: `pkg/meta/redis_split_integration_test.go` использует реальные master/replica клиенты, подключает go-redis hooks и проверяет, что write-путь (`Mkdir`) уходит на master, а `GetAttr` читается с replica.
    - Эндпоинты по умолчанию: `redis://100.123.245.11:6379/1` (master) и `redis://100.93.213.27:16379/1` (replica); их можно переопределить через переменные окружения `JFS_REDIS_SPLIT_MASTER` / `JFS_REDIS_SPLIT_REPLICA`.
    - Запуск: тест автоматически пропускается, если подключение к указанным Redis недоступно. Для ручного прогона:
       ```pwsh
       go test ./pkg/meta -run TestRedisSplitIntegration -v
       ```
       При необходимости установите `JFS_REDIS_SPLIT_MASTER` и `JFS_REDIS_SPLIT_REPLICA` перед запуском.
    - Acceptance: успешное завершение теста подтверждает базовый сценарий FR1–FR4 с реальным master / replica.

8) Фейлы и откат: тесты перехода на мастер при недоступной реплике *(✅ выполнено – `TestReplicaUnavailable_FallbackToMaster`, фоновый health-check реплики)*
    - Реализация: `redis_split.go` отслеживает состояние реплики, логирует первый сбой и переводит чтения на master до восстановления. Реализована периодическая попытка `PING` (по умолчанию каждые 5s) для автоматического возврата на replica.
    - Юнит-тест `TestReplicaUnavailable_FallbackToMaster` (в `pkg/meta/redis_split_test.go`) эмулирует ошибку `GET` на реплике, проверяет fallback на master и последующее восстановление после успешного `PING`.
    - Для ручного прогона (при необходимости настройки интервала):
       ```pwsh
       go test ./pkg/meta -run TestReplicaUnavailable_FallbackToMaster -v
       ```
       *(на Windows сборка пакета `pkg/meta` по-прежнему требует POSIX-констант из `syscall`; запускайте на Linux/macOS либо используйте флаги для исключения соответствующих тестов).* 

9) Документация и конфигурация *(✅ выполнено – обновлена глава "How to Set Up Metadata Engine" с разделом про `redis-split`)*
   - В `docs/en/reference/how_to_set_up_metadata_engine.md` добавлен подраздел «Redis split (master + replica)» с форматами URL, примерами `juicefs format`/`juicefs mount`, рекомендациями по кешам (`--attr-cache`, `--entry-cache`, `--open-cache`) и описанием автоматического fallback-а на master.
   - Обновлённые инструкции покрывают требования PRD (консистентность, использование переменных окружения и подготовку репликации Redis).

10) CI и Makefile
   - Добавить target `make test.meta.redis-split` (опционально) или расширить `test.meta.core` для покрытия новых unit-тестов.
   - Обновить `.github/workflows/unittests.yml` только если нужны новые сервисы; предпочтительнее запускать интеграционные тесты отдельно (долгие) в `integrationtests.yml`.

11) Post-MVP (TDD циклы для FR5–FR7)
   - Sticky session (read-your-writes): тесты для временной «липкой» маршрутизации после записи (напр., `TestStickySession_ReadAfterWrite`).
   - Механизм выбора среди множества реплик: `TestSelectLowestLatencyReplica`.

Edge-cases и тестовые сценарии (включить в 각 шаг):
- Неполный/некорректный URI; отсутствие replica.
- Поведение при ошибках сети и таймаутах.
- Латентность реплики (устаревшие данные) — документировать ожидаемый компромисс.

Критерии окончания MVP (Done):
- `pkg/meta/redis_split.go` создан и зарегистрирован как `redis-split`.
- Unit-тесты: парсинг URL, маршрутизация операций, txn-forwarding — все проходят.
- Интеграционный сценарий с двумя Redis проходит локально/в CI (или ручной инструкцией для локального запуска).
- Документация обновлена, примеры конфигурации добавлены.

Полезные команды
- Запуск конкретного теста:
```pwsh
go test ./pkg/meta -run TestParseRedisSplitURL -v
```
- Полный пакет meta (быстро):
```pwsh
make test.meta.core
```

Если хотите — могу создать начальный каркас файлов (`pkg/meta/redis_split.go` и `pkg/meta/redis_split_test.go`) и пройти первые 3 шага (парсинг URL, регистрация, создание клиентов) в репозитории. Напишите, нужно ли автоматически добавить эти каркасы сейчас.
