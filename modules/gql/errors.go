package gql

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// Client-facing error hygiene.
//
// gqlgen's default presenter returns err.Error() verbatim, so whatever a
// resolver or scalar happened to return reached the client: parser messages
// ("strconv.ParseUint: parsing \"x\": invalid syntax"), Go type names
// ("cannot unmarshal string into Go value of type int64") and MongoDB server
// text. None of it helps a client and all of it describes the implementation.
//
// presentError keeps every error that is already meant for a client (gqlgen's
// own parse/validation errors, and resolver messages like "contract not
// found") and replaces only the kinds that leak internals. Database errors
// are also logged in full here, so nothing is lost for the operator.

const (
	msgInvalidNumber = "invalid number"
	msgInvalidValue  = "invalid value"
	msgInternal      = "internal error"
)

// sanitize returns the client-safe replacement for err, and whether err is an
// internal failure worth an operator log line. An empty replacement means the
// message is already fine to show.
func sanitize(err error) (replacement string, internal bool) {
	if err == nil {
		return "", false
	}

	var numErr *strconv.NumError
	if errors.As(err, &numErr) {
		return msgInvalidNumber, false
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return msgInvalidValue, false
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return msgInvalidValue, false
	}

	var timeErr *time.ParseError
	if errors.As(err, &timeErr) {
		return msgInvalidValue, false
	}

	// Anything the database said. ServerError covers CommandError,
	// WriteException and BulkWriteException.
	var srvErr mongo.ServerError
	if errors.As(err, &srvErr) {
		return msgInternal, true
	}
	if errors.Is(err, bson.ErrDecodeToNil) || mongo.IsNetworkError(err) || mongo.IsTimeout(err) {
		return msgInternal, true
	}

	// Errors that were flattened to strings before they reached us (fmt.Errorf
	// with %v, or a library that does not wrap) still carry these markers.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "strconv."):
		return msgInvalidNumber, false
	case strings.Contains(msg, "Go value of type"),
		strings.Contains(msg, "Go struct field"),
		strings.Contains(msg, "parsing time "):
		return msgInvalidValue, false
	case strings.Contains(msg, "(BadValue)"),
		strings.Contains(msg, "(Location"),
		strings.Contains(msg, "mongo:"),
		strings.Contains(msg, "bson:"),
		strings.Contains(msg, "server selection error"),
		strings.Contains(msg, "connection("):
		return msgInternal, true
	}
	return "", false
}

// presentError is installed with SetErrorPresenter.
func presentError(ctx context.Context, err error) *gqlerror.Error {
	gerr := graphql.DefaultErrorPresenter(ctx, err)
	replacement, internal := sanitize(err)
	if replacement == "" {
		return gerr
	}
	if internal {
		gqlLog.Warn("graphql request failed", "path", gerr.Path.String(), "err", err)
	}
	gerr.Message = replacement
	return gerr
}
