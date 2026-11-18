package submission

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

// WatchSubmission подписывается до первого чтения, чтобы переход между ними не потерялся.
func (s *Service) WatchSubmission(req *v1.WatchSubmissionRequest,
	stream grpc.ServerStreamingServer[v1.WatchSubmissionResponse],
) error {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return status.Error(codes.InvalidArgument, "id must be a UUID")
	}
	ctx, cancel := context.WithTimeout(stream.Context(), 10*time.Minute)
	defer cancel()
	if s.events == nil {
		return status.Error(codes.Unavailable, "submission events unavailable")
	}
	changed, unsubscribe := s.events.subscribe(id)
	defer unsubscribe()
	// NOTIFY не является журналом: сверка страхует потерю последнего сигнала при обрыве.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	var previous *v1.Submission
	for {
		readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		sub, err := s.loadVisible(readCtx, id)
		stop()
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		if err != nil {
			return err
		}
		current := toProto(sub)
		if !proto.Equal(previous, current) {
			if err := stream.Send(&v1.WatchSubmissionResponse{Submission: current}); err != nil {
				return fmt.Errorf("send submission state: %w", err)
			}
			previous = current
		}
		if sub.Status == "done" || sub.Status == "failed" {
			return nil
		}
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-changed:
		case <-ticker.C:
		}
	}
}
