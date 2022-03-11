package pipeline

import (
	"bytes"
	"fmt"
	"github.com/loft-sh/devspace/pkg/devspace/config/versions/latest"
	devspacecontext "github.com/loft-sh/devspace/pkg/devspace/context"
	"github.com/loft-sh/devspace/pkg/devspace/pipeline/engine"
	"github.com/loft-sh/devspace/pkg/devspace/pipeline/engine/basichandler"
	"github.com/loft-sh/devspace/pkg/devspace/pipeline/engine/pipelinehandler"
	"github.com/loft-sh/devspace/pkg/devspace/pipeline/env"
	"github.com/loft-sh/devspace/pkg/devspace/pipeline/types"
	"github.com/loft-sh/devspace/pkg/util/scanner"
	"github.com/loft-sh/devspace/pkg/util/tomb"
	"github.com/pkg/errors"
	"io"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"os"
	"strings"
	"sync"
)

type Job struct {
	Pipeline types.Pipeline
	Config   *latest.Pipeline
	ExtraEnv map[string]string

	m sync.Mutex
	t *tomb.Tomb
}

func (j *Job) Terminated() bool {
	j.m.Lock()
	defer j.m.Unlock()

	if j.t != nil {
		return j.t.Terminated()
	}

	return false
}

func (j *Job) Stop() error {
	j.m.Lock()
	t := j.t
	j.m.Unlock()

	if t == nil {
		return nil
	}

	t.Kill(nil)
	return t.Wait()
}

func (j *Job) Run(ctx *devspacecontext.Context) error {
	if ctx.IsDone() {
		return ctx.Context.Err()
	}

	j.m.Lock()
	if j.t != nil && !j.t.Terminated() {
		j.m.Unlock()
		return fmt.Errorf("already running, please stop before rerunning")
	}
	ctx, j.t = ctx.WithNewTomb()
	t := j.t
	j.m.Unlock()

	t.Go(func() error {
		// start the actual job
		done := t.NotifyGo(func() error {
			return j.doWork(ctx, t)
		})

		// wait until job is dying
		select {
		case <-ctx.Context.Done():
			return nil
		case <-done:
		}

		// check if errored
		if !t.Alive() {
			return t.Err()
		}

		return nil
	})

	return t.Wait()
}

func (j *Job) doWork(ctx *devspacecontext.Context, parent *tomb.Tomb) error {
	// loop over steps and execute them
	for i, step := range j.Config.Steps {
		var (
			execute = true
			err     error
		)
		if step.If != "" {
			execute, err = j.shouldExecuteStep(ctx, &step)
			if err != nil {
				return errors.Wrapf(err, "error checking if at step %d", i)
			}
		}
		if execute {
			err = j.executeStep(ctx, &step, parent)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (j *Job) shouldExecuteStep(ctx *devspacecontext.Context, step *latest.PipelineStep) (bool, error) {
	// check if step should be rerun
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	_, err := engine.ExecutePipelineShellCommand(ctx.Context, step.Run, os.Args[1:], ctx.ResolvePath(step.WorkingDir), false, stdout, stderr, os.Stdin, j.getEnv(ctx), basichandler.NewBasicExecHandler())
	if err != nil {
		if status, ok := interp.IsExitStatus(err); ok && status == 1 {
			return false, nil
		}

		return false, fmt.Errorf("error: %v (stdout: %s, stderr: %s)", err, stdout.String(), stderr.String())
	} else if strings.TrimSpace(stdout.String()) == "false" {
		return false, nil
	}

	return true, nil
}

func (j *Job) executeStep(ctx *devspacecontext.Context, step *latest.PipelineStep, parent *tomb.Tomb) error {
	ctx = ctx.WithLogger(ctx.Log)
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutWriter.Close()
	parent.Go(func() error {
		s := scanner.NewScanner(stdoutReader)
		for s.Scan() {
			ctx.Log.Info(s.Text())
		}
		return nil
	})

	handler := pipelinehandler.NewPipelineExecHandler(ctx, stdoutWriter, j.Pipeline)
	_, err := engine.ExecutePipelineShellCommand(ctx.Context, step.Run, os.Args[1:], ctx.ResolvePath(step.WorkingDir), step.ContinueOnError, stdoutWriter, stdoutWriter, os.Stdin, j.getEnv(ctx), handler)
	return err
}

func (j *Job) getEnv(ctx *devspacecontext.Context) expand.Environ {
	envMap := map[string]string{}
	for k, v := range j.ExtraEnv {
		envMap[k] = v
	}

	return env.NewVariableEnvProvider(ctx.Config, ctx.Dependencies, envMap)
}