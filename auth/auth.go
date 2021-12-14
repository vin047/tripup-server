package auth

import (
	"crypto/sha256"
	"encoding/hex"
)

// AuthProviders contains the possible authorisation mechanisms.
type AuthProviders struct {
	PhoneNumber	*string
	Email		*string
	AppleID 	*string
}

// shasum256 calculates the sha256 sum of the provided string.
func shasum256(value string) string {
	hasher := sha256.New()
	hasher.Write([]byte(value))
    return hex.EncodeToString(hasher.Sum(nil))
}

// shasum256P does the same as `shasum256` but with pointers.
func shasum256P(value *string) *string {
	if value == nil {
		return nil
	}
	result := shasum256(*value)
	return &result
}
