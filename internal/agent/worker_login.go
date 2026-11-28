package agent

// WorkerLoginStatus is an opt-in contract for a worker's final login check.
// The command's clean zero exit must certify that this configuration is logged
// in. The resident runs it under the same bounded process/EOF guard as login;
// output is bounded and discarded, never included in diagnostics or events.
// Adapters whose status command requires interpreting output must not opt in
// until that interpretation and its real CLI captures are implemented.
type WorkerLoginStatus interface {
	LoginStatusLaunch(Spec) (Launch, error)
}
