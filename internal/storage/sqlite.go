package storage

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"load-balancer/internal/config"

	_ "modernc.org/sqlite"
)

// ClientState хранит текущее состояние корзины токенов клиента.
type ClientState struct {
	Tokens     float64
	LastRefill time.Time
}

// DB представляет обертку над соединением с базой данных.
// Это позволяет легко подменять реализацию хранилища в будущем.
type DB struct {
	Conn *sql.DB
	Mu   sync.Mutex
}

// NewSQLiteDB инициализирует соединение с базой данных SQLite и создает таблицу, если она не существует.
func NewSQLiteDB(dataSourceName string) (*DB, error) {
	// Открываем (или создаем) файл базы данных, используя драйвер "sqlite".
	// Имя драйвера для modernc.org/sqlite - просто "sqlite".
	conn, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		return nil, fmt.Errorf("ошибка открытия БД SQLite '%s': %w", dataSourceName, err)
	}

	// Проверяем соединение.
	if err = conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ошибка подключения к БД SQLite '%s': %w", dataSourceName, err)
	}

	// Создаем таблицу для хранения лимитов, если она еще не существует.
	// Добавляем колонки для хранения текущего состояния.
	query := `
	CREATE TABLE IF NOT EXISTS client_rate_limits (
		client_id TEXT PRIMARY KEY,
		rate REAL NOT NULL,
		capacity REAL NOT NULL,
		current_tokens REAL NOT NULL DEFAULT 0.0,  -- Текущее количество токенов
		last_refill TEXT NOT NULL DEFAULT ''     -- Время последнего пополнения (RFC3339Nano)
	);
	`
	_, err = conn.Exec(query)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ошибка создания таблицы client_rate_limits: %w", err)
	}

	log.Printf("[Storage] Успешно подключено к SQLite DB (pure-go): %s", dataSourceName)
	return &DB{Conn: conn}, nil
}

// Close закрывает соединение с базой данных.
func (db *DB) Close() error {
	if db.Conn != nil {
		return db.Conn.Close()
	}
	return nil
}

// GetClientLimitAndState извлекает полное состояние лимита для клиента из базы данных.
func (db *DB) GetClientLimitAndState(clientID string) (rate, capacity, tokens float64, lastRefill time.Time, found bool, err error) {
	var rateDB, capacityDB, tokensDB float64
	var lastRefillStr string
	query := "SELECT rate, capacity, current_tokens, last_refill FROM client_rate_limits WHERE client_id = ?"

	row := db.Conn.QueryRow(query, clientID)
	errScan := row.Scan(&rateDB, &capacityDB, &tokensDB, &lastRefillStr)
	if errScan != nil {
		if errScan == sql.ErrNoRows {
			return 0, 0, 0, time.Time{}, false, nil // Не найдено
		}
		log.Printf("[Storage] Ошибка получения состояния для клиента '%s': %v", clientID, errScan)
		return 0, 0, 0, time.Time{}, false, fmt.Errorf("ошибка запроса состояния клиента '%s': %w", clientID, errScan)
	}

	// Парсим время
	lastRefillTime, errParse := time.Parse(time.RFC3339Nano, lastRefillStr)
	if errParse != nil && lastRefillStr != "" { // Игнорируем ошибку парсинга для пустой строки (дефолт)
		log.Printf("[Storage] Ошибка парсинга last_refill ('%s') для клиента '%s': %v. Используется нулевое время.", lastRefillStr, clientID, errParse)
		lastRefillTime = time.Time{} // Используем нулевое время при ошибке парсинга
	}

	return rateDB, capacityDB, tokensDB, lastRefillTime, true, nil
}

// GetClientLimitConfig извлекает только конфигурацию лимита (rate, capacity) для клиента.
func (db *DB) GetClientLimitConfig(clientID string) (rate, capacity float64, found bool, err error) {
	var rateDB, capacityDB float64
	query := "SELECT rate, capacity FROM client_rate_limits WHERE client_id = ?"
	row := db.Conn.QueryRow(query, clientID)
	errScan := row.Scan(&rateDB, &capacityDB)
	if errScan != nil {
		if errScan == sql.ErrNoRows {
			return 0, 0, false, nil // Не найдено
		}
		log.Printf("[Storage] Ошибка получения конфига лимита для клиента '%s': %v", clientID, errScan)
		return 0, 0, false, fmt.Errorf("ошибка запроса конфига лимита клиента '%s': %w", clientID, errScan)
	}
	return rateDB, capacityDB, true, nil
}

// GetClientSavedState извлекает только сохраненное состояние (tokens, lastRefill) для клиента.
func (db *DB) GetClientSavedState(clientID string) (tokens float64, lastRefill time.Time, found bool, err error) {
	var tokensDB float64
	var lastRefillStr string
	query := "SELECT current_tokens, last_refill FROM client_rate_limits WHERE client_id = ?"
	row := db.Conn.QueryRow(query, clientID)
	errScan := row.Scan(&tokensDB, &lastRefillStr)
	if errScan != nil {
		if errScan == sql.ErrNoRows {
			return 0, time.Time{}, false, nil // Не найдено
		}
		log.Printf("[Storage] Ошибка получения сохраненного состояния для клиента '%s': %v", clientID, errScan)
		return 0, time.Time{}, false, fmt.Errorf("ошибка запроса сохраненного состояния клиента '%s': %w", clientID, errScan)
	}

	// Парсим время
	lastRefillTime, errParse := time.Parse(time.RFC3339Nano, lastRefillStr)
	if errParse != nil && lastRefillStr != "" { // Игнорируем ошибку парсинга для пустой строки (дефолт)
		log.Printf("[Storage] Ошибка парсинга last_refill ('%s') для клиента '%s': %v. Используется нулевое время.", lastRefillStr, clientID, errParse)
		lastRefillTime = time.Time{} // Используем нулевое время при ошибке парсинга
	}

	return tokensDB, lastRefillTime, true, nil
}

// CreateClientLimit добавляет нового клиента и его лимиты в БД, включая начальное состояние.
func (db *DB) CreateClientLimit(clientID string, limit config.ClientRateConfig) error {
	db.Mu.Lock()
	defer db.Mu.Unlock()

	// Устанавливаем начальное состояние: токены = емкость, время = сейчас
	initialTokens := limit.Capacity
	initialTimeStr := time.Now().Format(time.RFC3339Nano)

	query := `INSERT INTO client_rate_limits (client_id, rate, capacity, current_tokens, last_refill) VALUES (?, ?, ?, ?, ?)`
	_, err := db.Conn.Exec(query, clientID, limit.Rate, limit.Capacity, initialTokens, initialTimeStr)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return fmt.Errorf("клиент с ID '%s' уже существует", clientID)
		}
		return fmt.Errorf("ошибка добавления лимита для '%s': %w", clientID, err)
	}
	log.Printf("[Storage] Добавлен лимит для клиента '%s': Rate=%.2f, Capacity=%.2f, Tokens=%.2f", clientID, limit.Rate, limit.Capacity, initialTokens)
	return nil
}

// UpdateClientLimit обновляет настройки лимита (rate, capacity) для существующего клиента.
// Не меняет текущее состояние токенов и время.
func (db *DB) UpdateClientLimit(clientID string, limit config.ClientRateConfig) error {
	db.Mu.Lock()
	defer db.Mu.Unlock()

	query := `UPDATE client_rate_limits SET rate = ?, capacity = ? WHERE client_id = ?`
	res, err := db.Conn.Exec(query, limit.Rate, limit.Capacity, clientID)
	if err != nil {
		return fmt.Errorf("ошибка обновления лимита для '%s': %w", clientID, err)
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("ошибка получения количества обновленных строк для '%s': %w", clientID, err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("клиент с ID '%s' не найден для обновления", clientID)
	}

	log.Printf("[Storage] Обновлен лимит (rate/capacity) для клиента '%s': Rate=%.2f, Capacity=%.2f", clientID, limit.Rate, limit.Capacity)
	return nil
}

// DeleteClientLimit удаляет лимиты для указанного клиента из БД.
// Возвращает ошибку, если клиент не найден или произошла ошибка БД.
func (db *DB) DeleteClientLimit(clientID string) error {
	db.Mu.Lock()
	defer db.Mu.Unlock()

	query := `DELETE FROM client_rate_limits WHERE client_id = ?`
	res, err := db.Conn.Exec(query, clientID)
	if err != nil {
		return fmt.Errorf("ошибка удаления лимита для '%s': %w", clientID, err)
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("ошибка получения количества удаленных строк для '%s': %w", clientID, err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("клиент с ID '%s' не найден для удаления", clientID)
	}

	log.Printf("[Storage] Удален лимит для клиента '%s'", clientID)
	return nil
}

// BatchUpdateClientState обновляет состояние (tokens, last_refill) для нескольких клиентов в одной транзакции.
func (db *DB) BatchUpdateClientState(states map[string]ClientState) error {
	if len(states) == 0 {
		return nil // Нечего обновлять
	}
	db.Mu.Lock()
	defer db.Mu.Unlock()

	tx, err := db.Conn.Begin()
	if err != nil {
		return fmt.Errorf("ошибка начала транзакции для batch update: %w", err)
	}
	defer tx.Rollback() // Откат по умолчанию, если Commit не будет вызван

	stmt, err := tx.Prepare("UPDATE client_rate_limits SET current_tokens = ?, last_refill = ? WHERE client_id = ?")
	if err != nil {
		return fmt.Errorf("ошибка подготовки запроса для batch update: %w", err)
	}
	defer stmt.Close()

	updatedCount := 0
	for clientID, state := range states {
		lastRefillStr := state.LastRefill.Format(time.RFC3339Nano)
		res, err := stmt.Exec(state.Tokens, lastRefillStr, clientID)
		if err != nil {
			// Можно добавить логирование конкретной ошибки, но пока просто возвращаем общую
			log.Printf("[Storage] Ошибка обновления состояния для клиента '%s' в batch: %v", clientID, err)
			return fmt.Errorf("ошибка выполнения batch update для клиента '%s': %w", clientID, err)
		}
		// Проверяем, была ли строка действительно обновлена (на случай, если клиент был удален)
		rowsAffected, _ := res.RowsAffected()
		if rowsAffected > 0 {
			updatedCount++
		}
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("ошибка commit транзакции для batch update: %w", err)
	}

	log.Printf("[Storage] BatchUpdateClientState: Успешно обновлено состояние для %d из %d клиентов.", updatedCount, len(states))
	return nil
}

// SupportsStatePersistence возвращает true, т.к. *storage.DB поддерживает сохранение состояния.
func (db *DB) SupportsStatePersistence() bool {
	return true
}
