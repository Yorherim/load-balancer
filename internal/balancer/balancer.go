package balancer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"load-balancer/internal/config"
	"load-balancer/internal/response"
)

type Limiter interface {
	Allow(clientID string) bool
	GetClientID(r *http.Request) string
}

// ErrNoHealthyBackends возвращается, когда нет доступных для запроса бэкендов.
var ErrNoHealthyBackends = errors.New("нет доступных бэкендов")

// Backend представляет один бэкенд-сервер.
type Backend struct {
	URL   *url.URL
	Alive bool         // Флаг, указывающий, доступен ли бэкенд.
	mux   sync.RWMutex // Мьютекс для безопасного доступа к полю Alive.
	// ReverseProxy используется для перенаправления запросов на этот бэкенд.
	ReverseProxy *httputil.ReverseProxy
}

// SetAlive безопасно устанавливает статус работоспособности бэкенда.
func (b *Backend) SetAlive(alive bool) {
	b.mux.Lock()         // Блокируем на запись.
	defer b.mux.Unlock() // Гарантируем разблокировку.
	// Логируем только при изменении статуса
	if b.Alive != alive {
		b.Alive = alive
		status := "недоступен"
		if alive {
			status = "доступен"
		}
		log.Printf("[HealthCheck] Бэкенд %s теперь %s", b.URL.String(), status)
	}
}

// IsAlive безопасно проверяет статус работоспособности бэкенда.
func (b *Backend) IsAlive() bool {
	b.mux.RLock()         // Блокируем на чтение.
	defer b.mux.RUnlock() // Гарантируем разблокировку.
	return b.Alive
}

// Balancer является HTTP обработчиком, реализующим балансировку нагрузки.
type Balancer struct {
	backends            []*Backend
	current             atomic.Uint64 // Используется только для Round Robin
	algorithm           string        // Алгоритм балансировки ("round_robin" или "random")
	rng                 *rand.Rand    // Генератор случайных чисел (для Random)
	rateLimiter         Limiter       // Используем интерфейс вместо конкретного типа
	healthCheckConfig   config.HealthCheckConfig
	healthCheckStopChan chan struct{}
}

// New создает новый экземпляр Balancer.
// Теперь принимает интерфейс Limiter.
func New(backendUrls []string, rl Limiter, hcConfig config.HealthCheckConfig, algorithm string) (*Balancer, error) {
	if len(backendUrls) == 0 {
		return nil, fmt.Errorf("не указаны бэкенд-серверы")
	}

	// Приводим название алгоритма к нижнему регистру для надежности
	parsedAlgorithm := strings.ToLower(algorithm)
	if parsedAlgorithm != "round_robin" && parsedAlgorithm != "random" {
		log.Printf("[Warning] Неизвестный алгоритм балансировки '%s', используется 'round_robin'", algorithm)
		parsedAlgorithm = "round_robin"
	}

	b := &Balancer{
		rateLimiter:       rl,
		healthCheckConfig: hcConfig,
		algorithm:         parsedAlgorithm,
	}

	// Инициализируем RNG, если выбран Random
	if b.algorithm == "random" {
		// Используем новый источник, чтобы избежать проблем с глобальным локом
		source := rand.NewSource(time.Now().UnixNano())
		b.rng = rand.New(source)
		log.Println("[Balancer] Инициализирован генератор случайных чисел для Random алгоритма.")
	}

	backends := make([]*Backend, 0, len(backendUrls))

	// Убираем отладочные логи
	// log.Printf("[DEBUG] Начинаем парсинг бэкенд URL...")
	for i, rawURL := range backendUrls {
		// log.Printf("[DEBUG] Парсинг URL #%d: '%s'", i, rawURL)
		parsedURL, err := url.Parse(rawURL)
		// log.Printf("[DEBUG] Результат парсинга URL #%d: err = %v, parsedURL = %v", i, err, parsedURL)
		if err != nil {
			// log.Printf("[DEBUG] Обнаружена ошибка парсинга URL #%d, возвращаем ошибку.", i)
			return nil, fmt.Errorf("ошибка парсинга URL бэкенда #%d ('%s'): %w", i, rawURL, err)
		}

		// Добавляем проверку: URL должен быть абсолютным (иметь схему и хост)
		if parsedURL.Scheme == "" || parsedURL.Host == "" {
			return nil, fmt.Errorf("URL бэкенда #%d ('%s') должен быть абсолютным (например, 'http://host:port')", i, rawURL)
		}

		proxy := httputil.NewSingleHostReverseProxy(parsedURL)

		// Создаем копию индекса для замыкания ErrorHandler
		backendIndex := i

		proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
			log.Printf("--- Custom ErrorHandler ENTERED for %s ---", req.URL.Path) // Добавим лог входа

			// Используем GetClientID из переданного Rate Limiter
			// Проверяем на nil на всякий случай
			clientID := "unknown"
			// Используем интерфейс Limiter, проверка на nil не нужна, если интерфейс не nil
			// if rl != nil { // Больше не нужно, если rl интерфейс
			clientID = rl.GetClientID(req)
			// }
			log.Printf("[Balancer] Ошибка проксирования на Бэкенд #%d (%s) для запроса от '%s': %v. Помечаем как нерабочий.",
				backendIndex, parsedURL.String(), clientID, err)
			// Находим нужный бэкенд по индексу (теперь он есть в замыкании)
			// Нужна проверка на выход за границы на случай гонки состояний, хотя маловероятно
			if backendIndex < len(b.backends) {
				b.backends[backendIndex].SetAlive(false)
			} else {
				log.Printf("[Warning] ErrorHandler: Не удалось найти бэкенд с индексом %d для установки Alive=false", backendIndex)
			}
			// Используем response.RespondWithError для отправки JSON-ответа
			// Сообщение ошибки можно сделать более общим для пользователя
			// Используем уникальное сообщение
			response.RespondWithError(rw, http.StatusBadGateway, "Bad Gateway from Custom Handler")
			log.Printf("--- Custom ErrorHandler EXITED for %s ---", req.URL.Path) // Добавим лог выхода
		}

		backend := &Backend{
			URL:          parsedURL,
			Alive:        true,
			ReverseProxy: proxy,
		}
		// Добавляем во временный слайс
		backends = append(backends, backend)
		log.Printf("[Config] Бэкенд #%d добавлен: %s", i, backend.URL)
	}
	// log.Printf("[DEBUG] Парсинг бэкенд URL завершен успешно.")

	// Только после успешного парсинга всех URL присваиваем слайс балансировщику
	b.backends = backends

	if b.healthCheckConfig.Enabled {
		b.healthCheckStopChan = make(chan struct{})
		go b.startHealthChecks()
		log.Println("[Balancer] Health Checks запущены.")
	}

	return b, nil
}

// StopHealthChecks останавливает фоновые проверки состояния.
func (b *Balancer) StopHealthChecks() {
	if b.healthCheckStopChan != nil {
		close(b.healthCheckStopChan)
		log.Println("[Balancer] Остановка Health Checks...")
		// Можно добавить ожидание завершения, если это необходимо
	}
}

// GetBackends возвращает слайс бэкендов (для использования в тестах).
func (b *Balancer) GetBackends() []*Backend {
	return b.backends
}

// getRoundRobinHealthyBackend выбирает следующий работоспособный бэкенд по Round Robin.
func (b *Balancer) getRoundRobinHealthyBackend() (*Backend, int, error) {
	numBackends := len(b.backends)
	if numBackends == 0 {
		return nil, -1, ErrNoHealthyBackends
	}

	start := b.current.Add(1)

	for i := 0; i < numBackends; i++ {
		idx := int((start + uint64(i) - 1) % uint64(numBackends))
		backend := b.backends[idx]
		if backend.IsAlive() {
			return backend, idx, nil
		}
	}
	return nil, -1, ErrNoHealthyBackends
}

// getRandomHealthyBackend выбирает случайный работоспособный бэкенд.
func (b *Balancer) getRandomHealthyBackend() (*Backend, int, error) {
	// Создаем срез с индексами живых бэкендов
	healthyIndices := make([]int, 0, len(b.backends))
	for i, backend := range b.backends {
		if backend.IsAlive() {
			healthyIndices = append(healthyIndices, i)
		}
	}

	numHealthy := len(healthyIndices)
	if numHealthy == 0 {
		return nil, -1, ErrNoHealthyBackends
	}

	// Выбираем случайный индекс из среза *живых* индексов
	randomIndexInHealthySlice := b.rng.Intn(numHealthy)
	// Получаем оригинальный индекс бэкенда из среза healthyIndices
	originalIndex := healthyIndices[randomIndexInHealthySlice]

	return b.backends[originalIndex], originalIndex, nil
}

// ServeHTTP обрабатывает входящие запросы.
func (b *Balancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Логируем входящий запрос
	clientID := "unknown"
	// Используем интерфейс Limiter
	// if b.rateLimiter != nil { // Проверка больше не нужна, интерфейс будет либо nil, либо реализацией
	// Получаем идентификатор клиента ДО проверки лимита
	clientID = b.rateLimiter.GetClientID(r)
	// }
	log.Printf("[Request] Получен запрос: Метод=%s Путь=%s От=%s (%s)", r.Method, r.URL.Path, r.RemoteAddr, clientID)

	// 1. Rate Limiting (если включен)
	// Интерфейс будет nil, если rate limiter выключен или не передан
	if b.rateLimiter != nil {
		if !b.rateLimiter.Allow(clientID) {
			// Используем новую функцию для ответа
			response.RespondWithError(w, http.StatusTooManyRequests, "Rate limit exceeded")
			return
		}
	}

	// 2. Выбор бэкенда
	var targetBackend *Backend
	var backendIndex int
	var err error

	switch b.algorithm {
	case "random":
		targetBackend, backendIndex, err = b.getRandomHealthyBackend()
	case "round_robin":
		fallthrough // Используем round_robin как дефолтный
	default:
		targetBackend, backendIndex, err = b.getRoundRobinHealthyBackend()
	}

	if err != nil {
		log.Printf("[Balancer] Ошибка выбора бэкенда (%s): %v. Невозможно обработать запрос %s %s от '%s'.", b.algorithm, err, r.Method, r.URL.Path, clientID)
		// Используем response.RespondWithError для 503
		response.RespondWithError(w, http.StatusServiceUnavailable, "All backend servers are unavailable")
		return
	}

	// Настраиваем и выполняем проксирование
	targetUrl := targetBackend.URL
	log.Printf("[Balancer] Перенаправление запроса (%s) от '%s' -> Бэкенд #%d (%s)", b.algorithm, clientID, backendIndex, targetUrl)

	targetBackend.ReverseProxy.Director = func(r *http.Request) {
		// Устанавливаем целевой URL и хост
		r.URL.Scheme = targetUrl.Scheme
		r.URL.Host = targetUrl.Host
		r.URL.Path, r.URL.RawPath = r.URL.Path, r.URL.RawPath
		if _, ok := r.Header["User-Agent"]; !ok {
			// explicitly disable User-Agent so it's not set to default value
			r.Header.Set("User-Agent", "")
		}
		// Устанавливаем Host и X-Forwarded-*
		r.Host = targetUrl.Host
		if originalHost := r.Header.Get("Host"); originalHost != "" {
			r.Header.Set("X-Forwarded-Host", originalHost)
		} else {
			r.Header.Set("X-Forwarded-Host", r.Host)
		}
		// Очищаем X-Forwarded-For, httputil добавит IP балансировщика.
		r.Header.Del("X-Forwarded-For")
		// TODO: Аккуратно добавить IP клиента в X-Forwarded-For, если нужно.

		log.Printf("[Balancer] Перенаправление запроса от '%s' -> Бэкенд #%d (%s)", clientID, backendIndex, targetUrl)
	}

	targetBackend.ReverseProxy.ServeHTTP(w, r)
}

// --- Health Check Logic ---

// startHealthChecks запускает периодические проверки состояния для всех бэкендов.
func (b *Balancer) startHealthChecks() {
	log.Printf("[HealthCheck] Запуск проверок состояния: Интервал=%v, Таймаут=%v, Путь=%s",
		b.healthCheckConfig.Interval, b.healthCheckConfig.Timeout, b.healthCheckConfig.Path)

	// Создаем HTTP клиент с таймаутом
	client := &http.Client{
		Timeout: b.healthCheckConfig.Timeout,
		// Можно настроить Transport для переиспользования соединений и т.д.
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 5, // Пример настройки
			IdleConnTimeout:     30 * time.Second,
		},
	}

	// Создаем тикер
	ticker := time.NewTicker(b.healthCheckConfig.Interval)
	defer ticker.Stop() // Останавливаем тикер при выходе из функции

	// Выполняем первую проверку немедленно
	b.performChecks(client)

	// Запускаем цикл проверок
	for {
		select {
		case <-ticker.C:
			b.performChecks(client)
		case <-b.healthCheckStopChan:
			log.Println("[HealthCheck] Получен сигнал остановки проверок.")
			return // Выходим из горутины
		}
	}
}

// performChecks запускает проверку для каждого бэкенда в отдельной горутине.
func (b *Balancer) performChecks(client *http.Client) {
	log.Println("[HealthCheck] Выполнение цикла проверок...")
	var wg sync.WaitGroup // Используем WaitGroup для ожидания завершения всех горутин проверки (опционально)
	for _, backend := range b.backends {
		wg.Add(1)
		go func(be *Backend) {
			defer wg.Done()
			b.checkBackendHealth(be, client)
		}(backend) // Передаем копию указателя в горутину
	}
	// wg.Wait() // Можно раскомментировать, если нужно дождаться завершения всех проверок перед следующим тиком
}

// checkBackendHealth выполняет проверку состояния одного бэкенда.
func (b *Balancer) checkBackendHealth(backend *Backend, client *http.Client) {
	checkURL := backend.URL.JoinPath(b.healthCheckConfig.Path).String() // Формируем URL для проверки
	// Используем context с таймаутом, хотя клиент уже имеет таймаут.
	// Это полезно, если внутри client.Get есть другие потенциально долгие операции.
	ctx, cancel := context.WithTimeout(context.Background(), b.healthCheckConfig.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		log.Printf("[HealthCheck] Ошибка создания запроса для %s: %v", checkURL, err)
		backend.SetAlive(false) // Считаем нерабочим при ошибке создания запроса
		return
	}

	// Отправляем GET-запрос
	resp, err := client.Do(req)
	if err != nil {
		// Ошибка может быть связана с сетью, таймаутом или другими проблемами
		log.Printf("[HealthCheck] Ошибка проверки бэкенда %s: %v", checkURL, err)
		backend.SetAlive(false)
		return
	}
	defer resp.Body.Close()

	// Проверяем статус код (ожидаем 2xx)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Бэкенд считается живым
		backend.SetAlive(true)
	} else {
		log.Printf("[HealthCheck] Бэкенд %s вернул не-2xx статус: %d", checkURL, resp.StatusCode)
		backend.SetAlive(false)
	}
}
