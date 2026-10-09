package m1

import (
	"strings"
	"testing"
)

func TestPasswordHashBoundaries(t *testing.T) {
	for _, value := range []string{"short", strings.Repeat("a", 11), strings.Repeat("a", 129)} {
		if validPassword(value) {
			t.Fatalf("accepted password with %d characters", len([]rune(value)))
		}
	}
	password := "correct horse battery staple"
	hash, err := hashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("unexpected password hash parameters: %q", hash[:min(32, len(hash))])
	}
	if !verifyPassword(hash, password) {
		t.Fatal("correct password did not verify")
	}
	if verifyPassword(hash, password+"!") {
		t.Fatal("incorrect password verified")
	}
	if verifyPassword("argon2id$v=19$m=999999999,t=999,p=99$bad$bad", password) {
		t.Fatal("untrusted Argon2 parameters were accepted")
	}
}

func TestCSRFTokenIsBoundToCookie(t *testing.T) {
	store := &Store{cfg: Config{Pepper: []byte(strings.Repeat("p", 32))}}
	cookie := "cv1_" + strings.Repeat("a", 43)
	token := store.csrfForCookie(cookie)
	session := sessionRecord{CSRFHash: store.secretVerifier(token)}
	if !store.ValidateCSRF(session, token) {
		t.Fatal("derived CSRF token did not validate")
	}
	if store.ValidateCSRF(session, store.csrfForCookie("cv1_"+strings.Repeat("b", 43))) {
		t.Fatal("CSRF token was not bound to the session cookie")
	}
}

func TestStrictJSONRejectsDuplicateAndUnknownFields(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}
	for _, data := range []string{`{"name":"a","name":"b"}`, `{"name":"a","extra":true}`, `{"name":"a"} {}`} {
		var got payload
		if decodeStrictBytes([]byte(data), &got) == nil {
			t.Fatalf("accepted malformed frame %s", data)
		}
	}
	var got payload
	if err := decodeStrictBytes([]byte(`{"name":"valid"}`), &got); err != nil || got.Name != "valid" {
		t.Fatalf("valid JSON failed: %#v %v", got, err)
	}
}
