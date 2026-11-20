package tts

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateText_Empty(t *testing.T) {
	if err := validateText("", 100); err == nil {
		t.Fatal("expected error for empty text")
	}
}

func TestValidateText_TooLong(t *testing.T) {
	err := validateText(strings.Repeat("a", 101), 100)
	if err == nil {
		t.Fatal("expected error for text exceeding max length")
	}
	var tooLong *ErrTextTooLong
	if !errors.As(err, &tooLong) {
		t.Fatalf("expected ErrTextTooLong, got %T", err)
	}
	if tooLong.Length != 101 || tooLong.MaxLength != 100 {
		t.Fatalf("unexpected lengths: got %d/%d", tooLong.Length, tooLong.MaxLength)
	}
}

func TestValidateText_OK(t *testing.T) {
	if err := validateText("hello world", 100); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
