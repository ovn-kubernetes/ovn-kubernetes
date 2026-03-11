package main

import (
	"strings"
	"testing"
)

func TestRecoverComponentPanicCapturesPanic(t *testing.T) {
	var err error

	func() {
		defer recoverComponentPanic("cluster manager", &err)
		panic("boom")
	}()

	if err == nil {
		t.Fatal("expected panic to be converted to an error")
	}
	if !strings.Contains(err.Error(), "panic in cluster manager: boom") {
		t.Fatalf("expected component panic in error, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "goroutine ") {
		t.Fatalf("expected stack trace in error, got %q", err.Error())
	}
}

func TestRecoverComponentPanicIgnoresNormalReturn(t *testing.T) {
	err := error(nil)

	func() {
		defer recoverComponentPanic("cluster manager", &err)
	}()

	if err != nil {
		t.Fatalf("expected nil error when no panic occurs, got %v", err)
	}
}
