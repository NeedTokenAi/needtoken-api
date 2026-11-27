package router

import (
	"bytes"
	"embed"
	"html"
	"net/http"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-contrib/gzip"
	"github.com/gin-gonic/gin"
)

// WebAssets holds the embedded dashboard frontend assets.
type WebAssets struct {
	BuildFS   embed.FS
	IndexPage []byte
}

func renderWebIndexPage(indexPage []byte) []byte {
	logo := strings.TrimSpace(common.Logo)
	if logo == "" {
		return indexPage
	}
	parsed, err := url.Parse(logo)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return indexPage
	}
	escaped := html.EscapeString(logo)
	const defaultIconTag = `<link rel="icon" type="image/svg+xml" href="/icon.svg" />`
	if !bytes.Contains(indexPage, []byte(defaultIconTag)) {
		return indexPage
	}
	customIconTag := `<link rel="icon" href="` + escaped + `" />`
	replaced := bytes.Replace(indexPage, []byte(defaultIconTag), []byte(customIconTag), 1)
	replaced = bytes.Replace(replaced, []byte(`<meta property="og:image" content="/logo.png" />`), []byte(`<meta property="og:image" content="`+escaped+`" />`), 1)
	return replaced
}

func SetWebRouter(router *gin.Engine, assets WebAssets, pluginDispatcher gin.HandlerFunc) {
	frontendFS := common.EmbedFolder(assets.BuildFS, "web/dist")
	compress := gzip.Gzip(gzip.DefaultCompression)

	router.NoRoute(
		pluginDispatcher,
		middleware.RouteTag("web"),
		func(c *gin.Context) {
			// ServeFrontendFiles sends build files already compressed.
			if !frontendFS.Exists("/", c.Request.URL.Path) {
				compress(c)
			}
		},
		middleware.AccessTokenAudit(),
		middleware.GlobalWebRateLimit(frontendFS),
		middleware.Cache(),
		middleware.ServeFrontendFiles(frontendFS),
		func(c *gin.Context) {
			if strings.HasPrefix(c.Request.RequestURI, "/v1") || strings.HasPrefix(c.Request.RequestURI, "/api") || strings.HasPrefix(c.Request.RequestURI, "/assets") {
				controller.RelayNotFound(c)
				return
			}
			c.Header("Cache-Control", "no-cache")
			c.Data(http.StatusOK, "text/html; charset=utf-8", renderWebIndexPage(assets.IndexPage))
		},
	)
}
