package livereload

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

		select {
		case <-started:
		case <-t.Context().Done():
			require.FailNow(t, "start was never called")
		}
		assert.Equal(t, int32(1), count.Load())
		assert.Empty(t, started)
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

		select {
		case <-started:
		case <-t.Context().Done():
			require.FailNow(t, "start was never called")
		}
		assert.Equal(t, int32(1), count.Load())
		assert.Empty(t, started)
	})
}

func TestInitialize_runsHubOnce(t *testing.T) {
	Initialize()
	Initialize()

	c := &connection{send: make(chan []byte, 1)}
	wsHub.register <- c
	wsHub.unregister <- c

	// unregister closes the connection's send channel: only a running hub does that.
	select {
	case _, ok := <-c.send:
		assert.False(t, ok)
	case <-t.Context().Done():
		require.FailNow(t, "hub did not process the unregister request")
	}
}
