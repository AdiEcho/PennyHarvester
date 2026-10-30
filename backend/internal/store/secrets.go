package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Secrets in the system settings row are encrypted at rest with AES-256-GCM.
// The key lives next to the database in a 0600 file (or is generated in memory
// for an in-memory database), so a copied database without its key does not
// expose wallet credentials. Existing plaintext rows are transparently
// re-encrypted on load.
const (
	secretPrefix = "enc:v1:"
	secretKeyLen = 32
)

type secretBox struct {
	aead    cipher.AEAD
	keyPath string
}

func newSecretBox(databasePath string) (*secretBox, error) {
	if databasePath == ":memory:" || strings.HasPrefix(databasePath, "file::memory:") {
		key := make([]byte, secretKeyLen)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		return boxFromKey(key, "")
	}
	keyPath := databasePath + ".key"
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("读取数据库密钥文件 %s: %w", keyPath, err)
		}
		key := make([]byte, secretKeyLen)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		encoded := base64.StdEncoding.EncodeToString(key)
		if err := writeSecretFile(keyPath, []byte(encoded)); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return nil, fmt.Errorf("创建数据库密钥文件 %s: %w", keyPath, err)
			}
			// Another process won the race; use the key it wrote.
			raw, err = os.ReadFile(keyPath)
			if err != nil {
				return nil, fmt.Errorf("读取数据库密钥文件 %s: %w", keyPath, err)
			}
		} else {
			return boxFromKey(key, keyPath)
		}
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("解析数据库密钥文件 %s: %w", keyPath, err)
	}
	return boxFromKey(key, keyPath)
}

func boxFromKey(key []byte, keyPath string) (*secretBox, error) {
	if len(key) != secretKeyLen {
		return nil, fmt.Errorf("数据库密钥长度必须为 %d 字节", secretKeyLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &secretBox{aead: aead, keyPath: keyPath}, nil
}

func (b *secretBox) Encrypt(plain string) (string, error) {
	if plain == "" || strings.HasPrefix(plain, secretPrefix) {
		return plain, nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ciphertext := b.aead.Seal(nil, nonce, []byte(plain), nil)
	return secretPrefix + base64.RawStdEncoding.EncodeToString(append(nonce, ciphertext...)), nil
}

func (b *secretBox) Decrypt(value string) (string, bool, error) {
	if value == "" {
		return "", false, nil
	}
	if !strings.HasPrefix(value, secretPrefix) {
		return value, false, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(value, secretPrefix))
	if err != nil {
		return "", true, fmt.Errorf("解析加密字段: %w", err)
	}
	nonceSize := b.aead.NonceSize()
	if len(raw) < nonceSize {
		return "", true, errors.New("加密字段长度无效")
	}
	plain, err := b.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		return "", true, fmt.Errorf("解密字段失败（数据库密钥不匹配？）: %w", err)
	}
	return string(plain), true, nil
}

func writeSecretFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
