package service

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// encryptPanelSecret keeps optional integration credentials (for example DNS
// provider tokens and OIDC client secrets) unreadable at rest. The purpose is
// authenticated data, so ciphertext for one feature cannot be replayed into a
// different feature's column.
func encryptPanelSecret(purpose, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	key, err := panelSecretEncryptionKey(purpose)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), []byte(purpose))
	return base64.RawStdEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func decryptPanelSecret(purpose, ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	payload, err := base64.RawStdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	key, err := panelSecretEncryptionKey(purpose)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(payload) < gcm.NonceSize() {
		return "", errors.New("invalid encrypted panel secret")
	}
	plain, err := gcm.Open(nil, payload[:gcm.NonceSize()], payload[gcm.NonceSize():], []byte(purpose))
	if err != nil {
		return "", fmt.Errorf("decrypt panel secret: %w", err)
	}
	return string(plain), nil
}

func panelSecretEncryptionKey(purpose string) ([]byte, error) {
	secret, err := (&SettingService{}).GetSecret()
	if err != nil || len(secret) == 0 {
		return nil, errors.New("panel secret is unavailable")
	}
	sum := sha256.Sum256(append(append([]byte("PHOENIX integration secret v1\x00"), []byte(purpose)...), append([]byte{0}, secret...)...))
	return sum[:], nil
}
