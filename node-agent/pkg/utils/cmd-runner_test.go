package utils

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestGetHostname(t *testing.T) {
	hostname := GetHostname()

	// Should return a non-empty string on most systems
	assert.NotEmpty(t, hostname)

	// Should match os.Hostname()
	expectedHostname, err := os.Hostname()
	if err == nil {
		assert.Equal(t, expectedHostname, hostname)
	}
}

func TestCommandOptionsStruct(t *testing.T) {
	// Test that CommandOptions can be created with all fields
	var stdin bytes.Buffer
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	opts := &CommandOptions{
		Env:    []string{"FOO=bar", "BAZ=qux"},
		Dir:    "/tmp",
		Stdin:  &stdin,
		Stdout: &stdout,
		Stderr: &stderr,
	}

	assert.Equal(t, 2, len(opts.Env))
	assert.Equal(t, "/tmp", opts.Dir)
	assert.NotNil(t, opts.Stdin)
	assert.NotNil(t, opts.Stdout)
	assert.NotNil(t, opts.Stderr)
}

func TestCommandRunnerInterface(t *testing.T) {
	// Verify ExecRunner implements CommandRunner interface
	var runner CommandRunner = &ExecRunner{}
	assert.NotNil(t, runner)
}

func TestExecRunnerRunSimpleCommand(t *testing.T) {
	runner := &ExecRunner{}

	output, err := runner.Run("echo", []string{"hello", "world"}, nil)

	assert.NoError(t, err)
	assert.Contains(t, string(output), "hello world")
}

func TestExecRunnerRunWithEnv(t *testing.T) {
	runner := &ExecRunner{}

	opts := &CommandOptions{
		Env: append(os.Environ(), "TEST_VAR=test_value"),
	}

	// Use printenv to print a specific variable
	output, err := runner.Run("printenv", []string{"TEST_VAR"}, opts)

	assert.NoError(t, err)
	assert.Contains(t, string(output), "test_value")
}

func TestExecRunnerRunWithDir(t *testing.T) {
	runner := &ExecRunner{}
	tmpDir := t.TempDir()

	opts := &CommandOptions{
		Dir: tmpDir,
	}

	// pwd should return the specified directory
	output, err := runner.Run("pwd", []string{}, opts)

	assert.NoError(t, err)
	assert.Contains(t, string(output), tmpDir)
}

func TestExecRunnerRunWithStdin(t *testing.T) {
	runner := &ExecRunner{}

	input := "hello from stdin"
	opts := &CommandOptions{
		Stdin: strings.NewReader(input),
	}

	// cat should echo back stdin
	output, err := runner.Run("cat", []string{}, opts)

	assert.NoError(t, err)
	assert.Equal(t, input, string(output))
}

func TestExecRunnerRunCommandNotFound(t *testing.T) {
	runner := &ExecRunner{}

	output, err := runner.Run("nonexistent-command-xyz", []string{}, nil)

	assert.Error(t, err)
	// Output might be empty or contain error message
	_ = output
}

func TestExecRunnerRunCommandFails(t *testing.T) {
	runner := &ExecRunner{}

	// false command always exits with status 1
	output, err := runner.Run("false", []string{}, nil)

	assert.Error(t, err)
	// false doesn't produce output
	assert.Empty(t, output)
}

func TestExecRunnerRunWithNilOptions(t *testing.T) {
	runner := &ExecRunner{}

	// Should work with nil options
	output, err := runner.Run("echo", []string{"test"}, nil)

	assert.NoError(t, err)
	assert.Contains(t, string(output), "test")
}

func TestExecRunnerRunWithEmptyOptions(t *testing.T) {
	runner := &ExecRunner{}

	opts := &CommandOptions{}

	output, err := runner.Run("echo", []string{"test"}, opts)

	assert.NoError(t, err)
	assert.Contains(t, string(output), "test")
}

func TestExecRunnerRunWithMultipleArgs(t *testing.T) {
	runner := &ExecRunner{}

	output, err := runner.Run("echo", []string{"arg1", "arg2", "arg3"}, nil)

	assert.NoError(t, err)
	assert.Contains(t, string(output), "arg1 arg2 arg3")
}

func TestExecRunnerRunWithEmptyArgs(t *testing.T) {
	runner := &ExecRunner{}

	output, err := runner.Run("echo", []string{}, nil)

	assert.NoError(t, err)
	// echo with no args just outputs newline
	assert.Equal(t, "\n", string(output))
}

func TestExecRunnerRunDetached(t *testing.T) {
	runner := &ExecRunner{}

	// Create a temp file to verify the command ran
	tmpDir := t.TempDir()
	markerFile := filepath.Join(tmpDir, "marker.txt")

	// Run a command that creates a file
	err := runner.RunDetached("touch", []string{markerFile}, nil)

	assert.NoError(t, err)

	// Poll until the file is created
	assert.Eventually(t, func() bool {
		_, err := os.Stat(markerFile)
		return err == nil
	}, time.Second, 10*time.Millisecond, "marker file should be created")
}

func TestExecRunnerRunDetachedWithDir(t *testing.T) {
	runner := &ExecRunner{}
	tmpDir := t.TempDir()

	opts := &CommandOptions{
		Dir: tmpDir,
	}

	// Create a file in the specified directory
	err := runner.RunDetached("touch", []string{"detached-test.txt"}, opts)

	assert.NoError(t, err)

	// Poll until file is created in the specified directory
	markerFile := filepath.Join(tmpDir, "detached-test.txt")
	assert.Eventually(t, func() bool {
		_, err := os.Stat(markerFile)
		return err == nil
	}, time.Second, 10*time.Millisecond, "marker file should be created in specified directory")
}

func TestExecRunnerRunDetachedWithEnv(t *testing.T) {
	runner := &ExecRunner{}
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "env-output.txt")

	opts := &CommandOptions{
		Env: append(os.Environ(), "DETACHED_TEST=success"),
	}

	// Use shell to write env var to file
	err := runner.RunDetached("/bin/sh", []string{"-c", "echo $DETACHED_TEST > " + outputFile}, opts)

	assert.NoError(t, err)

	// Poll until file contains expected content
	assert.Eventually(t, func() bool {
		content, err := os.ReadFile(outputFile)
		return err == nil && strings.Contains(string(content), "success")
	}, time.Second, 10*time.Millisecond, "output file should contain env var value")
}

func TestExecRunnerRunDetachedWithNilOptions(t *testing.T) {
	runner := &ExecRunner{}
	tmpDir := t.TempDir()
	markerFile := filepath.Join(tmpDir, "nil-opts-marker.txt")

	// Should work with nil options
	err := runner.RunDetached("touch", []string{markerFile}, nil)

	assert.NoError(t, err)

	// Poll until file is created
	assert.Eventually(t, func() bool {
		_, err := os.Stat(markerFile)
		return err == nil
	}, time.Second, 10*time.Millisecond, "marker file should be created with nil options")
}

func TestExecRunnerRunDetachedCommandNotFound(t *testing.T) {
	runner := &ExecRunner{}

	// This should return an error since the command doesn't exist
	err := runner.RunDetached("nonexistent-command-xyz", []string{}, nil)

	// Start() returns error if command cannot be started
	assert.Error(t, err)
}

func TestExecRunnerRunDetachedWithStdout(t *testing.T) {
	runner := &ExecRunner{}
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "stdout-output.txt")

	// Write stdout to a file to avoid data race with bytes.Buffer
	err := runner.RunDetached("/bin/sh", []string{"-c", "echo captured > " + outputFile}, nil)

	assert.NoError(t, err)

	// Poll until file contains the expected output
	assert.Eventually(t, func() bool {
		content, err := os.ReadFile(outputFile)
		return err == nil && strings.Contains(string(content), "captured")
	}, time.Second, 10*time.Millisecond, "stdout should contain captured output")
}

func TestExecRunnerRunDetachedWithStderr(t *testing.T) {
	runner := &ExecRunner{}
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "stderr-output.txt")

	// Write stderr to a file to avoid data race with bytes.Buffer
	// Use a subshell so the stderr redirect captures the inner command's stderr
	err := runner.RunDetached("/bin/sh", []string{"-c", "(echo error >&2) 2> " + outputFile}, nil)

	assert.NoError(t, err)

	// Poll until file contains the expected output
	assert.Eventually(t, func() bool {
		content, err := os.ReadFile(outputFile)
		return err == nil && strings.Contains(string(content), "error")
	}, time.Second, 10*time.Millisecond, "stderr should contain error output")
}

func TestExecRunnerRunCombinedOutput(t *testing.T) {
	runner := &ExecRunner{}

	// Command that outputs to both stdout and stderr
	output, err := runner.Run("/bin/sh", []string{"-c", "echo stdout; echo stderr >&2"}, nil)

	assert.NoError(t, err)
	// CombinedOutput should contain both
	assert.Contains(t, string(output), "stdout")
	assert.Contains(t, string(output), "stderr")
}

func TestExecRunnerRunSplitOutput(t *testing.T) {
	runner := &ExecRunner{}

	output, err := runner.RunSplitOutput("/bin/sh", []string{"-c", "echo stdout; echo stderr >&2"}, nil)

	assert.NoError(t, err)
	assert.Contains(t, string(output.Stdout), "stdout")
	assert.NotContains(t, string(output.Stdout), "stderr")
	assert.Contains(t, string(output.Stderr), "stderr")
	assert.NotContains(t, string(output.Stderr), "stdout")
}

func TestExecRunnerRunExitCode(t *testing.T) {
	runner := &ExecRunner{}

	// Test command that exits with specific code
	_, err := runner.Run("/bin/sh", []string{"-c", "exit 42"}, nil)

	assert.Error(t, err)
	// The error should contain exit status information
	assert.Contains(t, err.Error(), "exit")
}

func TestExecRunnerRunWithAllOptions(t *testing.T) {
	runner := &ExecRunner{}
	tmpDir := t.TempDir()

	input := "test input"
	opts := &CommandOptions{
		Env:   append(os.Environ(), "ALL_OPTS_TEST=yes"),
		Dir:   tmpDir,
		Stdin: strings.NewReader(input),
	}

	// Complex command that uses all options
	output, err := runner.Run("/bin/sh", []string{"-c", "pwd && echo $ALL_OPTS_TEST && cat"}, opts)

	assert.NoError(t, err)
	assert.Contains(t, string(output), tmpDir)
	assert.Contains(t, string(output), "yes")
	assert.Contains(t, string(output), input)
}

func TestCommandOptionsEnvOverride(t *testing.T) {
	runner := &ExecRunner{}

	// Override an existing env var
	opts := &CommandOptions{
		Env: []string{"HOME=/custom/home"},
	}

	output, err := runner.Run("printenv", []string{"HOME"}, opts)

	assert.NoError(t, err)
	assert.Contains(t, string(output), "/custom/home")
}

func TestExecRunnerRunLargeOutput(t *testing.T) {
	runner := &ExecRunner{}

	// Generate large output (10000 lines)
	output, err := runner.Run("/bin/sh", []string{"-c", "for i in $(seq 1 10000); do echo line$i; done"}, nil)

	assert.NoError(t, err)
	lines := strings.Split(string(output), "\n")
	// Should have at least 10000 lines
	assert.GreaterOrEqual(t, len(lines), 10000)
}

func TestExecRunnerRunWithSpecialCharacters(t *testing.T) {
	runner := &ExecRunner{}

	// Test with special characters in arguments
	specialChars := "hello 'world' \"quoted\" $var `backtick`"
	output, err := runner.Run("echo", []string{specialChars}, nil)

	assert.NoError(t, err)
	// echo should preserve the literal string
	assert.Contains(t, string(output), specialChars)
}
