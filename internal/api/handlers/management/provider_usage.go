package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
)

// GetAntigravityCredits returns the in-memory Antigravity credits balance per
// auth ID. This is real provider quota data observed by the executor while
// refreshing tokens / executing requests.
func (h *Handler) GetAntigravityCredits(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"credits": executor.AntigravityCreditsSnapshot()})
}
