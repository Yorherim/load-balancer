// Package ratelimiter реализует ограничение частоты запросов по IP или заголовку
// с использованием алгоритма Token Bucket и хранением лимитов в БД.
package ratelimiter

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"load-balancer/internal/config"
	"load-balancer/internal/storage"
)

// RateLimitStore определяет методы, необходимые для управления *конфигурацией* лимитов.
// Состояние (токены, время) управляется отдельно и сохраняется/загружается
// напрямую через конкретную реализацию (например, *storage.DB).
type StoreConfigInterface interface {
	// GetClientLimitConfig извлекает только конфигурацию лимита (rate, capacity) для клиента.
	GetClientLimitConfig(clientID string) (rate, capacity float64, found bool, err error)
	CreateClientLimit(clientID string, limit config.ClientRateConfig) error
	UpdateClientLimit(clientID string, limit config.ClientRateConfig) error
	DeleteClientLimit(clientID string) error
	// SupportsStatePersistence указывает, поддерживает ли хранилище сохранение/загрузку состояния.
	SupportsStatePersistence() bool
}

// StateStore определяет методы для работы с сохраненным состоянием.
// Используется для type assertion, когда SupportsStatePersistence() == true.
type StateStore interface {
	GetClientSavedState(clientID string) (tokens float64, lastRefill time.Time, found bool, err error)
	BatchUpdateClientState(states map[string]storage.ClientState) error
}

// TokenBucket представляет "корзину токенов" для одного клиента.
type TokenBucket struct {
	// capacity - максимальное количество токенов в корзине.
	capacity float64
	// rate - скорость пополнения корзины (токенов в секунду).
	rate float64
	// tokens - текущее количество токенов.
	tokens float64
	// lastRefill - время последнего пополнения.
	lastRefill time.Time
	// mu - мьютекс для защиты доступа к полям корзины.
	mu sync.Mutex
}

// RateLimiter управляет корзинами токенов для разных клиентов.
type RateLimiter struct {
	// buckets - карта, где ключ - IP-адрес клиента, значение - его корзина токенов.
	buckets map[string]*TokenBucket
	// mu - мьютекс для защиты доступа к карте buckets (при добавлении новых клиентов).
	mu sync.RWMutex
	// store - хранилище для получения индивидуальных лимитов.
	store StoreConfigInterface
	// defaultRate - скорость пополнения по умолчанию для новых клиентов.
	defaultRate float64
	// defaultCapacity - емкость корзины по умолчанию для новых клиентов.
	defaultCapacity float64
	// identifierHeader - Имя заголовка для идентификации клиента.
	identifierHeader string
	// enabled - флаг, включен ли rate limiter.
	enabled bool

	// Поля для фонового пополнения
	ticker *time.Ticker
	quit   chan struct{}
}

// New создает новый экземпляр RateLimiter.
// Принимает конфигурацию и реализацию StoreConfigInterface.
func New(cfg *config.RateLimiterConfig, store StoreConfigInterface) (*RateLimiter, error) {
	if !cfg.Enabled {
		log.Println("[RateLimiter] Выключен.")
		return NewDisabled(), nil
	}

	if store == nil {
		log.Printf("[Warning][RateLimiter] Rate limiter включен, но хранилище (store) не предоставлено. Будут использоваться только дефолтные лимиты.")
	}

	rl := &RateLimiter{
		store:            store,
		buckets:          make(map[string]*TokenBucket),
		defaultRate:      cfg.DefaultRate,
		defaultCapacity:  cfg.DefaultCapacity,
		identifierHeader: cfg.IdentifierHeader,
		quit:             make(chan struct{}),
		enabled:          true,
	}

	logMsg := fmt.Sprintf("[RateLimiter] Инициализирован (Store: %T). Default Rate=%.2f/sec, Default Capacity=%.2f", store, cfg.DefaultRate, cfg.DefaultCapacity)
	if rl.identifierHeader != "" {
		logMsg += fmt.Sprintf(". Идентификация клиента по заголовку: '%s' (fallback на IP)", rl.identifierHeader)
	} else {
		logMsg += ". Идентификация клиента по IP-адресу."
	}
	log.Println(logMsg)

	rl.ticker = time.NewTicker(1 * time.Second)
	go rl.backgroundRefiller()
	log.Printf("[RateLimiter] Запущено фоновое пополнение корзин (каждую секунду).")

	return rl, nil
}

// NewDisabled создает "выключенный" экземпляр RateLimiter, который всегда разрешает запросы.
func NewDisabled() *RateLimiter {
	return &RateLimiter{
		enabled: false,
		buckets: make(map[string]*TokenBucket), // Инициализируем, чтобы избежать nil паники
		// Остальные поля не важны, так как enabled=false
	}
}

// Stop останавливает фоновую горутину пополнения.
func (rl *RateLimiter) Stop() {
	if rl.ticker != nil {
		rl.ticker.Stop() // Останавливаем тикер
		close(rl.quit)   // Закрываем канал, чтобы сигнализировать горутине
		log.Printf("[RateLimiter] Фоновое пополнение остановлено.")
	}
}

// backgroundRefiller - горутина, периодически пополняющая все активные корзины.
func (rl *RateLimiter) backgroundRefiller() {
	for {
		select {
		case <-rl.ticker.C: // Ждем сигнала от тикера
			// Проходим по всем существующим корзинам и пополняем их
			rl.mu.RLock() // Блокируем карту buckets на чтение
			for _, bucket := range rl.buckets {
				bucket.mu.Lock()   // Блокируем конкретную корзину на запись
				bucket.refill()    // Вызываем пополнение
				bucket.mu.Unlock() // Разблокируем корзину
			}
			rl.mu.RUnlock() // Разблокируем карту

		case <-rl.quit: // Ждем сигнала на выход
			// Получен сигнал завершения
			return
		}
	}
}

// refill пополняет корзину токенами на основе прошедшего времени.
// Должен вызываться под мьютексом bucket.mu.
func (tb *TokenBucket) refill() {
	now := time.Now()
	// Если lastRefill еще не установлен (нулевое время), считаем, что пополнение начинается сейчас
	if tb.lastRefill.IsZero() {
		tb.lastRefill = now
		return // Нет времени для пополнения
	}

	duration := now.Sub(tb.lastRefill)
	// Пропускаем пополнение, если время не прошло или rate нулевой
	if duration <= 0 || tb.rate <= 0 {
		return
	}
	tokensToAdd := duration.Seconds() * tb.rate
	tb.tokens = min(tb.capacity, tb.tokens+tokensToAdd)
	tb.lastRefill = now // Обновляем время ТОЛЬКО после успешного добавления
}

// updateBucketIfNeeded обновляет параметры rate и capacity существующей корзины, если они отличаются от переданных.
// Должен вызываться под блокировкой bucket.mu.
func updateBucketIfNeeded(bucket *TokenBucket, newRate, newCapacity float64, clientID, source string) {
	rateChanged := bucket.rate != newRate
	capacityChanged := bucket.capacity != newCapacity

	if rateChanged || capacityChanged {
		log.Printf("[RateLimiter] Обновление лимитов для '%s' (источник: %s): Rate: %.2f -> %.2f, Capacity: %.2f -> %.2f",
			clientID, source, bucket.rate, newRate, bucket.capacity, newCapacity)
		bucket.rate = newRate
		bucket.capacity = newCapacity
		if bucket.tokens > bucket.capacity {
			bucket.tokens = bucket.capacity
		}
	}
}

// getOrCreateBucket находит или создает корзину токенов в памяти для клиента,
// загружая начальное состояние из хранилища, если оно доступно.
func (rl *RateLimiter) getOrCreateBucket(clientID string) *TokenBucket {
	// 1. Поиск существующей корзины в памяти (под RLock)
	rl.mu.RLock()
	bucket, exists := rl.buckets[clientID]
	rl.mu.RUnlock()

	if exists {
		// Корзина найдена. Ее состояние (токены, время) актуально, т.к. управляется в памяти.
		// Но ее лимиты (rate, capacity) могли измениться в БД. Проверим и обновим их.
		var dbRate, dbCapacity float64
		var configFound bool
		var configErr error
		configSource := "дефолтными"

		if rl.store != nil {
			dbRate, dbCapacity, configFound, configErr = rl.store.GetClientLimitConfig(clientID)
			if configErr != nil {
				log.Printf("[RateLimiter] Ошибка получения конфига лимита для существующего клиента '%s', используются текущие. Ошибка: %v", clientID, configErr)
				// В случае ошибки оставляем текущие rate/capacity корзины
				bucket.mu.Lock() // Блокируем только для чтения текущих значений
				dbRate = bucket.rate
				dbCapacity = bucket.capacity
				bucket.mu.Unlock()
				configSource = "текущими (ошибка БД)"
			} else if configFound {
				configSource = "хранилища"
			} else {
				configSource = "дефолтными (не найден в хранилище)"
				dbRate = rl.defaultRate
				dbCapacity = rl.defaultCapacity
			}
		} else {
			// Store не задан, используем дефолтные (хотя корзина уже есть?)
			// Логичнее оставить текущие значения корзины
			bucket.mu.Lock()
			dbRate = bucket.rate
			dbCapacity = bucket.capacity
			bucket.mu.Unlock()
			configSource = "текущими (store=nil)"
		}

		bucket.mu.Lock()
		updateBucketIfNeeded(bucket, dbRate, dbCapacity, clientID, configSource)
		bucket.mu.Unlock()
		return bucket
	}

	// --- Корзины в памяти не было, создаем новую ---
	rl.mu.Lock() // Блокируем карту buckets для записи
	// Повторная проверка на случай, если корзину создали, пока мы ждали блокировку
	bucket, exists = rl.buckets[clientID]
	if exists {
		rl.mu.Unlock()
		// Повторно обновляем лимиты, как в блоке if exists выше
		// Код немного дублируется, но это проще, чем выносить в отдельную функцию
		var dbRate, dbCapacity float64
		var configFound bool
		var configErr error
		configSource := "дефолтными"
		if rl.store != nil {
			dbRate, dbCapacity, configFound, configErr = rl.store.GetClientLimitConfig(clientID)
			if configErr != nil {
				log.Printf("[RateLimiter] Ошибка получения конфига лимита для существующего клиента '%s' (повторно), используются текущие. Ошибка: %v", clientID, configErr)
				bucket.mu.Lock()
				dbRate = bucket.rate
				dbCapacity = bucket.capacity
				bucket.mu.Unlock()
				configSource = "текущими (ошибка БД)"
			} else if configFound {
				configSource = "хранилища"
			} else {
				configSource = "дефолтными (не найден в хранилище)"
				dbRate = rl.defaultRate
				dbCapacity = rl.defaultCapacity
			}
		} else {
			bucket.mu.Lock()
			dbRate = bucket.rate
			dbCapacity = bucket.capacity
			bucket.mu.Unlock()
			configSource = "текущими (store=nil)"
		}
		bucket.mu.Lock()
		updateBucketIfNeeded(bucket, dbRate, dbCapacity, clientID, configSource+" (повторная проверка)")
		bucket.mu.Unlock()
		return bucket
	}

	// --- Действительно создаем новую корзину ---

	// 2. Получаем конфигурацию (rate, capacity)
	initialRate := rl.defaultRate
	initialCapacity := rl.defaultCapacity
	configSource := "дефолтными"
	if rl.store != nil {
		dbRate, dbCapacity, configFound, configErr := rl.store.GetClientLimitConfig(clientID)
		if configErr != nil {
			log.Printf("[RateLimiter] Ошибка получения конфига лимита для нового клиента '%s', используются дефолтные. Ошибка: %v", clientID, configErr)
			// Оставляем дефолтные initialRate, initialCapacity
		} else if configFound {
			initialRate = dbRate
			initialCapacity = dbCapacity
			configSource = "хранилища"
		} else {
			configSource = "дефолтными (не найден в хранилище)"
		}
	}

	// 3. Получаем сохраненное состояние (tokens, lastRefill), если store поддерживает это.
	initialTokens := initialCapacity // По умолчанию - полная корзина
	initialLastRefill := time.Time{} // По умолчанию - нулевое время (refill начнется с now)
	stateSource := "начальное (полная корзина, время=0)"

	// Проверяем поддержку сохранения и делаем type assertion на StateStore
	if rl.store != nil && rl.store.SupportsStatePersistence() {
		stateStore, ok := rl.store.(StateStore)
		if !ok {
			// Это не должно происходить, если SupportsStatePersistence == true
			log.Printf("[Error][RateLimiter] Store (%T) сообщает о поддержке состояния, но не реализует StateStore!", rl.store)
		} else {
			// Используем интерфейс StateStore для доступа к методам
			savedTokens, savedLastRefill, stateFound, stateErr := stateStore.GetClientSavedState(clientID)
			if stateErr != nil {
				log.Printf("[RateLimiter] Ошибка получения сохраненного состояния для нового клиента '%s', используется начальное. Ошибка: %v", clientID, stateErr)
				// Оставляем начальные initialTokens, initialLastRefill
			} else if stateFound {
				initialTokens = savedTokens
				initialLastRefill = savedLastRefill // Используем сохраненное время
				stateSource = "сохраненное из БД"
				// Обрезаем токены по загруженной емкости
				if initialTokens > initialCapacity {
					initialTokens = initialCapacity
				}
			} else {
				stateSource = "начальное (не найдено в БД)"
			}
		}
	} else if rl.store != nil {
		log.Printf("[RateLimiter] Хранилище (%T) не поддерживает сохранение состояния для '%s'. Используется начальное.", rl.store, clientID)
	}

	log.Printf("[RateLimiter] Создается новая корзина для клиента '%s'. Конфиг: %s (Rate=%.2f, Capacity=%.2f). Состояние: %s (Tokens=%.2f, LastRefill=%v)",
		clientID, configSource, initialRate, initialCapacity, stateSource, initialTokens, initialLastRefill)

	newBucket := &TokenBucket{
		capacity:   initialCapacity,
		rate:       initialRate,
		tokens:     initialTokens,
		lastRefill: initialLastRefill, // Может быть time.Time{}
	}

	// 4. Выполняем первоначальное пополнение, если lastRefill было загружено из БД
	newBucket.mu.Lock()
	newBucket.refill() // Досчитает токены с lastRefill до now
	// Сохраняем актуальные значения после refill для сохранения
	currentTokens := newBucket.tokens
	currentLastRefill := newBucket.lastRefill
	newBucket.mu.Unlock()

	rl.buckets[clientID] = newBucket
	rl.mu.Unlock() // Разблокируем карту buckets ПОСЛЕ добавления

	log.Printf("[RateLimiter] Корзина для '%s' создана и инициализирована. Текущее состояние: Tokens=%.2f, LastRefill=%v",
		clientID, currentTokens, currentLastRefill)

	return newBucket
}

// Маленькое значение для сравнения float
const floatEpsilon = 1e-9

// Allow проверяет, разрешен ли запрос от данного клиента.
func (rl *RateLimiter) Allow(clientID string) bool {
	if !rl.enabled {
		return true
	}

	bucket := rl.getOrCreateBucket(clientID)

	bucket.mu.Lock()
	defer bucket.mu.Unlock()

	// Пополнение происходит в фоне тикером, здесь его вызывать не нужно.

	log.Printf("[RateLimiter] Проверка для '%s': %.2f токенов доступно (лимиты: rate=%.2f, capacity=%.2f)",
		clientID, bucket.tokens, bucket.rate, bucket.capacity)

	// Используем сравнение с эпсилон для float
	if bucket.tokens >= 1.0-floatEpsilon {
		bucket.tokens--
		return true
	}

	log.Printf("[RateLimiter] Запрос от '%s' отклонен (лимит превышен)", clientID)
	return false
}

// IsEnabled возвращает true, если Rate Limiter включен.
func (rl *RateLimiter) IsEnabled() bool {
	return rl.enabled
}

// GetClientID извлекает идентификатор клиента из HTTP-запроса.
// Сначала проверяет настроенный заголовок, затем IP-адрес.
// Возвращает ID клиента как строку.
func (rl *RateLimiter) GetClientID(r *http.Request) string {
	// 1. Проверяем кастомный заголовок, если он настроен.
	if rl.identifierHeader != "" {
		clientID := r.Header.Get(rl.identifierHeader)
		if clientID != "" {
			// Используем значение из заголовка.
			return clientID
		}
	}

	// 2. Если заголовок не настроен или пуст, используем IP-адрес.
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		parts := strings.Split(xff, ",")
		for _, part := range parts {
			ip := strings.TrimSpace(part)
			if ip != "" && net.ParseIP(ip) != nil {
				return ip // Возвращаем первый валидный IP из XFF
			}
		}
	}

	// Используем RemoteAddr как fallback.
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		if net.ParseIP(ip) != nil {
			return ip
		}
	}

	// Крайний случай: не удалось извлечь чистый IP.
	log.Printf("[Warning] Не удалось определить ID клиента (заголовок: '%s', XFF: '%s', RemoteAddr: '%s'). Используется RemoteAddr.", rl.identifierHeader, xff, r.RemoteAddr)
	return r.RemoteAddr
}

// Helper function min
func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// SaveState сохраняет текущее состояние всех корзин в хранилище,
// если хранилище поддерживает это.
func (rl *RateLimiter) SaveState() error {
	// Проверяем поддержку сохранения
	if rl.store == nil || !rl.store.SupportsStatePersistence() || !rl.enabled {
		storeType := "nil"
		if rl.store != nil {
			storeType = fmt.Sprintf("%T", rl.store)
		}
		log.Printf("[RateLimiter] Сохранение состояния не выполнено. Enabled: %t, Store: %s, SupportsState: %t",
			rl.enabled, storeType, rl.store != nil && rl.store.SupportsStatePersistence())
		return nil // Не ошибка, просто не сохраняем
	}

	// Делаем type assertion на StateStore
	stateStore, ok := rl.store.(StateStore)
	if !ok {
		log.Printf("[Error][RateLimiter] Store (%T) сообщает о поддержке состояния, но не реализует StateStore! Сохранение невозможно.", rl.store)
		return fmt.Errorf("store %T не реализует StateStore", rl.store)
	}

	// Собираем состояния всех корзин
	statesToSave := make(map[string]storage.ClientState) // Используем тип из storage

	rl.mu.RLock() // Блокируем карту buckets на чтение
	log.Printf("[RateLimiter] Подготовка к сохранению состояния %d корзин...", len(rl.buckets))
	for clientID, bucket := range rl.buckets {
		bucket.mu.Lock() // Блокируем конкретную корзину на время чтения ее состояния
		// Копируем актуальное состояние
		statesToSave[clientID] = storage.ClientState{
			Tokens:     bucket.tokens,
			LastRefill: bucket.lastRefill,
		}
		bucket.mu.Unlock() // Разблокируем корзину
	}
	rl.mu.RUnlock() // Разблокируем карту buckets

	if len(statesToSave) == 0 {
		log.Println("[RateLimiter] Нет активных корзин для сохранения.")
		return nil
	}

	log.Printf("[RateLimiter] Сохранение состояния %d корзин в хранилище (%T)...", len(statesToSave), rl.store)

	// Вызываем метод конкретной реализации *storage.DB
	err := stateStore.BatchUpdateClientState(statesToSave) // Передаем map[string]storage.ClientState
	if err != nil {
		log.Printf("[Error][RateLimiter] Ошибка при массовом обновлении состояния корзин: %v", err)
		return fmt.Errorf("ошибка сохранения состояния RateLimiter: %w", err) // Возвращаем ошибку
	}

	log.Printf("[RateLimiter] Состояние %d корзин успешно сохранено.", len(statesToSave))
	return nil
}
