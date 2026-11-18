package feedback

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

func kindFromProto(kind v1.FeedbackRequestType) Kind {
	switch kind {
	case v1.FeedbackRequestType_FEEDBACK_REQUEST_TYPE_COMPILER_UPDATE:
		return KindCompilerUpdate
	case v1.FeedbackRequestType_FEEDBACK_REQUEST_TYPE_NEW_LIBRARY:
		return KindNewLibrary
	case v1.FeedbackRequestType_FEEDBACK_REQUEST_TYPE_OTHER:
		return KindOther
	default:
		return KindUnspecified
	}
}

func kindToProto(kind Kind) v1.FeedbackRequestType {
	switch kind {
	case KindCompilerUpdate:
		return v1.FeedbackRequestType_FEEDBACK_REQUEST_TYPE_COMPILER_UPDATE
	case KindNewLibrary:
		return v1.FeedbackRequestType_FEEDBACK_REQUEST_TYPE_NEW_LIBRARY
	case KindOther:
		return v1.FeedbackRequestType_FEEDBACK_REQUEST_TYPE_OTHER
	default:
		return v1.FeedbackRequestType_FEEDBACK_REQUEST_TYPE_UNSPECIFIED
	}
}

func statusFromProto(state v1.FeedbackRequestStatus) Status {
	switch state {
	case v1.FeedbackRequestStatus_FEEDBACK_REQUEST_STATUS_NEW:
		return StatusNew
	case v1.FeedbackRequestStatus_FEEDBACK_REQUEST_STATUS_ACCEPTED:
		return StatusAccepted
	case v1.FeedbackRequestStatus_FEEDBACK_REQUEST_STATUS_REJECTED:
		return StatusRejected
	case v1.FeedbackRequestStatus_FEEDBACK_REQUEST_STATUS_COMPLETED:
		return StatusCompleted
	default:
		return StatusUnspecified
	}
}

func statusToProto(state Status) v1.FeedbackRequestStatus {
	switch state {
	case StatusNew:
		return v1.FeedbackRequestStatus_FEEDBACK_REQUEST_STATUS_NEW
	case StatusAccepted:
		return v1.FeedbackRequestStatus_FEEDBACK_REQUEST_STATUS_ACCEPTED
	case StatusRejected:
		return v1.FeedbackRequestStatus_FEEDBACK_REQUEST_STATUS_REJECTED
	case StatusCompleted:
		return v1.FeedbackRequestStatus_FEEDBACK_REQUEST_STATUS_COMPLETED
	default:
		return v1.FeedbackRequestStatus_FEEDBACK_REQUEST_STATUS_UNSPECIFIED
	}
}

func toProto(request Request) *v1.FeedbackRequest {
	return &v1.FeedbackRequest{
		Id: request.ID.String(), UserId: request.UserID.String(), Type: kindToProto(request.Kind),
		LanguageId: request.LanguageID, Title: request.Title, Text: request.Text,
		Status: statusToProto(request.Status), AdminComment: request.AdminComment,
		CreateTime: timestamppb.New(request.CreatedAt), UpdateTime: timestamppb.New(request.UpdatedAt),
	}
}
