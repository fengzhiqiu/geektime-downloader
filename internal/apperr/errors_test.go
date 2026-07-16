package apperr_test

import (
	"context"
	"testing"

	"github.com/nicoxiang/geektime-downloader/internal/apperr"
	"github.com/nicoxiang/geektime-downloader/internal/geektime"
)

func TestMapErrorAuthExpired(t *testing.T) {
	err := apperr.MapError(geektime.ErrAuthFailed)
	if err.Code != apperr.CodeAuthExpired {
		t.Fatalf("got %s", err.Code)
	}
	if err.Action != "UPDATE_COOKIES" {
		t.Fatalf("got action %s", err.Action)
	}
}

func TestMapErrorCancelled(t *testing.T) {
	err := apperr.MapError(context.Canceled)
	if err.Code != apperr.CodeCancelled {
		t.Fatalf("got %s", err.Code)
	}
}

func TestMapErrorNotPurchased(t *testing.T) {
	err := apperr.MapError(apperr.ErrNotPurchased)
	if err.Code != apperr.CodeNotPurchased {
		t.Fatalf("got %s", err.Code)
	}
	if err.Retryable {
		t.Fatal("expected not retryable")
	}
}

func TestMapErrorPassthrough(t *testing.T) {
	orig := &apperr.APIError{Code: apperr.CodeBadRequest, Message: "x", HTTPStatus: 400}
	got := apperr.MapError(orig)
	if got.Code != orig.Code || got.Message != orig.Message {
		t.Fatal("expected passthrough")
	}
}

func TestMapErrorDeadlineExceeded(t *testing.T) {
	err := context.DeadlineExceeded
	apiErr := apperr.MapError(err)
	if apiErr.Code != apperr.CodeTimeout {
		t.Fatalf("want CodeTimeout, got %s", apiErr.Code)
	}
	if !apiErr.Retryable {
		t.Fatal("DeadlineExceeded should be retryable")
	}
	if apiErr.HTTPStatus != 504 {
		t.Fatalf("want 504, got %d", apiErr.HTTPStatus)
	}
}
