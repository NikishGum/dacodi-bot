# Telegram Support Bot

Бот поддержки для инфраструктурного провайдера (CDN, SecurityCDN, AI-защита,
Dedicated/VPS, Protected DNS, API, Terraform). Задача бота — **сократить время
между сообщением клиента и реакцией оператора**: он собирает структурированную
информацию и заводит тикет в Zoho Desk. Бот ничего не решает сам.

Интерфейс с клиентом — на русском (поддержка региона СНГ).

## Что делает бот

1. Клиент пишет в свой выделенный чат → бот здоровается и показывает кнопки
   доступных сервисов.
2. По выбранному сервису задаёт 2–6 точечных вопросов (часть — кнопками).
3. Показывает сводку → клиент подтверждает.
4. Бот создаёт тикет в Zoho Desk, **сообщает его номер** и **замолкает**: дальше
   только пересылает ответы оператора и пишет реплики клиента комментариями в
   тикет.
5. **Один активный тикет на клиента:** пока обращение открыто, новый `/start`
   не запускает сбор данных — бот напоминает номер активного тикета. После
   закрытия тикета (событие из Zoho) состояние сбрасывается, и клиент может
   создать новое обращение тем же flow.

### Ключевые особенности

- **Минимум присутствия бота** — только сбор данных, без лишних фраз.
- **Полная изоляция клиентов** — каждый в своём чате, друг о друге не знают.
- **Полный контекст оператору** — структурированное описание без повторных
  вопросов; приоритет HIGH для полного простоя / нефильтруемого DDoS / полного
  отказа CDN, иначе NORMAL.
- **Двусторонняя связь** — ответ оператора из Zoho приходит клиенту в Telegram
  (вебхук, доставка < 3 c); сообщения клиента уходят комментариями в тикет.
- **Один активный тикет** — новый создаётся только после закрытия текущего;
  клиенту показывается номер обращения.
- **Устойчивость к сбоям Zoho** — если создать тикет не удалось, запрос кладётся
  в очередь Redis и повторяется каждые 30 c; реплики клиента буферизуются.
- **Аутентификация по одноразовой ссылке** — см. ниже.

## Аутентификация и авторизация

Источник истины о клиентах — **аккаунты Zoho Desk**. Каждый key-клиент — это
Account с custom-полями: `cf_bot_client_id` (наш короткий стабильный ID),
`cf_bot_services` (доступные сервисы), `cf_telegram_chat_id` (привязанный чат).

- **Привязка чата (разово):** оператор через сервис `tokengen` генерирует
  одноразовую ссылку `t.me/<bot>?start=<token>` для конкретного `bot_client_id`
  и отправляет её клиенту. Клиент жмёт Start → бот проверяет HMAC-токен (срок,
  одноразовость) → записывает `chat_id` в аккаунт Zoho и в кэш Redis.
- **Авторизация (на каждое сообщение):** `chat_id` ищется в Redis
  (`auth:{chat_id}`); при промахе — в Zoho по `cf_telegram_chat_id`. Полный
  скан аккаунтов при «холодном» промахе ограничен по частоте
  (`ZOHO_SCAN_MIN_INTERVAL`, по умолчанию 60s), чтобы незнакомцы не выжигали
  кредиты Zoho API.
- **Непривязанный чат не игнорируется молча:** бот здоровается и просит ключ
  доступа. Любой следующий текст трактуется как токен и проверяется. Попытки
  ограничены по частоте на чат; при недоступности Redis ограничитель
  **fail-closed** — бот отвечает «Временная ошибка», привязка не происходит.
- `config/clients.json` — опциональный bootstrap-allowlist по `chat_id` для
  тестовых/служебных чатов (без токена). В репозитории — только плейсхолдеры;
  реальные `chat_id` держите в неотслеживаемом `config/clients.local.json` и
  укажите его через `CLIENTS_FILE`.

Токен: `bot_client_id | exp | jti | HMAC-SHA256[:16]`, base64url (~43 символа,
влезает в лимит Telegram). Секрет `BINDING_TOKEN_SECRET` общий у бота и tokengen.

## Архитектура

```
Telegram ─▶ Nginx ─▶ /telegram/webhook ─┐
                                         ├─▶ Бот ─▶ Zoho Desk (тикеты/аккаунты)
Zoho Desk ─▶ Nginx ─▶ /zoho/webhook ─────┘     │
                                               ▼
                                  Redis: FSM, привязки, кэш токена, очередь
```

Хранилище Redis (AOF): `fsm:{chat}` (диалог, 24ч), `ticket:{chat}` (↔ тикет,
30д), `auth:{chat}` (привязка), `client:{id}` (кэш профиля), `zoho:access_token`
(3500с), `queue:tickets` (очередь повторов).

## Структура

```
main.go            точка входа, HTTP-сервер, graceful shutdown
config/            загрузка env + bootstrap clients.json
auth/              формат и проверка токенов привязки (HMAC)
registry/          резолв "чей это чат": Redis-кэш → Zoho → bootstrap
zoho/              OAuth2, HTTP-клиент с ретраями, методы Desk (тикеты/аккаунты)
bot/               FSM диалога, хендлеры, клавиатуры, привязка по /start
webhook/           приём вебхуков Zoho → пересылка в Telegram
queue/             фоновый воркер повторного создания тикетов
redis/             типизированные операции Redis
cmd/tokengen/      небольшой HTTP-сервис выпуска токенов привязки
nginx/ systemd/    готовые конфиги для прод-развёртывания
```

## Технологии

Go 1.22+ · `go-telegram-bot-api/v5` · `redis/go-redis/v9` · `net/http` ·
`log/slog` · `godotenv`.

## Быстрый старт (локально, polling)

```bash
# Redis с AOF
docker run -d --name dacodi-redis -p 6379:6379 redis:7 \
  redis-server --appendonly yes --appendfsync everysec

cp .env.example .env   # заполнить TELEGRAM_BOT_TOKEN; для теста диалога Zoho-поля = dummy
# TELEGRAM_MODE=polling, BINDING_TOKEN_SECRET=любая строка

go run .
```

Без реального Zoho привязка по токену недоступна — для локального теста добавьте
свой `chat_id` в `config/clients.json` (bootstrap), тогда бот ответит на `/start`.

Проверка здоровья: `curl 127.0.0.1:8080/healthz` → `ok`.

## Выпуск токена привязки (tokengen)

```bash
BOT_USERNAME=YourSupportBot TOKENGEN_API_KEY=... BINDING_TOKEN_SECRET=... \
  go run ./cmd/tokengen
# затем:
curl -X POST 127.0.0.1:8090/token -H "X-API-Key: ..." \
  -H "Content-Type: application/json" -d '{"bot_client_id":1001}'
# -> { "link": "https://t.me/YourSupportBot?start=...", "expires_at": "..." }
```

`BINDING_TOKEN_SECRET` обязан совпадать у бота и tokengen. Эндпоинт умеет
выпускать пропуск для любого клиента — держите его только во внутренней сети.

## Прод-развёртывание на VPS

Деплой — это статически слинкованный бинарь под `systemd`, за `nginx` (TLS +
IP-allowlist), с локальным Redis (AOF). Никаких контейнеров в проде не требуется.
Цели для сборки/заливки/эксплуатации см. в `Makefile` (`make help`).

### Требования

**Локальная машина (откуда деплоите):**
- Go 1.22+ (кросс-компиляция в `linux/amd64`, `CGO_ENABLED=0`).
- `make`, `ssh`/`scp`, доступ по SSH-ключу к VPS (root или sudo).

**VPS (Ubuntu/Debian-подобный, x86_64):**
- `systemd`, `redis-server` ≥ 7.
- **FastPanel** — управляет nginx и TLS. Nginx-конфиг руками **не правим**: панель
  перезаписывает его. Реверс-прокси и сертификат настраиваются через UI FastPanel
  (см. чеклист). Файл `nginx/supportbot.conf` в репозитории — **референс** того,
  какие защиты надо воспроизвести, а не файл для копирования.
- **BitNinja** — серверный WAF/анти-DoS. По умолчанию может челленджить (captcha)
  или блокировать POST-вебхуки Telegram и Zoho как подозрительный бот-трафик,
  поэтому их источники нужно **внести в whitelist** (см. чеклист).
- Публичное DNS-имя (например `bot.example.com`), указывающее на VPS.
- Открытые порты: **443/tcp** и **80/tcp** (FastPanel/Let's Encrypt). Сам бот
  слушает только `127.0.0.1:8080` — наружу не публикуется.
- Исходящий HTTPS к `api.telegram.org`, `accounts.zoho.eu`, `desk.zoho.eu`
  (бот сам ходит за токеном, тикетами и **скачивает вложения из Telegram**).

**Внешние доступы (подготовить заранее):**
- Telegram-бот от @BotFather → `TELEGRAM_BOT_TOKEN`.
- Zoho Desk: server-based OAuth-приложение со scope
  `Desk.tickets.CREATE,Desk.tickets.READ,Desk.tickets.UPDATE,Desk.basic.READ`,
  оттуда `ZOHO_CLIENT_ID`/`ZOHO_CLIENT_SECRET`/`ZOHO_REFRESH_TOKEN`; `ZOHO_ORG_ID`,
  `ZOHO_DEPT_ID`. Custom-поля на аккаунте: `cf_bot_client_id`, `cf_bot_services`,
  `cf_bot_priority`, `cf_telegram_chat_id` (последнее — и на тикете тоже).
- Сгенерированные секреты: `TELEGRAM_WEBHOOK_SECRET`, `ZOHO_WEBHOOK_TOKEN`,
  `BINDING_TOKEN_SECRET`, `TOKENGEN_API_KEY` (`openssl rand -hex 32`).

### Первичный деплой — чеклист

1. **DNS и пользователь.** `A`-запись `bot.example.com → <IP>`. На VPS:
   ```bash
   sudo useradd --system --no-create-home --shell /usr/sbin/nologin supportbot
   sudo mkdir -p /opt/bot/config && sudo chown -R supportbot:supportbot /opt/bot
   ```
2. **Redis с AOF и паролем.** Установите `redis-server`, затем поставьте unit из
   репозитория (он включает `--appendonly yes` и требует пароль, FIX-16):
   ```bash
   # Пароль держим вне world-readable unit-файла:
   echo "requirepass $(openssl rand -hex 24)" | sudo tee /etc/redis/requirepass.conf
   sudo chmod 600 /etc/redis/requirepass.conf && sudo chown redis:redis /etc/redis/requirepass.conf
   # Каталог AOF — только для redis:
   sudo chmod 700 /var/lib/redis && sudo chown redis:redis /var/lib/redis
   sudo cp systemd/redis.service /etc/systemd/system/redis.service
   sudo systemctl daemon-reload && sudo systemctl enable --now redis
   redis-cli ping                 # NOAUTH (ожидаемо — без пароля доступа нет)
   redis-cli -a "$(awk '{print $2}' /etc/redis/requirepass.conf)" ping   # PONG
   ```
   Тот же пароль пропишите как `REDIS_PASSWORD` в `/opt/bot/.env`. Если используете
   штатный пакетный `redis`, вместо этого включите в `/etc/redis/redis.conf`
   `appendonly yes`, `appendfsync everysec` и `requirepass <secret>`.
3. **FastPanel: сайт, TLS и реверс-прокси** (вместо ручного nginx).
   - Создайте сайт для `bot.example.com` (тип — статический/прокси, PHP не нужен).
   - Выпустите Let's Encrypt сертификат для домена (FastPanel → SSL), включите
     авто-продление и редирект HTTP→HTTPS.
   - Настройте сайт как **reverse proxy** на `http://127.0.0.1:8080`. Если в вашей
     версии FastPanel нет готового типа «proxy», добавьте в поле сайта
     **«Дополнительные директивы nginx»** (server/location-контекст — переживает
     регенерацию конфига) проксирование только двух путей:
     ```nginx
     location = /telegram/webhook {
         proxy_pass http://127.0.0.1:8080;
         proxy_set_header Host $host;
         proxy_set_header X-Real-IP $remote_addr;
         proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
         proxy_set_header X-Forwarded-Proto $scheme;
         proxy_read_timeout 35s;
         client_max_body_size 2m;
     }
     location = /zoho/webhook {
         proxy_pass http://127.0.0.1:8080;
         proxy_set_header Host $host;
         proxy_set_header X-Real-IP $remote_addr;
         proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
         proxy_set_header X-Forwarded-Proto $scheme;
         proxy_read_timeout 10s;
     }
     location / { return 404; }
     ```
   - IP-allowlist и rate-limit из `nginx/supportbot.conf` используют `http`-контекст
     (`geo`, `limit_req_zone`), который FastPanel обычно регенерирует, — поэтому в
     этой схеме они **делегируются BitNinja** (шаг 4), а на уровне приложения оба
     вебхука и так защищены секретными токенами (constant-time проверка).
4. **BitNinja: whitelist источников вебхуков.** Чтобы WAF/анти-DoS не челленджил и
   не блокировал входящие POST от Telegram и Zoho, добавьте их в whitelist
   (BitNinja UI/CLI, например `bitninjacli --whitelist --add --ip=<IP>`):
   - **Telegram:** `149.154.160.0/20`, `91.108.4.0/22` (стабильны).
   - **Zoho:** актуальные диапазоны вебхуков из документации Zoho для вашего ДЦ
     (см. `geo $zoho_allowed` в `nginx/supportbot.conf` как стартовый список —
     **обязательно сверьте**).
   DoS-защиту и IP-репутацию для этих эндпоинтов оставляем на BitNinja. После
   изменений проверьте, что тестовый вебхук проходит без captcha-страницы.
5. **`.env`.** На основе `.env.example` создайте `/opt/bot/.env`
   (`TELEGRAM_MODE=webhook`, `PUBLIC_BASE_URL=https://bot.example.com`, все
   обязательные переменные — см. раздел «Требования»). Закройте права:
   ```bash
   sudo chown supportbot:supportbot /opt/bot/.env && sudo chmod 600 /opt/bot/.env
   ```
   `BINDING_TOKEN_SECRET` должен быть **байт-в-байт одинаков** у бота и tokengen.
   tokengen теперь читает **отдельный** `EnvironmentFile=/opt/bot/tokengen.env`
   (см. `tokengen.env.example`) — только переменные токенов, без Zoho/Telegram
   секретов (FIX-9). Код подрезает хвостовые пробелы, но не полагайтесь на это —
   держите значения идентичными. Закройте права и на этот файл (`chmod 600`).
   Также задайте `REDIS_PASSWORD` (см. п. про Redis / `requirepass`, FIX-16).
6. **(Опционально) bootstrap-клиенты.** Реальные `chat_id` держите в
   неотслеживаемом `config/clients.local.json` и укажите `CLIENTS_FILE` в `.env`.
   Залить на VPS: `make deploy-config` (по умолчанию шлёт `config/clients.local.json`;
   переопределяется `CLIENTS_SRC=`). В репозитории `config/clients.json` —
   только плейсхолдеры, не перезаписывайте им прод.
7. **Бинари и unit-файлы.** С локальной машины (укажите цель деплоя через
   `make deploy VPS=user@host`; в `Makefile` дефолт — плейсхолдер
   `deploy@your-vps-host`, а уход от SSH под `root` — отдельная ops-задача):
   ```bash
   make check                 # vet + сборка локально
   make deploy-units          # supportbot.service + tokengen.service + daemon-reload
   make deploy                # build-linux -> scp бинарей -> restart -> status
   sudo systemctl enable supportbot tokengen   # автозапуск (один раз, на VPS)
   ```
   `make deploy` сам делает `build-linux`, атомарную замену (`.new` → `mv`),
   `chown`, рестарт и печатает статус.
8. **Регистрация вебхуков.**
   - Telegram: бот регистрирует webhook сам при старте в режиме `webhook`
     (`PUBLIC_BASE_URL` + `/telegram/webhook`, секрет `TELEGRAM_WEBHOOK_SECRET`).
     Если `setWebhook` отклонён — процесс **падает на старте** (видно в логах).
   - Zoho Desk → `POST https://bot.example.com/zoho/webhook`, токен передавайте
     **только заголовком** `X-Webhook-Token: <ZOHO_WEBHOOK_TOKEN>` — query-параметр
     `?token=` больше не принимается (попадает в логи nginx/FastPanel/BitNinja,
     FIX-6). **Два правила**:
     - **публичный** комментарий оператора →
       `{ "ticketId", "ticketNumber", "chatId" (из cf_telegram_chat_id), "content" }`;
     - закрытие тикета (смена статуса) →
       `{ "ticketId", "ticketNumber", "chatId", "status": "Closed" }`.
     > ⚠️ Правило на комментарий должно срабатывать **только на публичные**
     > комментарии. Сообщения клиента бот пишет в тикет приватными комментариями
     > именно для того, чтобы не зациклить эхо; если правило ловит все комментарии —
     > ответы будут возвращаться клиенту бесконечно.
9. **Проверка.**
   ```bash
   make health                       # -> ok (redis доступен)
   make logs                         # старт без ошибок, "telegram webhook configured"
   ```
   Сквозной тест: выпустите токен (`make run-tokengen` локально или сервис на VPS),
   привяжите тестовый чат, пройдите диалог, приложите скриншот, подтвердите →
   тикет в Zoho с вложением; ответьте публично из Zoho → ответ пришёл в Telegram;
   закройте тикет → клиент получил уведомление и снова доступен `/start`.

### Последующие деплои

Обычное обновление кода — одна команда с локальной машины:
```bash
make check     # vet + сборка, чтобы не выкатить нерабочее
make deploy    # build-linux -> залить бинари -> restart supportbot+tokengen -> status
```
Деплой атомарный (заливается `*.new`, затем `mv`), Redis/`.env`/nginx не трогаются,
очередь тикетов и FSM переживают рестарт (AOF). Точечные операции:

| Изменилось | Команда |
|------------|---------|
| Только код | `make deploy` |
| `config/clients.json` | `make deploy-config` (не трогает `.env`) |
| systemd unit-файлы | `make deploy-units` (делает `daemon-reload`) |
| `.env` | отредактировать `/opt/bot/.env` на VPS вручную → `make restart` |
| Прокси/TLS/домен | через UI FastPanel (nginx-конфиг руками не править) |
| Диапазоны вебхуков Zoho изменились | обновить whitelist в BitNinja |

**Эксплуатация:** `make logs` / `make logs-tokengen` (journalctl -f), `make health`,
`make restart`. **Откат:** держите предыдущий бинарь (`supportbot.prev`) или
пересоберите из нужного коммита и повторите `make deploy`.

## Добавление клиента

1. Создать Account в Zoho Desk, заполнить `cf_bot_client_id` (уникальный, напр.
   1001), `cf_bot_services` (метки: `CDN`, `SecurityCDN`,
   `AI Full-Stack Env Protection`, `Dedicated Servers`, `VPS`, `Protected DNS`,
   `API services`, `Terraform provider integration`), `cf_bot_priority`.
2. Через `tokengen` выпустить ссылку для этого `bot_client_id`.
3. Отправить ссылку клиенту по доверенному каналу. `chat_id` привяжется
   автоматически при первом нажатии Start — вручную его вводить не нужно.
