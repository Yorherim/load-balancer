package storage

import "errors"

var (
	ErrClientNotFound      = errors.New("клиент не найден")
	ErrClientAlreadyExists = errors.New("клиент уже существует")
)
