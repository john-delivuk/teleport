/*
Copyright 2021 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apiserver

import (
	"context"

	"github.com/gravitational/trace"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// withErrorHandling is GRPC middleware that maps internal errors to proper GRPC error codes
func withErrorHandling(log logrus.FieldLogger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		resp, err := handler(ctx, req)
		if err != nil {
			log.WithError(err).Error("Request failed.")
			// do not return a full error stack on access denied errors
			if trace.IsAccessDenied(err) {
				return resp, toGRPC(trace.AccessDenied("access denied"))
			}
			return resp, toGRPC(err)
		}

		return resp, nil
	}
}

// toGRPC converts error to GRPC-compatible error
func toGRPC(err error) error {
	if err == nil {
		return nil
	}
	message := getUserMessage(err)
	if trace.IsNotFound(err) {
		return status.Errorf(codes.NotFound, message)
	}
	if trace.IsAlreadyExists(err) {
		return status.Errorf(codes.AlreadyExists, message)
	}
	if trace.IsAccessDenied(err) {
		return status.Errorf(codes.PermissionDenied, message)
	}
	if trace.IsCompareFailed(err) {
		return status.Errorf(codes.FailedPrecondition, message)
	}
	if trace.IsBadParameter(err) || trace.IsOAuth2(err) {
		return status.Errorf(codes.InvalidArgument, message)
	}
	if trace.IsLimitExceeded(err) {
		return status.Errorf(codes.ResourceExhausted, message)
	}
	if trace.IsConnectionProblem(err) {
		return status.Errorf(codes.Unavailable, message)
	}
	if trace.IsNotImplemented(err) {
		return status.Errorf(codes.Unimplemented, message)
	}
	return status.Errorf(codes.Unknown, message)
}

// getUserMessage returns the first (rather than the last) user error message from the stack
func getUserMessage(err error) string {
	if err == nil {
		return ""
	}

	traced, ok := err.(*trace.TraceErr)
	if !ok {
		return err.Error()
	}

	if len(traced.Messages) > 0 {
		return traced.Messages[0]
	}

	if traced.Message != "" {
		return traced.Message
	}

	return err.Error()
}
