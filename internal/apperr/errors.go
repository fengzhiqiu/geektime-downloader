package apperr

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/nicoxiang/geektime-downloader/internal/geektime"
)

const (
	CodeAuthExpired    = "AUTH_EXPIRED"
	CodeAuthInvalid    = "AUTH_INVALID"
	CodeRateLimited    = "RATE_LIMITED"
	CodeNotPurchased   = "NOT_PURCHASED"
	CodeInvalidProduct = "INVALID_PRODUCT"
	CodeTimeout        = "TIMEOUT"
	CodeCancelled      = "CANCELLED"
	CodeInternal       = "INTERNAL_ERROR"
	CodeNotFound       = "NOT_FOUND"
	CodeBadRequest     = "BAD_REQUEST"
	CodeUnauthorized   = "UNAUTHORIZED"
)

// APIError is the structured error returned by the HTTP API.
type APIError struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Action      string `json:"action,omitempty"`
	ActionHint  string `json:"action_hint,omitempty"`
	Retryable   bool   `json:"retryable"`
	HTTPStatus  int    `json:"-"`
	Details     any    `json:"details,omitempty"`
	FailedAID   int    `json:"failed_article,omitempty"`
	Underlying  error  `json:"-"`
}

func (e *APIError) Error() string {
	if e.Underlying != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Underlying)
	}
	return e.Message
}

func (e *APIError) Unwrap() error {
	return e.Underlying
}

// ErrNotPurchased indicates the user has not purchased the course.
var ErrNotPurchased = errors.New("尚未购买该课程")

// ErrInvalidProduct indicates product id does not match product type.
var ErrInvalidProduct = errors.New("输入的课程 ID 有误")

// MapError converts internal errors to APIError.
func MapError(err error) *APIError {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	switch {
	case errors.Is(err, ErrNotPurchased):
		return &APIError{
			Code: CodeNotPurchased, Message: err.Error(),
			Action: "NONE", Retryable: false, HTTPStatus: 400, Underlying: err,
		}
	case errors.Is(err, ErrInvalidProduct):
		return &APIError{
			Code: CodeInvalidProduct, Message: err.Error(),
			Action: "NONE", Retryable: false, HTTPStatus: 400, Underlying: err,
		}
	case errors.Is(err, geektime.ErrAuthFailed):
		return &APIError{
			Code: CodeAuthExpired, Message: geektime.ErrAuthFailed.Error(),
			Action: "UPDATE_COOKIES",
			ActionHint: "从浏览器获取 gcid/gcess，调用 PUT /api/v1/session/cookies 后 retry 任务",
			Retryable: true, HTTPStatus: 401, Underlying: err,
		}
	case errors.Is(err, geektime.ErrGeekTimeRateLimit):
		return &APIError{
			Code: CodeRateLimited, Message: geektime.ErrGeekTimeRateLimit.Error(),
			Action: "WAIT_AND_RETRY",
			ActionHint: "等待数分钟后调用 POST /api/v1/downloads/{id}/retry",
			Retryable: true, HTTPStatus: 429, Underlying: err,
		}
	case errors.Is(err, context.Canceled):
		return &APIError{
			Code: CodeCancelled, Message: "任务已取消",
			Action: "NONE", Retryable: false, HTTPStatus: 409, Underlying: err,
		}
	default:
		if os.IsTimeout(err) {
			return &APIError{
				Code: CodeTimeout, Message: "请求超时",
				Action: "RETRY", Retryable: true, HTTPStatus: 504, Underlying: err,
			}
		}
		return &APIError{
			Code: CodeInternal, Message: err.Error(),
			Action: "RETRY", Retryable: true, HTTPStatus: 500, Underlying: err,
		}
	}
}
