// SPDX-License-Identifier: MIT

package executor

// Fork returns file commands that share the directory and the root of f with
// a suffix of their own: every action step has its own manager in the runner
// (HostContext.CreateService<IFileCommandManager>(), ActionRunner.cs:173), so
// the steps of a composite action do not delete the files of the step around
// them. The fork must not be closed.
func (f *FileCommands) Fork() *FileCommands {
	return &FileCommands{dir: f.dir, root: f.root, owner: f.owner}
}

// Cleanup removes the files of the current step of a fork.
func (f *FileCommands) Cleanup() {
	if f.suffix == "" {
		return
	}
	for _, c := range fileCommands {
		_ = f.root.Remove(f.name(c, f.suffix))
	}
}
