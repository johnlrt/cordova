package domain

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestIDParsingAndGeneration(t *testing.T) {
	parsed, err := ParseID("4E345523-1A8C-4F4B-9D48-0593CF3E7ECA")
	if err != nil || parsed.String() != "4e345523-1a8c-4f4b-9d48-0593cf3e7eca" || !parsed.Valid() {
		t.Fatalf("unexpected parsed UUID: %q, %v", parsed.String(), err)
	}
	for _, invalid := range []string{
		"00000000-0000-0000-8000-000000000001",
		"00000000-0000-4000-7000-000000000001",
		"00000000000040008000000000000001",
	} {
		if _, err := ParseID(invalid); !errors.Is(err, ErrInvalidID) {
			t.Errorf("ParseID(%q) error = %v", invalid, err)
		}
	}
	generated, err := NewID()
	if err != nil || !generated.Valid() {
		t.Fatalf("generated ID is invalid: %q, %v", generated.String(), err)
	}
	encoded, err := json.Marshal(generated)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ID
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != generated {
		t.Fatalf("ID JSON round-trip failed: %s, %v", encoded, err)
	}
	if err := json.Unmarshal([]byte(`"00000000-0000-0000-8000-000000000001"`), &decoded); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("invalid UUID JSON accepted: %v", err)
	}
}

func TestUsernameNormalization(t *testing.T) {
	username, err := NormalizeUsername("  AlIcE  ")
	if err != nil || username.String() != "alice" || !username.Valid() {
		t.Fatalf("username normalization failed: %q, %v", username.String(), err)
	}
	unicodeName, err := NormalizeUsername("  Üser  ")
	if err != nil || unicodeName.String() != "üser" {
		t.Fatalf("unicode username normalization failed: %q, %v", unicodeName.String(), err)
	}
	for _, invalid := range []string{"ab", string(make([]byte, 65)), "abc\u0000def", string([]byte{0xff, 0xfe, 0xfd})} {
		if _, err := NormalizeUsername(invalid); !errors.Is(err, ErrInvalidUsername) {
			t.Errorf("NormalizeUsername(%q) error = %v", invalid, err)
		}
	}
	encoded, err := json.Marshal(username)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Username
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != username {
		t.Fatalf("username JSON round-trip failed: %s, %v", encoded, err)
	}
}

func TestPiconeroCanonicalJSONAndBounds(t *testing.T) {
	for _, valid := range []string{"0", "9007199254740993", "9000000000000000000"} {
		amount, err := ParsePiconero(valid)
		if err != nil || amount.String() != valid {
			t.Errorf("ParsePiconero(%q) = %q, %v", valid, amount.String(), err)
			continue
		}
		encoded, err := json.Marshal(amount)
		if err != nil || string(encoded) != `"`+valid+`"` {
			t.Errorf("Marshal(%q) = %s, %v", valid, encoded, err)
		}
		var decoded Piconero
		if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != amount {
			t.Errorf("JSON round-trip for %q failed: %v", valid, err)
		}
	}
	for _, invalid := range []string{"", "00", "01", "+1", "-1", "1e3", " 1", "9000000000000000001", "9223372036854775808"} {
		if _, err := ParsePiconero(invalid); !errors.Is(err, ErrInvalidPiconero) {
			t.Errorf("ParsePiconero(%q) error = %v", invalid, err)
		}
	}
	for _, invalidJSON := range []string{"1", `"01"`, `"-1"`, `null`, `"9000000000000000001"`} {
		var amount Piconero
		if err := json.Unmarshal([]byte(invalidJSON), &amount); !errors.Is(err, ErrInvalidPiconero) {
			t.Errorf("Unmarshal(%s) error = %v", invalidJSON, err)
		}
	}
	if _, err := NewPiconero(math.MaxInt64); !errors.Is(err, ErrInvalidPiconero) {
		t.Fatalf("value beyond C1 limit accepted: %v", err)
	}
}

func TestPiconeroCheckedArithmetic(t *testing.T) {
	max, _ := NewPiconero(MaxPiconero)
	one, _ := NewPiconero(1)
	if _, err := max.Add(one); !errors.Is(err, ErrPiconeroOverflow) {
		t.Fatalf("addition overflow error = %v", err)
	}
	if _, err := one.Sub(max); !errors.Is(err, ErrInsufficientAmount) {
		t.Fatalf("subtraction underflow error = %v", err)
	}
	three, _ := NewPiconero(3)
	if amount, err := three.Sub(three); err != nil || amount.Int64() != 0 {
		t.Fatalf("exact subtraction = %d, %v", amount.Int64(), err)
	}
}

func TestContractMoneyRounding(t *testing.T) {
	rate, _ := NewPiconero(10)
	for _, tc := range []struct {
		tokens int64
		want   int64
	}{{0, 0}, {1, 1}, {100_000, 1}, {1_000_000, 10}, {9_007_199_254_740_993, 90_071_992_548}} {
		got, err := CostForTokens(tc.tokens, rate)
		if err != nil || got.Int64() != tc.want {
			t.Errorf("CostForTokens(%d) = %d, %v; want %d", tc.tokens, got.Int64(), err, tc.want)
		}
	}
	if _, err := CostForTokens(-1, rate); !errors.Is(err, ErrInvalidPiconero) {
		t.Fatalf("negative token count error = %v", err)
	}
	maxRate, _ := NewPiconero(MaxPiconero)
	if _, err := CostForTokens(1_000_001, maxRate); !errors.Is(err, ErrPiconeroOverflow) {
		t.Fatalf("cost overflow error = %v", err)
	}
	actual, _ := NewPiconero(3)
	share, err := OwnerShare(actual, 8_000)
	if err != nil || share.Int64() != 2 {
		t.Fatalf("owner share must round down: %d, %v", share.Int64(), err)
	}
	if _, err := OwnerShare(actual, 10_001); !errors.Is(err, ErrInvalidPiconero) {
		t.Fatalf("out of range share error = %v", err)
	}
}

func TestLifecycleEnumValidation(t *testing.T) {
	if !RequestAccepted.Valid() || !RequestRecovering.Valid() || RequestStatus("complete").Valid() {
		t.Fatal("request status validation does not match C1")
	}
	if !AttemptReserved.Valid() || !AttemptFailed.Valid() || AttemptStatus("queued").Valid() {
		t.Fatal("attempt status validation does not match C5")
	}
	if !LeaseClaimed.Valid() || !LeaseReleased.Valid() || LeaseStatus("busy").Valid() {
		t.Fatal("lease status validation does not match server lease lifecycle")
	}
}
