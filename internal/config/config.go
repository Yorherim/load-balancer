package config

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ClientRateConfig содержит индивидуальные настройки скорости и емкости лимита для клиента.
type ClientRateConfig struct {
	Rate     float64 `yaml:"rate"`     // скорость пополнения
	Capacity float64 `yaml:"capacity"` // емкость корзины
}

// RateLimiterConfig содержит настройки для rate limiter'а.
type RateLimiterConfig struct {
	Enabled          bool    `yaml:"enabled"`           // Включен ли rate limiter.
	DefaultRate      float64 `yaml:"default_rate"`      // Токенов в секунду по умолчанию.
	DefaultCapacity  float64 `yaml:"default_capacity"`  // Емкость корзины по умолчанию.
	DatabasePath     string  `yaml:"database_path"`     // Путь к файлу SQLite.
	IdentifierHeader string  `yaml:"identifier_header"` // Имя заголовка для ID клиента (опционально).
}

// HealthCheckConfig содержит настройки для проверок состояния бэкендов.
type HealthCheckConfig struct {
	Enabled     bool   `yaml:"enabled"`
	IntervalStr string `yaml:"interval"` // Интервал проверки (строка, например "10s")
	TimeoutStr  string `yaml:"timeout"`  // Таймаут проверки (строка, например "2s")
	Path        string `yaml:"path"`     // Путь для проверки

	Interval time.Duration `yaml:"-"`
	Timeout  time.Duration `yaml:"-"`
}

// Config определяет структуру конфигурационного файла.
type Config struct {
	// Port - порт, на котором будет работать балансировщик.
	Port string `yaml:"port"`
	// BackendServers - список URL-адресов бэкенд-серверов.
	BackendServers []string `yaml:"backend_servers"`
	// LoadBalancingAlgorithm - алгоритм балансировки
	LoadBalancingAlgorithm string `yaml:"load_balancing_algorithm"`
	// RateLimiter - настройки для модуля Rate Limiting.
	RateLimiter RateLimiterConfig `yaml:"rate_limiter"`
	HealthCheck HealthCheckConfig `yaml:"health_check"`
}

// LoadConfig загружает конфигурацию из указанного файла.
func LoadConfig(configPath string) (*Config, error) {
	config := &Config{
		// Устанавливаем значения по умолчанию
		LoadBalancingAlgorithm: "round_robin",
		RateLimiter: RateLimiterConfig{
			Enabled:          false,
			DefaultRate:      1,
			DefaultCapacity:  1,
			DatabasePath:     "./rate_limits.db",
			IdentifierHeader: "",
		},
		HealthCheck: HealthCheckConfig{
			Enabled: false,
		},
	}

	file, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	err = yaml.Unmarshal(file, config)
	if err != nil {
		return nil, err
	}

	// Валидация алгоритма балансировки
	config.LoadBalancingAlgorithm = strings.ToLower(config.LoadBalancingAlgorithm)
	if config.LoadBalancingAlgorithm != "round_robin" && config.LoadBalancingAlgorithm != "random" {
		return nil, fmt.Errorf("неподдерживаемый load_balancing_algorithm: '%s'. Допустимые значения: 'round_robin', 'random'", config.LoadBalancingAlgorithm)
	}
	log.Printf("[Config] Используемый алгоритм балансировки: %s", config.LoadBalancingAlgorithm)

	// Дополнительная валидация
	if config.RateLimiter.Enabled {
		if config.RateLimiter.DefaultRate <= 0 {
			config.RateLimiter.DefaultRate = 1
			println("[Warning] rate_limiter.default_rate должен быть > 0, установлено значение по умолчанию 1")
		}
		if config.RateLimiter.DefaultCapacity <= 0 {
			config.RateLimiter.DefaultCapacity = 1
			println("[Warning] rate_limiter.default_capacity должен быть > 0, установлено значение по умолчанию 1")
		}
		if config.RateLimiter.DatabasePath == "" {
			config.RateLimiter.DatabasePath = "./rate_limits.db" // Устанавливаем дефолт, если не указан
			println("[Warning] rate_limiter.database_path не указан, используется значение по умолчанию ./rate_limits.db")
		}

	}

	// Парсим интервал и таймаут HealthCheck, если включено
	if config.HealthCheck.Enabled {
		if config.HealthCheck.IntervalStr == "" {
			config.HealthCheck.IntervalStr = "10s" // Значение по умолчанию
			fmt.Printf("[Config] Интервал HealthCheck не указан, используется значение по умолчанию: %s\n", config.HealthCheck.IntervalStr)
		}
		interval, err := time.ParseDuration(config.HealthCheck.IntervalStr)
		if err != nil {
			return nil, fmt.Errorf("неверный формат интервала HealthCheck (%s): %w", config.HealthCheck.IntervalStr, err)
		}
		if interval <= 0 {
			return nil, fmt.Errorf("интервал HealthCheck должен быть положительным: %s", config.HealthCheck.IntervalStr)
		}
		config.HealthCheck.Interval = interval

		if config.HealthCheck.TimeoutStr == "" {
			config.HealthCheck.TimeoutStr = "2s" // Значение по умолчанию
			fmt.Printf("[Config] Таймаут HealthCheck не указан, используется значение по умолчанию: %s\n", config.HealthCheck.TimeoutStr)
		}
		timeout, err := time.ParseDuration(config.HealthCheck.TimeoutStr)
		if err != nil {
			return nil, fmt.Errorf("неверный формат таймаута HealthCheck (%s): %w", config.HealthCheck.TimeoutStr, err)
		}
		if timeout <= 0 {
			return nil, fmt.Errorf("таймаут HealthCheck должен быть положительным: %s", config.HealthCheck.TimeoutStr)
		}
		if timeout >= interval {
			fmt.Printf("[Config] Внимание: Таймаут HealthCheck (%s) больше или равен интервалу (%s). Рекомендуется меньший таймаут.\n", config.HealthCheck.TimeoutStr, config.HealthCheck.IntervalStr)
		}
		config.HealthCheck.Timeout = timeout

		if config.HealthCheck.Path == "" {
			config.HealthCheck.Path = "/" // Значение по умолчанию
			fmt.Printf("[Config] Путь HealthCheck не указан, используется значение по умолчанию: %s\n", config.HealthCheck.Path)
		}
		// Добавляем '/' в начало пути, если его нет
		if len(config.HealthCheck.Path) == 0 || config.HealthCheck.Path[0] != '/' {
			config.HealthCheck.Path = "/" + config.HealthCheck.Path
		}

		fmt.Printf("[Config] Health Checks включены: Интервал=%v, Таймаут=%v, Путь=%s\n",
			config.HealthCheck.Interval, config.HealthCheck.Timeout, config.HealthCheck.Path)
	} else {
		fmt.Println("[Config] Health Checks выключены.")
	}

	return config, nil
}
