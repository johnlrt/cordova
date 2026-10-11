package m1

import (
	"strings"
	"testing"
)

func TestNewPropertyCanaryCreatesFreshBoundedChallenge(t *testing.T) {
	challenge, err := newPropertyCanary()
	if err != nil {
		t.Fatal(err)
	}
	if err := challenge.validate(); err != nil {
		t.Fatal(err)
	}
	if challenge.ID == challenge.Nonce || challenge.Left < 0 || challenge.Left >= 1_000_000 ||
		challenge.Right < 0 || challenge.Right >= 1_000_000 {
		t.Fatalf("generated canary challenge is invalid: %+v", challenge)
	}
}

func TestPropertyCanaryChecksVersionedPropertiesWithoutExactTextMatch(t *testing.T) {
	canary := propertyCanary{ID: "00000000-0000-4000-8000-000000000010", Nonce: "00000000-0000-4000-8000-000000000011", Left: 17, Right: 25}
	if err := canary.validate(); err != nil {
		t.Fatal(err)
	}
	prompt := canary.prompt()
	if !strings.Contains(prompt, propertyCanaryVersion) || !strings.Contains(prompt, canary.Nonce) ||
		!strings.Contains(prompt, "17") || !strings.Contains(prompt, "25") {
		t.Fatalf("canary prompt omitted versioned challenge inputs: %q", prompt)
	}
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "valid JSON with harmless whitespace", text: "{\n  \"result\": 42, \"nonce\": \"" + canary.Nonce + "\"\n}"},
		{name: "wrong nonce", text: `{"nonce":"00000000-0000-4000-8000-000000000012","result":42}`, want: "nonce_mismatch"},
		{name: "wrong answer", text: `{"nonce":"00000000-0000-4000-8000-000000000011","result":41}`, want: "property_mismatch"},
		{name: "fractional answer", text: `{"nonce":"00000000-0000-4000-8000-000000000011","result":42.0}`, want: "invalid_format"},
		{name: "duplicate property", text: `{"nonce":"00000000-0000-4000-8000-000000000011","result":42,"result":42}`, want: "invalid_format"},
		{name: "unexpected field", text: `{"nonce":"00000000-0000-4000-8000-000000000011","result":42,"explanation":"secret"}`, want: "invalid_format"},
		{name: "markdown wrapper", text: "```json\n{\"nonce\":\"" + canary.Nonce + "\",\"result\":42}\n```", want: "invalid_format"},
		{name: "oversize response", text: `{"nonce":"` + canary.Nonce + `","result":42,"padding":"` + strings.Repeat("x", 4096) + `"}`, want: "invalid_format"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := canary.failureCode(test.text); got != test.want {
				t.Fatalf("failure code = %q, want %q", got, test.want)
			}
		})
	}
}
