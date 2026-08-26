package api

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/managementasset"
)

// allowedAssetExt whitelists extensions the panel may reference from its
// static directory. Anything else (including management.yaml backups, auth
// material, or logs living in the same tree) is never served.
var allowedAssetExt = map[string]string{
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".ico":  "image/x-icon",
}

// serveManagementAsset serves one fixed file from the panel static dir.
func (s *Server) serveManagementAsset(name string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := s.managementPanelFile(); !ok {
			c.Status(http.StatusNotFound)
			return
		}
		ext := strings.ToLower(filepath.Ext(name))
		ct, ok := allowedAssetExt[ext]
		if !ok {
			c.Status(http.StatusNotFound)
			return
		}
		full := filepath.Join(managementasset.StaticDir(s.configFilePath), name)
		data, err := os.ReadFile(full)
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		c.Header("Cache-Control", "public, max-age=3600")
		c.Data(http.StatusOK, ct, data)
	}
}

// serveManagementProviderAsset serves provider icon files under
// providers/<name> from the panel static dir. Path cleaning plus an extension
// whitelist keep the handler from escaping providers/ or serving secrets.
func (s *Server) serveManagementProviderAsset(c *gin.Context) {
	if _, ok := s.managementPanelFile(); !ok {
		c.Status(http.StatusNotFound)
		return
	}
	raw := c.Param("path")
	name := path.Base(strings.TrimPrefix(raw, "/"))
	if name == "" || name == "." || name == "/" {
		c.Status(http.StatusNotFound)
		return
	}
	ext := strings.ToLower(filepath.Ext(name))
	ct, ok := allowedAssetExt[ext]
	if !ok {
		c.Status(http.StatusNotFound)
		return
	}
	full := filepath.Join(managementasset.StaticDir(s.configFilePath), "providers", filepath.FromSlash(name))
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		c.Status(http.StatusNotFound)
		return
	}
	data, err := os.ReadFile(full)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("Cache-Control", "public, max-age=86400")
	c.Data(http.StatusOK, ct, data)
}
