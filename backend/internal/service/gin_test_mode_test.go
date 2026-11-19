package service

import "github.com/gin-gonic/gin"

// Set Gin test mode once at package init so concurrent t.Parallel() tests never
// race on gin's package-global mode variable under `go test -race`.
func init() {
	gin.SetMode(gin.TestMode)
}
