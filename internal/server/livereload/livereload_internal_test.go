package livereload

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// startTimeout bounds how long a test waits for something that must happen.
	startTimeout = 5 * time.Second
	// settleTime is how long a test waits to prove something does not happen.
	settleTime = 100 * time.Millisecond
)

func TestStartOnce(t *testing.T) {
	t.Run("sequential calls start once", func(t *testing.T) {
		var once sync.Once
		var count atomic.Int32
		started := make(chan struct{}, 2)
		start := func() {
			count.Add(1)
			started <- struct{}{}
		}

		startOnce(&once, start)
		startOnce(&once, start)

		requireStartedOnce(t, started, &count)
	})

	t.Run("concurrent calls start once", func(t *testing.T) {
		var once sync.Once
		var count atomic.Int32
		started := make(chan struct{}, 8)
		start := func() {
			count.Add(1)
			started <- struct{}{}
		}

		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() { startOnce(&once, start) })
		}
		wg.Wait()

		requireStartedOnce(t, started, &count)
	})
}

func TestInitialize_runsHubOnce(t *testing.T) {
	Initialize()
	Initialize()

	// Do runs its function synchronously and only if the Once is still unused: Initialize
	// consumed hubOnce exactly when this function is skipped.
	consumed := true
	hubOnce.Do(func() { consumed = false })
	require.True(t, consumed, "Initialize did not guard the hub start with hubOnce")

	c := &connection{send: make(chan []byte, 1)}
	timeout := time.After(startTimeout)

	select {
	case wsHub.register <- c:
	case <-timeout:
		require.FailNow(t, "hub did not accept the register request")
	}
	select {
	case wsHub.unregister <- c:
	case <-timeout:
		require.FailNow(t, "hub did not accept the unregister request")
	}

	// unregister closes the connection's send channel: only a running hub does that.
	select {
	case _, ok := <-c.send:
		assert.False(t, ok)
	case <-timeout:
		require.FailNow(t, "hub did not process the unregister request")
	}
}

func requireStartedOnce(t *testing.T, started <-chan struct{}, count *atomic.Int32) {
	t.Helper()

	select {
	case <-started:
	case <-time.After(startTimeout):
		require.FailNow(t, "start was never called")
	}

	select {
	case <-started:
		require.FailNow(t, "start was called more than once")
	case <-time.After(settleTime):
	}
	assert.Equal(t, int32(1), count.Load())
}
