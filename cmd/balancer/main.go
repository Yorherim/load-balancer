package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"load-balancer/internal/api"
	"load-balancer/internal/config"

	"load-balancer/internal/balancer"

	"load-balancer/internal/ratelimiter"

	"load-balancer/internal/storage"

	_ "modernc.org/sqlite"
)

func main() {
	log.Println("Запуск балансировщика...")
	configPath := "config.yaml"

	// Загрузка конфигурации
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("[Error] Не удалось загрузить конфигурацию: %v", err)
	}

	// Проверяем базовые параметры конфигурации.
	if len(cfg.BackendServers) == 0 {
		log.Fatal("Список бэкенд-серверов (backend_servers) в конфигурации пуст.")
	}
	if cfg.Port == "" {
		log.Fatal("Порт (port) не указан в конфигурации.")
	}

	// Инициализация хранилища (если Rate Limiter включен и использует БД)
	var store *storage.DB
	if cfg.RateLimiter.Enabled && cfg.RateLimiter.DatabasePath != "" {
		log.Printf("[Storage] Инициализация SQLite из '%s'...", cfg.RateLimiter.DatabasePath)
		store, err = storage.NewSQLiteDB(cfg.RateLimiter.DatabasePath)
		if err != nil {
			log.Fatalf("[Error] Не удалось подключиться к БД SQLite: %v", err)
		}
		defer store.Close() // Закрываем БД при выходе
	} else {
		log.Println("[Storage] Используется хранилище в памяти или Rate Limiter выключен (API управления лимитами будет недоступно).")
		store = nil // APIHandler будет знать, что store недоступен
	}

	// Инициализация Rate Limiter
	// Передаем указатель на секцию RateLimiter из конфига и store (может быть nil)
	// ratelimiter.New ожидает *config.RateLimiterConfig
	rateLimiter, err := ratelimiter.New(&cfg.RateLimiter, store)
	if err != nil {
		// Обрабатываем ошибку от New, если она есть (хотя пока New ее не возвращает)
		log.Fatalf("[Error] Не удалось инициализировать Rate Limiter: %v", err)
	}

	// Инициализация балансировщика
	// balancer.New ожидает config.HealthCheckConfig (значение)
	lb, err := balancer.New(
		cfg.BackendServers,
		rateLimiter,
		cfg.HealthCheck, // Передаем значение структуры
		cfg.LoadBalancingAlgorithm,
	)
	if err != nil {
		log.Fatalf("[Error] Не удалось создать балансировщик: %v", err)
	}

	// Инициализация API обработчика (передаем store)
	apiHandler := api.NewAPIHandler(store)

	// Создаем основной маршрутизатор
	smux := http.NewServeMux()
	smux.Handle("/clients", http.StripPrefix("/clients", apiHandler))
	smux.Handle("/clients/", http.StripPrefix("/clients", apiHandler))
	smux.Handle("/", lb)

	// 7. Настраиваем и запускаем HTTP-сервер.
	addr := ":" + cfg.Port
	server := &http.Server{
		Addr:    addr,
		Handler: smux,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("Балансировщик запущен на %s", addr)
		log.Printf("API доступно по префиксу /clients/")
		log.Printf("Зарегистрированные бэкенды: %v", cfg.BackendServers)
		if cfg.RateLimiter.Enabled {
			log.Printf("Rate Limiter включен (Store: %T, Header: '%s')", store, cfg.RateLimiter.IdentifierHeader)
		} else {
			log.Printf("Rate Limiter выключен.")
		}
		if cfg.HealthCheck.Enabled {
			log.Printf("[Main] Health Checks включены (Interval: %v, Timeout: %v, Path: %s)",
				cfg.HealthCheck.Interval, cfg.HealthCheck.Timeout, cfg.HealthCheck.Path)
		} else {
			log.Println("[Main] Health Checks выключены.")
		}

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Ошибка запуска сервера: %v", err)
		}
	}()

	// Блокируем main горутину до получения сигнала.
	<-quit
	log.Println("Получен сигнал завершения, начинаем Graceful Shutdown...")

	// Создаем контекст с таймаутом для Shutdown.
	// Даем серверу 10 секунд на завершение обработки текущих запросов.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()

	// Останавливаем фоновые процессы:
	// Сначала останавливаем Health Checks балансировщика.
	// Метод StopHealthChecks вызывается всегда, даже если HealthCheck был nil/disabled,
	// внутри Balancer есть проверка healthCheckStopChan != nil
	lb.StopHealthChecks()

	// Затем останавливаем Ticker в Rate Limiter.
	if rateLimiter != nil {
		rateLimiter.Stop()

		// Сохраняем текущее состояние корзин Rate Limiter в БД.
		if err := rateLimiter.SaveState(); err != nil {
			// Логируем ошибку, но не прерываем Shutdown
			log.Printf("[Error] Ошибка сохранения состояния Rate Limiter: %v", err)
		}
	}

	// Выполняем Graceful Shutdown сервера.
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Ошибка при Graceful Shutdown сервера: %v", err)
	} else {
		log.Println("HTTP-сервер корректно остановлен.")
	}

	// Закрываем соединение с базой данных.
	if err := store.Close(); err != nil {
		log.Printf("[Error] Ошибка закрытия БД: %v", err)
	} else {
		log.Println("Соединение с БД закрыто.")
	}

	log.Println("Балансировщик успешно завершил работу.")
}
