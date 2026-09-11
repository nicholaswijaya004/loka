package booking

import (
	"errors"
)

var ErrSoldOut = errors.New("inventory unit sold out")
var ErrMinBook = errors.New("booking quantity is less than minimum book")
var ErrInvalidQty = errors.New("booking quantity must be greater than zero")
var ErrKeyReused = errors.New("idempotency key already used")
var ErrRequestInFlight = errors.New("request in flight")
var ErrCorruptIdempotencyRecord = errors.New("idempotency record is missing its booking id")
var ErrUnexpectedIdempotencyState = errors.New("unexpected idempotency key state")
