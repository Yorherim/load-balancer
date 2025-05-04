// Package response содержит вспомогательные функции для формирования HTTP-ответов.
package response

import (
	"encoding/json"
	"log"
	"net/http"
)

// ErrorResponse представляет стандартный формат ответа для ошибок API.
// Изменено поле Error на Message и добавлено поле Code.
type ErrorResponse struct {
	Code    int    `json:"code"`    // HTTP статус код
	Message string `json:"message"` // Сообщение об ошибке
}

// RespondWithError отправляет JSON-ответ с ошибкой.
func RespondWithError(w http.ResponseWriter, statusCode int, message string) {
	// Логируем ошибку перед отправкой ответа
	log.Printf("[Error] Status: %d, Message: %s", statusCode, message)
	responsePayload := ErrorResponse{
		Code:    statusCode,
		Message: message,
	}
	RespondWithJSON(w, statusCode, responsePayload)
}

// RespondWithJSON отправляет JSON-ответ с указанным статус-кодом и телом.
// Используется как для успешных ответов, так и внутри RespondWithError.
func RespondWithJSON(w http.ResponseWriter, statusCode int, payload interface{}) {
	response, err := json.Marshal(payload)
	if err != nil {
		// В случае ошибки маршалинга, отправляем текстовую ошибку 500
		log.Printf("[Error] Ошибка маршалинга JSON-ответа: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Internal Server Error")) // Пишем как []byte
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, err = w.Write(response)
	if err != nil {
		// Логируем ошибку записи ответа, но статус уже отправлен
		log.Printf("[Error] Ошибка записи JSON-ответа клиенту: %v", err)
	}
}
