package utils

import (
	"bytes"
	"io"
	"log"
	"os"
	"os/exec"
	"syscall"
)

type CommandRunner interface {
	Run(name string, args []string, opts *CommandOptions) ([]byte, error)
	RunSplitOutput(name string, args []string, opts *CommandOptions) (CommandOutput, error)
	RunDetached(name string, args []string, opts *CommandOptions) error
}

type CommandOutput struct {
	Stdout []byte
	Stderr []byte
}

func GetHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return ""
	}
	return hostname
}

type CommandOptions struct {
	Env    []string
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type ExecRunner struct{}

/*
func (r *ExecRunner) Run(name string, args []string, opts *CommandOptions) ([]byte, error) {
	log.Printf("attempting to run command: name=%s, args=%s\n", name, args)

	cmd := exec.Command("chmod +x", name)
	if opts != nil {
		if opts.Env != nil {
			cmd.Env = opts.Env
		}
		if opts.Dir != "" {
			cmd.Dir = opts.Dir
		}
		if opts.Stdin != nil {
			cmd.Stdin = opts.Stdin
		}
	}

	cmd = exec.Command(name, args...)
	return cmd.CombinedOutput()
}
*/

func (r *ExecRunner) Run(name string, args []string, opts *CommandOptions) ([]byte, error) {
	log.Printf("attempting to run command: name=%s, args=%v\n", name, args)
	log.Printf("cmd CommandOptions: %v\n", opts)

	cmd := exec.Command(name, args...)
	if opts != nil {
		if opts.Env != nil {
			cmd.Env = opts.Env
		}
		if opts.Dir != "" {
			cmd.Dir = opts.Dir
		}
		if opts.Stdin != nil {
			if stdin, ok := opts.Stdin.(interface {
				Read(p []byte) (n int, err error)
			}); ok {
				cmd.Stdin = stdin
			}
		}
	}

	return cmd.CombinedOutput()
}

func (r *ExecRunner) RunSplitOutput(name string, args []string, opts *CommandOptions) (CommandOutput, error) {
	log.Printf("attempting to run command: name=%s, args=%v\n", name, args)
	log.Printf("cmd CommandOptions: %v\n", opts)

	cmd := exec.Command(name, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if opts != nil {
		if opts.Env != nil {
			cmd.Env = opts.Env
		}
		if opts.Dir != "" {
			cmd.Dir = opts.Dir
		}
		if opts.Stdin != nil {
			if stdin, ok := opts.Stdin.(interface {
				Read(p []byte) (n int, err error)
			}); ok {
				cmd.Stdin = stdin
			}
		}
	}

	err := cmd.Run()
	return CommandOutput{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, err
}

func (r *ExecRunner) RunDetached(name string, args []string, opts *CommandOptions) error {
	log.Printf("starting detached command: name=%s, args=%s\n", name, args)

	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if opts != nil {
		if opts.Env != nil {
			cmd.Env = opts.Env
		}
		if opts.Dir != "" {
			cmd.Dir = opts.Dir
		}
		if opts.Stdin != nil {
			cmd.Stdin = opts.Stdin
		}
		if opts.Stdout != nil {
			cmd.Stdout = opts.Stdout
		}
		if opts.Stderr != nil {
			cmd.Stderr = opts.Stderr
		}
	} else {
		// Default to discarding output if none provided
		cmd.Stdout = nil
		cmd.Stderr = nil
	}

	return cmd.Start() // <- does NOT wait
}

// ResolvePython returns the path to a working Python interpreter,
// preferring "python3" over "python". Many systems (e.g. Ubuntu 22.04+)
// only ship python3 and do not provide an unversioned "python" symlink.
func ResolvePython() string {
	if p, err := exec.LookPath("python3"); err == nil {
		return p
	}
	if p, err := exec.LookPath("python"); err == nil {
		return p
	}
	// Fall back to "python3" and let the caller handle the exec error.
	return "python3"
}
