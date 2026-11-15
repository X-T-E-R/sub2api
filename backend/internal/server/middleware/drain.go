package middleware

import (
	"net/http"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

// DrainState atomically couples admission and the number of active handlers.
// The sign bit closes admission; the other bits count requests, including SSE
// handlers until their upstream consumption and usage bookkeeping have finished.
type DrainState struct{ state atomic.Int64 }

const drainBit int64 = -1 << 63

func NewDrainState() *DrainState { return &DrainState{} }

func (d *DrainState) BeginDrain() { d.state.Or(drainBit) }

// EndDrain cancels an admission hold before shutdown has started.
func (d *DrainState) EndDrain() { d.state.And(^drainBit) }

func (d *DrainState) Snapshot() (draining bool, inFlight int64) {
	state := d.state.Load()
	return state < 0, state & ^drainBit
}

func (d *DrainState) acquire() bool {
	for {
		state := d.state.Load()
		if state < 0 {
			return false
		}
		if d.state.CompareAndSwap(state, state+1) {
			return true
		}
	}
}

func (d *DrainState) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Liveness remains healthy throughout a drain. Readiness is separate.
		if c.Request.URL.Path == "/health" || c.Request.URL.Path == "/readyz" {
			c.Next()
			return
		}
		if !d.acquire() {
			c.Header("Retry-After", "1")
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "server_draining"})
			return
		}
		defer d.state.Add(-1)
		c.Next()
	}
}
