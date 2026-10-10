package source

import (
	"context"
	"errors"
	"fmt"

	"github.com/greed/cadence/internal/httpx"
)

type Severity string

const (
	SeverityCommon     Severity = "common"
	SeveritySuspicious Severity = "suspicious"
	SeverityFault      Severity = "fault"
)

type Failure struct {
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Cause    string   `json:"cause,omitempty"`
	Source   string   `json:"source,omitempty"`
}

func (f *Failure) Error() string {
	if f.Cause == "" {
		return f.Message
	}
	return f.Message + ": " + f.Cause
}

func Fail(severity Severity, src, message string, cause error) *Failure {
	f := &Failure{Severity: severity, Message: message, Source: src}
	if cause != nil {
		f.Cause = cause.Error()
	}
	return f
}

func Classify(src string, err error) *Failure {
	if err == nil {
		return nil
	}
	var f *Failure
	if errors.As(err, &f) {
		return f
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return Fail(SeverityCommon, src, "timed out", err)
	case httpx.IsStatus(err, 403), httpx.IsStatus(err, 429), httpx.IsStatus(err, 401):
		return Fail(SeveritySuspicious, src, "refused by source", err)
	case httpx.IsStatus(err, 404):
		return Fail(SeverityCommon, src, "not found at source", err)
	}
	return Fail(SeverityCommon, src, fmt.Sprintf("%s failed", src), err)
}
