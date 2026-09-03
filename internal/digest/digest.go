// SPDX-License-Identifier: Apache-2.0

// Package digest computes sha256 digests over bytes or files and returns
// them in the "sha256:<hex>" format used throughout grcli's manifests,
// provenance records, and source digests.
package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// Bytes returns the sha256 digest of b as "sha256:<hex>".
func Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// File streams path through sha256 and returns the digest as
// "sha256:<hex>". The file is not loaded entirely into memory.
func File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close() //nolint:errcheck
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}
