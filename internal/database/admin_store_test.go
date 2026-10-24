package database

import (
	"encoding/base64"
	"strings"
	"testing"
)

func testKey(b byte) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string(b), 32)))
}

func TestNewAdminPostgresStoreRejectsBadKey(t *testing.T) {
	for _, key := range []string{"", "not-base64!", base64.StdEncoding.EncodeToString([]byte("too short"))} {
		if _, err := NewAdminPostgresStore(nil, key); err == nil {
			t.Errorf("key %q should be rejected", key)
		}
	}
}

func TestPasswordEncryptionRoundTrip(t *testing.T) {
	store, err := NewAdminPostgresStore(nil, testKey('a'))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := store.encryptPassword("s3cret p@ss")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encrypted), "s3cret") {
		t.Fatal("ciphertext contains plaintext")
	}
	again, _ := store.encryptPassword("s3cret p@ss")
	if string(again) == string(encrypted) {
		t.Fatal("nonce reuse: identical ciphertexts")
	}
	got, err := store.DecryptPassword(encrypted)
	if err != nil || got != "s3cret p@ss" {
		t.Fatalf("DecryptPassword = %q, %v", got, err)
	}
}

func TestDecryptPasswordRejectsTamperingAndWrongKey(t *testing.T) {
	store, _ := NewAdminPostgresStore(nil, testKey('a'))
	other, _ := NewAdminPostgresStore(nil, testKey('b'))
	encrypted, _ := store.encryptPassword("s3cret")

	tampered := append([]byte(nil), encrypted...)
	tampered[len(tampered)-1] ^= 0xff
	for name, data := range map[string][]byte{"tampered": tampered, "truncated": encrypted[:4], "empty": nil} {
		if _, err := store.DecryptPassword(data); err == nil {
			t.Errorf("%s ciphertext should be rejected", name)
		}
	}
	if _, err := other.DecryptPassword(encrypted); err == nil {
		t.Error("wrong key should be rejected")
	}
}

func TestMigrationTitle(t *testing.T) {
	cases := map[string]string{
		"\n\n  CREATE TABLE x (id int);\nINSERT ...": "CREATE TABLE x (id int);",
		"":                       "",
		strings.Repeat("a", 100): strings.Repeat("a", 77) + "...",
	}
	for in, want := range cases {
		if got := migrationTitle(in); got != want {
			t.Errorf("migrationTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
