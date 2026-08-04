package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"time"

	pb "github.com/NVIDIA/nke/node-agent/pkg/agent/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	address := flag.String("address", "localhost:15005", "The server address in the format of host:port")
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

	// Create a test script that outputs numbers with a delay
	script := `#!/bin/bash
for i in {1..5}; do
    echo "Line $i"
    sleep 1
done
echo "Error message" >&2
exit 0`

	// Execute the script with streaming
	stream, err := c.StreamExecuteScript(ctx, &pb.ExecuteScriptRequest{
		ScriptName:     "test-stream.sh",
		ScriptContents: script,
	})
	if err != nil {
		log.Fatalf("could not execute script: %v", err)
	}

	fmt.Println("Receiving script output:")
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
		}
		if chunk.Error != "" {
			fmt.Printf("STDERR: %s", chunk.Error)
		}
		if chunk.IsComplete {
			fmt.Printf("\nScript completed with exit code: %d\n", chunk.ExitCode)
			break
		}
	}
}
