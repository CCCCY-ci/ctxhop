//go:build !windows

package main

type updateReplacementResult struct {
	targetPath  string
	scheduled   bool
	keepWorkDir bool
}

func replaceCurrentExecutable(sourcePath, _ string) (updateReplacementResult, error) {
	targetPath, err := currentExecutablePath()
	if err != nil {
		return updateReplacementResult{}, err
	}
	if err := installExecutableFile(sourcePath, targetPath); err != nil {
		return updateReplacementResult{}, err
	}
	return updateReplacementResult{targetPath: targetPath}, nil
}
