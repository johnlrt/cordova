// Package domain contains validated value types shared by the Go services.
package domain

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxPiconero int64 = 9_000_000_000_000_000_000

var (
	ErrInvalidID          = errors.New("invalid UUIDv4")
	ErrInvalidUsername    = errors.New("invalid username")
	ErrInvalidPiconero    = errors.New("invalid piconero amount")
	ErrPiconeroOverflow   = errors.New("piconero amount overflow")
	ErrInsufficientAmount = errors.New("insufficient piconero amount")
)

// ID is a canonical lowercase UUIDv4. Its zero value is invalid.
type ID struct{ value string }

func NewID() (ID, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return ID{}, err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return ParseID(formatID(value[:]))
}

func ParseID(value string) (ID, error) {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' {
		return ID{}, ErrInvalidID
	}
	compact := strings.ReplaceAll(value, "-", "")
	decoded, err := hex.DecodeString(compact)
	if err != nil || len(decoded) != 16 {
		return ID{}, ErrInvalidID
	}
	variant := decoded[8] >> 4
	if variant < 8 || variant > 11 {
		return ID{}, ErrInvalidID
	}
	return ID{value: strings.ToLower(value)}, nil
}

func formatID(value []byte) string {
	encoded := hex.EncodeToString(value)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func (id ID) String() string { return id.value }

func (id ID) Valid() bool {
	parsed, err := ParseID(id.value)
	return err == nil && parsed.value == id.value
}

func (id ID) MarshalJSON() ([]byte, error) {
	if !id.Valid() {
		return nil, ErrInvalidID
	}
	return json.Marshal(id.value)
}

func (id *ID) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return ErrInvalidID
	}
	parsed, err := ParseID(value)
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// Username stores the trimmed lowercase form used for account lookup.
type Username struct{ value string }

func NormalizeUsername(value string) (Username, error) {
	if !utf8.ValidString(value) {
		return Username{}, ErrInvalidUsername
	}
	normalized := strings.ToLower(strings.TrimSpace(value))
	runeCount := utf8.RuneCountInString(normalized)
	if runeCount < 3 || runeCount > 64 {
		return Username{}, ErrInvalidUsername
	}
	for _, r := range normalized {
		if unicode.IsControl(r) {
			return Username{}, ErrInvalidUsername
		}
	}
	return Username{value: normalized}, nil
}

func (u Username) String() string { return u.value }

func (u Username) Valid() bool {
	parsed, err := NormalizeUsername(u.value)
	return err == nil && parsed.value == u.value
}

func (u Username) MarshalJSON() ([]byte, error) {
	if !u.Valid() {
		return nil, ErrInvalidUsername
	}
	return json.Marshal(u.value)
}

func (u *Username) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return ErrInvalidUsername
	}
	parsed, err := NormalizeUsername(value)
	if err != nil {
		return err
	}
	*u = parsed
	return nil
}

// Piconero is a nonnegative amount serialized as a canonical decimal string.
// Its zero value represents zero piconero.
type Piconero struct{ value int64 }

func NewPiconero(value int64) (Piconero, error) {
	if value < 0 || value > MaxPiconero {
		return Piconero{}, ErrInvalidPiconero
	}
	return Piconero{value: value}, nil
}

func ParsePiconero(value string) (Piconero, error) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return Piconero{}, ErrInvalidPiconero
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return Piconero{}, ErrInvalidPiconero
		}
	}
	amount, err := strconv.ParseInt(value, 10, 64)
	if err != nil || amount > MaxPiconero {
		return Piconero{}, ErrInvalidPiconero
	}
	return Piconero{value: amount}, nil
}

func (p Piconero) Int64() int64   { return p.value }
func (p Piconero) String() string { return strconv.FormatInt(p.value, 10) }

func (p Piconero) valid() bool { return p.value >= 0 && p.value <= MaxPiconero }

func (p Piconero) Add(other Piconero) (Piconero, error) {
	if !p.valid() || !other.valid() {
		return Piconero{}, ErrInvalidPiconero
	}
	if p.value > MaxPiconero-other.value {
		return Piconero{}, ErrPiconeroOverflow
	}
	return Piconero{value: p.value + other.value}, nil
}

func (p Piconero) Sub(other Piconero) (Piconero, error) {
	if !p.valid() || !other.valid() {
		return Piconero{}, ErrInvalidPiconero
	}
	if p.value < other.value {
		return Piconero{}, ErrInsufficientAmount
	}
	return Piconero{value: p.value - other.value}, nil
}

// CostForTokens applies the C5 ceiling rule to a rate quoted per million tokens.
func CostForTokens(tokens int64, ratePerMillion Piconero) (Piconero, error) {
	if tokens < 0 || !ratePerMillion.valid() {
		return Piconero{}, ErrInvalidPiconero
	}
	product := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(ratePerMillion.value))
	denominator := big.NewInt(1_000_000)
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(product, denominator, remainder)
	if remainder.Sign() > 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() || quotient.Int64() > MaxPiconero {
		return Piconero{}, ErrPiconeroOverflow
	}
	return Piconero{value: quotient.Int64()}, nil
}

// OwnerShare applies the C5 floor rule to a basis-point share.
func OwnerShare(cost Piconero, shareBasisPoints uint32) (Piconero, error) {
	if !cost.valid() || shareBasisPoints > 10_000 {
		return Piconero{}, ErrInvalidPiconero
	}
	product := new(big.Int).Mul(big.NewInt(cost.value), new(big.Int).SetUint64(uint64(shareBasisPoints)))
	product.Quo(product, big.NewInt(10_000))
	return Piconero{value: product.Int64()}, nil
}

func (p Piconero) MarshalJSON() ([]byte, error) {
	if !p.valid() {
		return nil, ErrInvalidPiconero
	}
	return json.Marshal(p.String())
}

func (p *Piconero) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return ErrInvalidPiconero
	}
	parsed, err := ParsePiconero(value)
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}

type RequestStatus string

const (
	RequestAccepted   RequestStatus = "accepted"
	RequestRunning    RequestStatus = "running"
	RequestSettled    RequestStatus = "settled"
	RequestRefunded   RequestStatus = "refunded"
	RequestCancelled  RequestStatus = "cancelled"
	RequestRecovering RequestStatus = "recovering"
)

func (s RequestStatus) Valid() bool {
	switch s {
	case RequestAccepted, RequestRunning, RequestSettled, RequestRefunded, RequestCancelled, RequestRecovering:
		return true
	default:
		return false
	}
}

type AttemptStatus string

const (
	AttemptReserved    AttemptStatus = "reserved"
	AttemptDispatching AttemptStatus = "dispatching"
	AttemptRunning     AttemptStatus = "running"
	AttemptSettling    AttemptStatus = "settling"
	AttemptSettled     AttemptStatus = "settled"
	AttemptFailed      AttemptStatus = "failed"
	AttemptRefunded    AttemptStatus = "refunded"
	AttemptCancelled   AttemptStatus = "cancelled"
	AttemptRecovering  AttemptStatus = "recovering"
)

func (s AttemptStatus) Valid() bool {
	switch s {
	case AttemptReserved, AttemptDispatching, AttemptRunning, AttemptSettling, AttemptSettled, AttemptFailed, AttemptRefunded, AttemptCancelled, AttemptRecovering:
		return true
	default:
		return false
	}
}

type LeaseStatus string

const (
	LeaseClaimed  LeaseStatus = "claimed"
	LeaseActive   LeaseStatus = "active"
	LeaseReleased LeaseStatus = "released"
	LeaseExpired  LeaseStatus = "expired"
)

func (s LeaseStatus) Valid() bool {
	switch s {
	case LeaseClaimed, LeaseActive, LeaseReleased, LeaseExpired:
		return true
	default:
		return false
	}
}
