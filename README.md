# Приём платежей в USDT — сквозной сценарий

Реализация части 2 тестового задания. Дизайн всей системы в [DESIGN.md](DESIGN.md).

> партнёр создаёт счёт → на адрес приходит перевод USDT → ждём N подтверждений → AML-проверка → зачисление на баланс партнёра за вычетом комиссии **или** создание возврата

## Запуск

```bash
docker compose up --build
```

Поднимаются PostgreSQL и сервис на `http://localhost:8080`. Миграции применяются при старте. Включены симулятор цепи и ручки управления AML-моком (`SIM_ENABLED=true`).

Партнёры из `docker-compose.yml`:

| Партнёр | API-ключ | Комиссия |
|---|---|---|
| `acme` | `acme-secret-key-0001` | 1 % |
| `globex` | `globex-secret-key-0002` | 2,5 % |

AML-мок: адрес `0xbad0000000000000000000000000000000000bad` — высокий риск, `0xe000000000000000000000000000000000000e00` — горячий кошелёк биржи, всё остальное — низкий риск. Подтверждений нужно `N = 12`.

Тесты (нужен запущенный Docker, Postgres поднимается через testcontainers):

```bash
go test -race ./...
# или против своего сервера:
TEST_DATABASE_URL=postgres://user:pass@localhost:5432/postgres?sslmode=disable go test ./...
```

То же через `make` (список целей — `make help`):

```bash
make up         # docker compose up -d --build
make demo       # счёт → перевод → 12 подтверждений → зачисление, печатает счёт и баланс
make logs       # логи сервиса
make test-race  # тесты с race-детектором
make lint       # gofmt + go vet
make reset      # остановить и удалить данные
```

## Сценарий руками

```bash
B=http://localhost:8080
K="Authorization: Bearer acme-secret-key-0001"

# 1. Создать счёт. Повтор с тем же Idempotency-Key вернёт тот же счёт (200, Idempotent-Replayed: true),
#    тот же ключ с другим телом — 422.
curl -s -X POST $B/v1/invoices -H "$K" -H 'Idempotency-Key: order-42' \
  -d '{"amount":"100.00","external_id":"order-42"}'
# → {"id":"<ID>","deposit_address":"<ADDR>","status":"NEW",...}

ID=<ID>; ADDR=<ADDR>

# 2. Клиент платит: симулятор майнит блок с переводом USDT
curl -s -X POST $B/sim/transfers \
  -d "{\"from\":\"0x1111111111111111111111111111111111111111\",\"to\":\"$ADDR\",\"amount\":\"100\"}"

curl -s $B/v1/invoices/$ID -H "$K"     # status CONFIRMING, transfer DETECTED
curl -s $B/v1/balance -H "$K"          # available 0, on_hold 0

# 3. Добираем подтверждения (блок с переводом — первое)
curl -s -X POST $B/sim/mine -d '{"blocks":11}'
sleep 3

curl -s $B/v1/invoices/$ID -H "$K"     # status PAID, transfer CREDITED, fee 1.000000
curl -s $B/v1/balance -H "$K"          # available 99.000000

# Другой партнёр этот счёт не видит
curl -s $B/v1/invoices/$ID -H "Authorization: Bearer globex-secret-key-0002"   # 404
```

**Грязный платёж и недоступный AML:**

```bash
curl -s -X POST $B/sim/aml -d '{"available":false}'      # AML «упал»

curl -s -X POST $B/v1/invoices -H "$K" -H 'Idempotency-Key: order-43' -d '{"amount":"50"}'
curl -s -X POST $B/sim/transfers \
  -d '{"from":"0xbad0000000000000000000000000000000000bad","to":"<ADDR>","amount":"50"}'
curl -s -X POST $B/sim/mine -d '{"blocks":12}'

curl -s $B/v1/invoices/<ID> -H "$K"   # AML_REVIEW: ждёт AML, деньги в on_hold, не в available
curl -s -X POST $B/sim/aml -d '{"available":true}'
sleep 6
curl -s $B/v1/invoices/<ID> -H "$K"   # REFUNDING, refunds[0].status = READY_TO_SEND
```

**Реорг до набора подтверждений:**

```bash
# после перевода и пары блоков:
curl -s -X POST $B/sim/reorg -d '{"depth":3}'                  # перевод исчезает из цепи
curl -s -X POST $B/sim/reorg -d '{"depth":3,"reinclude":true}' # тот же tx попадает в другой блок
curl -s $B/v1/invoices/<ID> -H "$K"   # старая запись ORPHANED, счёт вернулся в NEW / CONFIRMING
```

Остальные ручки симулятора: `GET /sim/head`; у `POST /sim/transfers` есть поля `"token": "0x…"` (поддельный токен) и `"failed": true` (откатившаяся транзакция). Оба случая сервис игнорирует.

## API

| Метод | Путь | |
|---|---|---|
| `POST` | `/v1/invoices` | Заголовок `Idempotency-Key` обязателен. Тело `{"amount": "100.50", "external_id": "..."}` — сумма только строкой |
| `GET` | `/v1/invoices/{id}` | Счёт с переводами и возвратами. Чужой счёт → 404 |
| `GET` | `/v1/balance` | `available` и `on_hold` (подтверждено, ждёт AML) |

Аутентификация: `Authorization: Bearer <api key>`. В БД хранится только sha256 ключа.

## Устройство

```
cmd/server            main: конфиг, пул, graceful shutdown
internal/app          сборка компонентов; e2e-тесты сценария
internal/api          HTTP: партнёрский API и ручки симулятора
internal/invoice      создание счёта (идемпотентно), статус счёта выводится из переводов
internal/watcher      сканер блоков: подтверждения, реорги, фильтр токена и статуса tx
internal/processor    AML → зачисление за вычетом комиссии или возврат
internal/ledger       двойная запись
internal/chain        интерфейс к Ethereum; chain/sim — симулятор в Postgres
internal/aml          интерфейс AML и мок
internal/money        сумма USDT: int64 в базовых единицах, без float
```

Один бинарник, внутри HTTP и два воркера (watcher и processor). Всё состояние в PostgreSQL, поэтому можно запустить несколько реплик: тест `TestNoDoubleCredit_ParallelWorkersAndRestart` гоняет четыре экземпляра параллельно на одной БД.

## Как обеспечены требования

| Требование | Как сделано | Тест |
|---|---|---|
| Перевод не зачисляется дважды | Три независимых слоя: (1) processor берёт перевод через `FOR UPDATE SKIP LOCKED` с арендой и перед зачислением перепроверяет `status = CONFIRMED` под блокировкой строки; (2) у проводки ключ идемпотентности `transfer:<id>:settle`, общий для зачисления и возврата; (3) частичный уникальный индекс `(tx_hash, log_index) WHERE status <> 'ORPHANED'` — живая копия лога одна | `TestNoDoubleCredit_ParallelWorkersAndRestart`, `TestHighRisk_CreatesExactlyOneRefund` |
| Ни копейки до ответа AML | Подтверждённый перевод идёт на `liab:partner:<id>:hold`, партнёру не доступен. Ошибка AML или неизвестный ответ → повтор с экспоненциальной задержкой, «fail closed» | `TestAMLUnavailable_MoneyWaitsAndRetries`, `TestAMLRetryBackoff_NotRetriedEarly` |
| Реорг не оставляет висящего зачисления | До N подтверждений проводок нет вообще. Watcher хранит хэши блоков, сверяет `parent_hash`, при расхождении откатывается до общего предка и помечает переводы `ORPHANED`. Подтверждения считаются от проверенного нами tip, а не от head ноды. Реорг глубже N → watcher останавливается (`ErrDeepReorg`) | `TestReorgBeforeConfirmations_LeavesNoCredit`, `TestReorgReincludedTransfer_CreditedOnce`, `TestDeepReorg_StopsWatcher` |
| Не float, 6 знаков | `money.Amount` — int64 базовых единиц, `BIGINT` в БД. JSON только строкой, JSON-число отклоняется. Комиссия округляется вниз | `internal/money`, `TestInvoiceValidation`, `TestFeeIsPerPartnerAndRoundsDown` |
| Идемпотентное создание счёта | `UNIQUE (partner_id, idempotency_key)` + `ON CONFLICT DO NOTHING`; хэш тела хранится, повтор ключа с другим телом → 422 | `TestInvoiceCreationIsIdempotent` (в т. ч. 20 параллельных запросов) |
| Изоляция партнёров | Каждый запрос фильтрует по `partner_id` из API-ключа, чужой счёт → 404 | `TestPartnerIsolation` |
| Ledger сходится | Проводка из ≥ 2 строк с суммой 0 проверяется в Go и отложенным constraint trigger в БД; таблицы ledger append-only (триггер). Тесты после каждого сценария проверяют: активы = обязательства + доход | `internal/ledger`, `assertLedger` во всех e2e |

Поддельный USDT отсекается по адресу контракта (дважды: фильтр запроса и проверка в коде), перевод из откатившейся транзакции — по статусу receipt (`TestIgnoresFakeTokenAndFailedTransaction`).

### Проводки

| Событие | Дебет | Кредит |
|---|---|---|
| N подтверждений | `asset:deposits` | `liab:partner:P:hold` |
| AML ок | `liab:partner:P:hold` 100 | `liab:partner:P:available` 99, `rev:fees` 1 |
| AML отклонил | `liab:partner:P:hold` | `liab:refunds_payable` |

## Допущения и упрощения

- **Сумма — `BIGINT` базовых единиц, а не `NUMERIC(38,6)`, как написано в DESIGN.md.** Весь выпуск USDT (~1,5·10¹⁷ единиц) намного меньше `int64`, а целые исключают ошибки округления при сканировании и в Go. Значение, не влезающее в `int64`, watcher не пропускает молча, а останавливается с ошибкой.
- **Счета ledger агрегированы**: холд — один счёт на партнёра, обязательства по возвратам — один общий счёт, детали в сущностях `transfers` и `refunds`. В DESIGN.md счета на каждый счёт и возврат; на корректность это не влияет, а аккаунтов меньше.
- Статус счёта не хранится как машина состояний, а выводится из переводов (`invoice.RecomputeStatus`). Поэтому реорг не требует обратных переходов: осиротевший перевод просто выпадает из агрегата.
- Один перевод = одна AML-проверка и одно решение. Несколько переводов на один счёт суммируются: `PARTIALLY_PAID` → `PAID`. Переплата зачисляется целиком.
- **Не сделано (описано в DESIGN.md):** срок жизни счёта (`EXPIRED`), допуск недоплаты и возврат по истечении срока доплаты, вебхуки, отправка возврата и удержание газа (возврат доводится до `READY_TO_SEND` на полную сумму, газ удерживается при отправке), реальная генерация адресов (детерминированный псевдоадрес вместо BIP-32 от xpub), RPC-клиент к Ethereum, админка.
- Адрес возврата для биржевого кошелька не запрашивается: возврат остаётся в `NEEDS_ADDRESS` без адреса.
- Комиссия фиксируется в счёте при создании (`invoices.fee_bps`): смена тарифа не меняет условия уже выставленных счетов.
- Таблица `scanned_blocks` не чистится; в проде храним последние `MaxReorgDepth` блоков.
- Состояние AML-мока (доступен / нет) живёт в памяти процесса и после рестарта сбрасывается в «доступен».
- Симулятор хранит цепь в Postgres (`sim_*`), чтобы она переживала рестарт сервиса, как настоящая.

## Как я использовал AI

<!-- TODO: дополнить своими словами -->

Claude Code (Opus) писал код по готовому DESIGN.md, я ревьюил и направлял. Что пришлось ловить:

- Создание счёта ledger через `INSERT … ON CONFLICT DO UPDATE` держит блокировку строки до коммита, и все параллельные проводки одного партнёра выстраиваются в очередь. Заменили на `SELECT`, затем `INSERT … DO NOTHING`.
- Первая версия watcher запрашивала receipt для каждого USDT-перевода в блоке, хотя нам нужны только переводы на наши адреса. На mainnet это сотни лишних RPC-вызовов на блок.
- В тесте с параллельными репликами копия структуры харнесса при «рестарте» закрывала HTTP-сервер оригинала.
- Чтобы проверить, что тесты не проходят вхолостую, вносили мутации: реорг без пометки `ORPHANED`, ошибка AML как низкий риск, подтверждение без учёта N. Каждая роняет хотя бы два теста.
