// Package response содержит вспомогательные функции для формирования HTTP-ответов.
package response

import (
	"encoding/json"
	"log"
	"net/http"
)

// ErrorResponse структура для возврата ошибок в формате JSON.
type ErrorResponse struct {
	Error string `json:"error"`
}

// RespondWithError отправляет JSON-ответ с ошибкой.
func RespondWithError(w http.ResponseWriter, code int, message string) {
	log.Printf("[Error] Status: %d, Message: %s", code, message)
	RespondWithJSON(w, code, ErrorResponse{Error: message})
}

// RespondWithJSON отправляет JSON-ответ с указанным кодом и данными.
func RespondWithJSON(w http.ResponseWriter, code int, payload interface{}) {
	response, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[Response Error] Ошибка маршалинга JSON: %v", err)
		// Отправляем базовую ошибку сервера, если не можем сформировать JSON
		w.Header().Set("Content-Type", "application/json") // Все равно пытаемся установить заголовок
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"Internal Server Error"}`)) // Захардкоженный JSON
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, err = w.Write(response)
	if err != nil {
		log.Printf("[Response Error] Ошибка записи JSON ответа: %v", err)
	}
}
