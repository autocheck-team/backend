package playground

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

func projectProto(project Project) *v1.Project {
	result := &v1.Project{
		Id: project.ID.String(), UserId: project.UserID.String(), Title: project.Title,
		LanguageId: project.LanguageID, CreateTime: timestamppb.New(project.CreatedAt),
		UpdateTime: timestamppb.New(project.UpdatedAt), Files: make([]*v1.ProjectFile, 0, len(project.Files)),
	}
	for _, file := range project.Files {
		result.Files = append(result.Files, &v1.ProjectFile{Name: file.Name, Content: file.Content})
	}
	return result
}

func runProto(run Run) *v1.ProjectRun {
	result := &v1.ProjectRun{
		Id: run.ID.String(), ProjectId: run.ProjectID.String(), Verdict: run.Verdict,
		ExitCode: run.ExitCode, Stdout: run.Stdout, Stderr: run.Stderr, CompileLog: run.CompileLog,
		IsOutputTruncated: run.Truncated, TimeMs: run.TimeMS, MemoryKb: run.MemoryKB,
		CreateTime: timestamppb.New(run.CreatedAt), UpdateTime: timestamppb.New(run.UpdatedAt),
	}
	switch run.Status {
	case StatusQueued:
		result.Status = v1.ProjectRunStatus_PROJECT_RUN_STATUS_QUEUED
	case StatusCompiling:
		result.Status = v1.ProjectRunStatus_PROJECT_RUN_STATUS_COMPILING
	case StatusRunning:
		result.Status = v1.ProjectRunStatus_PROJECT_RUN_STATUS_RUNNING
	case StatusDone:
		result.Status = v1.ProjectRunStatus_PROJECT_RUN_STATUS_DONE
	case StatusFailed:
		result.Status = v1.ProjectRunStatus_PROJECT_RUN_STATUS_FAILED
	default:
		result.Status = v1.ProjectRunStatus_PROJECT_RUN_STATUS_UNSPECIFIED
	}
	return result
}
