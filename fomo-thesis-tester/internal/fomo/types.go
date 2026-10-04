package fomo

import "time"

type Thesis struct {
	Type         string  `json:"type"`
	ID           string  `json:"id"`
	TradeID      string  `json:"tradeId"`
	CreatedAt    string  `json:"createdAt"`
	UserID       string  `json:"userId"`
	DisplayName  string  `json:"displayName"`
	UserHandle   string  `json:"userHandle"`
	TokenAddress string  `json:"tokenAddress"`
	NetworkID    int64   `json:"networkId"`
	Ticker       *string `json:"ticker"`
	Comment      struct {
		ID        string `json:"id"`
		TradeID   string `json:"tradeId"`
		Comment   string `json:"comment"`
		CreatedAt string `json:"createdAt"`
	} `json:"comment"`
}

func (t Thesis) CreatedTime() (time.Time, error) {
	return time.Parse(time.RFC3339Nano, t.CreatedAt)
}

type ThesisQuery struct {
	Address    string
	NetworkID  int64
	AfterTime  time.Time
	BeforeTime time.Time
	Limit      int
	Threshold  int
	Mode       string
}

type FeedResponse struct {
	Success        bool   `json:"success"`
	Message        string `json:"message"`
	ResponseObject struct {
		Items       []Thesis `json:"items"`
		HasNextPage bool     `json:"hasNextPage"`
		Count       int      `json:"count"`
	} `json:"responseObject"`
	StatusCode int    `json:"statusCode"`
	Error      string `json:"error"`
}

type BrowserHTTPResponse struct {
	HTTPStatus        int    `json:"httpStatus"`
	Body              string `json:"body"`
	RetryAfter        string `json:"retryAfter"`
	Date              string `json:"date"`
	FetchStartedAt    string `json:"fetchStartedAt"`
	HeadersReceivedAt string `json:"headersReceivedAt"`
	BodyReadAt        string `json:"bodyReadAt"`
}

type Result struct {
	Feed                     FeedResponse
	HTTPStatus               int
	RetryAfter               string
	ServerDate               string
	RequestStartedAt         time.Time
	BrowserFetchStartedAt    time.Time
	BrowserHeadersReceivedAt time.Time
	BrowserBodyReadAt        time.Time
	ReceivedAt               time.Time
	Latency                  time.Duration
	Query                    ThesisQuery
}

type ErrorKind string

const (
	ErrAuth      ErrorKind = "auth"
	ErrForbidden ErrorKind = "forbidden"
	ErrRateLimit ErrorKind = "rate_limit"
	ErrNetwork   ErrorKind = "network"
	ErrAPI       ErrorKind = "api"
	ErrParse     ErrorKind = "parse"
)

type RequestError struct {
	Kind       ErrorKind
	HTTPStatus int
	BodyStatus int
	RetryAfter string
	Message    string
}

func (e *RequestError) Error() string {
	return string(e.Kind) + ": " + e.Message
}
