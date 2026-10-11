package m1

import (
	"crypto/rand"
	"errors"
	"math/big"
	"strconv"

	"cordova/server/internal/domain"
)

const propertyCanaryVersion = "arith-json-v1"

type propertyCanary struct {
	ID    string
	Nonce string
	Left  int64
	Right int64
}

type propertyCanaryAnswer struct {
	Nonce  string `json:"nonce"`
	Result int64  `json:"result"`
}

func newPropertyCanary() (propertyCanary, error) {
	id, err := domain.NewID()
	if err != nil {
		return propertyCanary{}, err
	}
	nonce, err := domain.NewID()
	if err != nil {
		return propertyCanary{}, err
	}
	const operandLimit = 1_000_000
	left, err := rand.Int(rand.Reader, big.NewInt(operandLimit))
	if err != nil {
		return propertyCanary{}, err
	}
	right, err := rand.Int(rand.Reader, big.NewInt(operandLimit))
	if err != nil {
		return propertyCanary{}, err
	}
	return propertyCanary{ID: id.String(), Nonce: nonce.String(), Left: left.Int64(), Right: right.Int64()}, nil
}

func (c propertyCanary) prompt() string {
	return "Cordova property canary arith-json-v1. Return exactly one JSON object with string field nonce and integer field result. " +
		"Set nonce to " + c.Nonce + " and result to the sum of " + strconv.FormatInt(c.Left, 10) + " and " +
		strconv.FormatInt(c.Right, 10) + ". Do not include markdown or other fields."
}

// failureCode checks response properties in memory and returns only a fixed,
// content-free code suitable for the canary event envelope.
func (c propertyCanary) failureCode(text string) string {
	const maxCanaryResponseBytes = 4096
	if len(text) == 0 || len(text) > maxCanaryResponseBytes {
		return "invalid_format"
	}
	var answer propertyCanaryAnswer
	if decodeStrictBytes([]byte(text), &answer) != nil {
		return "invalid_format"
	}
	if answer.Nonce != c.Nonce {
		return "nonce_mismatch"
	}
	if answer.Result != c.Left+c.Right {
		return "property_mismatch"
	}
	return ""
}

func (c propertyCanary) validate() error {
	if !validID(c.ID) || !validID(c.Nonce) || c.Left < 0 || c.Left >= 1_000_000 || c.Right < 0 || c.Right >= 1_000_000 {
		return errors.New("invalid property canary challenge")
	}
	return nil
}
