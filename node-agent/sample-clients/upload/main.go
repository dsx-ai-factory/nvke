package main

import (
	"context"
	"io"
	"log"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/NVIDIA/nke/node-agent/pkg/agent/proto"
)

func main() {
	cc, err := grpc.NewClient(
		"10.105.179.103:15005",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatalf("could not connect: %v", err)
	}
	defer cc.Close() //nolint:errcheck

	c := pb.NewNodeAgentClient(cc)

	file, err := os.Open("./test-bundle.tgz")
	if err != nil {
		log.Printf("failed to open test-bundle.tgz: %v", err)
		log.Fatal(err)
	}
	defer file.Close() //nolint:errcheck

	stream, err := c.UploadFileBundle(context.Background())
	if err != nil {
		log.Printf("failed call to UploadFileBundle: %v", err)
		log.Fatal(err)
	}

	buf := make([]byte, 1024*1024)
	n, err := file.Read(buf)
	if err != nil && err != io.EOF {
		log.Printf("failed read buffer: %v", err)
		log.Fatal(err)
	}

	err = stream.Send(&pb.FileChunk{
		Data:        buf[:n],
		Destination: "/tmp/test-bundle",
		Filename:    "test-bundle.tgz",
	})
	if err != nil {
		log.Printf("failed send chunk: %v", err)
		log.Fatal(err)
	}

	for {
		n, err := file.Read(buf)
		if err == io.EOF {
			log.Printf("finished reading file")
			break
		}

		if err != nil {
			log.Fatal(err)
		}

		err = stream.Send(&pb.FileChunk{
			Data:        buf[:n],
			Destination: "/tmp/test-bundle/test-bundle.tgz",
		})
		if err != nil {
			log.Printf("failed send chunk: %v", err)
			log.Fatal(err)
		}
	}

	response, err := stream.CloseAndRecv()
	if err != nil {
		log.Printf("failed close stream: %v", err)
		log.Fatal(err)
	}
	log.Printf("Response: %v", response)
}
