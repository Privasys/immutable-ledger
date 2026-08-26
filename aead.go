// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package ledger

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
)

// AES-256-GCM for value records and the checkpoint, in the same wire
// format as the Rust implementation: `nonce(12) ‖ ciphertext ‖ tag(16)`.

// KeySize is the size of both the commitment key and the storage key.
const KeySize = 32

const (
	aeadNonceSize = 12
	aeadTagSize   = 16
)

type aeadCipher struct {
	gcm cipher.AEAD
}

func newAEAD(key [KeySize]byte) (*aeadCipher, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, errCorruptedf("aead init: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errCorruptedf("aead init: %v", err)
	}
	return &aeadCipher{gcm: gcm}, nil
}

// encrypt returns `nonce ‖ ciphertext ‖ tag` with a fresh random nonce.
func (a *aeadCipher) encrypt(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, aeadNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, errCorruptedf("aead nonce: %v", err)
	}
	return a.gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// decrypt expects `nonce ‖ ciphertext ‖ tag`.
func (a *aeadCipher) decrypt(record, aad []byte) ([]byte, error) {
	if len(record) < aeadNonceSize+aeadTagSize {
		return nil, errCorrupted("aead record too short")
	}
	pt, err := a.gcm.Open(nil, record[:aeadNonceSize], record[aeadNonceSize:], aad)
	if err != nil {
		return nil, errCorrupted("aead open failed")
	}
	return pt, nil
}
