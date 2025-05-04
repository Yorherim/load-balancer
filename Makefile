# Имя бинарного файла приложения
BINARY_NAME=balancer

# Имя файла Docker Compose
DOCKER_COMPOSE_FILE=docker-compose.yml

# Путь к Go файлу для сборки
MAIN_GO_FILE=cmd/balancer/main.go

# Путь к файлу конфигурации
CONFIG_FILE=config.yaml

# Путь к файлу БД SQLite
DB_FILE=rate_limits.db

# Default target (вызывается при запуске 'make' без аргументов)
.DEFAULT_GOAL := help

## --- запрос --- ##
req:
	@curl -H "X-Client-ID: my-local-curl" http://localhost:8080 

## --- Сборка --- ##

build: ## Собрать бинарный файл приложения
	@echo "Сборка приложения $(BINARY_NAME)..."
	@go build -ldflags="-s -w" -o $(BINARY_NAME) $(MAIN_GO_FILE)
	@echo "Сборка завершена: $(BINARY_NAME)"

## --- Запуск (Локально) --- ##

run: build ## Собрать и запустить приложение локально (требует config.yaml и БД)
	@echo "Запуск приложения $(BINARY_NAME) локально..."
	@./$(BINARY_NAME)

## --- Тестирование --- ##

test: ## Запустить все тесты (юнит и интеграционные)
	@echo "Запуск тестов..."
	@go test -coverprofile=coverage.out ./... && go tool cover -html=coverage.out

race: ## Запустить тесты с детектором гонок
	@echo "Запуск тестов с детектором гонок..."
	@CGO_ENABLED=1 go test -race -v ./...

bench: ## Запустить бенчмарки
	@echo "Запуск бенчмарков..."
	@go test -bench=. -benchmem ./...

test-all: race bench ## Запустить тесты с детектором гонок и бенчмарки
	@echo "Полное тестирование завершено."

## --- Docker --- ##

docker-build: ## Собрать Docker образ(ы) с помощью Docker Compose
	@echo "Сборка Docker образа(ов)..."
	@docker-compose -f $(DOCKER_COMPOSE_FILE) build

docker-up: ## Запустить сервисы в Docker Compose (в фоновом режиме)
	@echo "Запуск Docker Compose сервисов..."
	@docker-compose -f $(DOCKER_COMPOSE_FILE) up -d

docker-down: ## Остановить и удалить контейнеры Docker Compose
	@echo "Остановка Docker Compose сервисов..."
	@docker-compose -f $(DOCKER_COMPOSE_FILE) down

docker-logs: ## Показать логи запущенных Docker Compose сервисов
	@echo "Просмотр логов Docker Compose..."
	@docker-compose -f $(DOCKER_COMPOSE_FILE) logs -f

docker-restart: docker-down docker-up ## Перезапустить сервисы Docker Compose
	@echo "Сервисы Docker Compose перезапущены."

## --- Утилиты --- ##

clean: ## Удалить собранный бинарный файл и файл БД
	@echo "Очистка..."
	@rm -f $(BINARY_NAME)
	@rm -f $(DB_FILE)
	@echo "Очистка завершена."

deps: ## Загрузить и привести в порядок зависимости Go
	@echo "Обновление зависимостей Go..."
	@go mod tidy
	@go mod download

help: ## Показать это сообщение помощи
	@echo "Доступные команды:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: build run test race bench test-all docker-build docker-up docker-down docker-logs docker-restart clean deps help 