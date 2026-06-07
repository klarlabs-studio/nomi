package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// fileStore is the fallback Store used when the OS keyring is unavailable.
// Layout:
//
//	<dataDir>/secret.key   — 32 bytes of OS-random, 0600, never rotated.
//	<dataDir>/secrets.vault — JSON {key -> base64(nonce||ciphertext||tag)},
//	                          each value encrypted with AES-256-GCM using
//	                          the contents of secret.key as the key.
//
// This is weaker than a platform keychain (anyone who can read the user's
// home directory can decrypt the vault) but strictly better than the prior
// "plaintext column in SQLite" state, and comparable in threat model to how
// .env / .pgpass are typically stored.
type fileStore struct {
	mu        sync.Mutex
	keyPath   string
	vaultPath string
	cachedKey []byte
}

const (
	fileStoreKeyName   = "secret.key"
	fileStoreVaultName = "secrets.vault"
	fileStoreKeyLen    = 32 // AES-256
)

func newFileStore(dataDir string) (*fileStore, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create data dir: %w", err)
	}
	s := &fileStore{
		keyPath:   filepath.Join(dataDir, fileStoreKeyName),
		vaultPath: filepath.Join(dataDir, fileStoreVaultName),
	}
	if _, err := s.loadOrCreateKey(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *fileStore) loadOrCreateKey() ([]byte, error) {
	if s.cachedKey != nil {
		return s.cachedKey, nil
	}
	if data, err := os.ReadFile(s.keyPath); err == nil {
		if len(data) != fileStoreKeyLen {
			return nil, fmt.Errorf("%s is corrupted (wrong length)", s.keyPath)
		}
		s.cachedKey = data
		return data, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("failed to read secret key: %w", err)
	}

	key := make([]byte, fileStoreKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("failed to generate secret key: %w", err)
	}
	if err := os.WriteFile(s.keyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("failed to persist secret key: %w", err)
	}
	s.cachedKey = key
	return key, nil
}

func (s *fileStore) readVault() (map[string]string, error) {
	data, err := os.ReadFile(s.vaultPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("failed to read vault: %w", err)
	}
	var v map[string]string
	if len(data) == 0 {
		return map[string]string{}, nil
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("vault JSON corrupted: %w", err)
	}
	return v, nil
}

func (s *fileStore) writeVault(v map[string]string) error {
	buf, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("failed to marshal vault: %w", err)
	}
	// Write to a temp file + rename to avoid torn writes.
	tmp, err := os.CreateTemp(filepath.Dir(s.vaultPath), "vault-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create vault tmp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to write vault tmp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to chmod vault tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to close vault tmp: %w", err)
	}
	if err := os.Rename(tmpPath, s.vaultPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to rename vault: %w", err)
	}
	return nil
}

func (s *fileStore) aead() (cipher.AEAD, error) {
	key, err := s.loadOrCreateKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to construct AES cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func (s *fileStore) encrypt(plaintext string) (string, error) {
	gcm, err := s.aead()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (s *fileStore) decrypt(ciphertext string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("vault entry not valid base64: %w", err)
	}
	gcm, err := s.aead()
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("vault entry too short")
	}
	nonce := raw[:gcm.NonceSize()]
	body := raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return "", fmt.Errorf("vault entry decrypt failed (key rotated or file tampered): %w", err)
	}
	return string(pt), nil
}

func (s *fileStore) Put(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	vault, err := s.readVault()
	if err != nil {
		return err
	}
	sealed, err := s.encrypt(value)
	if err != nil {
		return err
	}
	vault[key] = sealed
	return s.writeVault(vault)
}

func (s *fileStore) Get(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vault, err := s.readVault()
	if err != nil {
		return "", err
	}
	ct, ok := vault[key]
	if !ok {
		return "", ErrNotFound
	}
	return s.decrypt(ct)
}

func (s *fileStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	vault, err := s.readVault()
	if err != nil {
		return err
	}
	if _, ok := vault[key]; !ok {
		return nil
	}
	delete(vault, key)
	return s.writeVault(vault)
}
