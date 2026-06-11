# ── Параметры (можно переопределять: make deploy VPS=user@host) ─────────────
BINARY      ?= supportbot
TOKENGEN    ?= tokengen
BUILD_DIR   ?= build

# Override on the command line: make deploy VPS=user@host (FIX-10). Moving off
# root SSH is a separate ops task — track it in the README checklist.
VPS         ?= deploy@your-vps-host
REMOTE_DIR  ?= /opt/bot
REMOTE_OWNER?= supportbot:supportbot

GO          ?= go
LDFLAGS     := -s -w
LINUX_ENV   := CGO_ENABLED=0 GOOS=linux GOARCH=amd64

.DEFAULT_GOAL := help

# ── Помощь ──────────────────────────────────────────────────────────────────
.PHONY: help
help: ## Показать список целей
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# ── Локальная разработка ────────────────────────────────────────────────────
.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

.PHONY: fmt
fmt: ## Форматирование (gofmt)
	$(GO) fmt ./...

.PHONY: vet
vet: ## Статический анализ (go vet)
	$(GO) vet ./...

.PHONY: build
build: ## Локальная сборка обоих бинарей в build/
	mkdir -p $(BUILD_DIR)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) .
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(TOKENGEN) ./cmd/tokengen

.PHONY: run
run: ## Запустить бота локально (go run .)
	$(GO) run .

.PHONY: run-tokengen
run-tokengen: ## Запустить tokengen локально
	$(GO) run ./cmd/tokengen

.PHONY: check
check: vet build ## vet + сборка (быстрая проверка перед деплоем)

.PHONY: clean
clean: ## Удалить build/
	rm -rf $(BUILD_DIR)

# ── Кросс-компиляция под VPS (linux/amd64) ──────────────────────────────────
.PHONY: build-linux
build-linux: ## Собрать статические бинари под linux/amd64
	mkdir -p $(BUILD_DIR)
	$(LINUX_ENV) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) .
	$(LINUX_ENV) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(TOKENGEN) ./cmd/tokengen

# ── Деплой на VPS ───────────────────────────────────────────────────────────
.PHONY: deploy
deploy: build-linux ## Собрать под Linux, залить бинари и перезапустить сервисы
	scp $(BUILD_DIR)/$(BINARY)   $(VPS):$(REMOTE_DIR)/$(BINARY).new
	scp $(BUILD_DIR)/$(TOKENGEN) $(VPS):$(REMOTE_DIR)/$(TOKENGEN).new
	ssh $(VPS) 'set -e; \
		mv $(REMOTE_DIR)/$(BINARY).new   $(REMOTE_DIR)/$(BINARY); \
		mv $(REMOTE_DIR)/$(TOKENGEN).new $(REMOTE_DIR)/$(TOKENGEN); \
		chmod +x $(REMOTE_DIR)/$(BINARY) $(REMOTE_DIR)/$(TOKENGEN); \
		chown $(REMOTE_OWNER) $(REMOTE_DIR)/$(BINARY) $(REMOTE_DIR)/$(TOKENGEN); \
		systemctl restart supportbot tokengen; \
		systemctl --no-pager --lines=0 status supportbot tokengen'

# CLIENTS_SRC is the local file pushed to the remote allowlist. It defaults to
# the untracked clients.local.json holding real chat IDs (FIX-10); the tracked
# config/clients.json contains only placeholders and must not overwrite prod.
CLIENTS_SRC ?= config/clients.local.json

.PHONY: deploy-config
deploy-config: ## Залить $(CLIENTS_SRC) в config/clients.json на VPS (НЕ трогает .env)
	scp $(CLIENTS_SRC) $(VPS):$(REMOTE_DIR)/config/clients.json
	ssh $(VPS) 'chown $(REMOTE_OWNER) $(REMOTE_DIR)/config/clients.json'

.PHONY: deploy-units
deploy-units: ## Залить systemd unit-файлы + daemon-reload
	scp systemd/supportbot.service $(VPS):/etc/systemd/system/supportbot.service
	scp systemd/tokengen.service   $(VPS):/etc/systemd/system/tokengen.service
	ssh $(VPS) 'systemctl daemon-reload'

# ── Эксплуатация ────────────────────────────────────────────────────────────
.PHONY: logs
logs: ## Логи бота на VPS (journalctl -f)
	ssh $(VPS) 'journalctl -u supportbot -f'

.PHONY: logs-tokengen
logs-tokengen: ## Логи tokengen на VPS
	ssh $(VPS) 'journalctl -u tokengen -f'

.PHONY: restart
restart: ## Перезапустить сервисы на VPS
	ssh $(VPS) 'systemctl restart supportbot tokengen'

.PHONY: health
health: ## Проверка health-эндпоинта на VPS (через localhost)
	ssh $(VPS) 'curl -fsS http://127.0.0.1:8080/healthz && echo'
