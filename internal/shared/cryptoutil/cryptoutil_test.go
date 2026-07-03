package cryptoutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestMasterKeyCreateAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "master.key")
	k1, err := LoadOrCreateMasterKey(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(k1) != 32 {
		t.Fatalf("key len = %d", len(k1))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, want 0600", info.Mode().Perm())
	}
	k2, err := LoadOrCreateMasterKey(path)
	if err != nil || !bytes.Equal(k1, k2) {
		t.Fatalf("reload mismatch: %v", err)
	}
}

func TestSealOpenRoundtrip(t *testing.T) {
	key, _ := LoadOrCreateMasterKey(filepath.Join(t.TempDir(), "k"))
	plain := []byte("sk-relay-secret-key-中文")
	ct, err := Seal(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, plain) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := Open(key, ct)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("open: %q err=%v", got, err)
	}
	// Tamper detection.
	ct[len(ct)-1] ^= 0xFF
	if _, err := Open(key, ct); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	// Wrong key.
	key2 := append([]byte(nil), key...)
	key2[0] ^= 1
	ct2, _ := Seal(key, plain)
	if _, err := Open(key2, ct2); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestTokens(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewToken()
	if len(a) != 64 || a == b {
		t.Fatalf("token gen broken: %q %q", a, b)
	}
	if HashToken(a) == HashToken(b) || len(HashToken(a)) != 64 {
		t.Fatal("hash broken")
	}
}

func TestArgon2id(t *testing.T) {
	h, err := Argon2idHash("正确密码")
	if err != nil {
		t.Fatal(err)
	}
	if !Argon2idVerify("正确密码", h) {
		t.Fatal("correct password rejected")
	}
	if Argon2idVerify("错误密码", h) {
		t.Fatal("wrong password accepted")
	}
	if Argon2idVerify("x", "$argon2id$garbage") {
		t.Fatal("malformed hash accepted")
	}
	h2, _ := Argon2idHash("正确密码")
	if h == h2 {
		t.Fatal("salt reuse: identical hashes")
	}
}
