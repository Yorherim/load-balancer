// Package response_test содержит тесты для пакета response.
package response_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"load-balancer/internal/response" // Импортируем тестируемый пакет
	// TODO: Как протестировать логирование? Пока опускаем.
)

// TestRespondWithError проверяет функцию RespondWithError.
func TestRespondWithError(t *testing.T) {
	w := httptest.NewRecorder()
	code := http.StatusNotFound
	message := "Resource not found"

	response.RespondWithError(w, code, message)

	// Проверяем статус код
	assert.Equal(t, code, w.Code, "Неверный статус код")

	// Проверяем заголовок Content-Type
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"), "Неверный Content-Type")

	// Проверяем тело ответа
	var errResp response.ErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &errResp)
	require.NoError(t, err, "Не удалось распарсить JSON ответа")
	assert.Equal(t, code, errResp.Code, "Неверный код в теле ответа JSON")
	assert.Equal(t, message, errResp.Message, "Неверное сообщение об ошибке в JSON")
}

// TestRespondWithJSON проверяет функцию RespondWithJSON.
func TestRespondWithJSON(t *testing.T) {
	w := httptest.NewRecorder()
	code := http.StatusOK
	type Payload struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}
	payload := Payload{Name: "test", Value: 123}

	response.RespondWithJSON(w, code, payload)

	// Проверяем статус код
	assert.Equal(t, code, w.Code, "Неверный статус код")

	// Проверяем заголовок Content-Type
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"), "Неверный Content-Type")

	// Проверяем тело ответа
	var respPayload Payload
	err := json.Unmarshal(w.Body.Bytes(), &respPayload)
	require.NoError(t, err, "Не удалось распарсить JSON ответа")
	assert.Equal(t, payload, respPayload, "Неверные данные в JSON ответе")
}

// TestRespondWithJSON_MarshalError проверяет обработку ошибки маршалинга.
func TestRespondWithJSON_MarshalError(t *testing.T) {
	w := httptest.NewRecorder()
	code := http.StatusOK
	// Невалидные данные для JSON (канал)
	invalidPayload := make(chan int)

	response.RespondWithJSON(w, code, invalidPayload)

	// Ожидаем Internal Server Error
	assert.Equal(t, http.StatusInternalServerError, w.Code, "Ожидался статус 500 при ошибке маршалинга")

	// Проверяем тело ответа (должно быть текстовое "Internal Server Error")
	assert.Equal(t, "Internal Server Error", w.Body.String(), "Неверное сообщение при ошибке маршалинга")
}
