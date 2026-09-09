package booking

import (
	"errors"
)

var ErrSoldOut = errors.New("inventory unit sold out")
var ErrMinBook = errors.New("booking quantity is less than minimum book")
var ErrInvalidQty = errors.New("booking quantity must be greater than zero")
