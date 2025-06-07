package runner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/model"
)

type jobInfo interface {
	matrix() map[string]interface{}
	steps() []*model.Step
	startContainer() common.Executor
	stopContainer() common.Executor
	closeContainer() common.Executor
	interpolateOutputs() common.Executor
	result(result string)
}

//nolint:contextcheck
func newJobExecutor(info jobInfo, sf stepFactory, rc *RunContext) common.Executor {
	steps := make([]common.Executor, 0)
	preSteps := make([]common.Executor, 0)
	var postExecutor common.Executor

	steps = append(steps, func(ctx context.Context) error {
		logger := common.Logger(ctx)
		matrix := info.matrix()
		if len(matrix) > 0 {
			logger.Infof("\U0001F9EA  Matrix: %v", matrix)
			
			// Output detailed matrix variables in dryrun mode with --show-details
			if common.Dryrun(ctx) && rc.Config.ShowDetails {
				for key, value := range matrix {
					logger.Infof("*DRYRUN-ENV* %s=%v", key, value)
				}
			}
		}
		return nil
	})

	infoSteps := info.steps()

	if len(infoSteps) == 0 {
		return common.NewDebugExecutor("No steps found")
	}

	preSteps = append(preSteps, func(ctx context.Context) error {
		// Have to be skipped for some Tests
		if rc.Run == nil {
			return nil
		}
		rc.ExprEval = rc.NewExpressionEvaluator(ctx)
		// evaluate environment variables since they can contain
		// GitHub's special environment variables.
		for k, v := range rc.GetEnv() {
			rc.Env[k] = rc.ExprEval.Interpolate(ctx, v)
		}
		
		// Output environment variables in dryrun mode with --show-details
		if common.Dryrun(ctx) && rc.Config.ShowDetails {
			logger := common.Logger(ctx)
			
			// Output GitHub context environment variables
			github := rc.getGithubContext(ctx)
			if github != nil {
				logger.Infof("*DRYRUN-ENV* CI=true")
				logger.Infof("*DRYRUN-ENV* GITHUB_ACTIONS=true")
				logger.Infof("*DRYRUN-ENV* GITHUB_WORKFLOW=%s", github.Workflow)
				logger.Infof("*DRYRUN-ENV* GITHUB_RUN_ID=%s", github.RunID)
				logger.Infof("*DRYRUN-ENV* GITHUB_RUN_NUMBER=%s", github.RunNumber)
				logger.Infof("*DRYRUN-ENV* GITHUB_ACTOR=%s", github.Actor)
				logger.Infof("*DRYRUN-ENV* GITHUB_REPOSITORY=%s", github.Repository)
				logger.Infof("*DRYRUN-ENV* GITHUB_EVENT_NAME=%s", github.EventName)
				logger.Infof("*DRYRUN-ENV* GITHUB_SHA=%s", github.Sha)
				logger.Infof("*DRYRUN-ENV* GITHUB_REF=%s", github.Ref)
				if github.Workspace != "" {
					logger.Infof("*DRYRUN-ENV* GITHUB_WORKSPACE=%s", github.Workspace)
				}
				if github.BaseRef != "" {
					logger.Infof("*DRYRUN-ENV* GITHUB_BASE_REF=%s", github.BaseRef)
				}
				if github.HeadRef != "" {
					logger.Infof("*DRYRUN-ENV* GITHUB_HEAD_REF=%s", github.HeadRef)
				}
			}
			
			// Output job-level environment variables
			for k, v := range rc.Env {
				// Skip GitHub built-ins that were already shown above
				if !strings.HasPrefix(k, "GITHUB_") && k != "CI" {
					logger.Infof("*DRYRUN-ENV* %s=%s", k, v)
				}
			}
		}
		
		// Output parsable commands for bash script generation
		if common.Dryrun(ctx) {
			cmdLogger := common.Logger(ctx).WithField("command_output", true)
			
			// Output GitHub context environment variables
			github := rc.getGithubContext(ctx)
			if github != nil {
				cmdLogger.Infof("ACT_ENV: export CI=true")
				cmdLogger.Infof("ACT_ENV: export GITHUB_ACTIONS=true")
				cmdLogger.Infof("ACT_ENV: export GITHUB_WORKFLOW=\"%s\"", github.Workflow)
				cmdLogger.Infof("ACT_ENV: export GITHUB_RUN_ID=%s", github.RunID)
				cmdLogger.Infof("ACT_ENV: export GITHUB_RUN_NUMBER=%s", github.RunNumber)
				cmdLogger.Infof("ACT_ENV: export GITHUB_ACTOR=\"%s\"", github.Actor)
				cmdLogger.Infof("ACT_ENV: export GITHUB_REPOSITORY=\"%s\"", github.Repository)
				cmdLogger.Infof("ACT_ENV: export GITHUB_EVENT_NAME=%s", github.EventName)
				cmdLogger.Infof("ACT_ENV: export GITHUB_SHA=%s", github.Sha)
				cmdLogger.Infof("ACT_ENV: export GITHUB_REF=\"%s\"", github.Ref)
				if github.Workspace != "" {
					cmdLogger.Infof("ACT_ENV: export GITHUB_WORKSPACE=\"%s\"", github.Workspace)
				}
				if github.BaseRef != "" {
					cmdLogger.Infof("ACT_ENV: export GITHUB_BASE_REF=\"%s\"", github.BaseRef)
				}
				if github.HeadRef != "" {
					cmdLogger.Infof("ACT_ENV: export GITHUB_HEAD_REF=\"%s\"", github.HeadRef)
				}
			}
			
			// Output job-level environment variables
			for k, v := range rc.Env {
				// Skip GitHub built-ins that were already shown above
				if !strings.HasPrefix(k, "GITHUB_") && k != "CI" {
					cmdLogger.Infof("ACT_ENV: export %s=\"%s\"", k, v)
				}
			}
			
			// Output matrix variables
			matrix := info.matrix()
			for key, value := range matrix {
				cmdLogger.Infof("ACT_ENV: export %s=\"%v\"", key, value)
			}
		}
		
		return nil
	})

	var setJobError = func(ctx context.Context, err error) error {
		if err == nil {
			return nil
		}
		logger := common.Logger(ctx)
		logger.Errorf("%v", err)
		common.SetJobError(ctx, err)
		return err
	}

	for i, stepModel := range infoSteps {
		if stepModel == nil {
			return func(_ context.Context) error {
				return fmt.Errorf("invalid Step %v: missing run or uses key", i)
			}
		}
		if stepModel.ID == "" {
			stepModel.ID = fmt.Sprintf("%d", i)
		}

		step, err := sf.newStep(stepModel, rc)

		if err != nil {
			return common.NewErrorExecutor(err)
		}

		preSteps = append(preSteps, useStepLogger(rc, stepModel, stepStagePre, step.pre().ThenError(setJobError)))

		stepExec := step.main()
		steps = append(steps, useStepLogger(rc, stepModel, stepStageMain, func(ctx context.Context) error {
			err := stepExec(ctx)
			if err != nil {
				_ = setJobError(ctx, err)
			} else if ctx.Err() != nil {
				_ = setJobError(ctx, ctx.Err())
			}
			return nil
		}))

		postExec := useStepLogger(rc, stepModel, stepStagePost, step.post())
		if postExecutor != nil {
			// run the post executor in reverse order
			postExecutor = postExec.Finally(postExecutor)
		} else {
			postExecutor = postExec
		}
	}

	var stopContainerExecutor common.Executor = func(ctx context.Context) error {
		jobError := common.JobError(ctx)
		var err error
		if rc.Config.AutoRemove || jobError == nil {
			// always allow 1 min for stopping and removing the runner, even if we were cancelled
			ctx, cancel := context.WithTimeout(common.WithLogger(context.Background(), common.Logger(ctx)), time.Minute)
			defer cancel()

			logger := common.Logger(ctx)
			logger.Infof("Cleaning up container for job %s", rc.JobName)
			if err = info.stopContainer()(ctx); err != nil {
				logger.Errorf("Error while stop job container: %v", err)
			}
		}
		return err
	}

	var setJobResultExecutor common.Executor = func(ctx context.Context) error {
		jobError := common.JobError(ctx)
		setJobResult(ctx, info, rc, jobError == nil)
		setJobOutputs(ctx, rc)
		return nil
	}

	pipeline := make([]common.Executor, 0)
	pipeline = append(pipeline, preSteps...)
	pipeline = append(pipeline, steps...)

	return common.NewPipelineExecutor(
		common.NewFieldExecutor("step", "Set up job", common.NewFieldExecutor("stepid", []string{"--setup-job"},
			common.NewPipelineExecutor(common.NewInfoExecutor("\u2B50 Run Set up job"), info.startContainer(), rc.InitializeNodeTool()).
				Then(common.NewFieldExecutor("stepResult", model.StepStatusSuccess, common.NewInfoExecutor("  \u2705  Success - Set up job"))).
				ThenError(setJobError).OnError(common.NewFieldExecutor("stepResult", model.StepStatusFailure, common.NewInfoExecutor("  \u274C  Failure - Set up job"))))),
		common.NewPipelineExecutor(pipeline...).
			Finally(func(ctx context.Context) error { //nolint:contextcheck
				var cancel context.CancelFunc
				if ctx.Err() == context.Canceled {
					// in case of an aborted run, we still should execute the
					// post steps to allow cleanup.
					ctx, cancel = context.WithTimeout(common.WithLogger(context.Background(), common.Logger(ctx)), 5*time.Minute)
					defer cancel()
				}
				return postExecutor(ctx)
			}).
			Finally(common.NewFieldExecutor("step", "Complete job", common.NewFieldExecutor("stepid", []string{"--complete-job"},
				common.NewInfoExecutor("\u2B50 Run Complete job").
					Finally(stopContainerExecutor).
					Finally(
						info.interpolateOutputs().Finally(info.closeContainer()).Then(common.NewFieldExecutor("stepResult", model.StepStatusSuccess, common.NewInfoExecutor("  \u2705  Success - Complete job"))).
							OnError(common.NewFieldExecutor("stepResult", model.StepStatusFailure, common.NewInfoExecutor("  \u274C  Failure - Complete job"))),
					))))).Finally(setJobResultExecutor)
}

func setJobResult(ctx context.Context, info jobInfo, rc *RunContext, success bool) {
	logger := common.Logger(ctx)

	jobResult := "success"
	// we have only one result for a whole matrix build, so we need
	// to keep an existing result state if we run a matrix
	if len(info.matrix()) > 0 && rc.Run.Job().Result != "" {
		jobResult = rc.Run.Job().Result
	}

	if !success {
		jobResult = "failure"
	}

	info.result(jobResult)
	if rc.caller != nil {
		// set reusable workflow job result
		rc.caller.runContext.result(jobResult)
	}

	jobResultMessage := "succeeded"
	if jobResult != "success" {
		jobResultMessage = "failed"
	}

	logger.WithField("jobResult", jobResult).Infof("\U0001F3C1  Job %s", jobResultMessage)
}

func setJobOutputs(ctx context.Context, rc *RunContext) {
	if rc.caller != nil {
		// map outputs for reusable workflows
		callerOutputs := make(map[string]string)

		ee := rc.NewExpressionEvaluator(ctx)

		for k, v := range rc.Run.Workflow.WorkflowCallConfig().Outputs {
			callerOutputs[k] = ee.Interpolate(ctx, ee.Interpolate(ctx, v.Value))
		}

		rc.caller.runContext.Run.Job().Outputs = callerOutputs
	}
}

func useStepLogger(rc *RunContext, stepModel *model.Step, stage stepStage, executor common.Executor) common.Executor {
	return func(ctx context.Context) error {
		ctx = withStepLogger(ctx, stepModel.ID, rc.ExprEval.Interpolate(ctx, stepModel.String()), stage.String())

		rawLogger := common.Logger(ctx).WithField("raw_output", true)
		logWriter := common.NewLineWriter(rc.commandHandler(ctx), func(s string) bool {
			if rc.Config.LogOutput {
				rawLogger.Infof("%s", s)
			} else {
				rawLogger.Debugf("%s", s)
			}
			return true
		})

		oldout, olderr := rc.JobContainer.ReplaceLogWriter(logWriter, logWriter)
		defer rc.JobContainer.ReplaceLogWriter(oldout, olderr)

		return executor(ctx)
	}
}
