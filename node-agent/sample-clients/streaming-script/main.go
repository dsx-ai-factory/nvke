package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/NVIDIA/nke/node-agent/pkg/agent/proto"
)

func main() {
	address := flag.String(
		"address",
		"localhost:15005",
		"The server address in the format of host:port",
	)
	testCase := flag.String(
		"test",
		"basic",
		"Test case to run: basic, fast-exit, error-exit, mixed-output, massive-output, instant-exit, final-stderr, stress-test, exit-code-race",
	)
	flag.Parse()

	// Set up a connection to the server
	conn, err := grpc.NewClient(*address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	defer conn.Close() //nolint:errcheck
	c := pb.NewNodeAgentClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Select test script based on test case
	var script string
	switch *testCase {
	case "basic":
		// Basic test with delayed output
		script = `#!/bin/bash
for i in {1..5}; do
    echo "Line $i"
    sleep 1
done
echo "Final line"
exit 0`
		fmt.Println("Running basic test (slow output)...")

	case "fast-exit":
		// Tests that final output isn't lost when command exits quickly
		// This exercises the output draining logic we fixed
		script = `#!/bin/bash
for i in {1..20}; do
    echo "Fast output line $i"
done
echo "LAST LINE - should not be lost"
exit 0`
		fmt.Println("Running fast-exit test (tests output draining)...")

	case "error-exit":
		// Tests non-zero exit code handling
		script = `#!/bin/bash
echo "Starting process..."
echo "Error occurred!" >&2
echo "Cleaning up..."
exit 42`
		fmt.Println("Running error-exit test (exit code 42)...")

	case "mixed-output":
		// Tests interleaved stdout/stderr
		script = `#!/bin/bash
for i in {1..5}; do
    echo "STDOUT: Line $i"
    echo "STDERR: Line $i" >&2
done
echo "Final stdout"
echo "Final stderr" >&2
exit 0`
		fmt.Println("Running mixed-output test (interleaved stdout/stderr)...")

	case "massive-output":
		// Generates 10000 lines as fast as possible to stress output draining
		// This increases the chance of a race condition where the command exits
		// before all output is drained from the channels
		script = `#!/bin/bash
for i in {1..10000}; do
    echo "Output line $i"
done
echo "=== FINAL LINE - MUST NOT BE LOST ==="
exit 0`
		fmt.Println("Running massive-output test (10000 lines, tests race condition)...")

	case "instant-exit":
		// Process that outputs and exits as fast as possible
		// Maximum pressure on the output draining logic
		script = `#!/bin/bash
echo "Line 1"; echo "Line 2"; echo "Line 3"; echo "Line 4"; echo "Line 5"
echo "Line 6"; echo "Line 7"; echo "Line 8"; echo "Line 9"; echo "Line 10"
echo "FINAL LINE"
exit 0`
		fmt.Println("Running instant-exit test (exits immediately after output)...")

	case "final-stderr":
		// Tests if stderr output right before exit is lost
		// Specifically tests the case where stderr is the last thing written
		script = `#!/bin/bash
echo "Starting..."
for i in {1..100}; do
    echo "stdout $i"
done
echo "About to write final stderr..."
echo "=== CRITICAL STDERR - MUST NOT BE LOST ===" >&2
exit 42`
		fmt.Println("Running final-stderr test (critical stderr at end with non-zero exit)...")

	case "stress-test":
		// Maximum stress: lots of output on both streams, non-zero exit
		// Tests double cmd.Wait() bug AND output draining simultaneously
		script = `#!/bin/bash
for i in {1..5000}; do
    echo "STDOUT $i"
    if [ $((i % 100)) -eq 0 ]; then
        echo "STDERR checkpoint $i" >&2
    fi
done
echo "FINAL STDOUT LINE"
echo "FINAL STDERR LINE" >&2
exit 137`
		fmt.Println("Running stress-test (5000 lines mixed, exit code 137, tests all bugs)...")

	case "exit-code-race":
		// Specifically targets the cmd.Wait() / pipe race condition.
		// cmd.Wait() closes pipes, which can cause scanner goroutines to
		// close their channels before doneChan is selected. If the select
		// loop processes the channel closures first, cmdErr is still nil
		// and the exit code is swallowed as 0.
		//
		// We run this 20 times because the race is timing-dependent —
		// a single run may pass by luck.
		// Three sub-tests with increasing output volume. More output means
		// the scanner goroutines are more likely to be mid-send when
		// cmd.Wait() closes the pipes, forcing select to pick a
		// channel-close case before doneChan.
		subtests := []struct {
			name   string
			script string
		}{
			{
				name: "no-output",
				script: `#!/bin/bash
exit 7`,
			},
			{
				name: "small-output",
				script: `#!/bin/bash
for i in $(seq 1 50); do echo "line $i"; done
exit 7`,
			},
			{
				name: "large-output",
				script: `#!/bin/bash
for i in $(seq 1 5000); do echo "line $i"; done
echo "FINAL" >&2
exit 7`,
			},
		}

		fmt.Println("Running exit-code-race test (3 sub-tests x 50 iterations, expects exit code 7)...")
		totalFailures := 0
		for _, st := range subtests {
			failures := 0
			for i := 1; i <= 50; i++ {
				s, sErr := c.StreamExecuteScript(ctx, &pb.ExecuteScriptRequest{
					ScriptName:     fmt.Sprintf("exit-race-%s-%d.sh", st.name, i),
					ScriptContents: st.script,
				})
				if sErr != nil {
					log.Fatalf("[%s] iteration %d: could not execute script: %v", st.name, i, sErr)
				}
				gotFinal := false
				for {
					chunk, rErr := s.Recv()
					if rErr == io.EOF {
						break
					}
					if rErr != nil {
						log.Fatalf("[%s] iteration %d: recv error: %v", st.name, i, rErr)
					}
					if chunk.IsComplete {
						gotFinal = true
						if chunk.ExitCode != 7 {
							fmt.Printf("  [%s] iteration %d: FAIL exit code = %d (expected 7)\n", st.name, i, chunk.ExitCode)
							failures++
						}
						break
					}
				}
				if !gotFinal {
					fmt.Printf("  [%s] iteration %d: FAIL no final chunk (stream ended without IsComplete)\n", st.name, i)
					failures++
				}
			}
			status := "✅"
			if failures > 0 {
				status = "❌"
			}
			fmt.Printf("  %s [%s]: %d/50 failures\n", status, st.name, failures)
			totalFailures += failures
		}
		if totalFailures > 0 {
			fmt.Printf("❌ FAILED: %d/150 total iterations returned wrong exit code — race condition confirmed!\n", totalFailures)
		} else {
			fmt.Printf("✅ PASSED: All 150 iterations returned exit code 7\n")
		}
		return

	default:
		log.Fatalf("Unknown test case: %s", *testCase)
	}

	// Execute the script with streaming
	stream, err := c.StreamExecuteScript(ctx, &pb.ExecuteScriptRequest{
		ScriptName:     "test-stream.sh",
		ScriptContents: script,
	})
	if err != nil {
		log.Fatalf("could not execute script: %v", err)
	}

	fmt.Println("Receiving script output:")
	outputLines := 0
	errorLines := 0

	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatalf("failed to receive chunk: %v", err)
		}

		if chunk.Output != "" {
			fmt.Printf("STDOUT: %s", chunk.Output)
			outputLines++
		}
		if chunk.Error != "" {
			fmt.Printf("STDERR: %s", chunk.Error)
			errorLines++
		}
		if chunk.IsComplete {
			fmt.Printf("\n=== Completion ===\n")
			fmt.Printf("Exit code: %d\n", chunk.ExitCode)
			fmt.Printf("STDOUT lines: %d\n", outputLines)
			fmt.Printf("STDERR lines: %d\n", errorLines)

			// Validate test-specific expectations
			success := true
			switch *testCase {
			case "fast-exit":
				if outputLines < 21 {
					fmt.Printf("❌ FAILED: Expected 21 stdout lines but got %d - output was lost!\n", outputLines)
					success = false
				}
			case "massive-output":
				if outputLines < 10001 {
					fmt.Printf("❌ FAILED: Expected 10001 stdout lines but got %d - output was lost!\n", outputLines)
					success = false
				}
			case "instant-exit":
				if outputLines < 11 {
					fmt.Printf("❌ FAILED: Expected 11 stdout lines but got %d - output was lost!\n", outputLines)
					success = false
				}
			case "error-exit":
				if chunk.ExitCode != 42 {
					fmt.Printf("❌ FAILED: Expected exit code 42 but got %d - double cmd.Wait() bug likely!\n", chunk.ExitCode)
					success = false
				}
			case "final-stderr":
				if chunk.ExitCode != 42 {
					fmt.Printf("❌ FAILED: Expected exit code 42 but got %d - double cmd.Wait() bug!\n", chunk.ExitCode)
					success = false
				}
				if errorLines < 1 {
					fmt.Printf("❌ FAILED: Expected stderr output but got none - stderr was lost!\n")
					success = false
				}
			case "stress-test":
				if chunk.ExitCode != 137 {
					fmt.Printf("❌ FAILED: Expected exit code 137 but got %d - double cmd.Wait() bug!\n", chunk.ExitCode)
					success = false
				}
				if outputLines < 5001 {
					fmt.Printf("❌ FAILED: Expected 5001 stdout lines but got %d - output was lost!\n", outputLines)
					success = false
				}
				if errorLines < 51 {
					fmt.Printf("❌ FAILED: Expected ~51 stderr lines but got %d - stderr was lost!\n", errorLines)
					success = false
				}
			case "exit-code-race":
				// Handled in its own loop above; should never reach here.
			}

			if success {
				fmt.Printf("✅ PASSED: All expectations met!\n")
			}
			break
		}
	}
}
