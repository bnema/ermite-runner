// SPDX-License-Identifier: MIT

package executor

import "time"

// Conclusion is how a step or a job ended, with the names of the protocol
// (CiConclusion in ermite-api).
type Conclusion string

// Conclusions.
const (
	ConclusionSuccess   Conclusion = "success"
	ConclusionFailure   Conclusion = "failure"
	ConclusionCancelled Conclusion = "cancelled"
	ConclusionSkipped   Conclusion = "skipped"
	ConclusionTimedOut  Conclusion = "timed_out" // jobs only
)

// Reporter receives what happens in a job. The executor calls it from several
// goroutines but never concurrently: calls are serialised.
//
// Steps are numbered like GitHub's log: "Set up job" is 1, the steps of the
// workflow follow, and "Complete job" is last. Every log line belongs to the
// step that is running.
type Reporter interface {
	// StepStarted announces a step that begins to run, with its display name.
	// A step skipped by its condition gets StepStarted and StepCompleted too.
	StepStarted(number int, name string, at time.Time)
	// Log is one line of the job log, without its line terminator.
	Log(number int, at time.Time, line string)
	// StepCompleted ends a step.
	StepCompleted(number int, name string, conclusion Conclusion, at time.Time)
	// JobCompleted is the last call: the result of the job.
	JobCompleted(Result)
}

// Result is the outcome of a job.
type Result struct {
	Conclusion Conclusion
	// Outputs are the job outputs, evaluated at the end of the job. Empty values
	// are left out, like the official runner does.
	Outputs map[string]string
	// Error is a job-level error (a timeout, a workflow that cannot be run);
	// step errors are in the log.
	Error string
	// RunnerShutdown is true when the job was cut short because the runner was
	// told to stop (see ErrShutdown).
	RunnerShutdown bool
}
