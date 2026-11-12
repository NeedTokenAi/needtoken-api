package cloudflareprofile

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

const runtimeStatusPath = "/internal/cloudflare/runtime"

// Gate admits user traffic only after the supervisor has beaten at least
// once and still holds the fence. The status path stays available so the
// Durable Object can see that zero user traffic did not stop the supervisor.
func (rt *Runtime) Gate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.Path == runtimeStatusPath {
			c.Next()
			return
		}
		if !rt.tryEnter() {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		defer rt.leave()
		c.Next()
	}
}

// Mount registers the status document. It must be mounted after Gate.
func Mount(router gin.IRoutes, rt *Runtime) {
	router.GET(runtimeStatusPath, rt.ServeStatus)
}

// ServeStatus writes the supervisor document. The body is small and is not
// a streamed user response.
func (rt *Runtime) ServeStatus(c *gin.Context) {
	payload, err := common.Marshal(rt.Status())
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Data(http.StatusOK, "application/json", payload)
}
