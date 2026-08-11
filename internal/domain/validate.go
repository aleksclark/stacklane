package domain

import (
	"errors"
	"fmt"
	"regexp"
)

// ErrInvalidSlug indicates a DNS-label slug failed validation.
var ErrInvalidSlug = errors.New("invalid slug")

// ErrInvalidPort indicates a port number is out of range.
var ErrInvalidPort = errors.New("invalid port")

// slugRE matches a single DNS label: 1–63 chars, a-z0-9, hyphens not at ends.
// Also accepts a single [a-z0-9] character.
var slugRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidateSlug checks stacklane project/instance/endpoint slugs.
// Rejects uppercase, underscores, dots, leading/trailing hyphens, and length > 63.
func ValidateSlug(s string) error {
	if s == "" {
		return fmt.Errorf("%w: empty", ErrInvalidSlug)
	}
	if len(s) > 63 {
		return fmt.Errorf("%w: %q exceeds 63 characters", ErrInvalidSlug, s)
	}
	if !slugRE.MatchString(s) {
		return fmt.Errorf("%w: %q", ErrInvalidSlug, s)
	}
	return nil
}

// ValidatePort checks that port is in the inclusive range 1–65535.
func ValidatePort(port uint16) error {
	if port == 0 {
		return fmt.Errorf("%w: 0 is not allowed", ErrInvalidPort)
	}
	return nil
}
