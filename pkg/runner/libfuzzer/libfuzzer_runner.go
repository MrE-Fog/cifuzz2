package libfuzzer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"code-intelligence.com/cifuzz/pkg/log"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"

	"code-intelligence.com/cifuzz/pkg/minijail"
	libfuzzer_parser "code-intelligence.com/cifuzz/pkg/parser/libfuzzer"
	"code-intelligence.com/cifuzz/pkg/report"
	fuzzer_runner "code-intelligence.com/cifuzz/pkg/runner"
	"code-intelligence.com/cifuzz/util/envutil"
	"code-intelligence.com/cifuzz/util/executil"
	"code-intelligence.com/cifuzz/util/sliceutil"
	"code-intelligence.com/cifuzz/util/stringutil"
)

const (
	MaxBufferedReports = 10
	sendTimeout        = time.Second * 10
	// ExitGracePeriod is time we give libfuzzer to exit after
	// max_total_time was exceeded.
	// Must be more than 2 seconds, because in the CI it happened that
	// libfuzzer did not exit within 2 seconds.
	ExitGracePeriod = time.Second * 5
)

type RunnerOptions struct {
	FuzzTarget          string
	SeedsDir            string
	AdditionalSeedsDirs []string
	Dictionary          string
	LibraryDirs         []string
	EnvVars             []string
	EngineArgs          []string
	FuzzTargetArgs      []string
	ReportHandler       report.Handler
	Timeout             time.Duration
	UseMinijail         bool
}

func (options *RunnerOptions) ValidateOptions() error {
	if options.UseMinijail {
		if !strings.HasPrefix(runtime.GOOS, "linux") {
			return errors.Errorf("Minijail is only supported on Linux")
		}

		// To be able to make the fuzz target accessible to minijail,
		// its path must be absolute and all symlinks must be resolved.
		var err error
		options.FuzzTarget, err = filepath.EvalSymlinks(options.FuzzTarget)
		if err != nil {
			return errors.WithStack(err)
		}
		options.FuzzTarget, err = filepath.Abs(options.FuzzTarget)
		if err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

type Runner struct {
	*RunnerOptions
	SupportJazzer bool
}

func NewRunner(options *RunnerOptions) *Runner {
	return &Runner{options, false}
}

func (r *Runner) Run(ctx context.Context) error {
	err := r.ValidateOptions()
	if err != nil {
		return err
	}

	args := []string{r.FuzzTarget}

	// Tell libfuzzer to exit after the timeout
	timeoutSeconds := strconv.FormatInt(int64(r.Timeout.Seconds()), 10)
	args = append(args, "-max_total_time="+timeoutSeconds)

	// Tell libfuzzer which dictionary it should use
	if r.Dictionary != "" {
		args = append(args, "-dict="+r.Dictionary)
	}

	// Add user-specified libfuzzer options
	args = append(args, r.EngineArgs...)

	// Tell libfuzzer which corpus directory it should use
	args = append(args, r.SeedsDir)

	// Add any additional corpus directories as further positional arguments
	args = append(args, r.AdditionalSeedsDirs...)

	if len(r.FuzzTargetArgs) > 0 {
		// separate the libfuzzer and fuzz target arguments with a "--"
		args = append(args, "--")
		args = append(args, r.FuzzTargetArgs...)
	}

	// The environment to run libfuzzer in
	fuzzerEnv, err := r.FuzzerEnvironment()
	if err != nil {
		return err
	}

	// The environment we run minijail in
	wrapperEnv := os.Environ()

	if r.UseMinijail {
		libfuzzerArgs := args

		// Make libfuzzer create artifacts (e.g. crash files) in the
		// minijail output directory.
		libfuzzerArgs = append(libfuzzerArgs, "-artifact_prefix="+minijail.OutputDir+"/")

		bindings := []*minijail.Binding{
			// The fuzz target must be accessible
			{Source: r.FuzzTarget},
			// The first corpus directory must be writable, because
			// libfuzzer writes new test inputs to it
			{Source: r.SeedsDir, Writable: minijail.ReadWrite},
		}

		for _, dir := range r.AdditionalSeedsDirs {
			bindings = append(bindings, &minijail.Binding{Source: dir})
		}

		// Set up Minijail
		mj, err := minijail.NewMinijail(&minijail.Options{
			Args:     libfuzzerArgs,
			Bindings: bindings,
			Env:      fuzzerEnv,
		})
		if err != nil {
			return err
		}
		defer mj.Cleanup()

		// Use the command which runs libfuzzer via minijail
		args = mj.Args
	} else {
		// We don't use minijail, so we can set the environment
		// variables for the fuzzer in the wrapper environment
		for key, value := range envutil.ToMap(fuzzerEnv) {
			wrapperEnv, err = envutil.Setenv(wrapperEnv, key, value)
			if err != nil {
				return err
			}
		}
	}

	return r.RunLibfuzzerAndReport(ctx, args, wrapperEnv)
}

func (r *Runner) RunLibfuzzerAndReport(ctx context.Context, args []string, env []string) error {
	// Ideally, libfuzzer exits on its own after the timeout, because we
	// specified `-max_total_time` above. For the case that it does not,
	// we still set up a timeout handler here which sends a SIGTERM and
	// later a SIGKILL if it still didn't exit.
	// To give libfuzzer some time to exit on its own, we send the
	// SIGTERM a bit later than the timeout specified via `-max_total_time`.
	var cmdCtx context.Context
	var cancelCmdCtx context.CancelFunc
	if r.Timeout > 0 {
		terminateTimeout := r.Timeout + ExitGracePeriod
		cmdCtx, cancelCmdCtx = context.WithTimeout(ctx, terminateTimeout)
	} else {
		// No timeout
		cmdCtx, cancelCmdCtx = context.WithCancel(ctx)
	}
	defer cancelCmdCtx()
	cmd := executil.CommandContext(cmdCtx, args[0], args[1:]...)
	cmd.TerminateProcessGroupWhenContextDone = true

	cmd.Env = env
	// Write the command's stdout to stderr in order to only have
	// reports printed to stdout.
	cmd.Stdout = os.Stderr
	// Write the command's stderr to both a pipe and os.Stderr, so that
	// we can parse the output but still allow the caller to observe the
	// status and progress in realtime.
	stderrPipe, err := cmd.StderrTeePipe()
	if err != nil {
		return err
	}

	log.Debugf("Command: %s", strings.Join(stringutil.QuotedStrings(cmd.Args), " "))
	err = cmd.Start()
	if err != nil {
		return err
	}

	reporter := libfuzzer_parser.NewLibfuzzerOutputParser(&libfuzzer_parser.Options{
		SupportJazzer: r.SupportJazzer,
	})
	reportsCh := make(chan *report.Report, MaxBufferedReports)

	// Start a go routine which waits for the command to exit and
	// continuously parses the output
	routines, routinesCtx := errgroup.WithContext(ctx)
	routines.Go(func() error {
		waitErrCh := make(chan error)

		// Wait for the command to exit in a go routine, so that below
		// we can cancel waiting when the context is done
		go func() {
			waitErrCh <- cmd.Wait()
		}()

		// Wait until the reporter has finished parsing stderr, so that
		// we can check below whether the reporter has found something
		err := reporter.Parse(routinesCtx, stderrPipe, reportsCh)
		if err != nil {
			return err
		}

		select {
		case err := <-waitErrCh:
			if cmd.TerminatedAfterContextDone() {
				// The command was terminated because the timeout exceeded. We
				// don't return an error in that case.
				return nil
			}
			if IsExpectedExitError(err) && reporter.FindingReported {
				// Libfuzzer found an error and exited with an expected error
				// code. We don't want to return an error in that case.
				return nil
			}
			return err
		case <-routinesCtx.Done():
			return routinesCtx.Err()
		}
	})

	// Continuously send reports from the reports channel to the
	// receiver. By doing this in a separate go routine and using a
	// buffered reports channel, this allows the LibfuzzerOutputParser
	// function above to not block when creating a report (as long as
	// the buffer is not full).
	routines.Go(func() error {
		senderErrCh := make(chan error, 1)

		go func() {
			senderErrCh <- sendReports(r.ReportHandler, reportsCh)
		}()

		select {
		case err := <-senderErrCh:
			return err
		case <-routinesCtx.Done():
			// The routines context got cancelled, so either the
			// command failed or the reporter encountered an error.
			// We give the sender a few seconds to send pending reports.
			select {
			case err := <-senderErrCh:
				return err
			case <-time.After(sendTimeout):
				return errors.Errorf("Sending reports timed out (%s)", sendTimeout)
			}
		}
	})

	return routines.Wait()
}

func (r *Runner) FuzzerEnvironment() ([]string, error) {
	env, err := fuzzer_runner.FuzzerEnvironment()
	if err != nil {
		return nil, err
	}

	env, err = fuzzer_runner.SetLDLibraryPath(env, r.LibraryDirs)
	if err != nil {
		return nil, err
	}

	// Add the user-specified environment variables. We do this after
	// setting our defaults but before setting sanitizer options,
	// because there we  take care of overriding options which we need
	// to override and keeping other options.
	env, err = fuzzer_runner.AddEnvFlags(env, r.EnvVars)
	if err != nil {
		return nil, err
	}

	env, err = fuzzer_runner.SetCommonUBSANOptions(env)
	if err != nil {
		return nil, err
	}

	env, err = fuzzer_runner.SetCommonASANOptions(env)
	if err != nil {
		return nil, err
	}

	return env, nil
}

func sendReports(handler report.Handler, reportsCh <-chan *report.Report) error {
	for r := range reportsCh {
		err := handler.Handle(r)
		if err != nil {
			return err
		}
	}
	return nil
}

func IsExpectedExitError(err error) bool {
	expectedExitCodes := []int{
		fuzzer_runner.SanitizerErrorExitCode,
		fuzzer_runner.LibFuzzerErrorExitCode,
		fuzzer_runner.LibFuzzerOOMExitCode,
		fuzzer_runner.LibFuzzerTimeoutExitCode,
	}
	exitError, ok := errors.Cause(err).(*exec.ExitError)
	if !ok {
		return false
	}
	return sliceutil.Contains(expectedExitCodes, exitError.ExitCode())
}