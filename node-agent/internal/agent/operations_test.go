package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type failingOperationPersister struct{}

func (p *failingOperationPersister) Persist(*NodeStatus) error {
	return errors.New("persist failed")
}

func (p *failingOperationPersister) LoadFromStateFile() (*NodeStatus, error) {
	return &NodeStatus{}, nil
}

func TestStartNodeAgentOperationDoesNotPublishBeforePersist(t *testing.T) {
	server := &NodeAgentServer{
		NodeStatus: &NodeStatus{},
		Persister:  &failingOperationPersister{},
	}

	_, err := server.startNodeAgentOperation(nodeAgentOperationSpec{
		ID:   "op-1",
		Type: "test-operation",
	}, func(context.Context, nodeAgentOperationProgress) error {
		return nil
	})

	assert.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Empty(t, server.NodeStatus.NodeAgentOperations)
	assert.Empty(t, server.activeOperations)
}
