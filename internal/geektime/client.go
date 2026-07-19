package geektime

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/nicoxiang/geektime-downloader/internal/pkg/logger"
)

const (
	DefaultTimeout = 10 * time.Second
	// Origin ...
	Origin = "Origin"
	// UserAgent ...
	UserAgent = "User-Agent"
)

// A Client manages communication with the Geektime API.
type Client struct {
	RestyClient *resty.Client
	Cookies     []*http.Cookie
}

// ErrGeekTimeAPIBadCode ...
type ErrGeekTimeAPIBadCode struct {
	Path           string
	ResponseString string
}

// Error implements error interface
func (e ErrGeekTimeAPIBadCode) Error() string {
	return fmt.Sprintf("请求极客时间接口 %s 失败, ResponseBody: %s", e.Path, e.ResponseString)
}

var (
	// ErrWrongPassword ...
	ErrWrongPassword = errors.New("密码错误, 请尝试重新登录")
	// ErrTooManyLoginAttemptTimes ...
	ErrTooManyLoginAttemptTimes = errors.New("密码输入错误次数过多，已触发验证码校验，请稍后再试")
	// ErrGeekTimeRateLimit ...
	ErrGeekTimeRateLimit = errors.New("已触发限流, 你可以选择重新登录/重新获取 cookie, 或者稍后再试, 然后生成剩余的文章")
	// ErrAuthFailed ...
	ErrAuthFailed = errors.New("当前账户在其他设备登录或者登录已经过期, 请尝试重新登录")
)

// NewClient returns a new Geektime API client.
func NewClient(cs []*http.Cookie) *Client {
	restyClient := resty.New().
		SetCookies(cs).
		SetRetryCount(3).
		SetRetryWaitTime(2*time.Second).
		SetRetryMaxWaitTime(10*time.Second).
		SetTimeout(DefaultTimeout).
		SetHeader(RefererHeader, DefaultReferer).
		SetLogger(logger.DiscardLogger{}).
		// resty's default retry only covers transport errors, not HTTP status.
		// 451/452 are transient edge anti-bot blocks that self-heal within
		// minutes (verified in logs), so retry them in-process before
		// escalating to the worker cooldown. do()/CheckStatus observe only
		// the final response.
		AddRetryCondition(func(r *resty.Response, err error) bool {
			if err != nil {
				return true // transport / timeout errors
			}
			sc := r.StatusCode()
			return sc == 451 || sc == 452 || sc >= 500
		})
	ApplyBrowserHeaders(restyClient)

	c := &Client{RestyClient: restyClient, Cookies: cs}
	return c
}

// newRequest new http request
func (c *Client) newRequest(
	method string,
	baseURL string,
	path string,
	params map[string]string,
	body interface{},
	result interface{},
) *resty.Request {
	r := c.RestyClient.R()
	r.Method = method
	r.URL = baseURL + path
	r.SetHeader(Origin, baseURL)
	if len(params) > 0 {
		r.SetQueryParams(params)
	}
	if body != nil {
		r.SetBody(body)
	}
	r.SetResult(result)
	return r
}

// CheckStatus maps a non-2xx response to a sentinel error. Returns nil for 2xx.
// Shared by do() (geektime APIs) and video.getPlayInfo (VOD URL).
func CheckStatus(resp *resty.Response) error {
	statusCode := resp.RawResponse.StatusCode
	if statusCode >= 200 && statusCode < 300 {
		return nil
	}
	logNotOkResponse(resp)
	switch statusCode {
	case 451, 452:
		// 451 and 452 are transient edge anti-bot blocks (empty body,
		// self-healing, burst-correlated), not account expiry. Real account
		// expiry surfaces via JSON code -3050/-2000 in do(), which still
		// returns ErrAuthFailed. Map both to rate-limit so the worker
		// applies its WAITING_RATE_LIMIT cooldown + auto-resume instead of
		// demanding a pointless cookie refresh.
		return ErrGeekTimeRateLimit
	default:
		return ErrGeekTimeAPIBadCode{
			Path:           resp.RawResponse.Request.URL.String(),
			ResponseString: resp.String(),
		}
	}
}

// do perform http request
func do(request *resty.Request) (*resty.Response, error) {
	logger.Infof("Http request start, method: %s, url: %s, request body: %v",
		request.Method,
		request.URL,
		request.Body,
	)
	resp, err := request.Execute(request.Method, request.URL)
	if err != nil {
		return nil, err
	}

	logger.Infof("Http request end, method: %s, url: %s, status code: %d",
		resp.RawResponse.Request.Method,
		resp.RawResponse.Request.URL,
		resp.RawResponse.StatusCode,
	)

	if err := CheckStatus(resp); err != nil {
		return nil, err
	}

	rv := reflect.ValueOf(request.Result)
	f := reflect.Indirect(rv).FieldByName("Code")
	code := int(f.Int())

	if code == 0 {
		return resp, nil
	}

	logNotOkResponse(resp)
	// 未登录或者已失效
	if code == -3050 || code == -2000 {
		return nil, ErrAuthFailed
	}

	return nil, ErrGeekTimeAPIBadCode{request.URL, resp.String()}
}

func logNotOkResponse(resp *resty.Response) {
	logger.Warnf("Http request not ok, response body: %s", resp.String())
}
