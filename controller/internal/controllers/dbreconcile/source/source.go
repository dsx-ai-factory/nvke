// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package source

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	crsource "sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	DefaultBufferSize = 1024
)

type EventSource[T comparable] interface {
	Start(ctx context.Context, enqueue func(ctx context.Context, key T) error) error
}

type Source[T comparable] struct {
	events       chan event.TypedGenericEvent[T]
	eventSources []EventSource[T]
}

func NewSource[T comparable](bufferSize int, eventSources ...EventSource[T]) *Source[T] {
	if bufferSize <= 0 {
		bufferSize = DefaultBufferSize
	}

	return &Source[T]{
		events:       make(chan event.TypedGenericEvent[T], bufferSize),
		eventSources: append([]EventSource[T](nil), eventSources...),
	}
}

func (s *Source[T]) Enqueue(ctx context.Context, key T) error {
	select {
	case s.events <- event.TypedGenericEvent[T]{Object: key}:
		return nil
	case <-ctx.Done():
		return errors.Join(ctx.Err(), fmt.Errorf("failed to enqueue DB reconcile event"))
	}
}

func (s *Source[T]) Start(ctx context.Context, queue workqueue.TypedRateLimitingInterface[T]) error {
	inner := crsource.TypedChannel[T, T](
		s.events,
		handler.TypedEnqueueRequestsFromMapFunc(func(_ context.Context, key T) []T {
			return []T{key}
		}),
	)

	if err := inner.Start(ctx, queue); err != nil {
		return err
	}

	for _, eventSource := range s.eventSources {
		if err := eventSource.Start(ctx, s.Enqueue); err != nil {
			return fmt.Errorf("failed to start DB reconcile event source: %w", err)
		}
	}

	return nil
}
