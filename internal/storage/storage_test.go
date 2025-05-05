package storage_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"load-balancer/internal/config"
	"load-balancer/internal/storage"

	_ "modernc.org/sqlite"
)

// setupTestDB создает временную базу данных SQLite для теста.
func setupTestDB(t *testing.T) (*storage.DB, func()) {
	t.Helper()
	// Используем временный файл вместо :memory: для большей надежности с modernc/sqlite
	tmpFile, err := os.CreateTemp(t.TempDir(), "test_*.db")
	require.NoError(t, err, "Не удалось создать временный файл БД")
	tmpPath := tmpFile.Name()
	// Важно закрыть файл, чтобы NewSQLiteDB мог его открыть/создать
	require.NoError(t, tmpFile.Close(), "Не удалось закрыть временный файл БД")

	t.Logf("Используется тестовая БД: %s", tmpPath)

	db, err := storage.NewSQLiteDB(tmpPath)
	require.NoError(t, err, "Ошибка создания тестовой БД")
	require.NotNil(t, db, "DB не должен быть nil")

	cleanup := func() {
		t.Logf("Очистка тестовой БД: %s", tmpPath)
		err := db.Close()
		// Не проверяем ошибку удаления, т.к. t.TempDir() сам очистится
		assert.NoError(t, err, "Ошибка закрытия тестовой БД")
	}

	return db, cleanup
}

// TestDBCreateGetDeleteClientLimit проверяет базовые CRUD операции.
func TestDBCreateGetDeleteClientLimit(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	clientID := "test-client-crud"
	// Используем ClientRateConfig
	limit := config.ClientRateConfig{Rate: 5, Capacity: 50}

	// 1. Create
	err := db.CreateClientLimit(clientID, limit)
	require.NoError(t, err, "Ошибка создания лимита")

	// Попытка создать дубликат
	err = db.CreateClientLimit(clientID, limit)
	require.Error(t, err, "Должна быть ошибка при создании дубликата")
	assert.Contains(t, err.Error(), "уже существует", "Текст ошибки дубликата неверен")

	// 2. Get (используем новую функцию для получения всего)
	rate, capacity, tokens, lastRefill, found, err := db.GetClientLimitAndState(clientID)
	require.NoError(t, err, "Ошибка получения лимита и состояния")
	assert.True(t, found, "Клиент должен быть найден")
	assert.Equal(t, limit.Rate, rate, "Rate не совпадает")
	assert.Equal(t, limit.Capacity, capacity, "Capacity не совпадает")
	// Проверяем начальное состояние, установленное в CreateClientLimit
	assert.Equal(t, limit.Capacity, tokens, "Начальные токены должны быть равны capacity")
	assert.WithinDuration(t, time.Now(), lastRefill, 1*time.Second, "Время последнего пополнения должно быть близко к текущему")

	// 3. Update (проверяем только rate/capacity)
	updatedLimit := config.ClientRateConfig{Rate: 20, Capacity: 200}
	err = db.UpdateClientLimit(clientID, updatedLimit)
	require.NoError(t, err, "Ошибка обновления лимита")

	// Проверяем, что обновилось только rate/capacity
	rate, capacity, tokensAfterUpdate, lastRefillAfterUpdate, found, err := db.GetClientLimitAndState(clientID)
	require.NoError(t, err, "Ошибка получения после обновления")
	assert.True(t, found, "Клиент должен быть найден после обновления")
	assert.Equal(t, updatedLimit.Rate, rate, "Rate должен обновиться")
	assert.Equal(t, updatedLimit.Capacity, capacity, "Capacity должен обновиться")
	// Состояние не должно было измениться при UpdateClientLimit
	assert.Equal(t, tokens, tokensAfterUpdate, "Токены не должны меняться при UpdateClientLimit")
	assert.Equal(t, lastRefill.UnixNano(), lastRefillAfterUpdate.UnixNano(), "Время не должно меняться при UpdateClientLimit") // Сравниваем как UnixNano

	// Попытка обновить несуществующего
	err = db.UpdateClientLimit("non-existent", updatedLimit)
	require.Error(t, err, "Должна быть ошибка при обновлении несуществующего")
	assert.Contains(t, err.Error(), "не найден для обновления", "Текст ошибки обновления неверен")

	// 4. Delete
	err = db.DeleteClientLimit(clientID)
	require.NoError(t, err, "Ошибка удаления лимита")

	// Проверяем, что удалился
	_, _, _, _, found, err = db.GetClientLimitAndState(clientID)
	require.NoError(t, err, "Ошибка получения после удаления")
	assert.False(t, found, "Клиент не должен быть найден после удаления")

	// Попытка удалить несуществующего
	err = db.DeleteClientLimit("non-existent")
	require.Error(t, err, "Должна быть ошибка при удалении несуществующего")
	assert.Contains(t, err.Error(), "не найден для удаления", "Текст ошибки удаления неверен")
}

// TestGetClientLimitConfig проверяет получение только rate и capacity.
func TestGetClientLimitConfig(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	clientID := "test-client-config"
	limit := config.ClientRateConfig{Rate: 10.5, Capacity: 100.5}

	// Добавляем клиента с начальным состоянием
	err := db.CreateClientLimit(clientID, limit)
	require.NoError(t, err, "CreateClientLimit failed")

	// Тестируем получение конфига
	rate, capacity, found, err := db.GetClientLimitConfig(clientID)
	require.NoError(t, err, "GetClientLimitConfig failed")
	assert.True(t, found, "Client should be found")
	assert.Equal(t, limit.Rate, rate, "Rate should match")
	assert.Equal(t, limit.Capacity, capacity, "Capacity should match")

	// Тестируем несуществующего клиента
	_, _, found, err = db.GetClientLimitConfig("non-existent-client")
	require.NoError(t, err, "GetClientLimitConfig for non-existent client failed")
	assert.False(t, found, "Non-existent client should not be found")
}

// TestGetClientSavedState проверяет получение только tokens и lastRefill.
func TestGetClientSavedState(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	clientID := "test-client-state"
	limit := config.ClientRateConfig{Rate: 1, Capacity: 10}
	// Добавляем клиента, его начальное состояние (tokens=10, lastRefill=now) будет записано
	err := db.CreateClientLimit(clientID, limit)
	require.NoError(t, err, "CreateClientLimit failed")

	// Получаем начальное состояние для сравнения
	_, _, initialTokens, initialLastRefill, found, err := db.GetClientLimitAndState(clientID)
	require.NoError(t, err)
	require.True(t, found)

	// Тестируем получение только сохраненного состояния
	tokens, lastRefill, found, err := db.GetClientSavedState(clientID)
	require.NoError(t, err, "GetClientSavedState failed")
	assert.True(t, found, "Client state should be found")
	assert.Equal(t, initialTokens, tokens, "Tokens should match initial state")
	// Сравниваем время с небольшой погрешностью или через UnixNano
	assert.Equal(t, initialLastRefill.UnixNano(), lastRefill.UnixNano(), "LastRefill should match initial state")

	// Тестируем несуществующего клиента
	_, _, found, err = db.GetClientSavedState("non-existent-client")
	require.NoError(t, err, "GetClientSavedState for non-existent client failed")
	assert.False(t, found, "Non-existent client state should not be found")
}

// TestGetClientLimitAndState продублирован в TestDBCreateGetDeleteClientLimit, но оставим для явности.
func TestGetClientLimitAndState(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	clientID := "test-client-full"
	limit := config.ClientRateConfig{Rate: 7, Capacity: 77}
	err := db.CreateClientLimit(clientID, limit)
	require.NoError(t, err)

	rate, capacity, tokens, lastRefill, found, err := db.GetClientLimitAndState(clientID)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, limit.Rate, rate)
	assert.Equal(t, limit.Capacity, capacity)
	assert.Equal(t, limit.Capacity, tokens) // Начальное состояние
	assert.NotZero(t, lastRefill)           // Должно быть установлено

	_, _, _, _, found, err = db.GetClientLimitAndState("non-existent")
	require.NoError(t, err)
	assert.False(t, found)
}

// TestBatchUpdateClientState проверяет массовое обновление состояния.
func TestBatchUpdateClientState(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	client1 := "batch-client-1"
	client2 := "batch-client-2"
	client3NonExistent := "batch-client-3-non-existent"

	limit1 := config.ClientRateConfig{Rate: 1, Capacity: 10}
	limit2 := config.ClientRateConfig{Rate: 2, Capacity: 20}

	err := db.CreateClientLimit(client1, limit1)
	require.NoError(t, err)
	err = db.CreateClientLimit(client2, limit2)
	require.NoError(t, err)

	// Новое состояние для обновления
	now := time.Now().Truncate(time.Millisecond) // Округляем для надежного сравнения
	state1 := storage.ClientState{Tokens: 5.5, LastRefill: now.Add(-1 * time.Minute)}
	state2 := storage.ClientState{Tokens: 15.5, LastRefill: now.Add(-2 * time.Minute)}
	state3 := storage.ClientState{Tokens: 99, LastRefill: now} // Для несуществующего клиента

	statesToUpdate := map[string]storage.ClientState{
		client1:            state1,
		client2:            state2,
		client3NonExistent: state3, // Попытка обновить несуществующего
	}

	err = db.BatchUpdateClientState(statesToUpdate)
	require.NoError(t, err, "BatchUpdateClientState failed")

	// Проверяем состояние client1
	tokens1, lastRefill1, found1, err1 := db.GetClientSavedState(client1)
	require.NoError(t, err1)
	require.True(t, found1)
	assert.Equal(t, state1.Tokens, tokens1, "Client1 tokens mismatch")
	assert.Equal(t, state1.LastRefill.UnixNano(), lastRefill1.UnixNano(), "Client1 lastRefill mismatch")

	// Проверяем состояние client2
	tokens2, lastRefill2, found2, err2 := db.GetClientSavedState(client2)
	require.NoError(t, err2)
	require.True(t, found2)
	assert.Equal(t, state2.Tokens, tokens2, "Client2 tokens mismatch")
	assert.Equal(t, state2.LastRefill.UnixNano(), lastRefill2.UnixNano(), "Client2 lastRefill mismatch")

	// Проверяем, что client3 не был создан
	_, _, found3, err3 := db.GetClientSavedState(client3NonExistent)
	require.NoError(t, err3)
	assert.False(t, found3, "Non-existent client should not have been created")

	// Проверяем rate/capacity - они не должны были измениться
	rate1, capacity1, found1c, err1c := db.GetClientLimitConfig(client1)
	require.NoError(t, err1c)
	require.True(t, found1c)
	assert.Equal(t, limit1.Rate, rate1)
	assert.Equal(t, limit1.Capacity, capacity1)
}
